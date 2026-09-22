package memory

import "testing"

func validSelection() Selection {
	return Selection{RecordID: "rec-1", VersionID: "ver-1", Reason: "top match on the query"}
}

func TestRetrievalReceiptAcceptsAWellFormedReceipt(t *testing.T) {
	r := RetrievalReceipt{
		Principal:      Scope{Tenant: "t1", User: "u1"},
		RankingVersion: "arxi.rank/v1",
		Selections:     []Selection{validSelection()},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("a well-formed retrieval receipt was rejected: %v: a legitimate retrieval must be recordable", err)
	}
}

// TestAnEmptyRetrievalIsAValidReceipt pins that a retrieval which matched nothing
// still produces a receipt. A cross-tenant query returns zero selections, and
// that receipt -- proving nothing influenced the turn -- is exactly the evidence
// the leakage tests assert.
func TestAnEmptyRetrievalIsAValidReceipt(t *testing.T) {
	r := RetrievalReceipt{Principal: Scope{Tenant: "t1"}, RankingVersion: "arxi.rank/v1"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a retrieval that selected nothing was rejected: %v: a retrieval matching no records is "+
			"legitimate, and its empty receipt is the proof nothing influenced the turn", err)
	}
}

func TestRetrievalReceiptRequiresAnAuthorizablePrincipal(t *testing.T) {
	r := RetrievalReceipt{Principal: Scope{User: "u1"}, RankingVersion: "arxi.rank/v1"}
	if err := r.Validate(); err == nil {
		t.Fatal("a receipt with a tenant-less principal validated: a retrieval with no trust boundary is " +
			"not an authorized retrieval, so its receipt cannot be trusted to have respected one")
	}
}

func TestRetrievalReceiptRequiresARankingVersion(t *testing.T) {
	r := RetrievalReceipt{Principal: Scope{Tenant: "t1"}, Selections: []Selection{validSelection()}}
	if err := r.Validate(); err == nil {
		t.Fatal("a receipt with no ranking_version validated: a selection is only reproducible against the " +
			"ranking that produced it, so the receipt could not be replayed or audited")
	}
}

// TestEveryInfluenceIdentifiesItsSourceAndVersion is the exit-evidence assertion
// on the retrieval side: a selection missing its record or version is refused.
func TestEveryInfluenceIdentifiesItsSourceAndVersion(t *testing.T) {
	base := RetrievalReceipt{Principal: Scope{Tenant: "t1"}, RankingVersion: "arxi.rank/v1"}

	noRecord := base
	noRecord.Selections = []Selection{{VersionID: "ver-1", Reason: "match"}}
	if err := noRecord.Validate(); err == nil {
		t.Fatal("a selection with no record_id validated: an influence that does not name its source " +
			"cannot be traced, corrected or deleted")
	}

	noVersion := base
	noVersion.Selections = []Selection{{RecordID: "rec-1", Reason: "match"}}
	if err := noVersion.Validate(); err == nil {
		t.Fatal("a selection with no version_id validated: a correction cannot say whether the influence " +
			"was the superseded version or the one that replaced it")
	}
}

func TestRetrievalSelectionRequiresAReason(t *testing.T) {
	r := RetrievalReceipt{
		Principal:      Scope{Tenant: "t1"},
		RankingVersion: "arxi.rank/v1",
		Selections:     []Selection{{RecordID: "rec-1", VersionID: "ver-1"}},
	}
	if err := r.Validate(); err == nil {
		t.Fatal("a selection with no reason validated: an influence nobody can explain is an influence " +
			"nobody can review, and the exit evidence names reasons beside versions")
	}
}

func TestRetrievalReceiptRefusesADoubleCountedVersion(t *testing.T) {
	r := RetrievalReceipt{
		Principal:      Scope{Tenant: "t1"},
		RankingVersion: "arxi.rank/v1",
		Selections: []Selection{
			{RecordID: "rec-1", VersionID: "ver-1", Reason: "first"},
			{RecordID: "rec-1", VersionID: "ver-1", Reason: "again"},
		},
	}
	if err := r.Validate(); err == nil {
		t.Fatal("a receipt selecting the same version twice validated: a double-counted influence " +
			"overstates what one version contributed to the turn")
	}
}
