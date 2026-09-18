package contextprep

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/compaction"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

// testRoute is the destination every preparation test prepares for. The
// artifact binds it, so a test that forgot it would silently assert against
// a presentation bound to no model at all.
var testRoute = Route{Provider: "fake", Protocol: "openai.chat_completions", Model: "test-model",
	ToolSchemaVersion: "arxi.tools/v1", ContextPolicyVersion: "arxi.context-prep/v1"}

// projectedHistory builds a real transcript artifact from synthetic confirmed
// events, so preparation tests exercise the item shapes production produces.
func projectedHistory(t *testing.T, runID string, events []kernel.Event) transcript.Artifact {
	t.Helper()
	history, err := transcript.Project(runID, "backend", "cfg", events, events[len(events)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

func TestPrepareOrdersStaticContextBeforeCanonicalHistory(t *testing.T) {
	history := transcript.Artifact{Schema: transcript.Schema, RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 7, ContentDigest: "history", Items: []transcript.Item{
			{Kind: transcript.UserInput, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "continue"}}},
			{Kind: transcript.ModelOutput, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "prior answer"}}},
		}}
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend", Memory: "frozen fact", MaxTokens: 12000}}
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: testRoute, Generator: compaction.Extractive{}})
	if err != nil {
		t.Fatal(err)
	}
	// Five messages, not four: this fixture supplies memory, and under
	// ADR-0020 memory is its own user-role message instead of a paragraph
	// inside the system message. The guarantee under test is unchanged --
	// the frozen static layer still prefixes canonical history, and the
	// presentation still ends on a user message so provider requests stay
	// valid -- so the sequence is asserted rather than just the count.
	wantRoles := []turn.Role{
		turn.RoleSystem,    // operator-authored framing
		turn.RoleUser,      // memory, on the data channel
		turn.RoleUser,      // history: "continue"
		turn.RoleAssistant, // history: "prior answer"
		turn.RoleUser,      // this turn's input
	}
	if len(artifact.Messages) != len(wantRoles) {
		t.Fatalf("prepared roles = %#v: stable context must prefix canonical history and a final user instruction must keep provider requests valid", artifact.Messages)
	}
	for i, want := range wantRoles {
		if artifact.Messages[i].Role != want {
			t.Fatalf("prepared message %d role = %q, want %q (full presentation %#v): stable context must prefix canonical history and a final user instruction must keep provider requests valid",
				i, artifact.Messages[i].Role, want, artifact.Messages)
		}
	}
	if artifact.Measurement.Mode != "estimate" {
		t.Fatalf("measurement mode = %q: a rune upper bound is not an exact tokenizer and must never claim otherwise", artifact.Measurement.Mode)
	}
	if len(artifact.MemoryReceipts) != 1 || artifact.MemoryReceipts[0].EffectiveConfigSHA != "cfg" {
		t.Fatalf("memory receipts = %#v: frozen memory must identify the configuration that supplied it", artifact.MemoryReceipts)
	}
	if artifact.ContentDigest == artifact.PresentationDigest || artifact.ContentDigest == "" || artifact.PresentationDigest == "" {
		t.Fatalf("content digest %q, presentation digest %q: semantic source identity and exact presented bytes require distinct bindings", artifact.ContentDigest, artifact.PresentationDigest)
	}
	if artifact.PreparerVersion != PreparerVersion || PreparerVersion != "arxi.context-preparer/v2" {
		t.Fatalf("preparer version = %q: measured compaction is a v2 preparation contract", artifact.PreparerVersion)
	}
	if artifact.Overflow.Exceeded || artifact.Overflow.Compacted || artifact.Compaction != nil {
		t.Fatalf("overflow = %#v: pressure under a known limit must not claim compaction", artifact.Overflow)
	}
	if artifact.Measurement.TotalTokens != artifact.Measurement.StaticTokens+artifact.Measurement.VerbatimTokens+artifact.Measurement.InputTokens {
		t.Fatalf("measurement = %#v: the total is the sum of the layers or per-layer accounting proves nothing", artifact.Measurement)
	}
	if artifact.Measurement.InputLimit != 12000 {
		t.Fatalf("input limit = %d: a known limit must be recorded beside the pressure it governs", artifact.Measurement.InputLimit)
	}
}

// TestPrepareTellsTheMemberWhyItWasActivated protects the activation causes.
// The reducer computes them for every turn, and they are the only statement of
// what changed since the member last ran. A presentation that shows history and
// then says "Proceed." leaves the member to guess whether it was steered,
// answered, unblocked or merely re-run.
func TestPrepareTellsTheMemberWhyItWasActivated(t *testing.T) {
	history := projectedHistory(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "build it"}},
		{Seq: 2, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "first pass done"}},
	})
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend",
		Cause: []string{"reviewer replied to your question", "stage timer fired"}}}
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: testRoute, Generator: compaction.Extractive{}})
	if err != nil {
		t.Fatal(err)
	}
	last := artifact.Messages[len(artifact.Messages)-1]
	if last.Role != turn.RoleUser {
		t.Fatalf("last role = %q: the activation causes are this turn's input and must close the presentation", last.Role)
	}
	text := last.Content[0].Text
	for _, cause := range effect.Context.Cause {
		if !strings.Contains(text, cause) {
			t.Fatalf("final message %q omits cause %q: a member that is not told why it was activated cannot act on what changed", text, cause)
		}
	}
	if strings.Contains(text, "Proceed.") {
		t.Fatalf("final message %q: a generic instruction must not replace the causes the reducer computed", text)
	}
	if artifact.Measurement.InputTokens == 0 {
		t.Fatalf("input layer measured 0 tokens while causes were presented: the input layer must measure what this turn actually adds")
	}
}

