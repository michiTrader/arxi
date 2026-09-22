package memory

import "testing"

func TestVersionValidateRequiresBothIdentities(t *testing.T) {
	good := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a fully identified version was rejected: %v: a legitimate record cannot enter a lineage", err)
	}
	if err := (Version{VersionID: "ver-1"}).Validate(); err == nil {
		t.Fatal("a version with no record_id validated: it cannot be joined to its lineage, so a " +
			"correction could never find what it supersedes")
	}
	if err := (Version{RecordID: "rec-1"}).Validate(); err == nil {
		t.Fatal("a version with no version_id validated: a correction could not name the version it " +
			"replaces, which is the identity ADR-0021 requires")
	}
}

// TestSupersedeTilesTheBeliefAxisByConstruction is the point of making
// supersession an operation: the predecessor closes at exactly the instant the
// successor opens, without the caller aligning two timestamps by hand. ADR-0028
// proved tiling on literals; this proves the code that produces a correction
// cannot leave a gap or an overlap.
func TestSupersedeTilesTheBeliefAxisByConstruction(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1}}

	closed, successor, err := v1.Supersede("ver-2", t2, t1, "")
	if err != nil {
		t.Fatalf("superseding a current version failed: %v: correction is the core operation of a "+
			"governed store and must be expressible", err)
	}
	if closed.Validity.SupersededAt != successor.Validity.RecordedAt {
		t.Fatalf("predecessor superseded_at %q does not equal successor recorded_at %q: the operation "+
			"exists so the boundary is shared by construction, and a mismatch reintroduces the gap or "+
			"overlap it was meant to prevent", closed.Validity.SupersededAt, successor.Validity.RecordedAt)
	}
	if closed.VersionID != v1.VersionID || successor.RecordID != v1.RecordID {
		t.Fatalf("supersede changed an identity it must preserve (closed=%q successor.record=%q): the "+
			"predecessor keeps its version identity and the successor inherits the record identity, or "+
			"the lineage cannot be reassembled", closed.VersionID, successor.RecordID)
	}
	if v1.Validity.SupersededAt != "" {
		t.Fatal("Supersede mutated the receiver's validity: a correction appends and closes a copy; " +
			"mutating the original in place is the in-place edit ADR-0028 forbids")
	}
	if err := ValidateLineage([]Version{closed, successor}); err != nil {
		t.Fatalf("the chain produced by Supersede is not a valid lineage: %v: the operation must yield "+
			"a history its own validator accepts", err)
	}
}

func TestSupersedeRefusesAnAlreadySupersededVersion(t *testing.T) {
	// v1 is already closed at t2.
	v1closed := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	if _, _, err := v1closed.Supersede("ver-3", t3, t1, ""); err == nil {
		t.Fatal("superseding an already-superseded version was allowed: it forks the lineage into two " +
			"chains claiming the same predecessor, and a fork has no single answer to what was believed next")
	}
}

func TestSupersedeRefusesReusedOrMissingSuccessorIdentity(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	if _, _, err := v1.Supersede("", t2, t1, ""); err == nil {
		t.Fatal("supersede with an empty successor version_id was allowed: the correction cannot be told " +
			"apart from the version it replaces")
	}
	if _, _, err := v1.Supersede("ver-1", t2, t1, ""); err == nil {
		t.Fatal("supersede reusing the predecessor's version_id was allowed: a version identity is " +
			"immutable, so a correction is a new version, never the same one re-pointed")
	}
}

func TestSupersedeRefusesACorrectionBeforeTheRecordedInstant(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
	if _, _, err := v1.Supersede("ver-2", t1, t1, ""); err == nil {
		t.Fatal("supersede at an instant before the version's recorded_at was allowed: the closed belief " +
			"interval would run backwards and hide the version from the query meant to prove it was current")
	}
}

// TestValidateLineageAcceptsAContiguousAppendOnlyChain builds a three-version
// history through the operation and checks the validator accepts it, then checks
// the historical and present queries the chain is supposed to support.
func TestValidateLineageAcceptsAContiguousAppendOnlyChain(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	v1c, v2, err := v1.Supersede("ver-2", t2, t1, "")
	if err != nil {
		t.Fatalf("first correction failed: %v", err)
	}
	v2c, v3, err := v2.Supersede("ver-3", t3, t1, "")
	if err != nil {
		t.Fatalf("second correction failed: %v", err)
	}
	chain := []Version{v1c, v2c, v3}
	if err := ValidateLineage(chain); err != nil {
		t.Fatalf("a contiguous append-only chain was rejected: %v: a legitimate correction history must "+
			"validate, or the store cannot represent more than one correction", err)
	}
	// The last version is the only current one.
	if !v3.Validity.Current() || v1c.Validity.Current() || v2c.Validity.Current() {
		t.Fatal("more than one version reports current in a corrected chain: exactly the last is the " +
			"present belief")
	}
}

func TestValidateLineageRejectsAGapBetweenVersions(t *testing.T) {
	// v1 closes at t2 but v2 was recorded at t3: an instant [t2,t3) has no belief.
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	v2 := Version{RecordID: "rec-1", VersionID: "ver-2", Validity: Validity{ValidFrom: t1, RecordedAt: t3}}
	if err := ValidateLineage([]Version{v1, v2}); err == nil {
		t.Fatal("a lineage with a gap between the closed predecessor and its successor validated: an " +
			"instant between them has no belief, so an as-of query there answers with nothing while the " +
			"fact was in fact held")
	}
}

func TestValidateLineageRejectsANonFinalCurrentVersion(t *testing.T) {
	// v1 is still current but is not last: a fork.
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	v2 := Version{RecordID: "rec-1", VersionID: "ver-2", Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
	if err := ValidateLineage([]Version{v1, v2}); err == nil {
		t.Fatal("a lineage with a non-final current version validated: two live versions of one record " +
			"is a fork, and the history has two answers to what is believed now")
	}
}

func TestValidateLineageRejectsAMixedRecord(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	other := Version{RecordID: "rec-2", VersionID: "ver-2", Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
	if err := ValidateLineage([]Version{v1, other}); err == nil {
		t.Fatal("a lineage mixing two record ids validated: a correction to one fact would silently " +
			"answer a query about another")
	}
}

func TestValidateLineageRejectsAnEmptyChain(t *testing.T) {
	if err := ValidateLineage(nil); err == nil {
		t.Fatal("an empty lineage validated: an empty chain is not a history, and a correction query " +
			"over it can only answer vacuously")
	}
}

// TestValidateLineageAcceptsAFullyRetractedChain records that a lineage may end
// closed, with no current version. This is the tombstone shape a later
// deletion-lineage decision will build on; this test pins only that the chain
// validator does not require a current tail, because deletion semantics are
// undecided (roadmap item 7).
func TestValidateLineageAcceptsAFullyRetractedChain(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	if err := ValidateLineage([]Version{v1}); err != nil {
		t.Fatalf("a single fully-retracted version was rejected as a lineage: %v: a fact recorded and "+
			"then retracted is a legitimate history, and forbidding it here would decide deletion "+
			"semantics that remain open", err)
	}
}
