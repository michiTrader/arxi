package compaction

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
)

// buildItems projects a synthetic conversation into real transcript items so
// compaction tests exercise the same item shapes production produces.
func buildItems(t *testing.T, runID string, events []kernel.Event) []transcript.Item {
	t.Helper()
	artifact, err := transcript.Project(runID, "backend", "cfg", events, events[len(events)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	return artifact.Items
}

func TestDeriveBudgetsFixedQuarters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   int
		budgets Budgets
	}{
		{"divisible limit", 800, Budgets{Schema: BudgetSchema, Version: BudgetVersion, InputLimit: 800, Static: 200, Summary: 100, Verbatim: 400, Input: 100}},
		{"indivisible limit floors and keeps headroom", 807, Budgets{Schema: BudgetSchema, Version: BudgetVersion, InputLimit: 807, Static: 201, Summary: 100, Verbatim: 403, Input: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveBudgets(tc.limit)
			if got != tc.budgets {
				t.Fatalf("budgets = %+v, want %+v: a changed derivation would silently reinterpret every recorded budget", got, tc.budgets)
			}
			spent := got.Static + got.Summary + got.Verbatim + got.Input
			if spent > got.InputLimit {
				t.Fatalf("allocations %d exceed the limit %d: budgets are targets within the limit, not above it", spent, got.InputLimit)
			}
		})
	}
}

func TestExtractiveCompactionIsDeterministicAndAccountsForEveryItem(t *testing.T) {
	items := buildItems(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "investigate the flaky integration test and report the root cause"}},
		{Seq: 2, ID: "call", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c1", "tool": "grep", "args": map[string]any{"pattern": "timeout"}}},
		{Seq: 3, ID: "result", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c1", "result": "tests/integration_test.go:42"}},
		{Seq: 4, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "The race is in the shared temp directory. Next step: isolate it per worker."}},
	})
	budgets := DeriveBudgets(300)
	req := Request{ContextID: "context-x", RunID: "run-1", Subject: "backend", SourceThroughSeq: 4, Budgets: budgets, Items: items}
	first, err := Extractive{}.Compact(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Extractive{}.Compact(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Window == nil || first.Claims == nil || first.Omissions == nil || first.Ranges == nil {
		t.Fatalf("selection slices = %#v: nil slices would marshal differently between generator runs and break determinism", first)
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("two compactions of the same input produced different bytes: recovery could never reproduce the artifact it verifies")
	}
	seen := map[string]int{}
	for _, id := range first.Window {
		seen[id]++
	}
	for _, claim := range first.Claims {
		for _, id := range claim.Items {
			seen[id]++
		}
	}
	for _, omission := range first.Omissions {
		seen[omission.ItemID]++
	}
	if len(seen) != len(items) {
		t.Fatalf("accounted items = %d, want %d: every item must be verbatim, cited or omitted, or the summary hides material", len(seen), len(items))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("item %q accounted %d times: an item presented twice would inflate the conversation", id, count)
		}
	}
}

func TestCompactionKeepsAnchorsPresentedOrCited(t *testing.T) {
	items := buildItems(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "keep the billing migration reversible"}},
		{Seq: 2, ID: "call", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c1", "tool": "plan", "args": map[string]any{"step": 1}}},
		{Seq: 3, ID: "result", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c1", "result": "step 1 planned"}},
		{Seq: 4, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "Plan drafted."}},
	})
	artifact, err := Extractive{}.Compact(Request{ContextID: "context-x", RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 4, Budgets: DeriveBudgets(120), Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize(&artifact); err != nil {
		t.Fatal(err)
	}
	cited := map[string]bool{}
	for _, claim := range artifact.Claims {
		for _, id := range claim.Items {
			cited[id] = true
		}
	}
	opening := items[0].ID
	inWindow := false
	for _, id := range artifact.Window {
		if id == opening {
			inWindow = true
		}
	}
	if !cited[opening] && !inWindow {
		t.Fatalf("opening input neither cited nor verbatim (window %v, claims %+v): losing the goal silently is the failure compaction exists to prevent", artifact.Window, artifact.Claims)
	}
	final := items[len(items)-1].ID
	inWindow = false
	for _, id := range artifact.Window {
		if id == final {
			inWindow = true
		}
	}
	if !cited[final] && !inWindow {
		t.Fatalf("final answer neither cited nor verbatim: the last state of the conversation is the strongest continuity anchor")
	}
	for _, claim := range artifact.Claims {
		if !strings.Contains(claimText(items, claim), claim.Text) {
			t.Fatalf("claim %q not contained in its sources: extraction must quote, never paraphrase", claim.Text)
		}
	}
}

func TestVerifyRejectsInventedClaims(t *testing.T) {
	items := buildItems(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "summarize honestly"}},
		{Seq: 2, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "The budget was approved."}},
	})
	artifact, err := Extractive{}.Compact(Request{ContextID: "context-x", RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 2, Budgets: DeriveBudgets(80), Items: items})
	if err != nil {
		t.Fatal(err)
	}
	artifact.Claims = append(artifact.Claims, Claim{Text: "The budget was rejected and hidden from the user.", Items: []string{items[0].ID}})
	if err := Finalize(&artifact); err != nil {
		t.Fatal(err)
	}
	if err := Verify(artifact, items); err == nil {
		t.Fatalf("invented claim verified: a summary that asserts what its sources do not contain must fail closed before commit")
	}
}

