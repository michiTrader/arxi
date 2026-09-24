package memorystore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeRawVersion seals a record and writes its version file directly, bypassing
// Put's second-root guard. It stands in for an imported store, a replica or a
// concurrent first write — the ways a competing root arrives that a write-time
// guard on one process cannot prevent.
func writeRawVersion(t *testing.T, s *Store, r Record) Record {
	t.Helper()
	sealed, err := r.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	body, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), sealed.VersionID+ext), append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write raw version: %v", err)
	}
	return sealed
}

// TestApproveRefusesASecondRootForOneRecord is the write-time guard: two Approve
// calls for one record with different bodies would create two versions that
// supersede nothing, both current. The mistake is refused at the source, and the
// message names Correct as the verb that changes a record without forking it.
func TestApproveRefusesASecondRootForOneRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op"); err != nil {
		t.Fatalf("first approve failed: %v", err)
	}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body B is different", "op"); err == nil {
		t.Fatal("a second Approve for the same record with a different body succeeded: it creates a " +
			"second root, and two versions that supersede nothing are both current with no rule to " +
			"choose between them")
	}
	// An identical re-Approve is idempotent and must still succeed: version IDs
	// are content-addressed, so re-applying the same approval is the same version.
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op"); err != nil {
		t.Fatalf("an identical re-Approve was refused: %v: content-addressed writes are idempotent, "+
			"and refusing a retry breaks every caller above it", err)
	}
}

// TestApproveRefusesARootOverAProposedRecord covers the cross-kind case: a record
// that already exists as a candidate cannot gain a second root by being approved
// afresh. Promote is the transition; a new root is a fork.
func TestApproveRefusesARootOverAProposedRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Propose("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "candidate body", "model", "run-1", 1); err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "approved body", "op"); err == nil {
		t.Fatal("approving a record that already exists as a candidate created a second root: promotion " +
			"is the proposed-to-approved transition, and a fresh root forks the record instead")
	}
}

// TestAnImportedRootForkIsContainedNotSilentlyPresented is the read-side
// backstop for the case the write guard cannot prevent: two roots that arrive
// from outside one process. Before the fix retrieval returned both and tip chose
// one silently; now the record is contained, named by Forks, and refused by tip.
func TestAnImportedRootForkIsContainedNotSilentlyPresented(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op"); err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	// A competing root arrives out of band (import/replica/concurrent write).
	writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated, Confidence: Medium, Retention: Permanent, Body: "body B", Origin: "import"})

	recs, ev, err := s.Retrieve(Query{EvidenceClasses: []EvidenceClass{Stated}, Purposes: []Purpose{Operate}, Scopes: []Scope{sc}})
	if err != nil {
		t.Fatalf("retrieve errored on a forked record: %v: a fork must be contained, not raise a store-wide error", err)
	}
	if len(recs) != 0 {
		t.Fatalf("retrieval returned %d versions of a root-forked record: two current versions must be "+
			"contained, not presented together", len(recs))
	}
	found := false
	for _, id := range ev.Forked {
		if id == "r1" {
			found = true
		}
	}
	if !found {
		t.Fatal("a root-forked record the caller owns was excluded from retrieval without being named " +
			"in Forked: a record dropped with no trace is indistinguishable from one never written")
	}

	forks, err := s.Forks()
	if err != nil {
		t.Fatalf("Forks errored: %v", err)
	}
	if len(forks) != 1 || forks[0].RecordID != "r1" || forks[0].Predecessor != "" {
		t.Fatalf("Forks did not report the root fork as a predecessor-less fork: got %+v: a root fork "+
			"shares no predecessor, so it must be reported with an empty one rather than a fabricated version", forks)
	}

	if _, err := s.tip("r1"); err == nil {
		t.Fatal("tip returned a version for a root-forked record: with two current versions it must " +
			"refuse rather than silently pick one, which is the silent loss the store exists to prevent")
	}
}

// TestHealthyRecordsAreUnaffectedByTheGeneralizedForkCheck guards against the fix
// over-reaching: a normal record, a corrected record and a deleted record must
// still resolve to exactly one current version.
func TestHealthyRecordsAreUnaffectedByTheGeneralizedForkCheck(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("keep", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "original", "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Correct("keep", "corrected", "op"); err != nil {
		t.Fatalf("correct failed: %v", err)
	}
	if _, err := s.Approve("gone", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "temp", "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("gone", "op"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	tip, err := s.tip("keep")
	if err != nil || tip.Body != "corrected" {
		t.Fatalf("corrected record did not resolve to its correction (body=%q err=%v)", tip.Body, err)
	}
	forks, err := s.Forks()
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 0 {
		t.Fatalf("healthy records were reported as forked: %+v", forks)
	}
}
