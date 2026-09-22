# ADR-0035: A record composes scope and lineage, and visibility authorizes before selecting

- Status: accepted
- Affects: `internal/memory`
- Depends on: ADR-0028, ADR-0029, ADR-0033
- Enables: Phase 7 (read-only governed memory)

## Context

The parts of a governed memory record were built and tested separately: identity,
authority and validity on `Version` (ADR-0021, 0032, 0028); the correction and
deletion chain as `ValidateLineage` (ADR-0029, 0030); scope authorization as
`Scope.AuthorizedFor` (ADR-0033); the audit trail as `RetrievalReceipt`
(ADR-0034). Two things were left open and both point at the same missing type.

First, ADR-0022, ADR-0032 and ADR-0033 all deferred *where scope lives*, saying
it belongs "with the store." Scope is not a property of a version — a correction
does not move a record to another tenant — so it does not belong on `Version`.
It belongs on the record as a whole, and there was no record-as-a-whole type.

Second, Phase 7 states the order: "authorization occurs before semantic ranking
across ... and valid time." Authorization (`Scope.AuthorizedFor`) and bitemporal
selection (`Validity.LiveAt`) existed as separate primitives with nothing
composing them in the stated order. Ranking picks among records; but *within* a
record, "which version does this principal see as-of when" is the composition of
those two, and it needs neither a store nor a ranking decision.

## Decision

`internal/memory` gains `Record`: a `Scope` and the append-only `[]Version`
lineage. Scope sits on the aggregate, once, because every version of a record
shares its trust boundary and principals. `Record.Validate` composes the two
checks already built — `Scope.Validate` and `ValidateLineage` — so accepting a
record accepts a tenant-scoped record and a contiguous, resurrection-proof chain
in one call.

`Record.VisibleTo(principal, decisionAsOf, validAt)` is the record-level
retrieval primitive, and it enforces the stated order: it authorizes the
principal against the record's scope *first*, and only then scans the lineage for
the version believed as-of `decisionAsOf` that holds at `validAt`. A principal
outside the scope sees nothing, with **no error** — an unauthorized retrieval is
empty, not a failure. At most one version is live on the decision axis at a time
because the lineage tiles (ADR-0028/0029), so the scan returns the single version
in force.

This composes the whole record contract into the operation a store performs per
candidate record before ranking. It decides nothing about ranking — which orders
records against a query — and nothing about the physical store. It is the last
piece expressible in the pure leaf without either.

## Consequences

Scope's deferral is resolved, and on the correct type. A store keys records by
`RecordID`, holds each as a `Record`, and calls `VisibleTo` to filter before
ranking; the three separate primitives now have one composed entry point, tested
end to end. The bitemporal behavior survives composition: a corrected record
shows its current version to an authorized present query and the superseded one
to a historical query, and a retracted record shows nothing in the present but
its history in the past — now proven through `VisibleTo`, not only on raw
`Validity`.

`VisibleTo` returns the version *in force*, not a ranked list. Choosing among the
visible records of many is ranking, which stays undecided; this primitive answers
the prior question — is this record visible to this principal, and if so which
version — that ranking presupposes.

`Record` still carries no descriptive attributes (provenance, evidence class,
confidence, sensitivity, purpose, retention, lifecycle). They remain deferred for
the same reason: several interact with ranking, and committing their shape now
would pre-decide it.

## Discarded alternatives

**Put scope on `Version` after all.** Rejected on meaning: scope is invariant
across a record's corrections, so per-version scope would let two versions of one
record disagree about their tenant — a state that should be unrepresentable, and
would be representable if each version carried its own.

**Select first, then authorize.** Reverses the order Phase 7 states and leaks
work: selecting a version the principal may not see, then discarding it, means the
selection logic runs on unauthorized records. Authorization is the gate; nothing
downstream of it should run on a record that failed it.

**Return an error when the principal is unauthorized.** Conflates "you may not
see this" with "something went wrong." An unauthorized retrieval is a normal,
expected outcome — most records are out of scope for most principals — and the
leakage evidence wants it recorded as *empty*, not as a failure.

**Have `VisibleTo` also rank or return all live versions.** There is only one
live version on the decision axis at a time, so "all live" is one; and ranking
across records is a separate, undecided concern. Folding either in would smuggle a
ranking shape into the visibility primitive.

## How it is verified

`internal/memory/record_test.go`:

- `TestRecordValidateComposesScopeAndLineage` — a bad scope and a forked lineage
  each fail through `Record.Validate`.
- `TestVisibleToAuthorizesBeforeSelecting` — an out-of-scope user sees nothing
  (no error); the authorized principal sees the current version.
- `TestVisibleToRespectsTheAsOfInstant` — the authorized principal sees the
  superseded version as-of before the correction.
- `TestVisibleToReturnsNothingForARetractedRecordInThePresent` — a retracted
  record is invisible now and visible as-of before its deletion.
- `TestRecordIDIsTheSharedIdentity` — the record's identity is the one every
  version shares.