func TestVerifyRejectsBrokenLedger(t *testing.T) {
	items := buildItems(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "long conversation needs compaction"}},
		{Seq: 2, ID: "call-1", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c1", "tool": "read", "args": map[string]any{"path": "a.go"}}},
		{Seq: 3, ID: "result-1", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c1", "result": "package a"}},
		{Seq: 4, ID: "call-2", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c2", "tool": "plan", "args": map[string]any{"step": 2}}},
		{Seq: 5, ID: "result-2", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c2", "result": "step two completed with warnings"}},
		{Seq: 6, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "Done reading."}},
	})
	artifact, err := (Extractive{}).Compact(Request{ContextID: "context-x", RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 6, Budgets: DeriveBudgets(80), Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize(&artifact); err != nil {
		t.Fatal(err)
	}
	if err := Verify(artifact, items); err != nil {
		t.Fatalf("honest artifact refused: %v", err)
	}
	if len(artifact.Omissions) == 0 {
		t.Fatalf("omissions empty: this test needs dropped items to prove the ledger catches their loss")
	}
	tampered := artifact
	tampered.Omissions = tampered.Omissions[:len(tampered.Omissions)-1]
	if err := Verify(tampered, items); err == nil {
		t.Fatalf("ledger missing an omission verified: dropped material must stay visible by identity, not vanish")
	}
}

func TestWindowNeverSplitsToolPairs(t *testing.T) {
	items := buildItems(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "use the tool"}},
		{Seq: 2, ID: "call", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c1", "tool": "read", "args": map[string]any{"path": "big-file.txt"}}},
		{Seq: 3, ID: "result", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c1", "result": strings.Repeat("line of output that alone eats the verbatim budget\n", 6)}},
		{Seq: 4, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "Read complete."}},
	})
	artifact, err := Extractive{}.Compact(Request{ContextID: "context-x", RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 4, Budgets: DeriveBudgets(160), Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize(&artifact); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind != transcript.ToolResult {
			continue
		}
		inWindow := false
		for _, id := range artifact.Window {
			if id == item.ID {
				inWindow = true
			}
		}
		if !inWindow {
			continue
		}
		paired := false
		for _, id := range artifact.Window {
			if itemsByID(items)[id].Kind == transcript.ToolCall && itemsByID(items)[id].Call.ID == item.Result.CallID {
				paired = true
			}
		}
		if !paired {
			t.Fatalf("window holds result %q without its call: a presented result without its call is a conversation no provider accepts", item.Result.CallID)
		}
	}
	if err := Verify(artifact, items); err != nil {
		t.Fatalf("generator output refused by its own gate: %v", err)
	}
}

func TestCompactionRefusesUnknownBudgetPolicy(t *testing.T) {
	items := buildItems(t, "run-1", []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "anything"}},
	})
	budgets := DeriveBudgets(100)
	budgets.Schema = "someone.else/v9"
	if _, err := (Extractive{}).Compact(Request{ContextID: "c", RunID: "run-1", Subject: "backend", Budgets: budgets, Items: items}); err == nil {
		t.Fatalf("unknown budget policy accepted: recorded budgets would be uninterpretable at audit time")
	}
}

func claimText(items []transcript.Item, claim Claim) string {
	byID := itemsByID(items)
	var sources []string
	for _, id := range claim.Items {
		sources = append(sources, itemText(byID[id]))
	}
	return strings.Join(sources, "\n")
}

func itemsByID(items []transcript.Item) map[string]transcript.Item {
	byID := make(map[string]transcript.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	return byID
}
