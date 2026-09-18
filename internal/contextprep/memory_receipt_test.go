package contextprep

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/compaction"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

// These tests exist because ADR-0020 stated this receipt already recorded
// "the record identities and version IDs it carries" when no field of
// MemoryReceipt held either one. The sentence was wrong for the whole life of
// the accepted decision, and nothing failed, because nothing was measuring it.
//
// ADR-0021 adds RecordID and VersionID. The risk in adding them is that they
// become decoration: present in the struct, absent in the data, with every
// test still green -- the same defect one layer down. The third test below is
// the one that forecloses that, and the first two only matter because it
// exists.

const (
	receiptIdentity  = "backend"
	receiptSituation = "the build is red"
	receiptMemory    = "the operator prefers tabs"
)

// TestFrozenMemoryLeavesRecordAndVersionEmpty pins that the new fields cost
// Phase 5 nothing.
//
// ContextSpec.Memory is a configuration field on a frozen blueprint, not a
// stored record, so it has no record identity to name. Empty here is a
// measured fact about the source rather than an unfinished implementation --
// which is why Kind still has to distinguish the case, and is asserted.
func TestFrozenMemoryLeavesRecordAndVersionEmpty(t *testing.T) {
	artifact := prepareForReceipt(t, receiptMemory)

	if len(artifact.MemoryReceipts) != 1 {
		t.Fatalf("memory receipts = %#v, want exactly one for supplied frozen memory",
			artifact.MemoryReceipts)
	}
	receipt := artifact.MemoryReceipts[0]

	if receipt.Kind != KindFrozenContextMemory {
		t.Errorf("receipt kind = %q, want %q: the kind is what tells a reader why the record "+
			"and version fields are empty", receipt.Kind, KindFrozenContextMemory)
	}
	if receipt.RecordID != "" {
		t.Errorf("record_id = %q for frozen configuration memory, want empty: a configuration "+
			"field has no record identity, and inventing one produces a version no store can "+
			"correct", receipt.RecordID)
	}
	if receipt.VersionID != "" {
		t.Errorf("version_id = %q for frozen configuration memory, want empty", receipt.VersionID)
	}

	// The evidence that already existed must survive the addition. ADR-0020's
	// change was verified partly by checking the receipt still bound content;
	// this keeps that guarantee attached to the fields that carry it.
	if receipt.EffectiveConfigSHA == "" {
		t.Error("effective_config_sha is empty: the receipt must still identify the " +
			"configuration the memory was frozen from")
	}
	if receipt.ContentDigest == "" {
		t.Error("content_digest is empty: the receipt must still bind the presented bytes")
	}
	if err := receipt.Validate(); err != nil {
		t.Errorf("Validate() on a well-formed frozen receipt = %v, want nil", err)
	}
}

// TestPhase5ReceiptEncodesWithoutTheNewKeys asserts the artifact wire form is
// unchanged for every configuration that exists today.
//
// Asserted against the encoded bytes rather than the struct, because
// `omitempty` is a claim about encoding: inspecting the struct would report
// the empty strings and prove nothing about the JSON a stored artifact holds.
func TestPhase5ReceiptEncodesWithoutTheNewKeys(t *testing.T) {
	artifact := prepareForReceipt(t, receiptMemory)

	encoded, err := json.Marshal(artifact.MemoryReceipts)
	if err != nil {
		t.Fatalf("encode memory receipts: %v", err)
	}
	wire := string(encoded)

	for _, key := range []string{"record_id", "version_id"} {
		if strings.Contains(wire, key) {
			t.Errorf("Phase 5 receipt JSON contains %q: %s\n"+
				"  adding version identity must not change the encoded form of artifacts that "+
				"carry no governed record, or committed prepared contexts would stop matching",
				key, wire)
		}
	}
	for _, key := range []string{"kind", "effective_config_sha", "content_digest"} {
		if !strings.Contains(wire, key) {
			t.Errorf("Phase 5 receipt JSON is missing %q: %s", key, wire)
		}
	}
}

// TestGovernedReceiptWithoutAVersionIsRejected is the assertion that keeps
// RecordID and VersionID from being decoration.
//
// Without it, Phase 7 could ship a store that never sets a version: the
// receipt would encode, verify and present exactly like one that named its
// version, and every other test here would still pass. That is precisely how
// ADR-0020's false sentence survived -- the struct looked right and nothing
// failed on the difference.
//
// The table is deliberately adversarial about near-misses. A receipt carrying
// a RecordID but no VersionID is the realistic failure (a store that knows
// which record it returned but not which revision), and it is the one that
// breaks correction propagation: there is nothing for a correction to
// supersede.
func TestGovernedReceiptWithoutAVersionIsRejected(t *testing.T) {
	for _, c := range []struct {
		name    string
		receipt MemoryReceipt
		wantErr string
	}{
		{
			name: "governed record naming neither record nor version",
			receipt: MemoryReceipt{Kind: "governed_memory_record",
				EffectiveConfigSHA: "cfg", ContentDigest: "sha"},
			wantErr: "no record_id",
		},
		{
			name: "governed record naming a record but not a version",
			receipt: MemoryReceipt{Kind: "governed_memory_record", RecordID: "rec-1",
				EffectiveConfigSHA: "cfg", ContentDigest: "sha"},
			wantErr: "no version_id",
		},
		{
			name: "frozen memory that invents a version it cannot have",
			receipt: MemoryReceipt{Kind: KindFrozenContextMemory, VersionID: "v1",
				EffectiveConfigSHA: "cfg", ContentDigest: "sha"},
			wantErr: "has no record identity",
		},
		{
			name:    "a receipt that does not say what it is evidence of",
			receipt: MemoryReceipt{EffectiveConfigSHA: "cfg", ContentDigest: "sha"},
			wantErr: "no kind",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.receipt.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for %#v, want an error mentioning %q\n"+
					"  a field nothing fails on is not a guarantee, it is decoration",
					c.receipt, c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}

	// The positive case must pass, or the rejections above would also be
	// satisfied by a Validate that refuses everything.
	complete := MemoryReceipt{Kind: "governed_memory_record", RecordID: "rec-1",
		VersionID: "rec-1@3", EffectiveConfigSHA: "cfg", ContentDigest: "sha"}
	if err := complete.Validate(); err != nil {
		t.Errorf("Validate() = %v on a governed receipt naming both record and version, want nil", err)
	}
	if !complete.Governed() {
		t.Error("Governed() = false for a stored record receipt: the frozen-memory exemption " +
			"must not extend to governed records")
	}
}

func prepareForReceipt(t *testing.T, memory string) Artifact {
	t.Helper()
	history := transcript.Artifact{Schema: transcript.Schema, RunID: "run-1", Subject: receiptIdentity,
		SourceThroughSeq: 7, ContentDigest: "history", Items: []transcript.Item{
			{Kind: transcript.UserInput, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "continue"}}},
		}}
	effect := kernel.SpawnTurn{Agent: receiptIdentity, Context: kernel.ContextSpec{
		Identity: receiptIdentity, Situation: []string{receiptSituation}, Memory: memory, MaxTokens: 12000}}
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1",
		ParentWorkID: "work-1", EffectiveConfigSHA: "cfg", Effect: effect, History: history,
		Route: Route{Provider: "fake", Protocol: "openai.chat_completions",
			Model: "test-model", ToolSchemaVersion: "arxi.tools/v1",
			ContextPolicyVersion: "arxi.context-prep/v1"},
		Generator: compaction.Extractive{}})
	if err != nil {
		t.Fatalf("Prepare with memory %q: %v", memory, err)
	}
	return artifact
}
