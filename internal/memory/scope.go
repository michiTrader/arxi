package memory

import "fmt"

// Scope is the set of principals a memory record is confined to, and the set a
// retrieval is performed on behalf of. The vocabulary is the one ADR-0022 fixed
// after reconciling three disagreeing documents: tenant, user, application,
// project, team, agent, run. `subject` is deliberately absent -- it named two
// things at once and was removed there.
//
// The same type serves a record and a principal. On a record, a non-empty axis
// is a constraint ("this record belongs to project P"); an empty axis is the
// absence of a constraint on that axis ("not confined to any one project"). On a
// principal, an axis is the value the retrieval carries ("this run is in project
// P"). Authorization is the match between the two, and it runs before ranking:
// a store cannot rank what it is not allowed to see.
type Scope struct {
	Tenant      string `json:"tenant"`
	User        string `json:"user,omitempty"`
	Application string `json:"application,omitempty"`
	Project     string `json:"project,omitempty"`
	Team        string `json:"team,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Run         string `json:"run,omitempty"`
}

// Validate refuses a record scope with no tenant. The tenant is the trust
// boundary, and ADR-0022 named the asymmetry that makes it different from every
// other axis: a record with no project may legitimately be visible across
// projects, but a record with no tenant must be visible to nobody. Encoding that
// as "absent tenant is unconstrained" would make an un-scoped record visible in
// every tenant -- the maximal leak -- so a missing tenant fails closed at rest
// rather than being discovered as a query bug.
func (s Scope) Validate() error {
	if s.Tenant == "" {
		return fmt.Errorf("memory scope has no tenant: the tenant is the trust boundary no retrieval " +
			"crosses, so a record without one must never be visible -- it is refused here rather than " +
			"treated as unconstrained, which would make it visible in every tenant")
	}
	return nil
}

// AuthorizedFor reports whether this record scope permits retrieval by the given
// principal. It is the authorization that runs before semantic ranking.
//
// The tenant must match exactly -- the trust boundary is crossed by no one. Every
// other axis the record constrains must match the principal; an axis the record
// leaves empty is unconstrained and matches any principal within the tenant. So a
// record scoped to user U1 is invisible to user U2 (cross-user leakage returns
// nothing), while a record scoped only to a tenant is visible to any principal in
// it. The principal must name a tenant, or there is nothing to authorize against
// and defaulting to a match would cross the very boundary this exists to hold.
func (record Scope) AuthorizedFor(principal Scope) (bool, error) {
	if err := record.Validate(); err != nil {
		return false, err
	}
	if principal.Tenant == "" {
		return false, fmt.Errorf("authorization principal has no tenant: an un-scoped principal cannot be " +
			"authorized against a tenant-scoped record, and treating it as a match would cross the trust " +
			"boundary the tenant exists to hold")
	}
	if record.Tenant != principal.Tenant {
		return false, nil
	}
	// Every axis the record constrains must be satisfied by the principal. An
	// empty record axis is no constraint; a set one the principal does not carry
	// is a mismatch, which is how a project- or user-scoped record stays out of a
	// retrieval that is not in that project or for that user.
	axes := []struct {
		name              string
		record, principal string
	}{
		{"user", record.User, principal.User},
		{"application", record.Application, principal.Application},
		{"project", record.Project, principal.Project},
		{"team", record.Team, principal.Team},
		{"agent", record.Agent, principal.Agent},
		{"run", record.Run, principal.Run},
	}
	for _, a := range axes {
		if a.record != "" && a.record != a.principal {
			return false, nil
		}
	}
	return true, nil
}
