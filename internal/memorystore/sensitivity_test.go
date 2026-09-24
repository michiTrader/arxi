package memorystore_test

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestARecordOverTheCallersClearanceIsWithheld is the clearance leakage
// guarantee, the same shape as the cross-scope leakage test one dimension over.
//
// It asserts Considered as well as the empty result, because a leakage test that
// only checks the result is zero passes just as well against a store that never
// held the record. The secret record must be present and refused, not absent.
func TestARecordOverTheCallersClearanceIsWithheld(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("vault", user("ana"), memorystore.Secret, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "the signing key is in the HSM", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}, Purposes: []memorystore.Purpose{memorystore.Operate},
		Scopes: []memorystore.Scope{user("ana")}, Clearance: memorystore.Public})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a secret record was returned to a public clearance: sensitivity is not being "+
			"authorized, so a record correctly scoped but over-classified reaches an under-cleared "+
			"caller -- the leak this dimension exists to close, got %d records", len(got))
	}
	if evidence.Considered != 1 {
		t.Fatalf("retrieval considered %d records, want 1: the secret record must be present and "+
			"refused for this to be evidence of containment rather than of an empty store",
			evidence.Considered)
	}
	if evidence.Authorized != 0 {
		t.Fatalf("retrieval authorized %d of 1 over-clearance record: clearance was checked after "+
			"ranking or not at all, so a record the caller may not receive was treated as a "+
			"candidate", evidence.Authorized)
	}
	if evidence.Clearance != string(memorystore.Public) {
		t.Fatalf("retrieval evidence records clearance %q, want %q: an audit cannot say what "+
			"clearance a presentation was authorized under", evidence.Clearance, memorystore.Public)
	}
}

// TestClearanceAdmitsAtOrBelowItself pins the ranking direction rather than a
// single boundary: a clearance admits every level at or below it and withholds
// every level above it.
func TestClearanceAdmitsAtOrBelowItself(t *testing.T) {
	store := open(t)
	levels := map[string]memorystore.Sensitivity{
		"p": memorystore.Public,
		"i": memorystore.Internal,
		"c": memorystore.Confidential,
		"s": memorystore.Secret,
	}
	for id, level := range levels {
		if _, err := store.Approve(id, user("ana"), level, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "body of "+id, "operator"); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	got, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}, Purposes: []memorystore.Purpose{memorystore.Operate},
		Scopes: []memorystore.Scope{user("ana")}, Clearance: memorystore.Confidential})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.RecordID] = true
	}
	for _, id := range []string{"p", "i", "c"} {
		if !seen[id] {
			t.Fatalf("a confidential clearance did not return the %q record at or below it: the "+
				"clearance is excluding material the caller is cleared for, so a legitimate memory "+
				"is silently withheld", id)
		}
	}
	if seen["s"] {
		t.Fatal("a confidential clearance returned the secret record above it: the ceiling leaks " +
			"upward, so the most restricted material is the easiest to retrieve")
	}
}

// TestAnUnclassifiedOrUnknownRecordIsRefusedAtValidate pins the fail-closed
// direction of the record's own level: an empty level is refused rather than
// defaulted to public, and an unrecognized level gets no standing.
func TestAnUnclassifiedOrUnknownRecordIsRefusedAtValidate(t *testing.T) {
	store := open(t)
	_, err := store.Approve("unclassified", user("ana"), "", memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "a fact nobody classified", "operator")
	if err == nil {
		t.Fatal("a record with no sensitivity was stored: an unclassified level is unknown, and " +
			"treating unknown as public would disclose the material most likely to have been " +
			"written in a hurry -- the fail-open direction ADR-0042 refuses")
	}
	if !strings.Contains(err.Error(), "sensitivity") || !strings.Contains(err.Error(), "classify") {
		t.Fatalf("the refusal of an unclassified record does not name the field or the remedy: "+
			"%v\nit must tell the caller to classify the record, not report a bare invalid", err)
	}
	_, err = store.Approve("garbage", user("ana"), memorystore.Sensitivity("top-secret"), memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "body", "operator")
	if err == nil {
		t.Fatal("a record with an unrecognized sensitivity was stored: the vocabulary is closed " +
			"precisely so a level nobody enumerated gets no standing rather than the standing of " +
			"whichever real level it sorts beside")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown level does not name the closed vocabulary: %v", err)
	}
}

// TestAnUnknownClearanceIsRefused keeps a misspelled clearance from being
// silently narrowed to the public floor, which would present a different set
// than intended with no error to say so.
func TestAnUnknownClearanceIsRefused(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "a public fact", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}, Purposes: []memorystore.Purpose{memorystore.Operate},
		Scopes: []memorystore.Scope{user("ana")}, Clearance: memorystore.Sensitivity("cosmic")})
	if err == nil {
		t.Fatal("a query naming an unrecognized clearance was answered: a misspelled clearance was " +
			"narrowed to the public floor silently, so the caller receives a different set than it " +
			"asked for and nothing reports the mistake")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown clearance does not name the closed vocabulary: %v", err)
	}
}

// TestSensitivityIsPartOfTheVersionIdentity is the guarantee ADR-0034 pins for
// the edge fields, applied to sensitivity: two records identical but for their
// level must seal to different version IDs, so the field cannot leave the
// identity -- and a reclassification cannot become an in-place edit -- without
// this test failing.
func TestSensitivityIsPartOfTheVersionIdentity(t *testing.T) {
	base := memorystore.Record{RecordID: "r", Scope: user("ana"), Kind: memorystore.Approved,
		Purpose: memorystore.Operate, EvidenceClass: memorystore.Stated, Confidence: memorystore.Medium, Retention: memorystore.Permanent, Body: "the same body", Origin: "operator"}
	internal := base
	internal.Sensitivity = memorystore.Internal
	confidential := base
	confidential.Sensitivity = memorystore.Confidential

	sealedInternal, err := internal.Seal()
	if err != nil {
		t.Fatalf("seal internal: %v", err)
	}
	sealedConfidential, err := confidential.Seal()
	if err != nil {
		t.Fatalf("seal confidential: %v", err)
	}
	if sealedInternal.VersionID == sealedConfidential.VersionID {
		t.Fatalf("two records differing only in sensitivity sealed to the same version ID %q: the "+
			"level is outside the content-addressed identity, so reclassifying a record leaves its "+
			"version ID unchanged -- a receipt naming that version now describes a different "+
			"classification than the one presented, the mutable-record defect ADR-0021 forbids",
			sealedInternal.VersionID)
	}
}

// TestCorrectionCarriesSensitivityForward keeps a correction from silently
// declassifying a record by omission: the level is set once at creation and
// carried forward by every derived verb, like the scope.
func TestCorrectionCarriesSensitivityForward(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Confidential, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "the original", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	corrected, err := store.Correct("fact", "the amended fact", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if corrected.Sensitivity != memorystore.Confidential {
		t.Fatalf("the corrected version has sensitivity %q, want %q: a correction dropped the "+
			"classification, so amending a confidential record silently declassified it and it is "+
			"now retrievable under a lower clearance", corrected.Sensitivity, memorystore.Confidential)
	}
	// The declassification would be observable at retrieval: an internal
	// clearance must still not see the corrected confidential record.
	got, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}, Purposes: []memorystore.Purpose{memorystore.Operate},
		Scopes: []memorystore.Scope{user("ana")}, Clearance: memorystore.Internal})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an internal clearance retrieved the corrected confidential record: the "+
			"correction carried a lower level forward, got %d records", len(got))
	}
}
