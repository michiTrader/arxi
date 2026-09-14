package contextruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/compaction"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

// adapterEvents builds a synthetic confirmed conversation, including a
// committed native child result, the way the durable barrier supplies it.
func adapterEvents(t *testing.T) []kernel.Event {
	t.Helper()
	native, err := json.Marshal(map[string]any{"schema": "arxi.turn/v1", "id": "resp-1", "finish_reason": "stop",
		"content": []map[string]any{{"type": "text", "text": "The migration stays reversible."}}})
	if err != nil {
		t.Fatal(err)
	}
	return []kernel.Event{
		{Seq: 1, ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"prompt": strings.Repeat("keep the billing migration reversible ", 20)}},
		{Seq: 2, ID: "call", Type: kernel.ToolCall, Actor: "backend", Payload: map[string]any{"call_id": "c1", "tool": "read", "args": map[string]any{"path": "migrations/001.sql"}}},
		{Seq: 3, ID: "result", Type: kernel.ToolCallCompleted, Actor: "backend", Payload: map[string]any{"call_id": "c1", "result": "ALTER TABLE billing ADD COLUMN reverted BOOLEAN"}},
		{Seq: 4, ID: "child", Type: kernel.ExecWorkFinished, Actor: "runtime",
			Payload: map[string]any{"work_id": "work-child", "work_scope": "turn_child", "child_kind": "model", "agent": "backend", "result_json": string(native)}},
	}
}

// adapterHistory projects the events and returns both the artifact (for item
// identity assertions) and the exact JSON bytes the adapter must pass through.
func adapterHistory(t *testing.T) (transcript.Artifact, string) {
	t.Helper()
	history, err := transcript.Project("run-1", "backend", "cfg", adapterEvents(t), 4)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	return history, string(body)
}

// TestAdapterCompactsUnderPressureAndPreservesTranscript exercises the real
// wiring the supervisor installs: projection, digest binding, compaction and
// the pass-through of every overflow field the barrier commits.
func TestAdapterCompactsUnderPressureAndPreservesTranscript(t *testing.T) {
	history, body := adapterHistory(t)
	projected, err := Adapter{}.Project(exec.ContextProjection{RunID: "run-1", Subject: "backend",
		EffectiveConfigSHA: "cfg", Events: adapterEvents(t), Through: 4})
	if err != nil {
		t.Fatal(err)
	}
	if projected.JSON != body {
		t.Fatalf("projected transcript = %q, want %q: the adapter must pass the projector's exact bytes through unmodified", projected.JSON, body)
	}
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend",
		MaxTokens: 900, OnOverflow: "summarize"}}
	prepared, err := Adapter{}.Prepare(exec.ContextPreparation{ContextID: "context-1", RunID: "run-1",
		ParentWorkID: "work-1", EffectiveConfigSHA: "cfg", Effect: effect,
		History: exec.ContextTranscript{JSON: projected.JSON, Digest: projected.Digest,
			Schema: projected.Schema, ProjectorVersion: projected.ProjectorVersion,
			SourceFromSeq: projected.SourceFromSeq, SourceThroughEventID: projected.SourceThroughEventID,
			ContentDigest: projected.ContentDigest}})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.OverflowExceeded || !prepared.Compacted || prepared.CompactionDigest == "" || prepared.OverflowMode != "summarize" {
		t.Fatalf("overflow pass-through = exceeded %v compacted %v digest %q mode %q: the barrier can only commit what the adapter reports",
			prepared.OverflowExceeded, prepared.Compacted, prepared.CompactionDigest, prepared.OverflowMode)
	}
	if prepared.MeasurementJSON == "" || !strings.Contains(prepared.MeasurementJSON, `"total_tokens"`) {
		t.Fatalf("measurement JSON = %q: the per-layer measurement must travel as canonical evidence", prepared.MeasurementJSON)
	}
	if prepared.CompactionDigest != compactionDigestOf(t, prepared.JSON) {
		t.Fatalf("compaction digest disagrees with the embedded artifact: the overflow decision must bind the exact committed bytes")
	}
	if err := compaction.Verify(*preparedEmbeddedCompaction(t, prepared.JSON), history.Items); err != nil {
		t.Fatalf("embedded compaction failed its own gate: %v", err)
	}
	// Continuity probe: the goal survives as a cited claim, the tool evidence
	// and the final answer stay verbatim in the window.
	summarySeen, goalSeen := false, false
	for _, message := range prepared.Messages {
		for _, block := range message.Content {
			if block.Type != turn.BlockText {
				continue
			}
			if strings.Contains(block.Text, "[Earlier conversation compacted") {
				summarySeen = true
			}
			if strings.Contains(block.Text, "keep the billing migration reversible") {
				goalSeen = true
			}
		}
	}
	if !summarySeen || !goalSeen {
		t.Fatalf("summary %v goal %v: compacted presentations must keep goals and decisions reachable", summarySeen, goalSeen)
	}
}

// TestAdapterRejectsUnboundHistory pins the integrity check between the
// projected transcript and the pipeline binding.
func TestAdapterRejectsUnboundHistory(t *testing.T) {
	_, body := adapterHistory(t)
	_, err := Adapter{}.Prepare(exec.ContextPreparation{ContextID: "context-1", RunID: "run-1",
		ParentWorkID: "work-1", EffectiveConfigSHA: "cfg", Effect: kernel.SpawnTurn{Agent: "backend"},
		History: exec.ContextTranscript{JSON: body, ContentDigest: "not-the-digest"}})
	if err == nil || !strings.Contains(err.Error(), "disagrees with the pipeline binding") {
		t.Fatalf("unbound history error = %v: preparation must refuse a transcript that does not match its recorded digest", err)
	}
}

func compactionDigestOf(t *testing.T, preparedJSON string) string {
	t.Helper()
	artifact := preparedEmbeddedCompaction(t, preparedJSON)
	return artifact.ContentDigest
}

func preparedEmbeddedCompaction(t *testing.T, preparedJSON string) *compaction.Artifact {
	t.Helper()
	var artifact struct {
		Compaction *compaction.Artifact `json:"compaction"`
	}
	if err := json.Unmarshal([]byte(preparedJSON), &artifact); err != nil {
		t.Fatalf("decode prepared artifact: %v", err)
	}
	if artifact.Compaction == nil {
		t.Fatal("prepared artifact carries no embedded compaction")
	}
	return artifact.Compaction
}
