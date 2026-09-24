package memorystore

import (
	"testing"
)

// TestResolveHealsARootFork is the core of ADR-0032: a root fork that ADR-0031
// contains but leaves permanent is brought back to one current version by an
// operator naming the survivor. Before Resolve existed, Correct and Delete
// refused the forked record and a hand supersession left it still forked, so the
// remediation Fork.err printed was impossible to carry out.
func TestResolveHealsARootFork(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	keep, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op")
	if err != nil {
		t.Fatal(err)
	}
	// A competing root arrives out of band, the case the write guard cannot stop.
	loser := writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated, Confidence: Medium, Retention: Permanent, Body: "body B", Origin: "import"})

	if _, err := s.Resolve("r1", keep.VersionID, "op"); err != nil {
		t.Fatalf("Resolve refused to heal a root fork: %v: a contained fork with no way back to one "+
			"current version is a record lost forever, which is the silent loss the store exists to prevent", err)
	}

	forks, err := s.Forks()
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 0 {
		t.Fatalf("record is still forked after Resolve: %+v: Resolve must retire the losing heads so the "+
			"record has exactly one current version, or it has not resolved anything", forks)
	}

	tip, err := s.tip("r1")
	if err != nil {
		t.Fatalf("tip still refuses the record after Resolve: %v: a resolved record must read like any healthy one", err)
	}
	if tip.Body != "body A" {
		t.Fatalf("resolved record kept the wrong body %q, wanted the survivor's %q: Resolve keeps the "+
			"version the operator named, never a tiebreak the store invented", tip.Body, "body A")
	}

	// The retired version stays on disk: a receipt naming it must still resolve,
	// because append-only immutability is what ADR-0021's version identity rests
	// on. Resolve retires a head; it never unlinks the evidence.
	versions, err := s.Versions()
	if err != nil {
		t.Fatal(err)
	}
	present := false
	for _, v := range versions {
		if v.VersionID == loser.VersionID {
			present = true
		}
	}
	if !present {
		t.Fatalf("the retired version %s was removed from the store: Resolve retires a head by naming it, "+
			"not by deleting it, so a receipt already issued for it stays honest", loser.VersionID)
	}
}

// TestResolveHealsASupersessionFork covers the case ADR-0028 first saw: two
// versions superseding one predecessor. Resolve retires the losing branch's head
// the same way it retires a competing root.
func TestResolveHealsASupersessionFork(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "original", "op"); err != nil {
		t.Fatal(err)
	}
	winner, err := s.Correct("r1", "the correction that wins", "op")
	if err != nil {
		t.Fatal(err)
	}
	// A second successor of the same predecessor arrives out of band, the fork
	// the claim mechanism prevents sequentially but a replica can still deliver.
	writeRawVersion(t, s, Record{RecordID: "r1", Supersedes: winner.Supersedes, Scope: sc,
		Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated, Confidence: Medium, Retention: Permanent, Body: "the branch that loses", Origin: "import"})

	if _, err := s.Resolve("r1", winner.VersionID, "op"); err != nil {
		t.Fatalf("Resolve refused a supersession fork: %v", err)
	}
	tip, err := s.tip("r1")
	if err != nil {
		t.Fatalf("tip refused the record after resolving a supersession fork: %v", err)
	}
	if tip.Body != "the correction that wins" {
		t.Fatalf("resolved to %q, wanted the kept head %q", tip.Body, "the correction that wins")
	}
}

// TestResolveRefusesAVersionThatIsNotAHead guards the survivor argument: keeping
// a version that is not currently competing is a caller mistake, not a
// resolution, and picking a superseded or foreign version would leave the fork
// standing while reporting success.
func TestResolveRefusesAVersionThatIsNotAHead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op"); err != nil {
		t.Fatal(err)
	}
	writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated, Confidence: Medium, Retention: Permanent, Body: "body B", Origin: "import"})

	if _, err := s.Resolve("r1", "mv-doesnotexist", "op"); err == nil {
		t.Fatal("Resolve accepted a survivor that is not a current head: keeping a version that is not " +
			"competing retires nothing meaningful and leaves the fork in place while reporting success")
	}
}

// TestResolveRefusesAHealthyRecord keeps Resolve from being a second correction
// path. A record with one current version has nothing to resolve, and the
// message names Correct so the operator is not left guessing which verb applies.
func TestResolveRefusesAHealthyRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	head, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("r1", head.VersionID, "op"); err == nil {
		t.Fatal("Resolve healed a record that was not forked: resolving a healthy record would append a " +
			"redundant version and invite Resolve being used where Correct is meant")
	}
}

// TestResolveRefusesAnUnknownRecord reports the missing-record case as
// ErrNotFound rather than a confusing survivor error.
func TestResolveRefusesAnUnknownRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("nope", "mv-whatever", "op"); err == nil {
		t.Fatal("Resolve reported success for a record that does not exist")
	}
}

// TestResolvingAnAlreadyResolvedForkIsRefused is the safety half of the
// concurrency claim seen from the sequential side: once a fork is resolved the
// record has one head, so a second Resolve finds nothing to resolve and is
// refused rather than appending a redundant version. This is what stops a double
// resolution from re-forking the record it just healed.
func TestResolvingAnAlreadyResolvedForkIsRefused(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	keep, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op")
	if err != nil {
		t.Fatal(err)
	}
	writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated, Confidence: Medium, Retention: Permanent, Body: "body B", Origin: "import"})

	if _, err := s.Resolve("r1", keep.VersionID, "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("r1", keep.VersionID, "op"); err == nil {
		t.Fatal("Resolve ran a second time on an already-healed record: with one head there is no fork, and " +
			"resolving again would append a redundant version and could re-fork what was just healed")
	}
}

// TestResolveKeepingATombstoneLeavesRecordDeleted allows an operator to resolve a
// fork by deciding the record is deleted: the tombstone is a valid survivor, and
// keeping it must not resurrect the record.
func TestResolveKeepingATombstoneLeavesRecordDeleted(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, Medium, Permanent, Validity{}, "body A", "op"); err != nil {
		t.Fatal(err)
	}
	tomb, err := s.Delete("r1", "op")
	if err != nil {
		t.Fatal(err)
	}
	// A competing live root arrives out of band, forking the (now deleted) record.
	writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated, Confidence: Medium, Retention: Permanent, Body: "resurrected body", Origin: "import"})

	if _, err := s.Resolve("r1", tomb.VersionID, "op"); err != nil {
		t.Fatalf("Resolve refused to keep a tombstone: %v: an operator must be able to resolve a fork by "+
			"deciding the record stays deleted", err)
	}
	tip, err := s.tip("r1")
	if err != nil {
		t.Fatalf("tip refused after resolving to the tombstone: %v", err)
	}
	if !tip.Deleted {
		t.Fatal("resolving toward the tombstone left the record live: keeping the deletion must keep the " +
			"record deleted, or the fork became a resurrection the deletion guarantee forbids")
	}
	recs, _, err := s.Retrieve(Query{EvidenceClasses: []EvidenceClass{Stated}, Purposes: []Purpose{Operate}, Scopes: []Scope{sc}})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("a record resolved to its tombstone was still retrieved (%d records): a deleted record is "+
			"not presented", len(recs))
	}
}
