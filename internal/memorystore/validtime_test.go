package memorystore_test

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// Canonical instants reused across the valid-time guards. Each is UTC at seconds
// precision, the one spelling Validate accepts, so the store keeps them verbatim
// and the as-of filter compares them as plain strings.
const (
	y2025 memorystore.Instant = "2025-01-01T00:00:00Z"
	y2026 memorystore.Instant = "2026-01-01T00:00:00Z"
	y2027 memorystore.Instant = "2027-01-01T00:00:00Z"
	mid26 memorystore.Instant = "2026-06-01T00:00:00Z"
)

// timeQuery is the authorized query the valid-time guards vary only the as-of of:
// one user scope, the operate purpose, the stated class, public clearance floor.
func timeQuery(asOf memorystore.Instant) memorystore.Query {
	return memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated},
		AsOf:            asOf,
	}
}

// TestValidTimeWithholdsARecordOutsideTheAsOfWindow is the authorization witness for
// valid time, the same shape as the cross-scope leakage test one dimension over: a
// record whose validity window ended before the as-of is considered but not
// authorized, so removing the as-of filter leaks a fact that was true only in the
// past. The two records are authorized on every other dimension and differ only in
// when they are true, so the filter under test is the only thing that can separate
// them.
func TestValidTimeWithholdsARecordOutsideTheAsOfWindow(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("past", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2025, To: y2026}, "headcount was 200 in 2025", "operator"); err != nil {
		t.Fatalf("approve past: %v", err)
	}
	if _, err := store.Approve("current", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2026, To: y2027}, "headcount is 240 in 2026", "operator"); err != nil {
		t.Fatalf("approve current: %v", err)
	}
	got, evidence, err := store.Retrieve(timeQuery(mid26))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 || got[0].RecordID != "current" {
		t.Fatalf("as of %s the retrieval returned %v, want only \"current\": a record whose validity "+
			"window closed before the as-of leaked, so a fact true only in 2025 was presented as "+
			"current in 2026 -- the stale-fact disclosure the valid-time filter exists to prevent",
			mid26, recordIDs(got))
	}
	if evidence.Considered != 2 || evidence.Authorized != 1 {
		t.Fatalf("valid time considered %d and authorized %d, want 2 considered and 1 authorized: the "+
			"out-of-window record must be counted as present and then withheld, the same "+
			"considered-but-not-authorized witness a cross-scope record leaves, or the filter is not "+
			"measurable as an authorization step", evidence.Considered, evidence.Authorized)
	}
	if evidence.AsOf != string(mid26) {
		t.Fatalf("the retrieval evidence recorded as-of %q, want %q: an audit cannot tell as of when a "+
			"record was judged valid if the instant it was authorized against is not in the receipt",
			evidence.AsOf, mid26)
	}
}

// TestTheAsOfBoundaryIsHalfOpen pins the [From, To) rule at its two edges, the place
// a closed-vs-half-open confusion would hide. A record valid [2026, 2027) must be
// returned at exactly its From instant and withheld at exactly its To instant: if To
// were inclusive, the fact would be presented as current one instant after it stopped
// being true, and adjacent windows would both claim their shared boundary.
func TestTheAsOfBoundaryIsHalfOpen(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("y26", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2026, To: y2027}, "true through 2026", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	atFrom, _, err := store.Retrieve(timeQuery(y2026))
	if err != nil {
		t.Fatalf("retrieve at From: %v", err)
	}
	if len(atFrom) != 1 {
		t.Fatalf("as of the From instant %s the record was withheld: From is inclusive in [From, To), so "+
			"a record is valid at the instant its window opens -- treating From as exclusive loses the "+
			"first instant of every fact", y2026)
	}
	atTo, _, err := store.Retrieve(timeQuery(y2027))
	if err != nil {
		t.Fatalf("retrieve at To: %v", err)
	}
	if len(atTo) != 0 {
		t.Fatalf("as of the To instant %s the record was returned: To is exclusive in [From, To), so a "+
			"record is no longer valid at the instant its window closes -- treating To as inclusive "+
			"presents a fact as current one instant after it stopped being true and lets two adjacent "+
			"windows both claim their shared boundary", y2027)
	}
}

