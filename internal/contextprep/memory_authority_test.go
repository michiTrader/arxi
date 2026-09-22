package contextprep

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memory"
)

// ADR-0023 enumerates memory authority. These tests exist because a throwaway
// probe showed three things about the negation form that preceded it:
//
//	candidate.Governed() = true,  Validate() = <nil>
//	approved.Governed()  = true,  Validate() = <nil>
//	typo "governd_memory_recrd" -> Governed() = true, Validate() = <nil>
//
// A model-proposed candidate validated identically to an operator-approved
// record, and so did a misspelling. The roadmap requires that model material
// "may propose candidates but cannot create active memory", and nothing
// represented that rule.

// TestEachEnumeratedKindReportsItsAuthority is table-driven so that adding a
// kind without deciding its authority fails here rather than defaulting.
func TestEachEnumeratedKindReportsItsAuthority(t *testing.T) {
	for _, c := range []struct {
		kind            string
		wantPresentable bool
		wantGoverned    bool
	}{
		{KindFrozenContextMemory, true, false},
		{KindApprovedMemoryRecord, true, true},
		{KindProposedMemoryCandidate, false, true},
	} {
		t.Run(c.kind, func(t *testing.T) {
			r := MemoryReceipt{Kind: c.kind}
			if got := r.Presentable(); got != c.wantPresentable {
				t.Errorf("Presentable() = %v for kind %q, want %v", got, c.kind, c.wantPresentable)
			}
			if got := r.Governed(); got != c.wantGoverned {
				t.Errorf("Governed() = %v for kind %q, want %v", got, c.kind, c.wantGoverned)
			}
		})
	}

	// Every enumerated kind must be known to the pure-leaf enumeration the
	// predicates now read (ADR-0031), or a constant could exist that Validate
	// refuses as unknown.
	for _, kind := range []string{KindFrozenContextMemory, KindApprovedMemoryRecord, KindProposedMemoryCandidate} {
		if !memory.Kind(kind).Known() {
			t.Errorf("kind %q is declared as a constant but absent from the enumeration, so "+
				"Validate would refuse it as unknown", kind)
		}
	}
}

// TestUnknownKindIsRefusedRatherThanTrustedIsInverted is the assertion that
// inverts the failure direction.
//
// Under the negation form every string that was not "frozen_context_memory"
// was authority, so a typo, a kind from a newer store, or the literal word
// "candidate" all validated. Now an unrecognized kind fails closed.
func TestUnknownKindIsRefusedRatherThanTrusted(t *testing.T) {
	for _, kind := range []string{
		"governd_memory_recrd",   // realistic typo of an enumerated kind
		"governed_memory_record", // the string ADR-0021's tests had invented
		"candidate",
		"memory",
	} {
		t.Run(kind, func(t *testing.T) {
			r := MemoryReceipt{Kind: kind, RecordID: "rec-1", VersionID: "rec-1@1",
				EffectiveConfigSHA: "cfg", ContentDigest: "sha"}

			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for unknown kind %q\n"+
					"  an unenumerated kind must not inherit the authority of a record somebody "+
					"approved; this exact shape validated cleanly before ADR-0023", kind)
			}
			if !strings.Contains(err.Error(), "unknown kind") {
				t.Errorf("Validate() = %q, want it to name the kind as unknown", err)
			}
			if r.Presentable() {
				t.Errorf("Presentable() = true for unknown kind %q: material nobody enumerated "+
					"must not reach a prepared context", kind)
			}
		})
	}
}

// TestProposedCandidateIsRefusedByThePreparer asserts the containment rule
// directly, because the preparer has no path that constructs a candidate
// receipt.
//
// Without this assertion the candidate kind would be an enumeration entry that
// nothing enforces -- a field that nothing fails on, which is what ADR-0021 was
// written about.
func TestProposedCandidateIsRefusedByThePreparer(t *testing.T) {
	candidate := MemoryReceipt{Kind: KindProposedMemoryCandidate, RecordID: "cand-1",
		VersionID: "cand-1@1", EffectiveConfigSHA: "cfg", ContentDigest: "sha"}

	err := candidate.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a model-proposed candidate: the roadmap requires that " +
			"model material cannot create active memory, and a candidate that can be presented " +
			"is not a candidate")
	}
	if !strings.Contains(err.Error(), "never presented") {
		t.Errorf("Validate() = %q, want it to say a candidate is never presented", err)
	}
	if candidate.Presentable() {
		t.Error("Presentable() = true for a proposed candidate")
	}

	// A candidate is still a governed record: it has identity and versions, and
	// Phase 7 must be able to store, inspect and promote it. Refusing to
	// present it must not cost it that status, or promotion would have nothing
	// to promote.
	if !candidate.Governed() {
		t.Error("Governed() = false for a proposed candidate: a candidate is a stored record " +
			"with a version, so it must remain governed even though it is never presented")
	}
}

// TestPhase5ReceiptSurvivesTheEnumeration pins that the authority change costs
// the only kind in production nothing.
func TestPhase5ReceiptSurvivesTheEnumeration(t *testing.T) {
	artifact := prepareForReceipt(t, receiptMemory)
	if len(artifact.MemoryReceipts) != 1 {
		t.Fatalf("memory receipts = %#v, want exactly one", artifact.MemoryReceipts)
	}
	receipt := artifact.MemoryReceipts[0]

	if err := receipt.Validate(); err != nil {
		t.Errorf("Validate() = %v for the receipt the preparer actually builds, want nil", err)
	}
	if !receipt.Presentable() {
		t.Error("Presentable() = false for frozen configuration memory: the operator's own " +
			"frozen prose is the one source that was always allowed to be presented")
	}
	if receipt.Governed() {
		t.Error("Governed() = true for frozen configuration memory: a configuration field is " +
			"not a stored record, which is why naming a record identity is refused")
	}
}
