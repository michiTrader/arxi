// Package contextprep freezes the exact provider-neutral presentation used by a model child.
package contextprep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/michiTrader/arxi/internal/compaction"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

const (
	Schema          = "arxi.prepared-context/v1"
	RequestSchema   = "arxi.context-prepare/v1"
	PreparerVersion = "arxi.context-preparer/v2"
)

// Measurement records how much context the presentation uses, per layer, and
// against which limits. Every figure counts runes of the canonical JSON
// encoding; that is an upper-bound estimate, never an exact tokenizer count,
// and unknown limits stay absent instead of being invented. The total is the
// arithmetic sum of the layers: each layer's JSON framing makes the sum a
// conservative bound on the whole presentation, which is the honest direction
// for a pressure figure.
type Measurement struct {
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
	Mode           string `json:"mode"`
	StaticTokens   int    `json:"static_tokens"`
	SummaryTokens  int    `json:"summary_tokens,omitempty"`
	VerbatimTokens int    `json:"verbatim_tokens,omitempty"`
	InputTokens    int    `json:"input_tokens"`
	TotalTokens    int    `json:"total_tokens"`
	InputLimit     int    `json:"input_limit,omitempty"`
	OutputLimit    int    `json:"output_limit,omitempty"`
}

// OverflowDecision records the outcome of pressure: whether a known limit was
// exceeded, which declared mode governed, and — when compaction ran — which
// verified artifact the presentation shed material into.
type OverflowDecision struct {
	Exceeded         bool   `json:"exceeded"`
	Mode             string `json:"mode,omitempty"`
	Compacted        bool   `json:"compacted"`
	CompactionDigest string `json:"compaction_digest,omitempty"`
}

type MemoryReceipt struct {
	Kind               string `json:"kind"`
	EffectiveConfigSHA string `json:"effective_config_sha"`
	ContentDigest      string `json:"content_digest"`
}

// Route binds the presentation to the destination it was prepared for. The
// spec requires the artifact to prove not just what was presented but to which
// model under which tool schema: without it, a recovered presentation could be
// replayed against a different model and the artifact would still look valid.
// Credentials never appear here — only the non-secret route identity.
type Route struct {
	Provider             string `json:"provider,omitempty"`
	Protocol             string `json:"protocol,omitempty"`
	Model                string `json:"model,omitempty"`
	BaseURL              string `json:"base_url,omitempty"`
	ToolSchemaVersion    string `json:"tool_schema_version,omitempty"`
	ContextPolicyVersion string `json:"context_policy_version,omitempty"`
}

type Artifact struct {
	Schema             string               `json:"schema"`
	ContextID          string               `json:"context_id"`
	RunID              string               `json:"run_id"`
	ParentWorkID       string               `json:"parent_work_id"`
	Subject            string               `json:"subject_agent"`
	SourceThroughSeq   int64                `json:"source_through_seq"`
	EffectiveConfigSHA string               `json:"effective_config_sha,omitempty"`
	Transcript         transcript.Artifact  `json:"transcript"`
	PreparerVersion    string               `json:"preparer_version"`
	Route              Route                `json:"route"`
	Messages           []turn.Message       `json:"messages"`
	Measurement        Measurement          `json:"token_measurement"`
	Overflow           OverflowDecision     `json:"overflow_decision"`
	Compaction         *compaction.Artifact `json:"compaction,omitempty"`
	MemoryReceipts     []MemoryReceipt      `json:"memory_use_receipts"`
	ContentDigest      string               `json:"content_digest"`
	PresentationDigest string               `json:"presentation_digest"`
}

// Request names one preparation commission. It is a struct rather than a
// positional list because every field is an identity that must be recorded
// exactly, and a transposed pair of strings would be invisible at the call
// site and wrong in the artifact.
type Request struct {
	ContextID          string
	RunID              string
	ParentWorkID       string
	EffectiveConfigSHA string
	Effect             kernel.SpawnTurn
	History            transcript.Artifact
	Route              Route
	OutputLimit        int
	Generator          compaction.Generator
}

// OverflowError marks every failure on the overflow path — an unusable mode, a
// generator error, a budget that cannot be satisfied — so the durable barrier
// can record the failure class without importing this package.
type OverflowError struct{ Err error }

func (e *OverflowError) Error() string { return e.Err.Error() }

