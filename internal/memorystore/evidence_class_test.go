package memorystore_test

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestARecordOutsideTheQueryEvidenceClassesIsWithheld is the cross-class leakage
// guarantee, the same shape as the cross-scope, cross-clearance and cross-purpose
// tests one dimension over.
//
// It asserts Considered as well as the empty result, because a leakage test that
// only checks the result is zero passes just as well against a store that never
// held the record. The observed record must be present and refused, not absent.
func TestARecordOutsideTheQueryEvidenceClassesIsWithheld(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("inferred", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Observed, memorystore.Medium, "ana probably works in CET", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a record backed by observed evidence was returned to a query accepting only stated: "+
			"evidence class is not being authorized, so an inference reaches a caller that asked only for "+
			"what a user stated -- the ungoverned influence this dimension exists to close, got %d records",
			len(got))
	}
	if evidence.Considered != 1 {
		t.Fatalf("retrieval considered %d records, want 1: the observed record must be present and "+
			"refused for this to be evidence of containment rather than of an empty store",
			evidence.Considered)
	}
	if evidence.Authorized != 0 {
		t.Fatalf("retrieval authorized %d of 1 out-of-class record: evidence class was checked after "+
			"ranking or not at all, so a record the caller does not accept was treated as a candidate",
			evidence.Authorized)
	}
}

// TestAQueryAcceptsOnlyItsDeclaredEvidenceClasses pins membership rather than a
// single boundary: a query returns records whose class is in its accepted set and
// withholds every record whose class is not.
func TestAQueryAcceptsOnlyItsDeclaredEvidenceClasses(t *testing.T) {
	store := open(t)
	classes := map[string]memorystore.EvidenceClass{
		"st": memorystore.Stated,
		"ob": memorystore.Observed,
		"im": memorystore.Imported,
	}
	for id, class := range classes {
		if _, err := store.Approve(id, user("ana"), memorystore.Public, memorystore.Operate, class,
			memorystore.Medium, "body of "+id, "operator"); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	got, _, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated, memorystore.Imported}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.RecordID] = true
	}
	for _, id := range []string{"st", "im"} {
		if !seen[id] {
			t.Fatalf("a query accepting stated and imported did not return the %q record backed by one "+
				"of them: the accepted set is excluding a class the caller named, so a legitimate memory "+
				"is silently withheld", id)
		}
	}
	if seen["ob"] {
		t.Fatal("a query accepting stated and imported returned the observed record: an unaccepted " +
			"class leaked in, so an inference is reachable by a caller that asked only for stated and " +
			"imported evidence")
	}
}

// TestAQueryWithNoEvidenceClassAcceptsNothing pins the fail-closed floor of an
// unranked dimension. Evidence class has no least-privilege member, so an empty
// accepted set admits nothing rather than falling back to some default class --
// the honest floor, and the guard that stops anyone from later giving evidence
// class a wildcard default the way an empty clearance defaults to the public floor.
func TestAQueryWithNoEvidenceClassAcceptsNothing(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, "deploys land on Tuesdays", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:   []memorystore.Scope{user("ana")},
		Purposes: []memorystore.Purpose{memorystore.Operate}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a query naming no evidence class returned %d records: an empty accepted set was "+
			"treated as a wildcard, which is the fail-open direction -- an unranked dimension has no "+
			"floor member to fall back to, so naming no class must accept nothing", len(got))
	}
	if evidence.Considered != 1 || evidence.Authorized != 0 {
		t.Fatalf("retrieval considered %d and authorized %d, want 1 and 0: the record must be present "+
			"and refused, so this is containment rather than an empty store", evidence.Considered,
			evidence.Authorized)
	}
}

