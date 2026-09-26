package memorystore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestRetrievalReasonNamesEveryRankingFactItClaimsToCarry pins the free-text
// Reason on each Selection -- the one receipt field Phase 7's exit evidence names
// outright, requiring retrieval receipts to record "ranking/index versions and
// reasons". The reason on every selection is built to name four facts at once:
// the scope-specificity rank the record ranked at, the principal it belongs to,
// the confidence that broke the tie, and the ranker version that produced the
// ordering. retrieve.go states the load-bearing claim explicitly -- "The reason
// string on every selection says exactly which rule applied, which is what the
// exit evidence asks for" -- so an audit reading it must be able to trust all
// four, not one.
//
// It is a distinct gap from the structured Selection fields ADR-0049 witnessed and
// the last unwitnessed content on the retrieval receipt. ADR-0049 deliberately
// stopped at the machine-readable fields and left the reason to confidence_test's
// substring check, and store_test only asserts the reason is non-empty. Probed by
// mutation, three of the four facts were the field nothing fails on: crediting the
// ordering to a ranker that never ran ("wrong.ranker/v0") left the whole suite
// green, so did printing the wrong specificity number, and so did naming the wrong
// principal -- only the "confidence high" substring confidence_test pins was
// caught. So the reason was witnessed at its narrowest point, one of four facts and
// only as a substring, which is the verified-at-its-narrowest-point shape the
// corpus keeps recording. Asserting only confidence again would reproduce it; the
// honest scope is every fact the reason claims to carry.
//
// Each expected fact is derived from the returned record or the exported ranker
// constant rather than restated as a literal, so the property under test is that
// the reason names the record it explains -- not that it matches a hand-copied
// template a reword could drift from. The specificity fragment carries a trailing
// space and the principal its parentheses so a wrong number or principal cannot
// pass by sharing a leading digit or a substring with the correct one.
func TestRetrievalReasonNamesEveryRankingFactItClaimsToCarry(t *testing.T) {
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
		t.Fatalf("retrieval returned %d records and %d selections, want one of each: the reason under "+
			"test lives on the single selection, so the assertions below have nothing to check if the "+
			"two disagree", len(got), len(evidence.Selections))
	}
	rec := got[0]
	reason := evidence.Selections[0].Reason

	// Specificity: the reason must name the rank the record actually ranked at,
	// derived from the principal's own containment rank. The trailing space bounds
	// the number so a wrong rank that shares a leading digit -- rank 1 versus 1000 --
	// cannot pass by prefix match.
	wantRank, known := rec.Scope.Principal.Specificity()
	if !known {
		t.Fatalf("the returned record's principal %q has no specificity rank: the test record must rank "+
			"through the vocabulary for the reason to have a rank to name", rec.Scope.Principal)
	}
	if !strings.Contains(reason, fmt.Sprintf("specificity %d ", wantRank)) {
		t.Fatalf("the selection reason %q does not name specificity rank %d: an audit reading the receipt "+
			"cannot tell why this record outranked another at a different specificity, so a ranking that "+
			"put a broader record ahead of a narrower one would leave no trace in the reason (ADR-0049)",
			reason, wantRank)
	}

	// Principal: the reason must name the holder the record belongs to, wrapped in
	// the parentheses the reason delimits it with, so a reason naming the wrong
	// principal is caught rather than one that merely contains the right substring
	// somewhere.
	if !strings.Contains(reason, fmt.Sprintf("(%s)", rec.Scope.Principal)) {
		t.Fatalf("the selection reason %q does not name the principal %q it ranked: an audit cannot tell "+
			"which holder the specificity rank refers to, so a reason crediting the rank to the wrong "+
			"principal would read as consistent (ADR-0027)", reason, rec.Scope.Principal)
	}

	// Confidence: the fact confidence_test already pins, derived from the record
	// rather than the literal so it belongs to this whole-reason witness rather than
	// standing alone against a hand-copied value.
	if !strings.Contains(reason, fmt.Sprintf("confidence %s", rec.Confidence)) {
		t.Fatalf("the selection reason %q does not name the confidence %q that broke the tie: an audit "+
			"cannot tell a record was placed for its confidence rather than withheld for something else "+
			"(ADR-0045)", reason, rec.Confidence)
	}

	// Ranker version: the reason must name the ranker that produced the ordering,
	// derived from the exported constant so it cannot drift from the identity the
	// receipt header records. This is the "ranking version" the exit evidence asks
	// for, carried in the reason as well as the header, and the fact whose mutation
	// -- crediting the ordering to a ranker that never ran -- left the whole suite
	// green.
	if !strings.Contains(reason, memorystore.RetrievalVersion) {
		t.Fatalf("the selection reason %q does not name the ranker version %q that ordered it: an audit "+
			"reading the reason cannot tell which ranker's rules applied, so an ordering credited to a "+
			"ranker that never ran would leave no trace -- the ranking version the exit evidence names "+
			"(ADR-0051)", reason, memorystore.RetrievalVersion)
	}
}
