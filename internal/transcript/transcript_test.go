package transcript

import (
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
