package memory

import "testing"

func TestVersionValidateRequiresBothIdentities(t *testing.T) {
	good := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a fully identified version was rejected: %v: a legitimate record cannot enter a lineage", err)
	}
	if err := (Version{VersionID: "ver-1", Kind: KindApprovedMemoryRecord}).Validate(); err == nil {
		t.Fatal("a version with no record_id validated: it cannot be joined to its lineage, so a " +
			"correction could never find what it supersedes")
	}
	if err := (Version{RecordID: "rec-1", Kind: KindApprovedMemoryRecord}).Validate(); err == nil {
		t.Fatal("a version with no version_id validated: a correction could not name the version it " +
			"replaces, which is the identity ADR-0021 requires")
	}
}

// TestVersionRequiresAGovernedKind pins ADR-0032: a stored version carries its
// authority, and that authority must be a governed record kind. Frozen
// configuration memory has no record identity, so it is presented from a
// blueprint and never stored as a version; an unknown or empty kind fails
// closed.
func TestVersionRequiresAGovernedKind(t *testing.T) {
	if err := (Version{RecordID: "rec-1", VersionID: "ver-1", Validity: Validity{ValidFrom: t1, RecordedAt: t1}}).Validate(); err == nil {
		t.Fatal("a version with no kind validated: a stored record must say what authority it carries, " +
			"or a store cannot authorize it before ranking")
	}
	if err := (Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindFrozenContextMemory, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}).Validate(); err == nil {
		t.Fatal("a version of kind frozen_context_memory validated: frozen configuration memory has no " +
			"record identity, so it is never a stored version -- storing one would mint a record id a " +
			"blueprint field does not have")
	}
	candidate := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindProposedMemoryCandidate, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	if err := candidate.Validate(); err != nil {
		t.Fatalf("a proposed candidate version was rejected: %v: a candidate is a governed stored record "+
			"that may be inspected and promoted, so it is a valid version even though it is never presented", err)
	}
}

// TestSupersedeTilesTheBeliefAxisByConstruction is the point of making
// supersession an operation: the predecessor closes at exactly the instant the
// successor opens, without the caller aligning two timestamps by hand. ADR-0028
// proved tiling on literals; this proves the code that produces a correction
// cannot leave a gap or an overlap.
func TestSupersedeTilesTheBeliefAxisByConstruction(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}

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
	if successor.Kind != v1.Kind {
		t.Fatalf("supersede changed the authority kind from %q to %q: a correction to a record keeps its "+
			"authority, or a store could launder an approved record into a candidate (or the reverse) "+
			"through a correction", string(v1.Kind), string(successor.Kind))
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
	v1closed := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	if _, _, err := v1closed.Supersede("ver-3", t3, t1, ""); err == nil {
		t.Fatal("superseding an already-superseded version was allowed: it forks the lineage into two " +
			"chains claiming the same predecessor, and a fork has no single answer to what was believed next")
	}
}

func TestSupersedeRefusesReusedOrMissingSuccessorIdentity(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
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
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
	if _, _, err := v1.Supersede("ver-2", t1, t1, ""); err == nil {
		t.Fatal("supersede at an instant before the version's recorded_at was allowed: the closed belief " +
			"interval would run backwards and hide the version from the query meant to prove it was current")
	}
}

// TestValidateLineageAcceptsAContiguousAppendOnlyChain builds a three-version
// history through the operation and checks the validator accepts it, then checks
// the historical and present queries the chain is supposed to support.
func TestValidateLineageAcceptsAContiguousAppendOnlyChain(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
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
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	v2 := Version{RecordID: "rec-1", VersionID: "ver-2", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t3}}
	if err := ValidateLineage([]Version{v1, v2}); err == nil {
		t.Fatal("a lineage with a gap between the closed predecessor and its successor validated: an " +
			"instant between them has no belief, so an as-of query there answers with nothing while the " +
			"fact was in fact held")
	}
}

