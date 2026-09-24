package memorystore_test

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestARecordOutsideTheQueryPurposeIsWithheld is the cross-purpose leakage
// guarantee, the same shape as the cross-scope and cross-clearance tests one
// dimension over.
//
// It asserts Considered as well as the empty result, because a leakage test that
// only checks the result is zero passes just as well against a store that never
// held the record. The recommend record must be present and refused, not absent.
func TestARecordOutsideTheQueryPurposeIsWithheld(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("promo", user("ana"), memorystore.Public, memorystore.Recommend,
		memorystore.Stated, memorystore.Medium, "suggest the premium plan", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		Scopes:   []memorystore.Scope{user("ana")},
		Purposes: []memorystore.Purpose{memorystore.Operate}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a record approved for recommend was returned to a query authorized only for "+
			"operate: purpose is not being authorized, so a record approved for one use reaches a "+
			"caller acting under another -- the purpose creep this dimension exists to close, got %d "+
			"records", len(got))
	}
	if evidence.Considered != 1 {
		t.Fatalf("retrieval considered %d records, want 1: the recommend record must be present and "+
			"refused for this to be evidence of containment rather than of an empty store",
			evidence.Considered)
	}
	if evidence.Authorized != 0 {
		t.Fatalf("retrieval authorized %d of 1 out-of-purpose record: purpose was checked after "+
			"ranking or not at all, so a record the caller may not use was treated as a candidate",
			evidence.Authorized)
	}
}

// TestAQueryAuthorizesOnlyItsDeclaredPurposes pins membership rather than a
// single boundary: a query returns records whose purpose is in its authorized
// set and withholds every record whose purpose is not.
func TestAQueryAuthorizesOnlyItsDeclaredPurposes(t *testing.T) {
	store := open(t)
	uses := map[string]memorystore.Purpose{
		"op": memorystore.Operate,
		"pe": memorystore.Personalize,
		"re": memorystore.Recommend,
	}
	for id, use := range uses {
		if _, err := store.Approve(id, user("ana"), memorystore.Public, use, memorystore.Stated, memorystore.Medium, "body of "+id, "operator"); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	got, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		Scopes:   []memorystore.Scope{user("ana")},
		Purposes: []memorystore.Purpose{memorystore.Operate, memorystore.Personalize}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.RecordID] = true
	}
	for _, id := range []string{"op", "pe"} {
		if !seen[id] {
			t.Fatalf("a query authorized for operate and personalize did not return the %q record "+
				"approved for one of them: the authorized set is excluding a use the caller named, so "+
				"a legitimate memory is silently withheld", id)
		}
	}
	if seen["re"] {
		t.Fatal("a query authorized for operate and personalize returned the recommend record: an " +
			"unauthorized purpose leaked in, so a record approved for one use is reachable under " +
			"another the caller never named")
	}
}

// TestAQueryWithNoPurposeAuthorizesNothing pins the fail-closed floor of an
// unranked dimension. Purpose has no least-privilege member, so an empty
// authorized set admits nothing rather than falling back to some default use --
// the honest floor, and the guard that stops anyone from later giving purpose a
// wildcard default the way an empty clearance defaults to the public floor.
func TestAQueryWithNoPurposeAuthorizesNothing(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, "deploys land on Tuesdays", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a query naming no purpose returned %d records: an empty authorized set was treated "+
			"as a wildcard, which is the fail-open direction -- an unranked dimension has no floor "+
			"member to fall back to, so naming no purpose must authorize nothing", len(got))
	}
	if evidence.Considered != 1 || evidence.Authorized != 0 {
		t.Fatalf("retrieval considered %d and authorized %d, want 1 and 0: the record must be present "+
			"and refused, so this is containment rather than an empty store", evidence.Considered,
			evidence.Authorized)
	}
}

