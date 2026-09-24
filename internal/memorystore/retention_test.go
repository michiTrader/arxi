package memorystore_test

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/memorystore"
)

// TestExpireTombstonesEphemeralAndKeepsPermanent is the lifecycle guarantee and
// the witness the mutation must break: retention is neither an authorization nor
// a ranking dimension, so its only mechanism is the Expire sweep, and the check
// is an expiry rather than a leakage or a reordering one.
//
// One ephemeral and one permanent record share everything else. After a sweep the
// ephemeral record is gone from retrieval and the permanent one remains, so the
// permanent check inside Expire is observable here: removing it expires a record a
// user committed to keep.
func TestExpireTombstonesEphemeralAndKeepsPermanent(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("kept", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Permanent, "ana works in Madrid", "operator"); err != nil {
		t.Fatalf("approve permanent: %v", err)
	}
	if _, err := store.Approve("transient", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Ephemeral, "ana opened a file", "operator"); err != nil {
		t.Fatalf("approve ephemeral: %v", err)
	}

	expired, err := store.Expire("lifecycle")
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if len(expired) != 1 || expired[0] != "transient" {
		t.Fatalf("Expire reported %v, want [transient]: the sweep must tombstone the ephemeral record "+
			"and only it -- expiring the permanent one destroys a record a user committed to keep, and "+
			"expiring nothing leaves a lifecycle dimension nothing acts on", expired)
	}

	got, _, err := store.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 || got[0].RecordID != "kept" {
		t.Fatalf("after Expire the surviving records are %v, want [kept]: an ephemeral record survived "+
			"the sweep or a permanent one did not, so Expire is not honoring retention -- it either kept "+
			"expirable material or tombstoned a record marked to be kept", recordIDs(got))
	}
}

// TestRetentionNeverWithholdsARecord is the distinction from every dimension
// before it, made a test. The four authorization dimensions withhold a record
// before ranking; confidence reorders it; retention does neither. Before any
// sweep, an ephemeral record the caller is authorized for must be retrieved
// exactly as a permanent one is, so nobody later moves retention into the
// authorization step and filters retrieval by it.
func TestRetentionNeverWithholdsARecord(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("transient", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Ephemeral, "ana opened a file", "operator"); err != nil {
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
		t.Fatalf("an ephemeral record the caller is authorized for was not returned (got %d): retention "+
			"is withholding a record instead of only deciding whether a sweep may expire it, so a "+
			"lifecycle dimension has been turned into an authorization one -- material the caller may see "+
			"is hidden because it is expirable", len(got))
	}
	if evidence.Authorized != 1 {
		t.Fatalf("retrieval authorized %d of 1 ephemeral record: retention is being checked in the "+
			"authorization step, where it does not belong -- it decides expiry, never whether a caller "+
			"may receive a record", evidence.Authorized)
	}
}

// TestAnUnstatedOrUnknownRetentionIsRefusedAtValidate pins the fail-closed
// direction of the record's own retention: an empty retention is refused rather
// than defaulted to a policy, and an unrecognized one gets no standing.
//
// The empty case is the load-bearing one. Both defaults are dishonest in opposite
// directions -- Permanent hoards a record meant to be transient, Ephemeral expires
// one meant to be kept -- so an unstated retention is refused rather than made to
// look like a lifecycle decision nobody took.
func TestAnUnstatedOrUnknownRetentionIsRefusedAtValidate(t *testing.T) {
	store := open(t)
	_, err := store.Approve("unset", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, "", "a record nobody assigned a lifecycle", "operator")
	if err == nil {
		t.Fatal("a record with no retention was stored: an unstated retention is no lifecycle decision, " +
			"and defaulting it would either hoard a record meant to be transient or expire one meant to " +
			"be kept -- the fail-open direction ADR-0046 refuses")
	}
	if !strings.Contains(err.Error(), "retention") || !strings.Contains(err.Error(), "state it as") {
		t.Fatalf("the refusal of an unstated retention does not name the field or the remedy: %v\nit must "+
			"tell the caller to state the retention as a known policy, not report a bare invalid", err)
	}
	_, err = store.Approve("garbage", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Retention("forever"), "body", "operator")
	if err == nil {
		t.Fatal("a record with an unrecognized retention was stored: the vocabulary is closed precisely " +
			"so a policy nobody defined gets no standing rather than being expired on the strength of a " +
			"value the sweep does not recognize")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal of an unknown retention does not name the closed vocabulary: %v", err)
	}
}

