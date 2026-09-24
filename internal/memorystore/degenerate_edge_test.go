package memorystore

import "testing"

// TestASelfReferentialRetireCannotFreezeARecord pins an emergent safety property,
// not a guard in the code: a version cannot name its own ID in Retires, so the
// degenerate "a version retires itself" that would leave a record with zero heads
// is unconstructable rather than merely unhandled.
//
// The reason is content addressing. A version ID is the digest of an identity
// that includes Retires (ADR-0032) and Supersedes, so the moment a version lists
// a target its own ID shifts, and it can never come out equal to a target it
// already contains. An attempt to build a self-retire therefore names a version
// that is not in the store, and ADR-0033 ignores an edge whose target is not the
// declaring record's own -- here it is no record's -- so the writer stays a
// healthy head.
//
// This test exists because that safety is one deleted struct field away from
// gone. If Retires (or Supersedes) were dropped from `identity`, the ID would no
// longer cover it, a self-retire would become constructable, and the record would
// be frozen with zero heads, unreadable and invisible to Forks -- the ADR-0029
// pathology through a new door. Removing the field from `identity` makes this
// test fail, which is the point: it is why no runtime guard against a self-edge
// is needed.
func TestASelfReferentialRetireCannotFreezeARecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}

	// The ID a plain version would have. An importer trying to make a version
	// retire itself would aim at exactly this.
	plain, err := Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, Body: "body A", Origin: "op"}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	// Writing that same record but naming plain's ID in Retires produces a
	// DIFFERENT version, because Retires is part of the identity.
	written := writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate,
		Body: "body A", Origin: "op", Retires: []string{plain.VersionID}})
	if written.VersionID == plain.VersionID {
		t.Fatal("a version that lists a target in Retires kept the same version ID as one that does not: " +
			"Retires is not part of the identity, so a version can now name its own ID and retire itself, " +
			"freezing the record with zero heads. Retires must stay in `identity`")
	}

	tip, err := s.tip("r1")
	if err != nil {
		t.Fatalf("the record is frozen after a self-referential retire attempt: %v: the retire targeted a "+
			"non-existent version and should have been inert, leaving one healthy head", err)
	}
	if tip.Body != "body A" {
		t.Fatalf("record resolved to %q, want its own body", tip.Body)
	}
}

// TestMutuallyRetiringRootsAreAVisibleForkNotAFrozenRecord is the two-version form
// of the same property: two roots that each try to retire the other cannot both
// hold the other's real ID, so neither edge lands, and the record surfaces as a
// visible root fork (resolvable via ADR-0032) rather than a silent zero-head
// freeze. If Retires left the identity, the IDs would line up, both would be
// retired, and the record would vanish with no fork to show it.
func TestMutuallyRetiringRootsAreAVisibleForkNotAFrozenRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := Scope{Principal: User, ID: "u1"}
	a, err := Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, Body: "body A", Origin: "op"}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, Body: "body B", Origin: "op"}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, Body: "body A",
		Origin: "op", Retires: []string{b.VersionID}})
	writeRawVersion(t, s, Record{RecordID: "r1", Scope: sc, Kind: Approved, Sensitivity: Public, Purpose: Operate, Body: "body B",
		Origin: "op", Retires: []string{a.VersionID}})

	forks, err := s.Forks()
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 1 || forks[0].RecordID != "r1" {
		t.Fatalf("two mutually-retiring roots were not reported as a fork: got %+v: if their retire edges "+
			"had landed the record would have zero heads and disappear silently, so the visible fork is the "+
			"safe outcome and its absence means the edges are removing heads they should not", forks)
	}
	if _, err := s.tip("r1"); err == nil {
		t.Fatal("tip returned a version for a forked record: a root fork must be refused, not silently resolved")
	}
}