// TestATimelessRecordIsValidAtEveryAsOf keeps an unbounded validity from being read
// as an unsatisfiable window. A record with neither bound stated is a standing
// assertion true at every instant, so it must be returned at a concrete as-of and at
// the empty one alike. The mutation this catches is a contains() that treats an empty
// bound as a real edge and so withholds the timeless record everywhere.
func TestATimelessRecordIsValidAtEveryAsOf(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("standing", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{}, "ana prefers dark mode", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	atInstant, _, err := store.Retrieve(timeQuery(mid26))
	if err != nil {
		t.Fatalf("retrieve at instant: %v", err)
	}
	if len(atInstant) != 1 {
		t.Fatalf("a timeless record was withheld at as-of %s: an empty bound is the unbounded end of "+
			"the interval, not an edge no instant clears, so a record making no temporal claim is valid "+
			"at every instant", mid26)
	}
	atEmpty, _, err := store.Retrieve(timeQuery(""))
	if err != nil {
		t.Fatalf("retrieve at empty as-of: %v", err)
	}
	if len(atEmpty) != 1 {
		t.Fatal("a timeless record was withheld from a query naming no as-of: the timeless floor admits " +
			"exactly the records that declared no window, so a record making no temporal claim is the " +
			"one kind a temporal-question-free query may still see")
	}
}

// TestAnEmptyAsOfSeesOnlyTimelessRecords is the fail-closed floor made a test, and
// the distinction from a wildcard. A query naming no as-of is not asking "give me
// everything" -- it is asking a non-temporal question, so a record that declared a
// validity window is withheld because the caller supplied no instant to evaluate it
// against, while a timeless record is returned. The mutation this catches is an
// empty as-of that admits bounded records, which would present a dated fact with no
// check that the caller is asking about a time it was true.
func TestAnEmptyAsOfSeesOnlyTimelessRecords(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("dated", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2025, To: y2026}, "a dated fact", "operator"); err != nil {
		t.Fatalf("approve dated: %v", err)
	}
	if _, err := store.Approve("timeless", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{}, "a standing fact", "operator"); err != nil {
		t.Fatalf("approve timeless: %v", err)
	}
	got, evidence, err := store.Retrieve(timeQuery(""))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 || got[0].RecordID != "timeless" {
		t.Fatalf("a query naming no as-of returned %v, want only \"timeless\": an empty as-of is the "+
			"timeless floor, not a wildcard, so a record carrying a validity window must be withheld "+
			"when the caller gave no instant to test it against -- admitting it presents a dated fact "+
			"unjudged", recordIDs(got))
	}
	if evidence.Considered != 2 || evidence.Authorized != 1 {
		t.Fatalf("the empty-as-of retrieval considered %d and authorized %d, want 2 and 1: the bounded "+
			"record must be present and then withheld, so the floor is measurable as fail-closed rather "+
			"than as an absence of records", evidence.Considered, evidence.Authorized)
	}
}

// TestAMisspelledQueryAsOfIsRefusedNotNarrowed keeps a malformed as-of from silently
// collapsing a temporal query into the timeless floor. A non-canonical as-of is
// refused with an error, exactly as an unknown clearance is, because narrowing a
// misspelled as-of to "timeless only" would present a different set than intended
// with nothing to say so -- the closed-form rule the record's own instants obey,
// applied to the query.
func TestAMisspelledQueryAsOfIsRefusedNotNarrowed(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("dated", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2025, To: y2027}, "a dated fact", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, _, err := store.Retrieve(timeQuery("2026-06-01"))
	if err == nil {
		t.Fatal("a query with a non-canonical as-of \"2026-06-01\" was accepted: a malformed as-of " +
			"silently narrowed to the timeless floor would withhold every dated record with no error " +
			"to say the query was misread, the fail-open direction the clearance check already closes")
	}
	if !strings.Contains(err.Error(), "canonical") && !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("the refusal of a malformed as-of does not name the expected instant form: %v\nit must "+
			"tell the caller the canonical UTC spelling, not report a bare invalid", err)
	}
}

