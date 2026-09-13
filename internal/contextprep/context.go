// Package contextprep freezes the exact provider-neutral presentation used by a model child.
package contextprep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

const (
	Schema          = "arxi.prepared-context/v1"
	RequestSchema   = "arxi.context-prepare/v1"
	PreparerVersion = "arxi.context-preparer/v1"
)

type Measurement struct {
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
	Mode           string `json:"mode"`
	InputTokens    int    `json:"input_tokens"`
	InputLimit     int    `json:"input_limit,omitempty"`
	OutputLimit    int    `json:"output_limit,omitempty"`
}

type MemoryReceipt struct {
	Kind               string `json:"kind"`
	EffectiveConfigSHA string `json:"effective_config_sha"`
	ContentDigest      string `json:"content_digest"`
}

type Artifact struct {
	Schema             string              `json:"schema"`
	ContextID          string              `json:"context_id"`
	RunID              string              `json:"run_id"`
	ParentWorkID       string              `json:"parent_work_id"`
	Subject            string              `json:"subject_agent"`
	SourceThroughSeq   int64               `json:"source_through_seq"`
	EffectiveConfigSHA string              `json:"effective_config_sha,omitempty"`
	Transcript         transcript.Artifact `json:"transcript"`
	PreparerVersion    string              `json:"preparer_version"`
	Messages           []turn.Message      `json:"messages"`
	Measurement        Measurement         `json:"token_measurement"`
	MemoryReceipts     []MemoryReceipt     `json:"memory_use_receipts"`
	ContentDigest      string              `json:"content_digest"`
	PresentationDigest string              `json:"presentation_digest"`
}

func Prepare(contextID, runID, parentWorkID, effectiveSHA string, effect kernel.SpawnTurn, history transcript.Artifact) (Artifact, error) {
	artifact := Artifact{Schema: Schema, ContextID: contextID, RunID: runID, ParentWorkID: parentWorkID,
		Subject: effect.Agent, SourceThroughSeq: history.SourceThroughSeq, EffectiveConfigSHA: effectiveSHA,
		Transcript: history, PreparerVersion: PreparerVersion, MemoryReceipts: []MemoryReceipt{}}
	artifact.Messages = staticMessages(effect.Context)
	artifact.Messages = append(artifact.Messages, transcriptMessages(history.Items)...)
	if len(artifact.Messages) == 0 || artifact.Messages[len(artifact.Messages)-1].Role != turn.RoleUser {
		artifact.Messages = append(artifact.Messages, textMessage(turn.RoleUser, "Proceed."))
	}
	if memory := strings.TrimSpace(effect.Context.Memory); memory != "" {
		artifact.MemoryReceipts = append(artifact.MemoryReceipts, MemoryReceipt{Kind: "frozen_context_memory",
			EffectiveConfigSHA: effectiveSHA, ContentDigest: digest("arxi.context-memory/v1", []byte(memory))})
	}
	presentation, err := json.Marshal(artifact.Messages)
	if err != nil {
		return Artifact{}, fmt.Errorf("encode prepared presentation: %w", err)
	}
	artifact.PresentationDigest = digest("arxi.context-presentation/v1", presentation)
	artifact.Measurement = Measurement{Implementation: "unicode-rune-upper-bound", Version: "v1", Mode: "estimate",
		InputTokens: utf8.RuneCount(presentation), InputLimit: effect.Context.MaxTokens}
	content, err := json.Marshal(struct {
		TranscriptDigest string          `json:"transcript_digest"`
		Messages         []turn.Message  `json:"messages"`
		Memory           []MemoryReceipt `json:"memory"`
	}{history.ContentDigest, artifact.Messages, artifact.MemoryReceipts})
	if err != nil {
		return Artifact{}, fmt.Errorf("encode prepared content: %w", err)
	}
	artifact.ContentDigest = digest("arxi.context-content/v1", content)
	return artifact, nil
}

func staticMessages(context kernel.ContextSpec) []turn.Message {
	var system strings.Builder
	if context.Identity != "" {
		system.WriteString("You are ")
		system.WriteString(context.Identity)
		system.WriteString(".\n")
	}
	writeSection(&system, "Situation", context.Situation)
	if memory := strings.TrimSpace(context.Memory); memory != "" {
		if system.Len() > 0 {
			system.WriteString("\n")
		}
		system.WriteString("Memory:\n")
		system.WriteString(memory)
		system.WriteString("\n")
	}
	writeSection(&system, "Shared", context.Shared)
	var messages []turn.Message
	if text := strings.TrimSpace(system.String()); text != "" {
		messages = append(messages, textMessage(turn.RoleSystem, text))
	}
	return messages
}

func transcriptMessages(items []transcript.Item) []turn.Message {
	messages := make([]turn.Message, 0, len(items))
	for _, item := range items {
		switch item.Kind {
		case transcript.UserInput:
			messages = append(messages, turn.Message{Role: turn.RoleUser, Content: item.Content})
		case transcript.ModelOutput:
			messages = append(messages, turn.Message{Role: turn.RoleAssistant, Content: item.Content})
		case transcript.ToolCall:
			if item.Call != nil && item.Call.ID != "" {
				call := *item.Call
				messages = append(messages, turn.Message{Role: turn.RoleAssistant,
					Content: []turn.ContentBlock{{Type: turn.BlockToolCall, ToolCall: &call}}})
			}
		case transcript.ToolResult:
			if item.Result != nil && item.Result.CallID != "" {
				result := *item.Result
				messages = append(messages, turn.Message{Role: turn.RoleTool,
					Content: []turn.ContentBlock{{Type: turn.BlockToolResult, ToolResult: &result}}})
			}
		case transcript.HumanDecision:
			if item.Decision != "" {
				messages = append(messages, textMessage(turn.RoleUser, "Human decision: "+item.Decision))
			}
		}
	}
	return messages
}

func textMessage(role turn.Role, text string) turn.Message {
	return turn.Message{Role: role, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: text}}}
}

func writeSection(builder *strings.Builder, title string, values []string) {
	if len(values) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString(title)
	builder.WriteString(":\n")
	for _, value := range values {
		if value != "" {
			builder.WriteString("- ")
			builder.WriteString(value)
			builder.WriteString("\n")
		}
	}
}

func digest(domain string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
