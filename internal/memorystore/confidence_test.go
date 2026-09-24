package memorystore_test

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestConfidenceOrdersRetrievalAtTheSameSpecificity is the ranking guarantee, and
// the witness the mutation must break: confidence is a ranking dimension, so its
// only mechanism is the comparator, and the check is a reordering rather than a
// leakage one.
//
// Both records share a scope, so scope specificity cannot separate them and the
// order is decided by confidence alone. The record IDs are chosen so that without
// the confidence clause the tie would fall back to record-ID order and place the
// low-confidence record first: "a-guess" sorts before "b-fact". So a passing test
// proves the high-confidence record led *because of* its confidence, not by an
// accident of naming, and removing the clause is observable here.
func TestConfidenceOrdersRetrievalAtTheSameSpecificity(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("a-guess", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Low, memorystore.Permanent, memorystore.Validity{}, "ana might prefer dark mode", "operator"); err != nil {
		t.Fatalf("approve low: %v", err)
	}
	if _, err := store.Approve("b-fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent, memorystore.Validity{}, "ana prefers dark mode", "operator"); err != nil {
		t.Fatalf("approve high: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("retrieval returned %d records, want 2: confidence must order the set, not shrink it -- "+
			"a low-confidence record is ranked lower, never withheld", len(got))
	}
	if got[0].RecordID != "b-fact" {
		t.Fatalf("the low-confidence record %q ranked first over the high-confidence one: confidence is "+
			"not ordering the result, so the ranker fell back to record-ID order and presented a guess "+
			"ahead of a vouched-for fact -- the reordering the confidence clause exists to prevent",
			got[0].RecordID)
	}
	if !strings.Contains(evidence.Selections[0].Reason, "confidence high") {
		t.Fatalf("the winning selection's reason %q does not name the confidence that placed it: an audit "+
			"cannot tell a record was ranked first for its confidence rather than withheld for something "+
			"else", evidence.Selections[0].Reason)
	}
}

// TestConfidenceNeverWithholdsARecord is the distinction from the four
// authorization dimensions, made a test. Scope, sensitivity, purpose and evidence
// class each withhold a record that fails them before the ranker runs; confidence
// withholds nothing. A lone low-confidence record the caller is authorized for must
// be returned and counted as authorized, so nobody later moves confidence into the
// authorization step and filters by it.
func TestConfidenceNeverWithholdsARecord(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("weak", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Low, memorystore.Permanent, memorystore.Validity{}, "ana might work in CET", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a low-confidence record the caller is authorized for was not returned (got %d): "+
			"confidence is withholding a record instead of only ranking it, so a ranking dimension has "+
			"been turned into an authorization one -- material the caller may see is hidden because the "+
			"writer was unsure", len(got))
	}
	if evidence.Authorized != 1 {
		t.Fatalf("retrieval authorized %d of 1 low-confidence record: confidence is being checked in the "+
			"authorization step, where it does not belong -- it ranks the authorized set and never reduces "+
			"it", evidence.Authorized)
	}
}

// TestAnUnstatedOrUnknownConfidenceIsRefusedAtValidate pins the fail-closed
// direction of the record's own confidence: an empty confidence is refused rather
// than defaulted to a member, and an unrecognized level gets no standing.
//
// The empty case is the one the roadmap warns about. Because confidence is ranked
// it has a floor, and defaulting an unstated confidence to that floor looks
// conservative -- but an unstated confidence is no assessment, not a low one, and
// recording it as low fabricates a judgment nobody made.
func TestAnUnstatedOrUnknownConfidenceIsRefusedAtValidate(t *testing.T) {
	store := open(t)
	_, err := store.Approve("unrated", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, "", memorystore.Permanent, memorystore.Validity{}, "a fact nobody rated", "operator")
	if err == nil {
		t.Fatal("a record with no confidence was stored: an unstated confidence is no assessment, not a " +
			"low one, and defaulting it would launder a missing judgment into a stated one -- the " +
			"fail-open direction ADR-0045 refuses")
	}
	if !strings.Contains(err.Error(), "confidence") || !strings.Contains(err.Error(), "state it as") {
		t.Fatalf("the refusal of an unstated confidence does not name the field or the remedy: %v\nit must "+
			"tell the caller to state the confidence as a known level, not report a bare invalid", err)
	}
	_, err = store.Approve("garbage", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Confidence("certain"), memorystore.Permanent, memorystore.Validity{}, "body", "operator")
	if err == nil {
		t.Fatal("a record with an unrecognized confidence was stored: the vocabulary is closed precisely " +
			"so a level nobody defined gets no standing rather than the standing of whatever it sorts beside")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown confidence does not name the closed vocabulary: %v", err)
	}
}

