package memorystore_test

import (
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestRetrievalSelectionWitnessesTheRecordItNames pins the structured content of
// the retrieval receipt, which is the artifact Phase 7's exit evidence rests on:
// every returned record leaves a Selection recording its identity and the full
// classification it was authorized under, so an audit can answer not only which
// records were presented but under what scope, clearance, purpose, evidence class,
// confidence and valid-time window each was admitted.
//
// It is a distinct gap from the carry-forward ADR-0048 closed, and deliberately
// wider than one dimension. ADR-0042 through ADR-0047 each added a witness field to
// Selection and each justified it as "recorded so an audit can answer ...", but no
// test asserted any of those structured fields: store_test checks only the count of
// selections and that Reason is non-empty, and confidence_test checks the free-text
// Reason string, never the machine-readable fields an audit tool parses. Probed by
// mutation, blanking every structured field of Selection -- RecordID, VersionID,
// Scope, Sensitivity, Purpose, EvidenceClass, Confidence, ValidFrom, ValidTo -- left
// the whole suite green, so the audit witness was the field nothing fails on, across
// every dimension at once. Asserting only valid time here would reproduce the
// verified-at-its-narrowest-point error the corpus keeps recording; the honest scope
// is the whole witness.
//
// The expected values are read off the returned record rather than restated as
// literals, so the guard cannot go stale against a value the test forgot to update:
// the property under test is that the Selection describes the record it names, not
// that either matches a hand-copied constant. Every dimension is set to a distinct
// value so a witness populated from the wrong field is caught as well as one dropped.
func TestRetrievalSelectionWitnessesTheRecordItNames(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Confidential, memorystore.Personalize,
		memorystore.Observed, memorystore.High, memorystore.Ephemeral,
		memorystore.Validity{From: y2025, To: y2026}, "headcount was 200 in 2025", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Clearance:       memorystore.Confidential,
		Purposes:        []memorystore.Purpose{memorystore.Personalize},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Observed},
		AsOf:            y2025,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 || len(evidence.Selections) != 1 {
		t.Fatalf("retrieval returned %d records and %d selections, want one of each: the witness is "+
			"one selection per returned record, so the assertions below have nothing to check if the "+
			"two disagree", len(got), len(evidence.Selections))
	}
	rec := got[0]
	sel := evidence.Selections[0]

	// Identity: a selection that does not name the exact record version it describes
	// is evidence for a presentation that cannot be reproduced -- a receipt naming no
	// version, or the wrong one, cannot be resolved against the store that issued it,
	// which is the whole of ADR-0021's version-identity guarantee read at the receipt.
	if sel.RecordID != rec.RecordID {
		t.Fatalf("selection recorded record_id %q for the record %q it names: the retrieval receipt "+
			"identifies the wrong record, so an audit resolving it lands on a different record than the "+
			"one presented", sel.RecordID, rec.RecordID)
	}
	if sel.VersionID != rec.VersionID {
		t.Fatalf("selection recorded version_id %q for version %q: the receipt names a version the "+
			"presentation did not use, so it cannot be resolved against the immutable version that was "+
			"actually presented (ADR-0021)", sel.VersionID, rec.VersionID)
	}
	if sel.Scope != rec.Scope.String() {
		t.Fatalf("selection recorded scope %q for a record scoped %q: the witness of which holder the "+
			"record belongs to is wrong, so an audit cannot confirm the record was presented within its "+
			"own scope rather than leaked across one", sel.Scope, rec.Scope.String())
	}

	// Authorization and ranking witness: each dimension the record was admitted under
	// is recorded so an audit can answer under what classification it was presented,
	// the "every influence identifies its source" requirement widened dimension by
	// dimension across ADR-0042 through ADR-0047.
	if sel.Sensitivity != string(rec.Sensitivity) {
		t.Fatalf("selection recorded sensitivity %q for a record classified %q: an audit reading the "+
			"receipt cannot tell what clearance the record was disclosed under, so a disclosure above "+
			"the caller's clearance would leave no trace in the evidence (ADR-0042)",
			sel.Sensitivity, rec.Sensitivity)
	}
	if sel.Purpose != string(rec.Purpose) {
		t.Fatalf("selection recorded purpose %q for a record approved for %q: the receipt cannot show "+
			"which use the record was authorized for, so purpose creep is invisible to an audit "+
			"(ADR-0043)", sel.Purpose, rec.Purpose)
	}
	if sel.EvidenceClass != string(rec.EvidenceClass) {
		t.Fatalf("selection recorded evidence class %q for a record backed by %q: the receipt cannot "+
			"show what kind of evidence was presented, so an inference surfaced where an assertion was "+
			"asked for leaves no trace (ADR-0044)", sel.EvidenceClass, rec.EvidenceClass)
	}
	if sel.Confidence != string(rec.Confidence) {
		t.Fatalf("selection recorded confidence %q for a record rated %q: the receipt cannot show how "+
			"far the record was vouched for, so an audit cannot tell a record placed below another was "+
			"ranked there and not withheld (ADR-0045)", sel.Confidence, rec.Confidence)
	}
	if sel.ValidFrom != string(rec.Validity.From) {
		t.Fatalf("selection recorded valid_from %q for a record valid from %q: the receipt cannot show "+
			"the interval that admitted the record at the as-of, so an audit cannot confirm it was "+
			"returned because its window contained the as-of and not in spite of it (ADR-0047)",
			sel.ValidFrom, rec.Validity.From)
	}
	if sel.ValidTo != string(rec.Validity.To) {
		t.Fatalf("selection recorded valid_to %q for a record valid until %q: the receipt cannot show "+
			"the upper bound of the interval that admitted the record, so an audit cannot see when the "+
			"presented fact stopped being true (ADR-0047)", sel.ValidTo, rec.Validity.To)
	}
}
