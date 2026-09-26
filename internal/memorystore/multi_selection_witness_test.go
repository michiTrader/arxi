package memorystore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestEverySelectionWitnessesTheRecordAtItsOwnPosition widens the selection and
// reason guards past the first position. ADR-0049 pinned the structured Selection
// fields and ADR-0052 the free-text Reason, and both approve one record and
// retrieve it, so both assert against Selections[0] alone -- the single selection
// a one-record retrieval produces. Neither says anything about a later selection,
// because there is never a later record present.
//
// A retrieval receipt is a slice, and its purpose beyond one record is to explain
// an ordering: why this record was placed above that one. Probed by mutation,
// building every selection from the winning record instead of from the record the
// ranking loop is on -- "for range kept { r := kept[0]; ... }" -- left the whole
// suite green, so a retrieval of two records could emit two selections both naming
// the winner's identity, scope and reason while the record actually ranked second
// left no trace of its own classification. That is the misleading-but-coherent
// evidence ADR-0052 argued the reason must never produce, reached through the
// selections the single-record guards never touch.
//
// So this retrieves two records that differ in every ranking fact -- scope
// specificity (so they rank in a known order), principal, confidence and identity
// -- and asserts that each selection, at each position, describes the record
// actually placed there. The run-scoped record ranks first by specificity and
// deliberately carries the *lower* confidence, so a selection that borrowed the
// winner's confidence for the loser names the wrong level and one that borrowed
// the loser's for the winner does too: a cross-wiring is visible from either side.
// Every expected value is derived from the returned record or the exported
// RetrievalVersion constant, so the property under test is that each selection
// describes the record at its index rather than matching a hand-copied template.
func TestEverySelectionWitnessesTheRecordAtItsOwnPosition(t *testing.T) {
	store := open(t)
	runScope := memorystore.Scope{Principal: memorystore.Run, ID: "r1"}
	tenantScope := memorystore.Scope{Principal: memorystore.Tenant, ID: "acme"}
	// The more specific scope carries the lower confidence, so specificity alone
	// decides the order and the confidence a selection names cannot be right by
	// coincidence: the record that ranks first is not the one with the higher
	// confidence.
	if _, err := store.Approve("run-fact", runScope, memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Permanent, memorystore.Validity{},
		"this run agreed on spaces", "operator"); err != nil {
		t.Fatalf("approve run record: %v", err)
	}
	if _, err := store.Approve("tenant-fact", tenantScope, memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent, memorystore.Validity{},
		"the tenant standard is tabs", "operator"); err != nil {
		t.Fatalf("approve tenant record: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{runScope, tenantScope},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
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
		// Identity: the selection at this position must name the record placed here,
		// not the one above it. Under the probed mutation every selection copies the
		// winner, so the second selection names the winner's version and this fails.
		if sel.RecordID != rec.RecordID {
			t.Fatalf("selection %d recorded record_id %q for the record %q ranked at that position: a later "+
				"selection describes a different record than the one placed there, so an audit resolving the "+
				"receipt lands on the wrong record for every position but the first (ADR-0049)",
				i, sel.RecordID, rec.RecordID)
		}
		if sel.VersionID != rec.VersionID {
			t.Fatalf("selection %d recorded version_id %q for version %q ranked at that position: the receipt "+
				"names a version the record at this position did not use, so it cannot be resolved against the "+
				"immutable version actually placed here (ADR-0021)", i, sel.VersionID, rec.VersionID)
		}
		if sel.Scope != rec.Scope.String() {
			t.Fatalf("selection %d recorded scope %q for a record scoped %q at that position: the witness of "+
				"which holder the ranked record belongs to is wrong past the first selection, so a record "+
				"ranked below another appears to share its scope", i, sel.Scope, rec.Scope.String())
		}
		if sel.Confidence != string(rec.Confidence) {
			t.Fatalf("selection %d recorded confidence %q for a record rated %q at that position: a later "+
				"selection names a confidence the record does not hold, so an audit reading the ordering "+
				"cannot tell why each record ranked where it did (ADR-0045)", i, sel.Confidence, rec.Confidence)
		}

		reason := sel.Reason
		wantRank, known := rec.Scope.Principal.Specificity()
		if !known {
			t.Fatalf("the record at position %d has principal %q with no specificity rank: the fixture "+
				"records must rank through the vocabulary for their reasons to have a rank to name",
				i, rec.Scope.Principal)
		}
		// Specificity: the trailing space bounds the number so a rank sharing a leading
		// digit with another cannot pass by prefix, exactly as ADR-0052's single-record
		// guard requires -- held here for the reason at every position.
		if !strings.Contains(reason, fmt.Sprintf("specificity %d ", wantRank)) {
			t.Fatalf("selection %d reason %q does not name specificity rank %d: a later selection credits "+
				"its record with a rank it did not hold, so an audit cannot tell why one record outranked "+
				"another across positions (ADR-0052)", i, reason, wantRank)
		}
		// Principal: wrapped in the parentheses the reason delimits it with, so a wrong
		// principal is caught rather than one that merely appears as a substring.
		if !strings.Contains(reason, fmt.Sprintf("(%s)", rec.Scope.Principal)) {
			t.Fatalf("selection %d reason %q does not name the principal %q it ranked: a later selection "+
				"credits its rank to the wrong holder, so a reason describing the record above it reads as "+
				"consistent (ADR-0027)", i, reason, rec.Scope.Principal)
		}
		if !strings.Contains(reason, fmt.Sprintf("confidence %s", rec.Confidence)) {
			t.Fatalf("selection %d reason %q does not name the confidence %q that placed the record: a later "+
				"selection names the confidence of a different record, so the ordering it explains never "+
				"happened (ADR-0045)", i, reason, rec.Confidence)
		}
		if !strings.Contains(reason, memorystore.RetrievalVersion) {
			t.Fatalf("selection %d reason %q does not name the ranker version %q that ordered it: a later "+
				"selection leaves the ranker unnamed, so an ordering credited to a ranker that never ran "+
				"leaves no trace at that position (ADR-0051)", i, reason, memorystore.RetrievalVersion)
		}
	}
}
