package memory

import "testing"

func TestScopeValidateRefusesAMissingTenant(t *testing.T) {
	if err := (Scope{User: "u1"}).Validate(); err == nil {
		t.Fatal("a scope with no tenant validated: the tenant is the trust boundary, and an un-scoped " +
			"record treated as unconstrained would be visible in every tenant -- the maximal leak")
	}
	if err := (Scope{Tenant: "t1"}).Validate(); err != nil {
		t.Fatalf("a tenant-only scope was rejected: %v: a record confined to a tenant but no narrower is "+
			"legitimate, and forbidding it would make tenant-wide memory unrepresentable", err)
	}
}

// TestCrossTenantRetrievalReturnsNothing is the containment guarantee: no
// retrieval crosses the tenant, whatever the other axes say.
func TestCrossTenantRetrievalReturnsNothing(t *testing.T) {
	record := Scope{Tenant: "t1", User: "u1"}
	principal := Scope{Tenant: "t2", User: "u1"}
	ok, err := record.AuthorizedFor(principal)
	if err != nil {
		t.Fatalf("authorization errored on well-formed scopes: %v", err)
	}
	if ok {
		t.Fatal("a record in tenant t1 was authorized for a principal in tenant t2: the tenant is the " +
			"trust boundary no retrieval crosses, and a matching user across tenants must not open it")
	}
}

// TestCrossUserAndCrossProjectReturnNothing is the exit-evidence leakage check:
// a record constrained to a user or project is invisible to a different one.
func TestCrossUserAndCrossProjectReturnNothing(t *testing.T) {
	userScoped := Scope{Tenant: "t1", User: "u1"}
	if ok, _ := userScoped.AuthorizedFor(Scope{Tenant: "t1", User: "u2"}); ok {
		t.Fatal("a record scoped to user u1 was authorized for user u2: cross-user leakage must return zero records")
	}
	projectScoped := Scope{Tenant: "t1", Project: "p1"}
	if ok, _ := projectScoped.AuthorizedFor(Scope{Tenant: "t1", Project: "p2"}); ok {
		t.Fatal("a record scoped to project p1 was authorized for project p2: cross-project leakage must return zero records")
	}
}

// TestAnUnconstrainedAxisMatchesAnyPrincipal pins the other half: a record that
// does not constrain an axis is visible across it, within the tenant. A
// tenant-only record is visible to any principal in the tenant.
func TestAnUnconstrainedAxisMatchesAnyPrincipal(t *testing.T) {
	tenantWide := Scope{Tenant: "t1"}
	ok, err := tenantWide.AuthorizedFor(Scope{Tenant: "t1", User: "u1", Project: "p1", Run: "r1"})
	if err != nil {
		t.Fatalf("authorization errored: %v", err)
	}
	if !ok {
		t.Fatal("a tenant-wide record was not authorized for a fully-scoped principal in the same tenant: " +
			"an empty record axis is no constraint, so a tenant-wide record must be visible to anyone in it")
	}
}

// TestAConstrainedRecordIsInvisibleToABroaderPrincipal is the direction that is
// easy to get backwards: a record scoped to project p1 must not be visible to a
// principal that names no project. The constraint is on the record; a principal
// lacking the value does not satisfy it.
func TestAConstrainedRecordIsInvisibleToABroaderPrincipal(t *testing.T) {
	projectScoped := Scope{Tenant: "t1", Project: "p1"}
	if ok, _ := projectScoped.AuthorizedFor(Scope{Tenant: "t1"}); ok {
		t.Fatal("a project-scoped record was authorized for a principal with no project: a record confined " +
			"to a project must not surface in a retrieval that is not in that project, or the project " +
			"boundary is one-directional")
	}
}

func TestAuthorizationRefusesAPrincipalWithNoTenant(t *testing.T) {
	record := Scope{Tenant: "t1"}
	if _, err := record.AuthorizedFor(Scope{User: "u1"}); err == nil {
		t.Fatal("authorization accepted a principal with no tenant: an un-scoped principal cannot be " +
			"authorized against a tenant-scoped record, and defaulting to a match would cross the boundary")
	}
}

// TestAuthorizationOnAllAxesMatchesTheNarrowestPrincipal is the fully-constrained
// case: a record pinned on every axis is visible only to the exact principal.
func TestAuthorizationOnAllAxesMatchesTheNarrowestPrincipal(t *testing.T) {
	full := Scope{Tenant: "t1", User: "u1", Application: "a1", Project: "p1", Team: "tm1", Agent: "ag1", Run: "r1"}
	if ok, err := full.AuthorizedFor(full); err != nil || !ok {
		t.Fatalf("a fully-scoped record was not authorized for the identical principal (ok=%v err=%v): an "+
			"exact match on every axis must be visible", ok, err)
	}
	mismatchAgent := full
	mismatchAgent.Agent = "ag2"
	if ok, _ := full.AuthorizedFor(mismatchAgent); ok {
		t.Fatal("a record pinned to agent ag1 was authorized for agent ag2: a single differing axis must " +
			"be enough to refuse, or the narrowest scope is not actually enforced")
	}
}