// TestConfidenceIsPartOfTheVersionIdentity is the guarantee ADR-0034 pins, applied
// to confidence: two records identical but for how far their writer vouched for
// them must seal to different version IDs, so the field cannot leave the identity
// -- and a re-rating cannot become an in-place edit -- without this test failing.
// Confidence never authorizes, but it is still identity: two confidences are two
// different assertions about trust.
func TestConfidenceIsPartOfTheVersionIdentity(t *testing.T) {
	base := memorystore.Record{RecordID: "r", Scope: user("ana"), Kind: memorystore.Approved,
		Sensitivity: memorystore.Public, Purpose: memorystore.Operate, EvidenceClass: memorystore.Stated,
		Body: "the same body", Origin: "operator"}
	low := base
	low.Confidence = memorystore.Low
	high := base
	high.Confidence = memorystore.High

	sealedLow, err := low.Seal()
	if err != nil {
		t.Fatalf("seal low: %v", err)
	}
	sealedHigh, err := high.Seal()
	if err != nil {
		t.Fatalf("seal high: %v", err)
	}
	if sealedLow.VersionID == sealedHigh.VersionID {
		t.Fatalf("two records differing only in confidence sealed to the same version ID %q: confidence "+
			"is outside the content-addressed identity, so re-rating a record leaves its version ID "+
			"unchanged -- a receipt naming that version now describes a different assessment than the "+
			"record was ranked under, the mutable-record defect ADR-0021 forbids", sealedLow.VersionID)
	}
}

// TestCorrectionCarriesConfidenceForward keeps a correction from silently re-rating
// a record by omission: the confidence is set once at creation and carried forward
// by every derived verb, like the scope, the sensitivity, the purpose and the
// evidence class. Unlike those four, dropping it would misorder rather than
// disclose -- which is why it needs a reordering witness, checked here through the
// tip the correction produces.
func TestCorrectionCarriesConfidenceForward(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Imported, memorystore.High, memorystore.Permanent, memorystore.Validity{}, "headcount is 240", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	corrected, err := store.Correct("fact", "headcount is 251", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if corrected.Confidence != memorystore.High {
		t.Fatalf("the corrected version has confidence %q, want %q: a correction dropped the confidence, "+
			"so amending a record the writer vouched for silently re-rated it and it now ranks as though "+
			"nobody stood behind it", corrected.Confidence, memorystore.High)
	}
}

// TestRankOrderReadsTheVocabularyNotTheString pins that the comparator reads the
// confidence rank through the vocabulary, not the raw string. "high" sorts before
// "low" alphabetically, so a naive refactor to string comparison would order a
// high-confidence record *after* a low one -- the exact inversion this test fails
// on. Both records share a scope so specificity cannot separate them, and the IDs
// are chosen so record-ID order alone would also place the low record first: only
// a correct rank read puts the high record ahead.
func TestRankOrderReadsTheVocabularyNotTheString(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("a-low", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Low, memorystore.Permanent, memorystore.Validity{}, "weak guess", "operator"); err != nil {
		t.Fatalf("approve low: %v", err)
	}
	if _, err := store.Approve("z-high", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent, memorystore.Validity{}, "vouched-for fact", "operator"); err != nil {
		t.Fatalf("approve high: %v", err)
	}
	got, _, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 2 || got[0].RecordID != "z-high" {
		t.Fatalf("the high-confidence record did not rank first: the comparator is reading confidence as "+
			"a string, where \"low\" sorts after \"high\" and wins, instead of through the rank the "+
			"vocabulary defines -- so a more-vouched-for record ranks below a weaker one, got order %v",
			recordIDs(got))
	}
}

// recordIDs renders the retrieved order for a failure message.
func recordIDs(records []memorystore.Record) []string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.RecordID)
	}
	return ids
}
