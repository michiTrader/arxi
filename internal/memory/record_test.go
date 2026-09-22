package memory

import "testing"

// correctedRecord builds a record whose fact was recorded at t1 and corrected at
// t2, scoped to one tenant and user. It is the fixture the visibility tests
// compose over.
func correctedRecord() Record {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	v1c, v2, err := v1.Supersede("ver-2", t2, t1, "")
	if err != nil {
		panic(err)
	}
	return Record{Scope: Scope{Tenant: "tn-1", User: "u1"}, Versions: []Version{v1c, v2}}
}

func TestRecordValidateComposesScopeAndLineage(t *testing.T) {
	if err := correctedRecord().Validate(); err != nil {
		t.Fatalf("a well-formed record was rejected: %v: a tenant-scoped record with a contiguous lineage must validate", err)
	}
	// A bad scope fails through the record.
	noTenant := correctedRecord()
	noTenant.Scope.Tenant = ""
	if err := noTenant.Validate(); err == nil {
		t.Fatal("a record with no tenant validated: the record's scope check must refuse the trust-boundary gap")
	}
	// A broken lineage fails through the record.
	forked := correctedRecord()
	forked.Versions[0].Validity.SupersededAt = "" // now two current versions
	if err := forked.Validate(); err == nil {
		t.Fatal("a record with a forked lineage validated: the record's lineage check must refuse a non-final current version")
	}
}

// TestVisibleToAuthorizesBeforeSelecting is the composed retrieval primitive: a
// principal outside the record's scope sees nothing, and a principal inside it
// sees the version in force.
func TestVisibleToAuthorizesBeforeSelecting(t *testing.T) {
	r := correctedRecord()

	// Wrong user: not authorized, so nothing is visible -- and no error, because
	// an unauthorized retrieval is empty, not a failure.
	if v, ok, err := r.VisibleTo(Scope{Tenant: "tn-1", User: "u2"}, t3, t3); err != nil || ok {
		t.Fatalf("a record scoped to user u1 was visible to user u2 (ok=%v v=%q err=%v): authorization "+
			"runs before selection, so an out-of-scope principal sees nothing", ok, v.VersionID, err)
	}

	// Right principal, present query: the corrected (current) version is in force.
	v, ok, err := r.VisibleTo(Scope{Tenant: "tn-1", User: "u1"}, t3, t3)
	if err != nil || !ok {
		t.Fatalf("the authorized principal saw nothing as-of now (ok=%v err=%v): the current version must be visible", ok, err)
	}
	if v.VersionID != "ver-2" {
		t.Fatalf("present query returned version %q, want the current ver-2: the version in force as-of now "+
			"is the correction, not the version it superseded", v.VersionID)
	}
}

// TestVisibleToRespectsTheAsOfInstant composes authorization with the historical
// axis: as-of before the correction, the same authorized principal sees the
// superseded version, so history is preserved through the retrieval primitive,
// not just on the raw validity.
func TestVisibleToRespectsTheAsOfInstant(t *testing.T) {
	r := correctedRecord()
	v, ok, err := r.VisibleTo(Scope{Tenant: "tn-1", User: "u1"}, t1, t3)
	if err != nil || !ok {
		t.Fatalf("a historical query saw nothing (ok=%v err=%v): the version believed at t1 must be visible as-of t1", ok, err)
	}
	if v.VersionID != "ver-1" {
		t.Fatalf("historical query as-of t1 returned %q, want ver-1: a retrieval as-of when the first "+
			"version was current must return it, or correction has rewritten history", v.VersionID)
	}
}

// TestVisibleToReturnsNothingForARetractedRecordInThePresent composes
// authorization with deletion: after retraction the authorized principal sees
// nothing now, but still sees the record as-of before the deletion.
func TestVisibleToReturnsNothingForARetractedRecordInThePresent(t *testing.T) {
	v1 := Version{RecordID: "rec-1", VersionID: "ver-1", Kind: KindApprovedMemoryRecord, Validity: Validity{ValidFrom: t1, RecordedAt: t1}}
	tomb, err := v1.Retract(t2)
	if err != nil {
		t.Fatalf("retract failed: %v", err)
	}
	r := Record{Scope: Scope{Tenant: "tn-1"}, Versions: []Version{tomb}}
	principal := Scope{Tenant: "tn-1"}

	if _, ok, err := r.VisibleTo(principal, t3, t3); err != nil || ok {
		t.Fatalf("a retracted record was visible as-of now (ok=%v err=%v): deletion removes the record from the present", ok, err)
	}
	if _, ok, err := r.VisibleTo(principal, t1, t3); err != nil || !ok {
		t.Fatalf("a retracted record was invisible as-of before its deletion (ok=%v err=%v): deletion "+
			"closes the belief, it does not erase the history", ok, err)
	}
}

func TestRecordIDIsTheSharedIdentity(t *testing.T) {
	if got := correctedRecord().RecordID(); got != "rec-1" {
		t.Fatalf("RecordID() = %q, want rec-1: the record's identity is the one every version shares", got)
	}
}