func TestPrepareMeasuresUnknownLimitAsAbsent(t *testing.T) {
	history := projectedHistory(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "anything"}},
	})
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend"}}
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: testRoute, Generator: compaction.Extractive{}})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Measurement.InputLimit != 0 {
		t.Fatalf("input limit = %d: an unknown limit must stay absent instead of being invented", artifact.Measurement.InputLimit)
	}
	if artifact.Overflow.Exceeded {
		t.Fatalf("overflow exceeded without a limit: pressure cannot be claimed against a limit nobody knows")
	}
}

func TestPrepareCompactsOverLimitUnderSummarize(t *testing.T) {
	history := projectedHistory(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "keep the billing migration reversible at every step and report the exact revert path"}},
		{Seq: 2, ID: "call-1", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c1", "tool": "read", "args": map[string]any{"path": "migrations/001.sql"}}},
		{Seq: 3, ID: "result-1", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c1", "result": "ALTER TABLE billing ADD COLUMN reverted BOOLEAN NOT NULL DEFAULT FALSE"}},
		{Seq: 4, ID: "answer-1", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "The first migration adds the revert column with a safe default."}},
		{Seq: 5, ID: "prompt-2", Type: kernel.RunPrompt, Payload: map[string]any{"text": "now verify the backfill job respects the reverted flag before touching production rows"}},
		{Seq: 6, ID: "call-2", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c2", "tool": "read", "args": map[string]any{"path": "jobs/backfill.go"}}},
		{Seq: 7, ID: "result-2", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c2", "result": "if row.Reverted { continue } // backfill skips reverted rows"}},
		{Seq: 8, ID: "call-3", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c3", "tool": "plan", "args": map[string]any{"step": "dry run"}}},
		{Seq: 9, ID: "result-3", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c3", "result": "dry run completed: 0 rows touched, 312 would update"}},
		{Seq: 10, ID: "answer-2", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "The backfill respects the reverted flag; the dry run touched nothing and would update 312 rows."}},
	})
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend",
		MaxTokens: 900, OnOverflow: "summarize"}}
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: testRoute, Generator: compaction.Extractive{}})
	if err != nil {
		t.Fatal(err)
	}
	if !artifact.Overflow.Exceeded || artifact.Overflow.Mode != "summarize" || !artifact.Overflow.Compacted {
		t.Fatalf("overflow decision = %#v: measured pressure over a known limit must be recorded with the mode that governed it", artifact.Overflow)
	}
	if artifact.Compaction == nil || artifact.Overflow.CompactionDigest != artifact.Compaction.ContentDigest {
		t.Fatalf("compaction binding = %#v: the overflow decision must name the exact artifact the presentation shed material into", artifact.Overflow)
	}
	if artifact.Measurement.TotalTokens > 900 {
		t.Fatalf("total after compaction = %d over limit 900: compaction that does not relieve pressure must fail visibly, not ship long", artifact.Measurement.TotalTokens)
	}
	if artifact.Measurement.TotalTokens >= artifact.Compaction.BeforeTokens {
		t.Fatalf("after %d >= before %d: compaction must record accounting that actually relieved pressure", artifact.Measurement.TotalTokens, artifact.Compaction.BeforeTokens)
	}
	full, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := json.Marshal(artifact.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if string(full) != string(embedded) {
		t.Fatalf("transcript changed under compaction: the canonical transcript is evidence and must survive byte-identical")
	}
	if err := compaction.Verify(*artifact.Compaction, history.Items); err != nil {
		t.Fatalf("committed compaction failed its own gate: %v", err)
	}
	summaryFound := false
	for _, message := range artifact.Messages {
		for _, block := range message.Content {
			if block.Type == turn.BlockText && strings.Contains(block.Text, "[Earlier conversation compacted") {
				summaryFound = true
			}
		}
	}
	if !summaryFound {
		t.Fatalf("no compaction summary in the presentation: %#v", artifact.Messages)
	}
}

func TestPrepareFailsClosedOnUnknownOverflowMode(t *testing.T) {
	history := projectedHistory(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": strings.Repeat("pressure this history well beyond the tiny limit ", 10)}},
	})
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend",
		MaxTokens: 60, OnOverflow: "truncate"}}
	_, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: testRoute, Generator: compaction.Extractive{}})
	var overflow *OverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("error = %v: an unsupported overflow mode must fail preparation as an overflow failure, not fall back to any silent policy", err)
	}
	if overflow.PreparationClass() != "compaction" {
		t.Fatalf("failure class = %q: the durable barrier must be able to classify this as a compaction failure", overflow.PreparationClass())
	}
}

func TestPrepareFailsVisiblyWhenCompactionCannotFit(t *testing.T) {
	history := projectedHistory(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "anything"}},
	})
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{
		Identity:  "backend",
		Situation: []string{strings.Repeat("a situation line long enough that the static layer alone exceeds the tiny limit ", 4)},
		MaxTokens: 90, OnOverflow: "summarize"}}
	_, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: testRoute, Generator: compaction.Extractive{}})
	var overflow *OverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("error = %v: a static layer that alone exceeds the limit must fail visibly instead of trimming silently", err)
	}
	if !strings.Contains(overflow.Error(), "presentation floor") || !strings.Contains(overflow.Error(), "static") {
		t.Fatalf("error = %q: the failure must name the constraint that no selection can relieve", overflow.Error())
	}
}