func TestValidateLineageRejectsANonFinalCurrentVersion(t *testing.T) {
	// v1 is still current but is not last: a fork.
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	v2 := Version{RecordID: "rec-1", VersionID: "ver-2", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
	if err := ValidateLineage([]Version{v1, v2}); err == nil {
		t.Fatal("a lineage with a non-final current version validated: two live versions of one record " +
			"is a fork, and the history has two answers to what is believed now")
	}
}

func TestValidateLineageRejectsAMixedRecord(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	other := Version{RecordID: "rec-2", VersionID: "ver-2", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
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

// TestValidateLineageAcceptsAClosedMidCorrectionTail records that a lineage may
// end on a closed, non-retracted version -- a correction whose successor has not
// been appended yet. That tail is distinct from a retraction, which is why the
// resurrection rule keys on the Retracted marker and not on closure alone.
func TestValidateLineageAcceptsAClosedMidCorrectionTail(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	if err := ValidateLineage([]Version{v1}); err != nil {
		t.Fatalf("a single closed non-retracted version was rejected as a lineage: %v: a fact whose "+
			"correction has not yet been appended is a legitimate in-progress history", err)
	}
}

// TestRetractProducesATerminalTombstoneThatPreservesHistory is the deletion
// scenario Phase 7 names: after retraction the present belief is nothing, and
// the history before it is untouched.
func TestRetractProducesATerminalTombstoneThatPreservesHistory(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	tomb, err := v1.Retract(t2)
	if err != nil {
		t.Fatalf("retracting a current version failed: %v: deletion is a required operation of a governed store", err)
	}
	if !tomb.Retracted || tomb.Validity.SupersededAt != t2 || tomb.Validity.Current() {
		t.Fatalf("retract did not produce a closed terminal tombstone (retracted=%v superseded_at=%q current=%v): "+
			"a deletion closes the belief at the deletion instant and marks the closure terminal",
			tomb.Retracted, tomb.Validity.SupersededAt, tomb.Validity.Current())
	}
	if v1.Retracted || v1.Validity.SupersededAt != "" {
		t.Fatal("Retract mutated the receiver: a deletion appends a tombstone; the live version's bytes are untouched")
	}
	// Present belief is nothing; history before the deletion is preserved.
	if live, err := tomb.Validity.LiveAt(t3, t3); err != nil || live {
		t.Fatalf("the deleted record is still live as-of now (live=%v err=%v): a retraction that does not "+
			"remove the record from the present is not a deletion", live, err)
	}
	if live, err := tomb.Validity.LiveAt(t1, t3); err != nil || !live {
		t.Fatalf("the deleted record is not visible as-of before its deletion (live=%v err=%v): deletion "+
			"closes the belief interval, it does not erase the history, or an audit of what was believed "+
			"becomes impossible", live, err)
	}
	if err := ValidateLineage([]Version{tomb}); err != nil {
		t.Fatalf("a single retracted version is not a valid lineage: %v", err)
	}
}

func TestRetractRefusesAnAlreadyClosedVersion(t *testing.T) {
	closed := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	if _, err := closed.Retract(t3); err == nil {
		t.Fatal("retracting an already-closed version was allowed: only the current version can be " +
			"deleted, and retracting a closed one would rewrite a belief interval the history already fixed")
	}
}

// TestSupersedeRefusesARetractedVersion is the resurrection guard at the
// operation: a deleted record cannot be corrected back into existence.
func TestSupersedeRefusesARetractedVersion(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	tomb, err := v1.Retract(t2)
	if err != nil {
		t.Fatalf("retract failed: %v", err)
	}
	if _, _, err := tomb.Supersede("ver-2", t3, t1, ""); err == nil {
		t.Fatal("superseding a retracted version was allowed: a deletion is terminal, and appending a " +
			"successor to it resurrects a deleted record")
	}
}

// TestValidateLineageRefusesResurrectionAfterRetraction is the resurrection
// guard at the chain: even a hand-built successor recorded exactly at the
// deletion instant is refused, because the predecessor was retracted, not
// superseded.
func TestValidateLineageRefusesResurrectionAfterRetraction(t *testing.T) {
	tomb := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Retracted: true, Validity: Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}}
	successor := Version{RecordID: "rec-1", VersionID: "ver-2", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t2}}
	if err := ValidateLineage([]Version{tomb, successor}); err == nil {
		t.Fatal("a chain with a version after a retraction validated: a retraction is terminal, so a " +
			"contiguous successor is a resurrection the deletion evidence forbids -- and it is indistinguishable " +
			"from a supersession without the terminal marker, which is why the marker exists")
	}
}

// TestValidateLineageAcceptsACorrectionThenRetraction is the mixed history: a
// fact corrected once and then deleted. The middle version is a normal
// supersession; only the last is terminal.
func TestValidateLineageAcceptsACorrectionThenRetraction(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	v1c, v2, err := v1.Supersede("ver-2", t2, t1, "")
	if err != nil {
		t.Fatalf("correction failed: %v", err)
	}
	tomb, err := v2.Retract(t3)
	if err != nil {
		t.Fatalf("retraction failed: %v", err)
	}
	if err := ValidateLineage([]Version{v1c, tomb}); err != nil {
		t.Fatalf("a correction-then-retraction history was rejected: %v: a fact that was fixed and later "+
			"deleted is a legitimate append-only history", err)
	}
}

func TestValidateRefusesARetractedButStillCurrentVersion(t *testing.T) {
	bad := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Retracted: true, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	if err := bad.Validate(); err == nil {
		t.Fatal("a retracted version with an open belief interval validated: a retraction closes the " +
			"belief at the deletion instant, so a retracted-but-current version would report the deleted " +
			"fact as the present answer")
	}
}
