// Package transcript projects confirmed run evidence into provider-neutral history.
package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

const (
	Schema           = "arxi.transcript/v1"
	ProjectorVersion = "arxi.transcript-projector/v1"
)

type Kind string

const (
	UserInput     Kind = "user_input"
	ModelOutput   Kind = "model_output"
	ToolCall      Kind = "tool_call"
	ToolResult    Kind = "tool_result"
	HumanDecision Kind = "human_decision"
)

type Item struct {
	ID          string              `json:"id"`
	Kind        Kind                `json:"kind"`
	Actor       string              `json:"actor,omitempty"`
	Audience    string              `json:"audience,omitempty"`
	SourceSeq   int64               `json:"source_seq"`
	SourceID    string              `json:"source_event_id"`
	SourceIndex int                 `json:"source_index"`
	Content     []turn.ContentBlock `json:"content,omitempty"`
	Call        *turn.ToolCall      `json:"call,omitempty"`
	Result      *turn.ToolResult    `json:"result,omitempty"`
	Decision    string              `json:"decision,omitempty"`
	Legacy      bool                `json:"legacy,omitempty"`
}

type Artifact struct {
	Schema               string `json:"schema"`
	RunID                string `json:"run_id"`
	Subject              string `json:"subject_agent"`
	ProjectorVersion     string `json:"projector_version"`
	SourceFromSeq        int64  `json:"source_from_seq"`
	SourceThroughSeq     int64  `json:"source_through_seq"`
	SourceThroughEventID string `json:"source_through_event_id"`
	EffectiveConfigSHA   string `json:"effective_config_sha,omitempty"`
	Items                []Item `json:"items"`
	ContentDigest        string `json:"content_digest"`
}

// Project consumes only the caller-supplied confirmed prefix. It never reads a
// log or current configuration, so the same bytes always project identically.
func Project(runID, subject, effectiveSHA string, events []kernel.Event, through int64) (Artifact, error) {
	artifact := Artifact{Schema: Schema, RunID: runID, Subject: subject, ProjectorVersion: ProjectorVersion,
		SourceFromSeq: 1, SourceThroughSeq: through, EffectiveConfigSHA: effectiveSHA, Items: []Item{}}
	for _, event := range events {
		if event.Seq <= 0 || event.Seq > through {
			continue
		}
		artifact.SourceThroughEventID = event.ID
		if err := projectEvent(&artifact, event, subject); err != nil {
			return Artifact{}, err
		}
	}
	body, err := json.Marshal(artifact.Items)
	if err != nil {
		return Artifact{}, fmt.Errorf("encode transcript items: %w", err)
	}
	artifact.ContentDigest = digest("arxi.transcript-content/v1", body)
	return artifact, nil
}

func projectEvent(artifact *Artifact, event kernel.Event, subject string) error {
	target := event.Str("to")
	visible := event.Actor == subject || event.Actor == "" && target == "" || target == subject
	if !visible {
		return nil
	}
	addText := func(kind Kind, text string, legacy bool) {
		if text == "" {
			return
		}
		artifact.Items = append(artifact.Items, Item{Kind: kind, Actor: event.Actor, Audience: event.Str("to"),
			SourceSeq: event.Seq, SourceID: event.ID, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: text}}, Legacy: legacy})
	}
	switch event.Type {
	case kernel.RunStarted:
		addText(UserInput, event.Str("prompt"), false)
	case kernel.RunPrompt, kernel.AgentSteered, kernel.AgentNotified:
		addText(UserInput, event.Str("text"), false)
	case kernel.LLMResponse:
		addText(ModelOutput, event.Str("text"), true)
	case kernel.ToolCall:
		call, err := toolCall(event)
		if err != nil {
			return err
		}
		artifact.Items = append(artifact.Items, Item{Kind: ToolCall, Actor: event.Actor, SourceSeq: event.Seq,
			SourceID: event.ID, Call: &call, Legacy: call.ID == ""})
	case kernel.ToolCallCompleted:
		result := turn.ToolResult{CallID: event.Str("call_id")}
		if text := event.Str("result"); text != "" {
			result.Content = []turn.ContentBlock{{Type: turn.BlockText, Text: text}}
		}
		artifact.Items = append(artifact.Items, Item{Kind: ToolResult, Actor: event.Actor, SourceSeq: event.Seq,
			SourceID: event.ID, Result: &result, Legacy: result.CallID == ""})
	case kernel.ToolCallDenied:
		addText(HumanDecision, "tool call denied by policy "+event.Str("policy"), event.Str("call_id") == "")
	case kernel.InboxReplied, kernel.AuthorizationGranted, kernel.AuthorizationDenied, kernel.AuthorizationExpired, kernel.AuthorizationConsumed:
		decision := event.Str("decision")
		if decision == "" {
			decision = string(event.Type)
		}
		artifact.Items = append(artifact.Items, Item{Kind: HumanDecision, Actor: event.Actor, SourceSeq: event.Seq,
			SourceID: event.ID, Decision: decision})
	}
	for i := range artifact.Items {
		item := &artifact.Items[i]
		if item.ID != "" {
			continue
		}
		item.SourceIndex = sourceIndex(artifact.Items, i)
		item.ID = digest("arxi.transcript-item/v1", []byte(fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%s", artifact.RunID, item.SourceSeq, item.SourceID, item.SourceIndex, item.Kind)))
	}
	return nil
}

func sourceIndex(items []Item, at int) int {
	index := 0
	for i := 0; i < at; i++ {
		if items[i].SourceSeq == items[at].SourceSeq && items[i].SourceID == items[at].SourceID {
			index++
		}
	}
	return index
}

func toolCall(event kernel.Event) (turn.ToolCall, error) {
	var args json.RawMessage
	if value, ok := event.Payload["args"]; ok {
		body, err := json.Marshal(value)
		if err != nil {
			return turn.ToolCall{}, fmt.Errorf("encode tool arguments at seq %d: %w", event.Seq, err)
		}
		args = body
	}
	return turn.ToolCall{ID: event.Str("call_id"), Name: event.Str("tool"), Arguments: args,
		ArgumentDigest: event.Str("argument_digest")}, nil
}

func digest(domain string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