// TestAnUnclassifiedOrUnknownPurposeIsRefusedAtValidate pins the fail-closed
// direction of the record's own purpose: an empty purpose is refused rather than
// defaulted to a wildcard, and an unrecognized purpose gets no standing.
func TestAnUnclassifiedOrUnknownPurposeIsRefusedAtValidate(t *testing.T) {
	store := open(t)
	_, err := store.Approve("unstated", user("ana"), memorystore.Public, "", memorystore.Stated, memorystore.Medium, "a fact approved for no stated use", "operator")
	if err == nil {
		t.Fatal("a record with no purpose was stored: an unstated use is unknown, and treating " +
			"unknown as any purpose would surface the record for every use -- purpose creep by " +
			"construction, the fail-open direction ADR-0043 refuses")
	}
	if !strings.Contains(err.Error(), "purpose") || !strings.Contains(err.Error(), "approve it for") {
		t.Fatalf("the refusal of an unstated purpose does not name the field or the remedy: %v\nit "+
			"must tell the caller to approve the record for a known use, not report a bare invalid", err)
	}
	_, err = store.Approve("garbage", user("ana"), memorystore.Public, memorystore.Purpose("marketing"),
		memorystore.Stated, memorystore.Medium, "body", "operator")
	if err == nil {
		t.Fatal("a record with an unrecognized purpose was stored: the vocabulary is closed precisely " +
			"so a use nobody enumerated gets no standing rather than the standing of whatever it is " +
			"mistaken for")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown purpose does not name the closed vocabulary: %v", err)
	}
}

// TestAnUnknownQueryPurposeIsRefused keeps a misspelled purpose from being
// silently dropped from the authorized set, which would narrow the caller's
// authorization and present a different set than intended with no error to say so.
func TestAnUnknownQueryPurposeIsRefused(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, "a fact", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		Scopes:   []memorystore.Scope{user("ana")},
		Purposes: []memorystore.Purpose{memorystore.Purpose("advertising")}})
	if err == nil {
		t.Fatal("a query naming an unrecognized purpose was answered: a misspelled purpose was " +
			"dropped from the authorized set silently, so the caller receives a different set than it " +
			"asked for and nothing reports the mistake")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown query purpose does not name the closed vocabulary: %v", err)
	}
}

// TestPurposeIsPartOfTheVersionIdentity is the guarantee ADR-0034 pins for the
// edge fields, applied to purpose: two records identical but for their use must
// seal to different version IDs, so the field cannot leave the identity -- and a
// re-purposing cannot become an in-place edit -- without this test failing.
func TestPurposeIsPartOfTheVersionIdentity(t *testing.T) {
	base := memorystore.Record{RecordID: "r", Scope: user("ana"), Kind: memorystore.Approved,
		Sensitivity: memorystore.Public, Body: "the same body", Origin: "operator"}
	operate := base
	operate.Purpose = memorystore.Operate
	recommend := base
	recommend.Purpose = memorystore.Recommend

	sealedOperate, err := operate.Seal()
	if err != nil {
		t.Fatalf("seal operate: %v", err)
	}
	sealedRecommend, err := recommend.Seal()
	if err != nil {
		t.Fatalf("seal recommend: %v", err)
	}
	if sealedOperate.VersionID == sealedRecommend.VersionID {
		t.Fatalf("two records differing only in purpose sealed to the same version ID %q: the use is "+
			"outside the content-addressed identity, so re-purposing a record leaves its version ID "+
			"unchanged -- a receipt naming that version now describes a different use than the one "+
			"approved, the mutable-record defect ADR-0021 forbids", sealedOperate.VersionID)
	}
}

// TestCorrectionCarriesPurposeForward keeps a correction from silently
// re-purposing a record by omission: the use is set once at creation and carried
// forward by every derived verb, like the scope and the sensitivity.
func TestCorrectionCarriesPurposeForward(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("pref", user("ana"), memorystore.Public, memorystore.Personalize,
		memorystore.Stated, memorystore.Medium, "prefers dark mode", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	corrected, err := store.Correct("pref", "prefers dark mode and compact density", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if corrected.Purpose != memorystore.Personalize {
		t.Fatalf("the corrected version has purpose %q, want %q: a correction dropped the use, so "+
			"amending a personalize record silently re-purposed it and it is now reachable under a "+
			"different authorization", corrected.Purpose, memorystore.Personalize)
	}
	// The re-purposing would be observable at retrieval: a query authorized only
	// for operate must still not see the corrected personalize record, and one
	// authorized for personalize must.
	only, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		Scopes:   []memorystore.Scope{user("ana")},
		Purposes: []memorystore.Purpose{memorystore.Operate}})
	if err != nil {
		t.Fatalf("retrieve operate: %v", err)
	}
	if len(only) != 0 {
		t.Fatalf("a query authorized for operate retrieved the corrected personalize record: the "+
			"correction carried a different use forward, got %d records", len(only))
	}
	kept, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		Scopes:   []memorystore.Scope{user("ana")},
		Purposes: []memorystore.Purpose{memorystore.Personalize}})
	if err != nil {
		t.Fatalf("retrieve personalize: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("a query authorized for personalize did not retrieve the corrected personalize "+
			"record: the correction failed to carry the use forward, got %d records", len(kept))
	}
}
