package memorystore_test

import (
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestEverySelectionWitnessesTheNonRankingClassificationAtItsOwnPosition widens
// the per-position selection witness past the fields ADR-0053's fixture varied.
// ADR-0053 retrieved two records that "differ in every ranking fact -- scope
// specificity, principal, confidence and identity" and asserted each selection
// describes the record at its position using those facts. Its two records are
// identical in the non-ranking classification, though: both are approved public,
// operate, stated, with an empty validity. A field the two records share cannot
// expose a selection that copied it from the wrong record, because the wrong
// record's value is the same value, so the per-position correspondence of
// sensitivity, purpose, evidence class and the valid-time bounds -- the fields
// ADR-0042, ADR-0043, ADR-0044 and ADR-0047 each added to Selection -- was
// witnessed only at Selections[0] by ADR-0049 and generalised to the slice.
//
// Probed by mutation, sourcing those five fields from the winning record instead
// of the record the ranking loop is on -- "Sensitivity: string(kept[0].Sensitivity),
// ..." -- left the whole suite green, including ADR-0053's own guard, which reads
// confidence (sourced correctly) but never the four classification fields. Under
// that mutation a retrieval of two records that differ in classification emits a
// second selection describing the winner's clearance, use, class and window: an
// audit reading the receipt sees the record ranked second disclosed under a
// classification that belongs to a different record, the disclosure ADR-0042
// through ADR-0047 each added their field to make visible, made invisible again
// for every position but the first.
//
// So this retrieves two records that differ in the non-ranking classification as
// well as the ranking, under a query that authorizes both records' values in
// every dimension and an as-of inside both intervals, and asserts each selection,
// at each position, names its own record's sensitivity, purpose, evidence class
// and valid-time bounds. The run-scoped record ranks first by specificity and
// carries the higher sensitivity, the earlier lower bound and a bounded upper
// end, so a cross-wiring is visible from either side. Every expected value is
// derived from the returned record, so the property under test is that each
// selection describes the record at its index rather than matching a template.
func TestEverySelectionWitnessesTheNonRankingClassificationAtItsOwnPosition(t *testing.T) {
	store := open(t)
	runScope := memorystore.Scope{Principal: memorystore.Run, ID: "r1"}
	tenantScope := memorystore.Scope{Principal: memorystore.Tenant, ID: "acme"}
	// The two records differ in every classification dimension, in the direction
	// that makes a copy from the winner visible on the loser and a copy from the
	// loser visible on the winner: the more specific (winning) record carries the
	// higher sensitivity, the earlier valid-from and a bounded valid-to, while the
	// less specific one carries the lower sensitivity, a later valid-from and an
	// unbounded valid-to.
	if _, err := store.Approve("run-fact", runScope, memorystore.Confidential, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Permanent,
		memorystore.Validity{From: y2025, To: y2027}, "this run agreed on spaces", "operator"); err != nil {
		t.Fatalf("approve run record: %v", err)
	}
	if _, err := store.Approve("tenant-fact", tenantScope, memorystore.Public, memorystore.Personalize,
		memorystore.Observed, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2026}, "the tenant standard is tabs", "operator"); err != nil {
		t.Fatalf("approve tenant record: %v", err)
	}
	// The query authorizes both records' values in every authorization dimension --
	// a clearance that admits both sensitivities, both purposes, both classes -- and
	// names an as-of inside both intervals, so both records survive authorization on
	// valid time while their recorded bounds still differ. A dimension the query did
	// not authorize would filter a record out before it could leave a selection, so
	// the mis-sourced field under test could never be seen.
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{runScope, tenantScope},
		Clearance:       memorystore.Confidential,
		Purposes:        []memorystore.Purpose{memorystore.Operate, memorystore.Personalize},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated, memorystore.Observed},
		AsOf:            y2026,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 2 || len(evidence.Selections) != 2 {
		t.Fatalf("retrieval returned %d records and %d selections, want two of each: the per-position "+
			"correspondence under test has nothing to check unless both records are present and each "+
			"leaves a selection", len(got), len(evidence.Selections))
	}
	// The more specific scope must rank first, or the positions the assertions below
	// key on do not mean what they say: a broken ranking would make "position 1" the
	// winner and hide a cross-wiring rather than expose it.
	if got[0].Scope.Principal != memorystore.Run {
		t.Fatalf("the run-scoped record ranked %v first instead of the run record: specificity is not "+
			"ordering the result, so the positions the correspondence check keys on are not the ranking "+
			"it means to witness", got[0].Scope.Principal)
	}

	for i, rec := range got {
		sel := evidence.Selections[i]
		// Sensitivity: under the probed mutation the second selection borrows the
		// winner's clearance, so a record ranked below another appears disclosed under
		// a clearance that is not its own -- the disclosure ADR-0042 added the field to
		// make visible, invisible again past the first selection.
		if sel.Sensitivity != string(rec.Sensitivity) {
			t.Fatalf("selection %d recorded sensitivity %q for a record classified %q at that position: a "+
				"later selection names a clearance the record does not hold, so an audit reading the receipt "+
				"cannot tell what clearance each ranked record was disclosed under (ADR-0042)",
				i, sel.Sensitivity, rec.Sensitivity)
		}
		if sel.Purpose != string(rec.Purpose) {
			t.Fatalf("selection %d recorded purpose %q for a record approved for %q at that position: a "+
				"later selection names a use the record was not approved for, so purpose creep across "+
				"positions is invisible to an audit (ADR-0043)", i, sel.Purpose, rec.Purpose)
		}
		if sel.EvidenceClass != string(rec.EvidenceClass) {
			t.Fatalf("selection %d recorded evidence class %q for a record backed by %q at that position: a "+
				"later selection names a class the record does not rest on, so an inference surfaced where "+
				"an assertion was asked for leaves no trace at that position (ADR-0044)",
				i, sel.EvidenceClass, rec.EvidenceClass)
		}
		// Valid time: both records survive authorization at the shared as-of, so a
		// mis-sourced bound is a recorded window that belongs to a different record
		// rather than a record filtered out -- the case that lets the correspondence be
		// witnessed rather than hidden by the as-of filter.
		if sel.ValidFrom != string(rec.Validity.From) {
			t.Fatalf("selection %d recorded valid_from %q for a record valid from %q at that position: a "+
				"later selection names the lower bound of a different record's window, so an audit cannot "+
				"confirm this record was returned because its own window contained the as-of (ADR-0047)",
				i, sel.ValidFrom, rec.Validity.From)
		}
		if sel.ValidTo != string(rec.Validity.To) {
			t.Fatalf("selection %d recorded valid_to %q for a record valid until %q at that position: a "+
				"later selection names the upper bound of a different record's window, so an audit cannot "+
				"see when this presented fact stopped being true (ADR-0047)", i, sel.ValidTo, rec.Validity.To)
		}
	}
}
