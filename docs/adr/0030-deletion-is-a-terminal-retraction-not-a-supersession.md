# ADR-0030: Deletion is a terminal retraction, distinct from supersession

- Status: accepted
- Affects: `internal/memory`
- Depends on: ADR-0028, ADR-0029
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0029 made supersession an operation and validated a lineage as an
append-only chain. It allowed a lineage to end on a closed version and
deliberately did not decide what a closed tail *means*, deferring deletion to its
own record. Measuring that output found the gap it left, and it is the deletion
decision the roadmap names.

A supersession and a deletion produce the **same bytes**. Both close the current
version's belief interval. `Supersede` closes v1 and opens v2; a deletion would
close v1 and open nothing. But `ValidateLineage` accepted any closed tail, so a
lineage `[v1(closed t1→t2)]` was indistinguishable from "v1 was superseded at t2
and the successor has not been appended yet." Nothing recorded that a version was
*deleted* rather than *awaiting a correction*.

The consequence is that resurrection could not be forbidden. Phase 7's exit
evidence requires deletion to propagate "without resurrection" — a deleted record
must not silently return. But given a closed tail with no terminal marker,
appending a contiguous successor validated cleanly: it looked exactly like a
supersession. A store could delete a record and a later write could bring it
back, and every check stayed green, because the model had no way to say "this
closure was final."

## Decision

**Deletion is a distinct terminal operation — retraction — and a retracted
version is the end of its lineage.** `Version` gains a `Retracted` marker, and
`internal/memory` gains `Version.Retract(at)`.

`Retract` closes the current version's belief interval at the deletion instant
and marks the closure terminal, opening no successor. It is a separate operation
from `Supersede` because the chain shape alone cannot distinguish a deletion from
a correction; only an explicit marker can. It refuses to retract a version that
is not current — retracting a closed version would rewrite a belief interval the
history already fixed.

Resurrection is refused in two places:

- `Version.Supersede` refuses a retracted receiver: appending a successor to a
  deletion resurrects it.
- `ValidateLineage` refuses any version that follows a retracted one: even a
  hand-built successor recorded exactly at the deletion instant — bytes that
  would tile as a valid supersession — is rejected because the predecessor was
  retracted, not superseded.

`Version.Validate` refuses a retracted version whose belief interval is still
open: a retraction closes the belief, so a retracted-but-current version would
report the deleted fact as the present answer.

The temporal semantics fall out of the existing bitemporal model with no new
query logic: a retracted version is closed, so a present as-of query (`LiveAt`
now) does not select it — the record is gone from the present — while a
historical query before the deletion instant still selects it — the history of
what was believed is preserved. Deletion closes the interval; it does not erase
it, which is the same append-only rule ADR-0028 gives for correction.

## Consequences

A deletion is now first-class and unforgeable at both the operation and the
chain: a store deletes by calling `Retract`, and a reassembled history with a
version after a retraction fails `ValidateLineage` at read. The "without
resurrection" clause is enforced by the type, not left to a store to remember.

`Retracted` is an additive boolean with `omitempty`, so an existing serialized
version without it decodes as not-retracted — the correct default, since every
version written before this decision was a supersession or a live head, never a
deletion.

This decides the *lineage* meaning of deletion — terminality and no-resurrection
within one record's chain. It does **not** yet decide *propagation* across
indexes, summaries, caches and unused prepared contexts, which the exit evidence
also names. That propagation is a store-and-index concern and needs the store to
exist; the lineage rule here is the invariant those propagation paths must
preserve, not a replacement for them.

## Discarded alternatives

**Treat any closed tail as a deletion.** Removes the marker and reintroduces the
ambiguity: a mid-correction lineage whose successor has not landed would be
indistinguishable from a deleted one, so either a legitimate pending correction
would be read as deleted, or a deletion would be resurrectable. The two states
are genuinely different and must be represented differently.

**Physically drop the version on deletion.** The intuitive meaning of "delete,"
and it erases the history ADR-0028 exists to preserve: a query about what was
believed last month would find nothing where a real belief once stood, and an
audit of a since-deleted fact becomes impossible. Deletion closes the belief; it
does not rewrite the past.

**Model deletion as a supersession by an empty version.** Reuses `Supersede` and
smuggles a sentinel — an "empty" successor whose emptiness means deleted. The
sentinel is a marker wearing a disguise, and every query would have to special-
case it; an explicit `Retracted` boolean says the same thing without a magic
value.

**Forbid a closed non-retracted tail entirely, forcing every lineage to end
current or retracted.** Tidier invariant, and it makes a pending correction —
predecessor closed, successor not yet appended — unrepresentable, which is a
normal intermediate state of an append-only store.

## How it is verified

`internal/memory/version_test.go`:

- `TestRetractProducesATerminalTombstoneThatPreservesHistory` — after retraction
  the present query returns nothing and a historical query still returns the
  version; the receiver is not mutated; the tombstone validates as a lineage.
- `TestRetractRefusesAnAlreadyClosedVersion` — only the current version is
  deletable.
- `TestSupersedeRefusesARetractedVersion` — resurrection refused at the
  operation.
- `TestValidateLineageRefusesResurrectionAfterRetraction` — resurrection refused
  at the chain, even for a successor whose bytes would tile as a valid
  supersession.
- `TestValidateLineageAcceptsACorrectionThenRetraction` and
  `TestValidateLineageAcceptsAClosedMidCorrectionTail` — a corrected-then-deleted
  history and a pending-correction tail are both legitimate.
- `TestValidateRefusesARetractedButStillCurrentVersion` — a retraction must close
  the belief interval.