func (e *OverflowError) Unwrap() error { return e.Err }

func (e *OverflowError) PreparationClass() string { return "compaction" }

// Prepare freezes one presentation from a projected transcript. It is a pure
// function of its request: the same confirmed history, route and effect always
// yield the same bytes, which is what lets the durable barrier reproduce the
// artifact it committed.
func Prepare(req Request) (Artifact, error) {
	contextID, runID, history, effect := req.ContextID, req.RunID, req.History, req.Effect
	artifact := Artifact{Schema: Schema, ContextID: contextID, RunID: runID, ParentWorkID: req.ParentWorkID,
		Subject: effect.Agent, SourceThroughSeq: history.SourceThroughSeq, EffectiveConfigSHA: req.EffectiveConfigSHA,
		Transcript: history, PreparerVersion: PreparerVersion, Route: req.Route, MemoryReceipts: []MemoryReceipt{}}
	static := staticMessages(effect.Context)
	prior := transcriptMessages(history.Items)
	trailing := inputMessages(effect.Context, prior)
	full := joinMessages(static, prior, trailing)
	// Encoding is validated once here: every layer measurement marshals a
	// subset of these exact message elements, so no layer measure can fail
	// after the whole presentation was proven encodable.
	if _, err := json.Marshal(full); err != nil {
		return Artifact{}, fmt.Errorf("encode prepared presentation: %w", err)
	}
	limit := effect.Context.MaxTokens
	switch {
	case limit <= 0 || pressure(static, nil, prior, trailing) <= limit:
		// An unknown limit means unknown pressure: no budget is derived, no
		// compaction runs, and the limit fields stay absent rather than invented.
		artifact.Messages = full
		artifact.Measurement = measurement(static, nil, prior, trailing, limit)
		artifact.Measurement.OutputLimit = req.OutputLimit
	default:
		presented, selected, m, err := compact(contextID, runID, effect, history, limit, static, prior, trailing, req.Generator)
		if err != nil {
			return Artifact{}, err
		}
		artifact.Messages = presented
		artifact.Compaction = &selected
		artifact.Overflow = OverflowDecision{Exceeded: true, Mode: effect.Context.OnOverflow,
			Compacted: true, CompactionDigest: selected.ContentDigest}
		m.OutputLimit = req.OutputLimit
		artifact.Measurement = m
	}
	if memory := strings.TrimSpace(effect.Context.Memory); memory != "" {
		artifact.MemoryReceipts = append(artifact.MemoryReceipts, MemoryReceipt{Kind: "frozen_context_memory",
			EffectiveConfigSHA: req.EffectiveConfigSHA, ContentDigest: digest("arxi.context-memory/v1", []byte(memory))})
	}
	presentation, err := json.Marshal(artifact.Messages)
	if err != nil {
		return Artifact{}, fmt.Errorf("encode prepared presentation: %w", err)
	}
	artifact.PresentationDigest = digest("arxi.context-presentation/v1", presentation)
	// The route joins the content digest because "what was presented" is not
	// complete without "to whom": the same messages sent to a different model
	// under a different tool schema are a different presentation.
	content, err := json.Marshal(struct {
		TranscriptDigest string          `json:"transcript_digest"`
		Route            Route           `json:"route"`
		Messages         []turn.Message  `json:"messages"`
		Memory           []MemoryReceipt `json:"memory"`
		CompactionDigest string          `json:"compaction_digest,omitempty"`
	}{history.ContentDigest, artifact.Route, artifact.Messages, artifact.MemoryReceipts, artifact.Overflow.CompactionDigest})
	if err != nil {
		return Artifact{}, fmt.Errorf("encode prepared content: %w", err)
	}
	artifact.ContentDigest = digest("arxi.context-content/v1", content)
	return artifact, nil
}

