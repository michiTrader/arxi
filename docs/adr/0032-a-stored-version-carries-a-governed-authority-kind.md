# ADR-0032: A stored memory version carries a governed authority kind

- Status: accepted
- Affects: `internal/memory`
- Depends on: ADR-0021, ADR-0023, ADR-0031
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0029 gave `internal/memory` a `Version` with a record identity and a
bitemporal validity; ADR-0031 moved the authority-kind enumeration into the same
pure leaf so both the stored record and the presentation receipt read one
vocabulary. Each of those deferred the obvious composition: the `Version` still
did not carry a kind, so it did not know its own authority.

Phase 7 authorizes before it ranks — "authorization occurs before semantic
ranking across tenant, user, application, project, team, agent, run, evidence
class, sensitivity, purpose and valid time." A version whose authority is not on
the version cannot be authorized as the stored thing; the authority would have to
be recovered from the presentation receipt, which is the wrong layer and does not
exist until presentation time. The record has to carry its own kind.

There is also an invariant waiting to be stated. `Version.Validate` already
requires a `RecordID` and a `VersionID` (ADR-0021). `KindFrozenContextMemory` is
precisely the kind that has *no* record identity — it is a blueprint
configuration field, presented from the blueprint and never stored (ADR-0021,
ADR-0023). So a `Version` carrying the frozen kind is a contradiction: a stored
record identity for a thing that has none.

## Decision

**A `Version` carries a `Kind`, and it must be a governed kind.**
`Version.Validate` refuses a kind that is empty or unenumerated (delegating to
`memory.Kind.Validate`, ADR-0031), and then refuses any kind that is not
`Governed` — which excludes exactly `KindFrozenContextMemory`. The two governed
kinds, `KindApprovedMemoryRecord` and `KindProposedMemoryCandidate`, are both
valid stored versions; a candidate is stored so it can be inspected and promoted,
even though it is never presented.

`Version.Supersede` propagates the predecessor's kind to the successor. A
correction to an approved record stays approved and a correction to a candidate
stays a candidate: a supersession is a new version of the *same* record, and its
authority is a property of the record, not something a correction may silently
change. Promotion — a candidate becoming approved — is a distinct governed
operation that belongs to Phase 10 consolidation, not to a correction, so it is
deliberately not expressible through `Supersede`.

This composes identity (ADR-0021), authority (ADR-0023/0031) and validity
(ADR-0028) into the stored unit a governed store holds and authorizes. It is the
record struct the roadmap named, minus the descriptive attributes that interact
with ranking.

## Consequences

A stored version now states its authority, so a store can authorize it before
ranking without consulting a presentation. The frozen kind is refused at the
type: an attempt to store blueprint configuration prose as a governed version
fails `Validate` rather than minting a record identity a configuration field does
not have.

`Version` still carries no provenance, evidence class, confidence, sensitivity,
purpose, retention or lifecycle. Those are left out on purpose: several interact
with ranking, which has no decision yet, and adding them now would be guessing
their shape. Identity, authority and validity are the three that are settled and
that the correction, deletion and authorization paths already need.

The `Kind` field made every existing `Version` literal in the tests incomplete,
which is the churn ADR-0031 avoided by deferring the field. Each test version is
now an explicit `KindApprovedMemoryRecord`, and the two negative-identity cases
keep failing on identity because the kind check runs after it.

## Discarded alternatives

**Require only that the kind is enumerated (`Known`), not that it is
`Governed`.** Weaker and wrong: it would admit a `Version` of kind
`frozen_context_memory`, a stored record identity for a thing ADR-0021 says has
none. The `Governed` requirement is what ties the identity ADR-0021 requires to
the kind that is allowed to have one.

**Let `Supersede` take a new kind, so a correction can promote.** Convenient and
a layering violation: promotion is a governed consolidation decision (Phase 10)
with its own authorization, and folding it into a correction would let any
correction launder a candidate into an approved record with no separate check.

**Add all the descriptive attributes now to finish the record struct.** Larger
and premature: provenance and evidence class are relatively safe, but confidence,
sensitivity, purpose and retention interact with ranking and deletion policy that
remain undecided, and committing their shape here would pre-decide those.

**Keep the kind on the receipt only and have the store read it from there.** The
receipt is a presentation artifact that exists only when memory is presented; a
stored record must carry its authority whether or not it has ever been presented,
or it cannot be authorized at rest.

## How it is verified

`internal/memory/version_test.go`:

- `TestVersionRequiresAGovernedKind` — a version with no kind and a version of
  kind `frozen_context_memory` are both refused; a proposed candidate is
  accepted, since it is a governed stored record even though it is never
  presented.
- `TestSupersedeTilesTheBeliefAxisByConstruction` gains an assertion that the
  successor inherits the predecessor's kind, so a correction cannot change a
  record's authority.
- Every other version and lineage test now constructs a governed version
  explicitly, so the whole suite exercises the new requirement.
