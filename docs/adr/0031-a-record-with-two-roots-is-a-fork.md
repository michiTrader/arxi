# ADR-0031: A record with two roots is a fork, not a silently chosen tip

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0021, ADR-0027, ADR-0028
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0028 made the memory store contain a fork instead of failing store-wide, and
defined a fork as it had seen one: two versions superseding the same predecessor.
It detected that by counting successors per predecessor — a predecessor with two
claimants condemns its record. Probing that detector, the way each memory turn
has probed the last, found the case it could not see.

Two `Approve` calls for one record with different bodies produce two versions
that each supersede **nothing** — two roots. They share no predecessor, so the
successor count never rises above one for either, and both were reported as
current. A probe confirmed it, not supposed:

```text
Approve("r1", "body A"); Approve("r1", "body B is different")
Retrieve -> considered=2 authorized=2 returned=2   (both current versions presented)
Forks()  -> 0                                       (the detector saw nothing)
tip("r1") -> "body B is different"                  (one chosen silently by version-ID order)
```

Retrieval returned two current versions of one record, `Forks()` reported no
fork, and `tip` — which `Correct`, `Delete` and `Promote` all resolve through —
silently picked one by lexical version-ID order. That is the silent loss this
store exists to prevent (ADR-0028's own words), reached through the gap in the
fork test rather than through the supersession race the claim mechanism already
covers. It is reachable by an ordinary caller mistake: approving a record twice,
or approving one that already had a `Propose`.

## Decision

**The invariant is one current version per record, and any record with more than
one is forked — however the extra one arose.** The detector is rephrased against
the invariant directly: group the current versions (those nothing supersedes) by
record, and any record holding two or more is a fork. This subsumes the
multi-successor case ADR-0028 handled — two successors of one predecessor are
both current — and catches the multi-root case the successor count could not.

`Fork` gains a second shape. A supersession fork names the version its claimants
split from; a root fork shares no predecessor, so its `Predecessor` is empty and
the refusal says the record "forked at its root" rather than printing a missing
version as though one were lost. Containment is unchanged: the forked record is
excluded from retrieval, named in `Forks()` and the caller's own `Forked` list,
and refused by `tip`, exactly as a supersession fork is.

A second guard sits at the source. A **root write** — one that supersedes nothing
— is refused when the record already holds a different version, with `Correct`
named as the verb that changes a record without forking it. An identical re-write
is still idempotent (content-addressed version IDs make a re-`Approve` the same
version), so only a genuine second root is refused. The claim mechanism cannot
cover this the way it covers a supersession race — a root has no predecessor to
claim — so two concurrent first writes still both land and are contained at read,
exactly as an imported fork is. The write guard removes the sequential mistake,
which is the common one; the read-side containment remains the backstop.

## Consequences

Retrieval can no longer present two current versions of one record, and `tip` can
no longer choose between them silently: a root fork is contained and visible
through `Forks()` like any other. The generalized detector is strictly wider than
the old one, so every supersession fork it caught it still catches — the
multi-successor tests are unchanged and still pass.

A double-`Approve` now fails immediately with guidance instead of quietly
producing a stuck, contained record, which is the better outcome for the mistake
that produces it most often. A deleted record's identity stays terminal: a fresh
root over a tombstone is refused by the same guard, so deletion's "without
resurrection" guarantee is not reopened by a back door the supersession path had
closed.

The two-shape `Fork` is a small widening of an inspection type. `Predecessor`
becomes optional in the wire form (empty for a root fork), which readers that
only ever saw supersession forks will now encounter; it is documented as the
signal that distinguishes the two rather than a missing value.

## Discarded alternatives

**Refuse a second root only at write, without generalizing the detector.**
Leaves the store defenseless against a root fork that arrives from an import, a
replica or two concurrent first writes — the cases a single process cannot
prevent — which is exactly the blast-radius argument ADR-0028 makes for
containing at read rather than only refusing at write.

**Resolve a root fork by a tiebreak — newest mtime, lowest version ID.** The same
temptation ADR-0027 and ADR-0028 refused for supersession forks, wrong for the
same reason: every available tiebreak is deterministic and unrelated to which
version the user meant, and `tip` picking the lower version ID is precisely the
silent behavior this ADR removes.

**Let `Approve` of an existing record supersede its tip instead of refusing.**
Turns a create into an update silently, so a caller that approved the wrong
record ID would overwrite an unrelated record's current version rather than being
told it already exists. `Correct` is the update verb and names the record it
changes; `Approve` starting a record that already exists is a mistake worth
surfacing, not papering over.

## How it is verified

`internal/memorystore/multiroot_test.go`:

- `TestApproveRefusesASecondRootForOneRecord` — a second `Approve` with a
  different body is refused, and an identical re-`Approve` stays idempotent.
- `TestApproveRefusesARootOverAProposedRecord` — a record that exists as a
  candidate cannot gain a second root by being approved afresh.
- `TestAnImportedRootForkIsContainedNotSilentlyPresented` — a root fork that
  arrives out of band is excluded from retrieval, named by `Forks()` with an
  empty predecessor, and refused by `tip`, rather than presented twice.
- `TestHealthyRecordsAreUnaffectedByTheGeneralizedForkCheck` — a normal, a
  corrected and a deleted record each still resolve to exactly one current
  version, so the wider detector does not over-reach.