// compact runs the overflow path: select, present, re-measure and verify.
// Selection is iterated against the measured presentation because the summary
// framing and JSON encoding add weight the item-level cost model cannot see
// in advance: each round shrinks the verbatim budget by the measured
// overshoot, deterministically, until the presentation fits or the window is
// already minimal and the limit cannot be met. Any failure here is an
// OverflowError — compaction that quietly degraded into truncation, or a mode
// the runtime does not implement, must surface as a terminal preparation
// failure, never as a shorter silent prompt.
func compact(contextID, runID string, effect kernel.SpawnTurn, history transcript.Artifact, limit int,
	static, prior, trailing []turn.Message, generator compaction.Generator) ([]turn.Message, compaction.Artifact, Measurement, error) {
	if effect.Context.OnOverflow != "summarize" {
		return nil, compaction.Artifact{}, Measurement{}, &OverflowError{fmt.Errorf(
			"context pressure %d exceeds input limit %d and on_overflow %q is not summarize: unknown modes fail closed instead of guessing a policy",
			pressure(static, nil, prior, trailing), limit, effect.Context.OnOverflow)}
	}
	budgets := compaction.DeriveBudgets(limit)
	request := compaction.Request{ContextID: contextID, RunID: runID, Subject: effect.Agent,
		SourceThroughSeq: history.SourceThroughSeq, Budgets: budgets, Items: history.Items}
	for {
		selected, err := generator.Compact(request)
		if err != nil {
			return nil, compaction.Artifact{}, Measurement{}, &OverflowError{fmt.Errorf("compact context %s: %w", contextID, err)}
		}
		summary := summaryMessage(selected)
		window := messagesForItems(history.Items, selected.Window)
		retained := messagesForItems(history.Items, selected.Retained)
		presented := joinMessages(static, summary, retained, window, trailing)
		afterTotal := pressure(static, summary, joinMessages(retained, window), trailing)
		if afterTotal <= limit {
			selected.BeforeTokens = pressure(static, nil, prior, trailing)
			selected.AfterTokens = afterTotal
			if err := compaction.Finalize(&selected); err != nil {
				return nil, compaction.Artifact{}, Measurement{}, &OverflowError{fmt.Errorf("finalize compaction for context %s: %w", contextID, err)}
			}
			if err := compaction.Verify(selected, history.Items); err != nil {
				return nil, compaction.Artifact{}, Measurement{}, &OverflowError{fmt.Errorf("verify compaction for context %s: %w", contextID, err)}
			}
			m := measurement(static, summary, joinMessages(retained, window), trailing, limit)
			return presented, selected, m, nil
		}
		if request.Budgets.Verbatim <= 0 {
			return nil, compaction.Artifact{}, Measurement{}, &OverflowError{fmt.Errorf(
				"compaction cannot bring context %s within limit %d: the presentation floor (static %d, summary %d, minimal window %d, input %d) already exceeds the limit, so no selection can relieve the pressure",
				contextID, limit, measurementOf(static), measurementOf(summary), measurementOf(window), measurementOf(trailing))}
		}
		request.Budgets.Verbatim -= afterTotal - limit
		if request.Budgets.Verbatim < 0 {
			request.Budgets.Verbatim = 0
		}
	}
}

// pressure sums the layer measures under one identity: runes of the canonical
// JSON encoding of each message slice. The sum is a conservative bound on the
// joined presentation, so pressure is never understated.
func pressure(static, summary, verbatim, trailing []turn.Message) int {
	return measurementOf(static) + measurementOf(summary) + measurementOf(verbatim) + measurementOf(trailing)
}

func measurement(static, summary, verbatim, trailing []turn.Message, limit int) Measurement {
	return Measurement{Implementation: "unicode-rune-upper-bound", Version: "v1", Mode: "estimate",
		StaticTokens: measurementOf(static), SummaryTokens: measurementOf(summary),
		VerbatimTokens: measurementOf(verbatim), InputTokens: measurementOf(trailing),
		TotalTokens: pressure(static, summary, verbatim, trailing), InputLimit: limit}
}

// measurementOf counts runes of the canonical JSON encoding, the same identity
// the layer pressures use. Callers must have proven the presentation encodable
// before measuring, which makes the panic below unreachable.
func measurementOf(messages []turn.Message) int {
	if len(messages) == 0 {
		return 0
	}
	body, err := json.Marshal(messages)
	if err != nil {
		panic(fmt.Sprintf("contextprep: encode messages for measurement: %v", err))
	}
	return utf8.RuneCount(body)
}

// summaryMessage renders the extractive claims as one user message. The
// artifact carries the citations; the presentation stays lean and states the
// claims, which are proven excerpts of their sources either way.
func summaryMessage(selected compaction.Artifact) []turn.Message {
	if len(selected.Claims) == 0 {
		return nil
	}
	var body strings.Builder
	body.WriteString("[Earlier conversation compacted: extractive summary of the turns before the recent history. The compaction artifact records every citation and omission.]\n")
	for _, claim := range selected.Claims {
		body.WriteString("- ")
		body.WriteString(claim.Text)
		if claim.Incomplete {
			body.WriteString(" [incomplete]")
		}
		body.WriteString("\n")
	}
	return []turn.Message{textMessage(turn.RoleUser, strings.TrimRight(body.String(), "\n"))}
}