// TestAnUnclassifiedOrUnknownEvidenceClassIsRefusedAtValidate pins the
// fail-closed direction of the record's own class: an empty class is refused
// rather than defaulted to a wildcard, and an unrecognized class gets no standing.
func TestAnUnclassifiedOrUnknownEvidenceClassIsRefusedAtValidate(t *testing.T) {
	store := open(t)
	_, err := store.Approve("unstated", user("ana"), memorystore.Public, memorystore.Operate, "",
		memorystore.Medium, "a fact backed by no stated evidence", "operator")
	if err == nil {
		t.Fatal("a record with no evidence class was stored: unstated evidence is unknown, and treating " +
			"unknown as any class would surface the record wherever any evidence is accepted -- the " +
			"fail-open direction ADR-0044 refuses")
	}
	if !strings.Contains(err.Error(), "evidence class") || !strings.Contains(err.Error(), "record it as") {
		t.Fatalf("the refusal of an unstated evidence class does not name the field or the remedy: %v\nit "+
			"must tell the caller to record the evidence class as a known kind, not report a bare invalid", err)
	}
	_, err = store.Approve("garbage", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.EvidenceClass("rumored"), memorystore.Medium, "body", "operator")
	if err == nil {
		t.Fatal("a record with an unrecognized evidence class was stored: the vocabulary is closed " +
			"precisely so a class nobody enumerated gets no standing rather than the standing of whatever " +
			"it is mistaken for")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown evidence class does not name the closed vocabulary: %v", err)
	}
}

// TestAnUnknownQueryEvidenceClassIsRefused keeps a misspelled class from being
// silently dropped from the accepted set, which would narrow what the caller
// accepts and present a different set than intended with no error to say so.
func TestAnUnknownQueryEvidenceClassIsRefused(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, "a fact", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, _, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.EvidenceClass("hearsay")}})
	if err == nil {
		t.Fatal("a query naming an unrecognized evidence class was answered: a misspelled class was " +
			"dropped from the accepted set silently, so the caller receives a different set than it asked " +
			"for and nothing reports the mistake")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown query evidence class does not name the closed vocabulary: %v", err)
	}
}

// TestEvidenceClassIsPartOfTheVersionIdentity is the guarantee ADR-0034 pins for
// the edge fields, applied to evidence class: two records identical but for what
// backs them must seal to different version IDs, so the field cannot leave the
// identity -- and a reclassification cannot become an in-place edit -- without
// this test failing.
func TestEvidenceClassIsPartOfTheVersionIdentity(t *testing.T) {
	base := memorystore.Record{RecordID: "r", Scope: user("ana"), Kind: memorystore.Approved,
		Sensitivity: memorystore.Public, Purpose: memorystore.Operate, Body: "the same body", Origin: "operator"}
	stated := base
	stated.EvidenceClass = memorystore.Stated
	observed := base
	observed.EvidenceClass = memorystore.Observed

	sealedStated, err := stated.Seal()
	if err != nil {
		t.Fatalf("seal stated: %v", err)
	}
	sealedObserved, err := observed.Seal()
	if err != nil {
		t.Fatalf("seal observed: %v", err)
	}
	if sealedStated.VersionID == sealedObserved.VersionID {
		t.Fatalf("two records differing only in evidence class sealed to the same version ID %q: the "+
			"class is outside the content-addressed identity, so reclassifying a record leaves its "+
			"version ID unchanged -- a receipt naming that version now describes different evidence than "+
			"the record was recorded under, the mutable-record defect ADR-0021 forbids", sealedStated.VersionID)
	}
}

// TestCorrectionCarriesEvidenceClassForward keeps a correction from silently
// reclassifying a record's evidence by omission: the class is set once at
// creation and carried forward by every derived verb, like the scope, the
// sensitivity and the purpose.
func TestCorrectionCarriesEvidenceClassForward(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Imported, memorystore.Medium, "headcount is 240", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	corrected, err := store.Correct("fact", "headcount is 251", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if corrected.EvidenceClass != memorystore.Imported {
		t.Fatalf("the corrected version has evidence class %q, want %q: a correction dropped the class, "+
			"so amending an imported record silently reclassified it and it is now reachable under a "+
			"different authorization", corrected.EvidenceClass, memorystore.Imported)
	}
	// The reclassification would be observable at retrieval: a query accepting only
	// stated must still not see the corrected imported record, and one accepting
	// imported must.
	none, _, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve stated: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("a query accepting only stated retrieved the corrected imported record: the correction "+
			"carried a different class forward, got %d records", len(none))
	}
	kept, _, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Imported}})
	if err != nil {
		t.Fatalf("retrieve imported: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("a query accepting imported did not retrieve the corrected imported record: the "+
			"correction failed to carry the class forward, got %d records", len(kept))
	}
}
