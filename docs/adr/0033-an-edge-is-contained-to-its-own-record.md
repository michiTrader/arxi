# ADR-0033: A supersede or retire edge is contained to its own record

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0027, ADR-0031, ADR-0032
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0032 added `Retires`, a head-removing edge: a resolution names the losing
heads and `headsByRecord` treats a retired version as no longer current. Probing
that new edge — the way each memory turn probes the last — found it honored
without asking which record its targets belonged to.

`Resolve` only ever populates `Retires` with heads of the record it is resolving,
so no public verb creates a cross-record edge. But `Retires` is part of the
persisted wire format, and `Put`, an import and a replica all accept any
`Record`. A version of one record naming **another** record's head was measured,
not supposed:

```text
Approve("victim",  tenant:acme, "important tenant memory")
Approve("attacker", user:u1,    "body A")
import a version of "attacker" whose Retires = [victim's head]

Retrieve(scopes=[tenant:acme]) -> 0 records
tip("victim")                  -> not found
Forks()                        -> reports "attacker", says nothing of "victim"
```

The victim ends with zero current versions: unreadable, with no fork and no
tombstone — the silent loss ADR-0028 and ADR-0031 exist to prevent, one record
reaching into another to cause it. Worse than a fork, which `Forks()` at least
names: this leaves no trace at all, because a record with no head is
indistinguishable from a record never written. And the victim was in a different
tenant, so the reach crossed the boundary ADR-0027 calls the one no retrieval may
cross.

The same probe found the pre-existing `Supersedes` edge had the identical hole. A
version superseding a foreign record's head removed it the same silent way.
`Supersedes` predates ADR-0032, but the containment argument is the same for both
edges, and fixing only the newer one would be verifying at the narrowest point —
the failure shape this project keeps recording — so both are addressed together.

## Decision

**An edge is honored only within one record.** `headsByRecord` builds the record
each version belongs to from the version set, and marks a target superseded or
retired only when the version declaring the edge belongs to the **same** record
as the target. A supersede or retire that names a version of a different record
is inert.

The consequence is containment, not repair. The target keeps its head, so a
foreign edge can no longer remove another record's current version. The version
carrying the foreign edge stays a head of its own record — it is not itself lost
— and if that leaves its record with two heads, the ordinary fork check reports
it. So a malformed import surfaces as a fork of the record that carries it, which
is visible and resolvable (ADR-0032), rather than as the silent disappearance of
whatever record it happened to point at.

This costs one map from version ID to record ID, built from the same version set
`headsByRecord` already walks. A version ID is content-addressed over its record
ID among other fields, so a version's own record is intrinsic and the map is
exact; there is no ambiguity about which record a target belongs to.

## Consequences

One record can no longer delete another, by either edge, whether the offending
version arrives from an import, a replica, a corrupt file or a hand-built `Put`.
The guarantee holds across scopes, so the containment is also a scope-boundary
guarantee: a version in one tenant cannot remove a head in another.

Within a record nothing changes. A correction still supersedes its predecessor, a
resolution still retires the losing heads, and both move the head exactly as
before — the guard ignores only edges whose target is a different record, which
no legitimate operation produces. The existing fork, resolution and correction
tests are unchanged and still pass.

A foreign edge is made inert rather than reported. The record that carries one is
still visible and, if it now has two heads, named by `Forks()`; the victim it
pointed at is simply unharmed. Reporting the dangling foreign pointer itself was
considered and left out: the property that matters is that no record is silently
lost, and that is delivered by the target keeping its head. A version whose
`Supersedes` or `Retires` names a foreign record is malformed data whose only
effect is now none.

## Discarded alternatives

**Validate the edge at write time and refuse a cross-record target in `Put`.**
Necessary but not sufficient, and sufficient alone only for the one path that
runs `Put`. The blast-radius argument ADR-0028 makes for containing a fork at
read applies here unchanged: an import, a replica or a restored backup never runs
this process's `Put`, so a write-time refusal cannot see the version that arrives
already written. Read-side containment covers every way a version arrives; a
write-time check could be added as an additional early failure for the local
mistake, but it cannot replace the containment.

**Drop a version that carries a foreign edge entirely.** Removes the carrier as
well as neutralizing the edge, which is a second silent loss — now of the
attacker's own record — for the sake of tidiness. Neutralizing the edge while
keeping the carrier as a head of its own record loses nothing and still routes a
genuinely forked carrier through the visible fork path.

**Treat a cross-record edge as a store-wide error.** The exact over-reach
ADR-0028 corrected for forks: one malformed imported version would deny retrieval
to every record and every tenant. Containment refuses the one bad edge, not the
store.

## How it is verified

`internal/memorystore/edge_containment_test.go`:

- `TestARetireCannotRemoveAnotherRecordsHead` — a foreign record's `Retires`
  naming the victim's head leaves the victim fully readable, in its own scope,
  with its own body.
- `TestASupersedeCannotRemoveAnotherRecordsHead` — the same for the pre-existing
  `Supersedes` edge, so the fix is measured at its widest point.
- `TestSameRecordEdgesStillApplyAfterContainment` — a within-record correction
  still moves the head, so the guard ignores only foreign edges and does not
  break `Correct` or `Resolve`.

Both containment guards were confirmed to fail closed by mutation: removing the
same-record condition lets a foreign edge delete the victim, and the first two
tests fail naming the vanished record.
