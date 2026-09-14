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
	// WorkID and ResponseID bind a native model_output item to the child work
	// record and provider response it was projected from. Compaction ranges and
	// omission ledgers cite items by ID, so without this binding a summary
	// could not be audited against the durable execution that produced its
	// sources. Legacy items have neither.
	WorkID     string `json:"work_id,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
	Legacy     bool   `json:"legacy,omitempty"`
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

// Message renders the item as the provider-neutral presentation message the
// preparer shows for it. The second result is false for items with nothing to
// present; compaction then always accounts them as omissions. Renderer and
// cost model share this one identity, so a window that fits its budget in
// cost also fits in the measured presentation.
func (item Item) Message() (turn.Message, bool) {
	switch item.Kind {
	case UserInput:
		return turn.Message{Role: turn.RoleUser, Content: item.Content}, true
	case ModelOutput:
		return turn.Message{Role: turn.RoleAssistant, Content: item.Content}, true
	case ToolCall:
		if item.Call != nil && item.Call.ID != "" {
			call := *item.Call
			return turn.Message{Role: turn.RoleAssistant,
				Content: []turn.ContentBlock{{Type: turn.BlockToolCall, ToolCall: &call}}}, true
		}
	case ToolResult:
		if item.Result != nil && item.Result.CallID != "" {
			result := *item.Result
			return turn.Message{Role: turn.RoleTool,
				Content: []turn.ContentBlock{{Type: turn.BlockToolResult, ToolResult: &result}}}, true
		}
	case HumanDecision:
		if item.Decision != "" {
			return turn.Message{Role: turn.RoleUser,
				Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "Human decision: " + item.Decision}}}, true
		}
	}
	return turn.Message{}, false
}

// Project consumes only the caller-supplied confirmed prefix. It never reads a
// log or current configuration, so the same bytes always project identically.
func Project(runID, subject, effectiveSHA string, events []kernel.Event, through int64) (Artifact, error) {
	artifact := Artifact{Schema: Schema, RunID: runID, Subject: subject, ProjectorVersion: ProjectorVersion,
		SourceFromSeq: 1, SourceThroughSeq: through, EffectiveConfigSHA: effectiveSHA, Items: []Item{}}
	native := nativeModelSegments(events)
	for _, event := range events {
		if event.Seq <= 0 || event.Seq > through {
			continue
		}
		artifact.SourceThroughEventID = event.ID
		if err := projectEvent(&artifact, event, subject, native); err != nil {
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

// nativeModelSegments marks which llm.response events are the final projection
// of a native turn whose exact content already survives in committed model
// child results. The boundary is the actor's previous agent.turn_done: a
// domain llm.response inside a segment that produced child results is derived
// evidence and is skipped, while a segment without children is a legacy
// text-only turn whose llm.response is the only surviving model output.
func nativeModelSegments(events []kernel.Event) map[int64]bool {
	native := map[int64]bool{}
	lastDone := map[string]int64{}
	children := map[string][]int64{}
	for _, event := range events {
		if event.Seq <= 0 {
			continue
		}
		switch event.Type {
		case kernel.AgentTurnDone:
			lastDone[event.Actor] = event.Seq
			children[event.Actor] = nil
		case kernel.ExecWorkFinished:
			if event.Str("child_kind") == "model" && event.Str("result_json") != "" {
				actor := event.Str("agent")
				children[actor] = append(children[actor], event.Seq)
			}
		case kernel.LLMResponse:
			start := lastDone[event.Actor]
			for _, seq := range children[event.Actor] {
				if seq > start && seq < event.Seq {
					native[event.Seq] = true
					break
				}
			}
		}
	}
	return native
}

func projectEvent(artifact *Artifact, event kernel.Event, subject string, native map[int64]bool) error {
	target := event.Str("to")
	// Native child results are appended by the runtime, not by the member, so
	// their subject is the agent recorded in the child payload rather than the
	// event actor.
	nativeChild := event.Type == kernel.ExecWorkFinished && event.Str("agent") == subject
	visible := nativeChild || event.Actor == subject || event.Actor == "" && target == "" || target == subject
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
		// A native segment's exact canonical content is projected from its
		// committed child results below; adding the derived llm.response text
		// would duplicate the final round inside one conversation.
		if !native[event.Seq] {
			addText(ModelOutput, event.Str("text"), true)
		}
	case kernel.ExecWorkFinished:
		if event.Str("child_kind") == "model" && event.Str("result_json") != "" && event.Str("agent") == subject {
			var response turn.Response
			if err := json.Unmarshal([]byte(event.Str("result_json")), &response); err != nil {
				return fmt.Errorf("decode native model result at seq %d: %w", event.Seq, err)
			}
			if len(response.Content) > 0 {
				artifact.Items = append(artifact.Items, Item{Kind: ModelOutput, Actor: subject,
					SourceSeq: event.Seq, SourceID: event.ID, Content: response.Content,
					WorkID: event.Str("work_id"), ResponseID: response.ID})
			}
		}
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