// TestRetentionIsPartOfTheVersionIdentity is the guarantee ADR-0034 pins, applied
// to retention: two records identical but for their lifecycle policy must seal to
// different version IDs, so the field cannot leave the identity -- and a re-tiering
// cannot become an in-place edit -- without this test failing. It matters more
// than for the other dimensions: if retention were editable in place, a permanent
// version a receipt named could be flipped to ephemeral and swept, expiring a
// version somebody was told would be kept.
func TestRetentionIsPartOfTheVersionIdentity(t *testing.T) {
	base := memorystore.Record{RecordID: "r", Scope: user("ana"), Kind: memorystore.Approved,
		Sensitivity: memorystore.Public, Purpose: memorystore.Operate, EvidenceClass: memorystore.Stated,
		Confidence: memorystore.Medium, Body: "the same body", Origin: "operator"}
	permanent := base
	permanent.Retention = memorystore.Permanent
	ephemeral := base
	ephemeral.Retention = memorystore.Ephemeral

	sealedPermanent, err := permanent.Seal()
	if err != nil {
		t.Fatalf("seal permanent: %v", err)
	}
	sealedEphemeral, err := ephemeral.Seal()
	if err != nil {
		t.Fatalf("seal ephemeral: %v", err)
	}
	if sealedPermanent.VersionID == sealedEphemeral.VersionID {
		t.Fatalf("two records differing only in retention sealed to the same version ID %q: retention "+
			"is outside the content-addressed identity, so re-tiering a record leaves its version ID "+
			"unchanged -- a receipt naming that version could then be flipped from permanent to ephemeral "+
			"and swept, expiring a version somebody was told would be kept", sealedPermanent.VersionID)
	}
}

// TestCorrectionCarriesRetentionForward keeps a correction from silently
// re-tiering a record by omission: the retention is set once at creation and
// carried forward by every derived verb, like the five dimensions before it.
// Dropping it here is not a misorder or a disclosure but a data-loss risk -- a
// corrected permanent record that came back ephemeral would be swept away by the
// next Expire, so the correction verb, not the operator, would have decided the
// record's lifetime.
func TestCorrectionCarriesRetentionForward(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Imported, memorystore.High, memorystore.Permanent, "headcount is 240", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	corrected, err := store.Correct("fact", "headcount is 251", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if corrected.Retention != memorystore.Permanent {
		t.Fatalf("the corrected version has retention %q, want %q: a correction dropped the retention, so "+
			"amending a record a user committed to keep silently re-tiered it -- the next Expire would "+
			"tombstone a record nobody chose to expire", corrected.Retention, memorystore.Permanent)
	}
}

// TestExpiredEphemeralRecordDoesNotResurrect ties retention to the deletion
// lineage the phase's exit evidence requires. Expiry is a tombstone, not an
// unlink, so a record expired by a sweep must stay gone when the store is reopened
// over the same directory -- a replica or backup holding the earlier versions
// cannot bring it back. An unlink-based expiry would pass every in-memory check
// and fail exactly here.
func TestExpiredEphemeralRecordDoesNotResurrect(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("transient", user("ana"), memorystore.Public, memorystore.Operate,
		memorystore.Stated, memorystore.Medium, memorystore.Ephemeral, "ana opened a file", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Expire("lifecycle"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	reopened, err := memorystore.Open(store.Dir())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, _, err := reopened.Retrieve(memorystore.Query{
		Scopes:          []memorystore.Scope{user("ana")},
		Purposes:        []memorystore.Purpose{memorystore.Operate},
		EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}})
	if err != nil {
		t.Fatalf("retrieve after reopen: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an expired ephemeral record reappeared after the store was reopened (got %d): expiry "+
			"unlinked the record instead of tombstoning it, so the versions left on disk resurrected it "+
			"-- the resurrection the deletion guarantee forbids, reached through a sweep", len(got))
	}
}