// TestAnUnparseableOrInvertedValidityIsRefusedAtValidate pins the fail-closed
// directions of the record's own window: a bound that is not a canonical instant is
// refused, and an interval whose From is not strictly before its To is refused
// because no instant falls inside it, so a record carrying one could never be
// retrieved -- the interval-shaped field-nothing-fails-on defect. A timeless record
// (both bounds empty) is not refused: an unbounded validity is a stated claim, not an
// unstated one.
func TestAnUnparseableOrInvertedValidityIsRefusedAtValidate(t *testing.T) {
	store := open(t)
	_, err := store.Approve("garbage", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: "last tuesday"}, "body", "operator")
	if err == nil {
		t.Fatal("a record with the un-parseable valid-time instant \"last tuesday\" was stored: a bound " +
			"that is not an instant would sort against real ones at retrieval, so it is refused at the " +
			"write where the caller can fix it")
	}
	if !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("the refusal of a non-instant bound does not name the expected form: %v", err)
	}
	_, err = store.Approve("noon-utc", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: "2026-01-01T05:00:00+05:00"}, "body", "operator")
	if err == nil {
		t.Fatal("a record with a non-UTC valid-time instant was stored: the same moment in two zones " +
			"digests to two version IDs, breaking the portable identity ADR-0034 rests on, so a bound " +
			"is required in the one canonical UTC spelling rather than normalized silently")
	}
	if !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("the refusal of a non-UTC instant does not name canonical form or the remedy: %v", err)
	}
	_, err = store.Approve("inverted", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2027, To: y2025}, "body", "operator")
	if err == nil {
		t.Fatal("a record whose valid-time From is after its To was stored: the half-open interval " +
			"[2027, 2025) contains no instant, so the record could never be retrieved for any as-of -- " +
			"dead weight nothing would ever fail on, refused at the write")
	}
	if !strings.Contains(err.Error(), "strictly before") {
		t.Fatalf("the refusal of an inverted interval does not name the From-before-To rule: %v", err)
	}
}

// TestValidTimeIsPartOfTheVersionIdentity is the guarantee ADR-0034 pins, applied to
// valid time: two records identical but for when their assertion is true must seal to
// different version IDs, so the window cannot leave the identity -- and a re-dating
// cannot become an in-place edit -- without this test failing. A window edited in
// place would let a record a receipt named as true through 2027 be silently narrowed
// to 2026, so a retrieval that correctly withheld it at a 2027 as-of would start
// returning it with no version to show the window moved.
func TestValidTimeIsPartOfTheVersionIdentity(t *testing.T) {
	base := memorystore.Record{RecordID: "r", Scope: user("ana"), Kind: memorystore.Approved,
		Sensitivity: memorystore.Public, Purpose: memorystore.Operate, EvidenceClass: memorystore.Stated,
		Confidence: memorystore.High, Retention: memorystore.Permanent, Body: "the same body",
		Origin: "operator"}
	through26 := base
	through26.Validity = memorystore.Validity{From: y2025, To: y2026}
	through27 := base
	through27.Validity = memorystore.Validity{From: y2025, To: y2027}

	sealed26, err := through26.Seal()
	if err != nil {
		t.Fatalf("seal through-2026: %v", err)
	}
	sealed27, err := through27.Seal()
	if err != nil {
		t.Fatalf("seal through-2027: %v", err)
	}
	if sealed26.VersionID == sealed27.VersionID {
		t.Fatalf("two records differing only in their valid-time window sealed to the same version ID "+
			"%q: the window is outside the content-addressed identity, so re-dating a record leaves its "+
			"version ID unchanged -- a receipt naming that version now describes a different truth "+
			"interval than the record was retrieved under, the mutable-record defect ADR-0021 forbids",
			sealed26.VersionID)
	}
}

// TestCorrectionCarriesValidityForward keeps a correction from silently re-dating a
// record by omission: the window is set once at creation and carried forward by every
// derived verb, like the scope, sensitivity, purpose, evidence class, confidence and
// retention. Dropping it would not disclose or hide the corrected record directly --
// it would move when the fact is true, so a later as-of query returns or withholds it
// against a window nobody set. Checked here through the tip the correction produces
// and an as-of the original window excludes.
func TestCorrectionCarriesValidityForward(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.High, memorystore.Permanent,
		memorystore.Validity{From: y2025, To: y2026}, "headcount was 200", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	corrected, err := store.Correct("fact", "headcount was 210", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if corrected.Validity.From != y2025 || corrected.Validity.To != y2026 {
		t.Fatalf("the corrected version has window [%s, %s), want [%s, %s): a correction dropped the "+
			"valid-time window, so amending a fact true only in 2025 silently re-dated when it was true",
			corrected.Validity.From, corrected.Validity.To, y2025, y2026)
	}
	got, _, err := store.Retrieve(timeQuery(mid26))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("as of %s the corrected record was returned: the correction did not carry the original "+
			"window forward, so a fact true only in 2025 is now presented as current in 2026 -- the "+
			"re-dating the carry-forward exists to prevent", mid26)
	}
}
