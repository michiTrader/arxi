package memorystore

import "testing"

// TestARetireCannotRemoveAnotherRecordsHead is ADR-0033 for the edge ADR-0032
// introduced. Retires is a head-removing edge, and nothing validated that its
// targets belong to the record declaring them. A version of one record naming
// another record's head in Retires must not remove that head: the victim would
// otherwise have zero current versions -- unreadable, with no fork and no
// tombstone -- and the victim can be in another tenant, so the loss crosses the
// one boundary no retrieval may cross.
func TestARetireCannotRemoveAnotherRecordsHead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	victimScope := Scope{Principal: Tenant, ID: "acme"}
	attackerScope := Scope{Principal: User, ID: "u1"}

	victim, err := s.Approve("victim", victimScope, Public, Operate, Stated, "important tenant memory", "op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve("attacker", attackerScope, Public, Operate, Stated, "attacker body", "op"); err != nil {
		t.Fatal(err)
	}
	// An imported/replicated/corrupt version of a different record names the
	// victim's head in Retires.
	writeRawVersion(t, s, Record{RecordID: "attacker", Scope: attackerScope, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated,
		Body: "retires a foreign record", Origin: "import", Retires: []string{victim.VersionID}})

	tip, err := s.tip("victim")
	if err != nil {
		t.Fatalf("a foreign record's Retires removed the victim's head: %v: an edge that crosses records "+
			"must be ignored, or one record can silently delete another -- across a scope boundary, no "+
			"less -- with no fork and no tombstone to show it happened", err)
	}
	if tip.Body != "important tenant memory" {
		t.Fatalf("victim resolved to %q, not its own body: a foreign edge must not change what a record says", tip.Body)
	}
	recs, _, err := s.Retrieve(Query{EvidenceClasses: []EvidenceClass{Stated}, Purposes: []Purpose{Operate}, Scopes: []Scope{victimScope}})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("victim retrieved %d records by its own scope, want 1: a foreign retire must leave the "+
			"record fully readable", len(recs))
	}
}

// TestASupersedeCannotRemoveAnotherRecordsHead is the same containment for the
// pre-existing Supersedes edge, measured at its widest point rather than only for
// the field ADR-0032 added. A version superseding a foreign record's head would
// otherwise remove it the same silent way.
func TestASupersedeCannotRemoveAnotherRecordsHead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	victimScope := Scope{Principal: Tenant, ID: "acme"}
	attackerScope := Scope{Principal: User, ID: "u1"}

	victim, err := s.Approve("victim", victimScope, Public, Operate, Stated, "important tenant memory", "op")
	if err != nil {
		t.Fatal(err)
	}
	writeRawVersion(t, s, Record{RecordID: "attacker", Scope: attackerScope, Kind: Approved, Sensitivity: Public, Purpose: Operate, EvidenceClass: Stated,
		Body: "supersedes a foreign record", Origin: "import", Supersedes: victim.VersionID})

	tip, err := s.tip("victim")
	if err != nil {
		t.Fatalf("a foreign record's Supersedes removed the victim's head: %v: supersession is a "+
			"within-record edge, and honoring it across records lets one record silently delete another", err)
	}
	if tip.Body != "important tenant memory" {
		t.Fatalf("victim resolved to %q, not its own body", tip.Body)
	}
}

// TestSameRecordEdgesStillApplyAfterContainment guards against the fix
// over-reaching: within one record, supersession and retirement must still remove
// the head, or Correct and Resolve would stop working.
func TestSameRecordEdgesStillApplyAfterContainment(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	if _, err := s.Approve("r1", sc, Public, Operate, Stated, "original", "op"); err != nil {
		t.Fatal(err)
	}
	corrected, err := s.Correct("r1", "corrected", "op")
	if err != nil {
		t.Fatalf("a same-record correction was refused after the containment change: %v", err)
	}
	tip, err := s.tip("r1")
	if err != nil {
		t.Fatal(err)
	}
	if tip.VersionID != corrected.VersionID {
		t.Fatalf("same-record supersession no longer moves the head: tip is %s, want the correction %s: "+
			"the containment guard must ignore only foreign edges, never within-record ones",
			tip.VersionID, corrected.VersionID)
	}
}
