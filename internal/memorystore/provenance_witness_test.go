package memorystore_test

import (
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestRetrievalProvenanceWitnessesTheRankerAndSchemaThatProducedIt pins the two
// provenance fields of the Retrieval header -- Schema and RetrievalVersion -- which
// are the last unwitnessed content on the receipt after ADR-0049 (the per-record
// Selection) and ADR-0050 (the authorization envelope). Phase 7's exit evidence
// requires retrieval receipts to record "ranking/index versions and reasons", and
// RetrievalVersion is that ranking version made explicit: the const's own comment
// says "the identity of the ranker is part of the evidence, not a build detail".
// Schema is the companion tag that says which receipt shape an audit tool is
// parsing. Both name the provenance of the evidence itself rather than any record
// in it, and an audit that cannot trust them cannot tell one ranker's output from
// another's or one receipt schema from the next.
//
// Probed by mutation, this is the same field-nothing-fails-on gap ADR-0049 and
// ADR-0050 closed, one field over. Blanking Schema in the header left the whole
// suite green -- zero coverage. RetrievalVersion was covered only at its floor: the
// prior assertion checks it is non-empty, so a wrong non-empty ranker name (an
// audit attributing the ranking to a ranker that never ran) passed, while only the
// empty case was caught. Guarding one of the two, or guarding RetrievalVersion at
// its non-empty floor as before, would reproduce the verified-at-its-narrowest-point
// shape the corpus keeps recording; the honest subject is both provenance fields at
// their content.
//
// The expected values are read off the package's own exported constants rather than
// restated as literals, so the property under test is that the receipt names the
// ranker and schema this build actually is -- not that either matches a hand-copied
// string a later const bump would forget to update. That is the same
// derive-the-subject-from-the-corpus discipline ADR-0049 applied to the Selection.
func TestRetrievalProvenanceWitnessesTheRankerAndSchemaThatProducedIt(t *testing.T) {
	store := open(t)
	// One record the query authorizes, so retrieval runs a real pass and stamps a
	// real receipt rather than short-circuiting on an empty store.
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Permanent,
		memorystore.Validity{}, "headcount was 200", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	_, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}

	if evidence.RetrievalVersion != memorystore.RetrievalVersion {
		t.Fatalf("retrieval evidence recorded retrieval_version %q, want %q: an audit cannot tell which "+
			"ranker produced a result if the receipt names the wrong one, so a later ranker change would "+
			"be attributed to this version -- or this run's ranking credited to a ranker that never ran -- "+
			"and the non-empty check alone passes both",
			evidence.RetrievalVersion, memorystore.RetrievalVersion)
	}

	if evidence.Schema != memorystore.Schema {
		t.Fatalf("retrieval evidence recorded schema %q, want %q: an audit tool parses the receipt by its "+
			"schema tag, so a blank or wrong tag makes the evidence unparseable or reads it against the "+
			"wrong shape, and the receipt Phase 7's exit evidence rests on becomes untrustworthy at its "+
			"own provenance", evidence.Schema, memorystore.Schema)
	}
}