// inputMessages closes the presentation with what this turn adds: the
// activation causes the reducer computed. They are not history — they state
// what changed since the member last ran, which is the difference between a
// member that knows it was steered and one that re-reads the conversation and
// guesses. A turn with no causes still needs a user message, because many
// providers reject a system-only conversation with a 400 that would surface as
// a domain error on a turn that was merely empty.
func inputMessages(context kernel.ContextSpec, prior []turn.Message) []turn.Message {
	var user strings.Builder
	writeSection(&user, "Why you were activated", context.Cause)
	if text := strings.TrimSpace(user.String()); text != "" {
		return []turn.Message{textMessage(turn.RoleUser, text)}
	}
	if len(prior) > 0 && prior[len(prior)-1].Role == turn.RoleUser {
		return nil
	}
	return []turn.Message{textMessage(turn.RoleUser, "Proceed.")}
}

// staticMessages renders the frozen framing: what the operator authored, and
// what memory supplied, as two different kinds of thing.
//
// The system message carries only operator-authored material — identity,
// situation, shared instructions. Memory occupies its own user-role message
// (ADR-0020), because the system channel is a structural grant of authority
// and memory content is data. It previously shared the system message, which
// meant a Phase 7 record would have arrived carrying the authority of "You are
// backend." no matter what its recorded provenance said.
//
// The user role rather than a second system message, and this is the part that
// is not obvious from here: internal/provider/anthropic.go concatenates EVERY
// system message into one `System` string. A second system message would look
// separated in this function and arrive fused on the wire — the appearance of
// a boundary with none of the effect. internal/provider/memory_channel_test.go
// pins that, because it is invisible at this layer.
//
// Both messages stay in the STATIC layer. Memory is still frozen blueprint
// prose measured against the static budget; only its channel changed. Moving
// it to another layer would have silently re-cut the budget quarters while
// claiming to be a channel change.
func staticMessages(context kernel.ContextSpec) []turn.Message {
	var system strings.Builder
	if context.Identity != "" {
		system.WriteString("You are ")
		system.WriteString(context.Identity)
		system.WriteString(".\n")
	}
	writeSection(&system, "Situation", context.Situation)
	writeSection(&system, "Shared", context.Shared)
	var messages []turn.Message
	if text := strings.TrimSpace(system.String()); text != "" {
		messages = append(messages, textMessage(turn.RoleSystem, text))
	}
	if memory := strings.TrimSpace(context.Memory); memory != "" {
		messages = append(messages, textMessage(turn.RoleUser, memoryMessageText(memory)))
	}
	return messages
}

// memoryMessageText labels the memory message for a reader without pretending
// the label is a security boundary.
//
// The prefix is a courtesy to the model, not a control: a record can contain
// the same words, which is exactly why ADR-0020 discarded delimiter-based
// marking inside the system message and moved the channel instead. The
// guarantee comes from the role; this text only makes the message legible.
func memoryMessageText(memory string) string {
	var b strings.Builder
	b.WriteString("Memory:\n")
	b.WriteString(memory)
	b.WriteString("\n")
	return b.String()
}

func transcriptMessages(items []transcript.Item) []turn.Message {
	messages := make([]turn.Message, 0, len(items))
	for _, item := range items {
		if message, ok := item.Message(); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

// messagesForItems renders exactly the named items, in transcript order, so
// the verbatim window and retained slots present the same bytes the full
// history would have presented for those items.
func messagesForItems(items []transcript.Item, ids []string) []turn.Message {
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	messages := make([]turn.Message, 0, len(ids))
	for _, item := range items {
		if !selected[item.ID] {
			continue
		}
		if message, ok := item.Message(); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

func textMessage(role turn.Role, text string) turn.Message {
	return turn.Message{Role: role, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: text}}}
}

func joinMessages(groups ...[]turn.Message) []turn.Message {
	var joined []turn.Message
	for _, group := range groups {
		joined = append(joined, group...)
	}
	return joined
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
