package contextprep

import (
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

func TestPrepareOrdersStaticContextBeforeCanonicalHistory(t *testing.T) {
	history := transcript.Artifact{Schema: transcript.Schema, RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 7, ContentDigest: "history", Items: []transcript.Item{
			{Kind: transcript.UserInput, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "continue"}}},
			{Kind: transcript.ModelOutput, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "prior answer"}}},
		}}
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{Identity: "backend", Memory: "frozen fact", MaxTokens: 12000}}
	artifact, err := Prepare("context-1", "run-1", "work-1", "cfg", effect, history)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Messages) != 4 || artifact.Messages[0].Role != turn.RoleSystem || artifact.Messages[1].Role != turn.RoleUser || artifact.Messages[2].Role != turn.RoleAssistant || artifact.Messages[3].Role != turn.RoleUser {
		t.Fatalf("prepared roles = %#v: stable context must prefix canonical history and a final user instruction must keep provider requests valid", artifact.Messages)
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
}
