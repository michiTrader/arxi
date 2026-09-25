package memorystore_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestRetrievalEnvelopeWitnessesTheQueryItAuthorizedUnder pins the top-level
// Retrieval header -- the authorization envelope the whole retrieval ran under --
// which is the companion witness to the per-record Selection ADR-0049 guarded.
// Every one of ADR-0027, ADR-0042, ADR-0043, ADR-0044 and ADR-0047 added a header
// field (authorized_scopes, clearance, authorized_purposes,
// authorized_evidence_classes, as_of) and justified it the same way: "recorded so
// an audit can answer ... under what clearance, for which use, on what kind of
// evidence, as of when the retrieval was authorized". An audit that cannot trust
// those fields cannot answer any of those questions, and the receipt is the
// artifact Phase 7's exit evidence rests on.
//
// It is a distinct gap from the Selection witness ADR-0049 closed, and one level
// up: Selection describes each returned record, this header describes the query
// envelope the records were authorized under. Probed by mutation, dropping
// authorized_scopes, authorized_purposes and authorized_evidence_classes to nil
// left the whole suite green, and cross-wiring them to each other's values passed
// too; forcing clearance to the public floor also passed, because the only prior
// assertion checks the empty-query floor case and never a non-public clearance
// correctly recorded. Only as_of was genuinely covered. So three of the five
// authorization dimensions in the audit envelope were the field nothing fails on,
// and a fourth was witnessed only at its narrowest point -- the verified-at-its-
// narrowest-point shape the corpus keeps recording. Asserting one dimension here
// would reproduce it; the honest scope is the whole envelope.
//
// The query names a non-public clearance, several scopes, several purposes and
// several evidence classes -- with a duplicate in each set -- so the guard catches
// a header that dropped a dimension, echoed the wrong one, forgot to deduplicate,
// forgot to sort, or narrowed a non-public clearance to the floor. The expected
// sets are derived from the same query values the retrieval saw rather than
// restated as literals, so the property under test is that the envelope describes
// the query it authorized, not that both match a hand-copied constant.
func TestRetrievalEnvelopeWitnessesTheQueryItAuthorizedUnder(t *testing.T) {
	store := open(t)
	// One record that the query authorizes, so retrieval runs a real authorization
	// pass rather than short-circuiting on an empty store. The envelope records the
	// authorization the retrieval ran under, not the intersection with the results,
	// so the query deliberately authorizes more scopes, purposes and classes than
	// this single record uses.
	if _, err := store.Approve("fact", user("ana"), memorystore.Confidential, memorystore.Personalize,
		memorystore.Observed, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2025, To: y2026}, "headcount was 200 in 2025", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	scopes := []memorystore.Scope{
		user("ana"),
		{Principal: memorystore.Project, ID: "arxi"},
		user("ana"), // duplicate: the envelope must deduplicate it, not report it twice
	}
	purposes := []memorystore.Purpose{memorystore.Personalize, memorystore.Operate, memorystore.Personalize}
	classes := []memorystore.EvidenceClass{memorystore.Observed, memorystore.Stated, memorystore.Observed}

	_, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          scopes,
		Clearance:       memorystore.Confidential,
		Purposes:        purposes,
		EvidenceClasses: classes,
		AsOf:            y2025,
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}

	wantScopes := dedupSorted(func() []string {
		out := make([]string, 0, len(scopes))
		for _, s := range scopes {
			out = append(out, s.String())
		}
		return out
	}())
	if !reflect.DeepEqual(evidence.Scopes, wantScopes) {
		t.Fatalf("retrieval evidence recorded authorized_scopes %v, want %v: an audit cannot tell which "+
			"holders the retrieval was authorized for if the envelope's scope set is wrong, so a "+
			"presentation that crossed a scope boundary would leave a receipt claiming it never did "+
			"(ADR-0027)", evidence.Scopes, wantScopes)
	}

	// A non-public clearance, so a header that always reported the public floor --
	// the only case the prior assertion covered -- is caught here.
	if evidence.Clearance != string(memorystore.Confidential) {
		t.Fatalf("retrieval evidence recorded clearance %q, want %q: an audit cannot tell what clearance "+
			"the retrieval was authorized under if the envelope narrows a non-public clearance to the "+
			"floor, so a disclosure cleared too high would leave no trace (ADR-0042)",
			evidence.Clearance, memorystore.Confidential)
	}

	wantPurposes := dedupSorted([]string{string(memorystore.Personalize), string(memorystore.Operate)})
	if !reflect.DeepEqual(evidence.Purposes, wantPurposes) {
		t.Fatalf("retrieval evidence recorded authorized_purposes %v, want %v: an audit cannot tell "+
			"which uses the retrieval was authorized for if the envelope's purpose set is wrong, so "+
			"purpose creep in what the caller was permitted would be invisible (ADR-0043)",
			evidence.Purposes, wantPurposes)
	}

	wantClasses := dedupSorted([]string{string(memorystore.Observed), string(memorystore.Stated)})
	if !reflect.DeepEqual(evidence.EvidenceClasses, wantClasses) {
		t.Fatalf("retrieval evidence recorded authorized_evidence_classes %v, want %v: an audit cannot "+
			"tell what kinds of evidence the retrieval was authorized to act on if the envelope's class "+
			"set is wrong, so acting on an inference where an assertion was required would leave no trace "+
			"(ADR-0044)", evidence.EvidenceClasses, wantClasses)
	}

	if evidence.AsOf != string(y2025) {
		t.Fatalf("retrieval evidence recorded as_of %q, want %q: an audit cannot tell as of when the "+
			"retrieval judged records valid if the envelope's instant is wrong, so a stale-fact "+
			"presentation would leave a receipt naming a different moment than the one asked about "+
			"(ADR-0047)", evidence.AsOf, y2025)
	}
}

// dedupSorted mirrors how Retrieve builds each authorized set -- deduplicate, then
// sort -- so the expected envelope is derived from the query values the retrieval
// saw rather than a hand-copied constant that could drift from them.
func dedupSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
