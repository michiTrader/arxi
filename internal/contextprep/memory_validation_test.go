package contextprep

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/compaction"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

// ADR-0024 gives MemoryReceipt.Validate a production caller. These tests exist
// because a throwaway probe asked whether anything called it, and nothing did:
//
//	approved-without-version -> Validate() = ... has no version_id
//	candidate                -> Validate() = ... reached the preparer
//	artifact + invalid + candidate: marshal err = <nil>
//	committed bytes contain candidate kind = true
//
// Validate refused correctly and was never invoked, so ADR-0021's version rule,
// ADR-0022's vocabulary and ADR-0023's enumeration were each reachable only
// from tests. An artifact carrying a candidate receipt marshalled cleanly and
// would have been committed by ADR-0013's barrier.
//
// The same probe found two receipts the preparer itself emits that prove
// nothing: one with an empty effective config SHA, and one with no content
// digest at all.

// validationHistory is the confirmed history these tests prepare from. Shared
// so the Phase 5 digest assertion below prepares the same presentation the
// refusal cases do, differing only in the field under test.
func validationHistory() transcript.Artifact {
	return transcript.Artifact{Schema: transcript.Schema, RunID: "run-1", Subject: "backend",
		SourceThroughSeq: 7, ContentDigest: "history", Items: []transcript.Item{
			{Kind: transcript.UserInput, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "continue"}}},
		}}
}

func validationEffect(memory string) kernel.SpawnTurn {
	return kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{
		Identity: "backend", Situation: []string{"phase 5"}, Memory: memory, MaxTokens: 12000}}
}

// TestPrepareRefusesMemoryItCannotAttribute drives the refusal through the real
// preparer rather than calling Validate directly, because "Validate has a
// caller" is the decision under test. Asserting Validate again would restate
// ADR-0021 and leave the gap ADR-0024 closes.
//
// The case is the one the probe caught the preparer emitting: memory supplied,
// no effective config SHA, so the receipt named no blueprint version and
// validated clean.
func TestPrepareRefusesMemoryItCannotAttribute(t *testing.T) {
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "", Effect: validationEffect("frozen fact"), History: validationHistory(),
		Route: testRoute, Generator: compaction.Extractive{}})
	if err == nil {
		t.Fatalf("Prepare() = nil error with memory presented and no effective config SHA.\n"+
			"The receipt it built names no blueprint version, so nothing can say which "+
			"configuration presented this memory, and Phase 7 owes exactly that attribution. "+
			"Consequence: memory reaches the model and the artifact cannot attribute it. "+
			"Remedy: keep the Validate call in Prepare and the config-SHA requirement for "+
			"frozen receipts.\nartifact receipts = %#v", artifact.MemoryReceipts)
	}
	if !strings.Contains(err.Error(), "effective_config_sha") {
		t.Errorf("Prepare() = %q, want it to name effective_config_sha as the missing evidence", err)
	}

	// The refusal must precede the digests. ADR-0013 freezes the artifact once
	// committed, so a check that ran after PresentationDigest and ContentDigest
	// were computed would describe bytes that are already immutable -- the
	// refusal would be reported while the barrier had a digest to commit.
	if artifact.PresentationDigest != "" || artifact.ContentDigest != "" {
		t.Errorf("refused preparation returned presentation_digest %q and content_digest %q, "+
			"want both empty.\nConsequence: a presentation that was refused still has digests "+
			"the durable barrier could commit. Remedy: validate receipts before computing "+
			"either digest.", artifact.PresentationDigest, artifact.ContentDigest)
	}
}

// TestValidateRequiresEvidenceOfWhatWasPresented covers the field that makes a
// receipt evidence at all. Before ADR-0024 an empty ContentDigest validated for
// every kind, so a receipt could satisfy Phase 7's requirement that "every
// influence identifies its source and version" while identifying neither.
func TestValidateRequiresEvidenceOfWhatWasPresented(t *testing.T) {
	for _, c := range []struct {
		name    string
		receipt MemoryReceipt
	}{
		{"frozen memory without a content digest",
			MemoryReceipt{Kind: KindFrozenContextMemory, EffectiveConfigSHA: "cfg"}},
		{"governed record without a content digest",
			MemoryReceipt{Kind: KindApprovedMemoryRecord, RecordID: "rec-1", VersionID: "ver-1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.receipt.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for %#v.\nA receipt is evidence of what memory was "+
					"presented, and this one names no content, so it proves nothing about the "+
					"presentation it accompanies. Consequence: a store can record influence "+
					"without recording what was influential. Remedy: require content_digest "+
					"for every kind.", c.receipt)
			}
			if !strings.Contains(err.Error(), "content_digest") {
				t.Errorf("Validate() = %q, want it to name content_digest as the missing evidence", err)
			}
		})
	}
}

// TestGovernedReceiptNeedsNoConfigSHA pins the asymmetry ADR-0024 decided, so a
// later uniformity cleanup fails here instead of silently binding stored records
// to the blueprint that retrieved them.
func TestGovernedReceiptNeedsNoConfigSHA(t *testing.T) {
	governed := MemoryReceipt{Kind: KindApprovedMemoryRecord, ContentDigest: "content",
		RecordID: "rec-1", VersionID: "ver-1"}
	if err := governed.Validate(); err != nil {
		t.Fatalf("Validate() = %v for a governed receipt naming record, version and content but "+
			"no config SHA, want nil.\nA stored record is not part of the blueprint: requiring a "+
			"config SHA would make one record version carry different evidence in two runs, "+
			"which ADR-0021 separated the identities to prevent.", err)
	}
}

// TestPhase5DigestsSurviveReceiptValidation is the assertion that keeps the
// refusals from passing vacuously: if Validate refused everything, or if the
// receipt shape changed, this fails. The digests are the values the preparer
// produced before ADR-0024, so contexts already committed under ADR-0013's
// barrier still verify byte for byte.
func TestPhase5DigestsSurviveReceiptValidation(t *testing.T) {
	const (
		wantPresentation = "6ecafa9c2c596f0579dd53f85035ac0a82c356f2dcbf2f986a1baa3d1893bae0"
		wantContent      = "7852539219a375f6845ebfd0eb22f2e5bf9adb4b23f226051b77acec7f1d6534"
	)
	artifact, err := Prepare(Request{ContextID: "context-1", RunID: "run-1", ParentWorkID: "work-1",
		EffectiveConfigSHA: "cfg", Effect: validationEffect("frozen fact"), History: validationHistory(),
		Route: testRoute, Generator: compaction.Extractive{}})
	if err != nil {
		t.Fatalf("Prepare() on the normal Phase 5 path = %v, want nil: validating receipts must "+
			"not refuse the receipt the preparer actually builds", err)
	}
	if len(artifact.MemoryReceipts) != 1 || !artifact.MemoryReceipts[0].Presentable() {
		t.Fatalf("Phase 5 preparation carries %#v, want exactly one presentable frozen receipt",
			artifact.MemoryReceipts)
	}
	if artifact.PresentationDigest != wantPresentation {
		t.Errorf("presentation_digest = %q, want %q.\nConsequence: prepared contexts committed "+
			"before this change no longer verify against the preparer that recovers them. "+
			"Remedy: receipt validation must not alter what is presented.",
			artifact.PresentationDigest, wantPresentation)
	}
	if artifact.ContentDigest != wantContent {
		t.Errorf("content_digest = %q, want %q.\nConsequence: the durable barrier's binding of "+
			"presentation to route and memory changed, so recovery rejects artifacts it "+
			"committed. Remedy: receipt validation must not alter the receipt bytes.",
			artifact.ContentDigest, wantContent)
	}
}
