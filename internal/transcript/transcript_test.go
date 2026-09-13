package transcript

import (
	"encoding/json"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
)

func TestProjectPreservesConfirmedConversationAndAudience(t *testing.T) {
	events := []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "build it"}},
		{Seq: 2, ID: "private", Type: kernel.RunPrompt, Payload: map[string]any{"text": "only backend", "to": "backend"}},
		{Seq: 3, ID: "other", Type: kernel.RunPrompt, Payload: map[string]any{"text": "only reviewer", "to": "reviewer"}},
		{Seq: 4, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "done"}},
	}
	artifact, err := Project("run-1", "backend", "cfg", events, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Items) != 3 {
		t.Fatalf("projected items = %d, want 3: a subject receiving another member's private input would break conversation authority", len(artifact.Items))
	}
	if got := artifact.Items[0].Content[0].Text; got != "build it" {
		t.Fatalf("opening input = %q: later turns must retain the confirmed run instruction", got)
	}
	if artifact.Items[2].Kind != ModelOutput || !artifact.Items[2].Legacy {
		t.Fatalf("final item = %#v: text-only model history must remain available but explicitly legacy", artifact.Items[2])
	}
	again, err := Project("run-1", "backend", "cfg", events, 4)
	if err != nil || again.ContentDigest != artifact.ContentDigest {
		t.Fatalf("second projection digest = %q, err %v, want %q: identical confirmed history must produce identical evidence", again.ContentDigest, err, artifact.ContentDigest)
	}
}

func TestProjectUsesExactNativeResultsWithoutDuplicatingDerivedText(t *testing.T) {
	native := map[string]any{"schema": "arxi.turn/v1", "finish_reason": "stop",
		"content": []map[string]any{{"type": "text", "text": "exact canonical answer"}}}
	body, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	events := []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "build it"}},
		{Seq: 2, ID: "child-prepared", Type: kernel.ExecWorkPrepared, Payload: map[string]any{"work_scope": "turn_child", "child_kind": "model", "agent": "backend"}},
		{Seq: 3, ID: "child-finished", Type: kernel.ExecWorkFinished, Actor: "runtime",
			Payload: map[string]any{"work_scope": "turn_child", "child_kind": "model", "agent": "backend", "result_json": string(body)}},
		{Seq: 4, ID: "final", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "exact canonical answer"}},
		{Seq: 5, ID: "done", Type: kernel.AgentTurnDone, Actor: "backend"},
	}
	artifact, err := Project("run-1", "backend", "cfg", events, 5)
	if err != nil {
		t.Fatal(err)
	}
	outputs := 0
	for _, item := range artifact.Items {
		if item.Kind != ModelOutput {
			continue
		}
		outputs++
		if item.Legacy {
			t.Fatalf("native model item %#v: exact committed child results must not be marked legacy", item)
		}
		if got := item.Content[0].Text; got != "exact canonical answer" {
			t.Fatalf("native model text = %q: later turns must receive the exact presented output, not a projection of it", got)
		}
	}
	if outputs != 1 {
		t.Fatalf("model outputs = %d: the derived llm.response of a native segment must not duplicate the committed child content", outputs)
	}

	legacy := []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": "build it"}},
		{Seq: 2, ID: "answer", Type: kernel.LLMResponse, Actor: "backend", Payload: map[string]any{"text": "text-only history"}},
		{Seq: 3, ID: "done", Type: kernel.AgentTurnDone, Actor: "backend"},
	}
	old, err := Project("run-1", "backend", "cfg", legacy, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Items) != 2 || old.Items[1].Kind != ModelOutput || !old.Items[1].Legacy {
		t.Fatalf("legacy items = %#v: a text-only turn's llm.response is its only surviving model output and stays explicitly legacy", old.Items)
	}
}
