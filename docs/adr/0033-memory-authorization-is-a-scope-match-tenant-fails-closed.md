# ADR-0033: Memory authorization is a scope match, and the tenant fails closed

- Status: accepted
- Affects: `internal/memory`
- Depends on: ADR-0022
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0022 fixed the memory scope vocabulary — tenant, user, application, project,
team, agent, run — after finding three documents that disagreed, and it stopped
there deliberately: "a scope type with no store to authorize would be a field
nothing fails on, which is exactly what ADR-0021 was written about." The record
side now has identity, authority and validity (ADR-0021, 0023/0031, 0028, 0032).
Phase 7 authorizes *before* it ranks, and its exit evidence leads with a
containment claim — "cross-user and cross-project leakage tests return zero
records." There was no type expressing that authorization, so there was nothing
to point those tests at.

The type ADR-0022 declined to add is now justified, because it comes with the
operation ADR-0022 said was missing: a scope that *authorizes* is not a field
nothing fails on.

## Decision

`internal/memory` gains a `Scope` (the seven axes) and the authorization
primitive `record.AuthorizedFor(principal)`. The same type describes a record's
confinement and a retrieval's principal, because authorization is the match
between them.

Two rules, and the asymmetry between them is the whole decision:

- **The tenant fails closed.** `Scope.Validate` refuses a record with no tenant,
  and `AuthorizedFor` requires the tenant to match exactly and refuses a
  principal with no tenant. A record with no tenant is visible to nobody, not to
  everybody. This is ADR-0022's named asymmetry made executable: treating an
  absent tenant as "unconstrained" would make an un-scoped record visible in
  every tenant, the maximal leak.

- **Every other axis narrows, and only when the record sets it.** A non-empty
  record axis is a constraint the principal must satisfy; an empty one is no
  constraint and matches any principal within the tenant. So a record scoped to
  user `u1` is invisible to user `u2` — cross-user leakage returns nothing — and
  a record scoped only to a tenant is visible to any principal in it.

The direction matters and is easy to invert: the constraint lives on the
*record*, so a record confined to project `p1` is invisible to a principal that
names no project. A project-scoped record must not surface in a retrieval that is
not in that project, or the boundary is one-directional.

This is the filter a store applies before ranking. It decides nothing about
ranking, retention or the physical store, and — following ADR-0022 — it does not
yet attach `Scope` to `Version`. The authorization primitive is what needed
deciding; wiring it onto the stored record is deferred with the other attributes
until the store exists, so this stays a reviewable unit and does not re-churn the
version tests.

## Consequences

Phase 7's leakage exit evidence now has a subject: `AuthorizedFor` is what
"returns zero records" is asserted against, in the same vocabulary ADR-0022 fixed
("cross-user" means the `user` axis). The tenant's fail-closed behavior is
enforced at two points — a record missing it does not validate, and a principal
missing it cannot authorize — so neither a malformed record nor an unscoped query
can cross the boundary.

`Scope` is pure and stays in the leaf, so authorization is a function of the two
scopes and nothing else; the same principal and record authorize identically on
replay. The purity guard covers it.

`Version` does not carry a `Scope` yet. Attaching it is the same deferral as the
descriptive attributes: it belongs with the store that authorizes records, and
adding it now would churn the version tests without a consumer that fails when it
is wrong.

## Discarded alternatives

**Treat an absent tenant as "any tenant," symmetric with the other axes.**
Simpler and catastrophic: an un-scoped record would be visible in every tenant.
The tenant is the one axis where absence must mean refusal, which is precisely
why ADR-0022 argued it into the vocabulary separately.

**Put the constraint on the principal instead of the record — a principal lists
what it may see.** Inverts the model and scatters policy: every retrieval site
would have to enumerate its allowances, and forgetting one would widen access
silently. A record declaring its own confinement is checked in one place.

**Make an empty principal axis match a constrained record (a broad principal sees
narrow records).** Reads as "an admin sees everything," and it makes the project
and user boundaries one-directional: a project-scoped record would leak to any
retrieval that simply omitted the project. Admin-style broad access, if it is
ever wanted, is a separate authority decision, not the default of an empty field.

**Attach `Scope` to `Version` in this change.** The same churn-and-premature
argument as ADR-0032 gave for the descriptive attributes: the field belongs with
the store, and the authorization primitive is decidable and testable without it.

## How it is verified

`internal/memory/scope_test.go`:

- `TestScopeValidateRefusesAMissingTenant` — a record with no tenant is refused;
  a tenant-only record is accepted.
- `TestCrossTenantRetrievalReturnsNothing` — a matching user across tenants does
  not open the boundary.
- `TestCrossUserAndCrossProjectReturnNothing` — the exit-evidence leakage checks
  in the vocabulary ADR-0022 fixed.
- `TestAnUnconstrainedAxisMatchesAnyPrincipal` — a tenant-wide record is visible
  to any principal in the tenant.
- `TestAConstrainedRecordIsInvisibleToABroaderPrincipal` — the constraint is on
  the record, so a project-scoped record is invisible to a principal with no
  project.
- `TestAuthorizationRefusesAPrincipalWithNoTenant` and
  `TestAuthorizationOnAllAxesMatchesTheNarrowestPrincipal` — the principal must
  name a tenant, and a single differing axis is enough to refuse.
