# ADR-0034: A degenerate self- or cyclic edge is unconstructable, not merely unhandled

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0021, ADR-0032, ADR-0033
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0032 added `Retires`, a head-removing edge, and ADR-0033 contained it so an
edge naming another record's head is inert. The obvious next probe, the way each
memory turn probes the last, is the edge that names a version of its **own**
record in a way that removes the record's last head:

- a version that names its own ID in `Retires` (retires itself), or
- two roots of one record that each name the other in `Retires` (a retire cycle).

Either would leave the record with zero current versions: unreadable, refused by
`tip`, and invisible to `Forks()` because a fork is two heads and this is none —
the "frozen and unseeable" pathology ADR-0029 recorded for an unfulfilled claim,
reached through the new edge. `headsByRecord` honors a same-record edge by
design, so on its face nothing stops a self-retire from removing the only head.

Probing it rather than supposing it found the case cannot be built. A version ID
is the digest of an `identity` that includes `Retires` and `Supersedes` (ADR-0021
made the identity explicit precisely so a field joins it only on purpose). The
moment a version lists a target, its own ID changes, so the ID can never equal a
target the version already contains. An attempt to make a version retire itself
therefore produces a version whose `Retires` names a **different**, non-existent
ID; ADR-0033 leaves that inert, and the writer stays a healthy head. Two roots
trying to retire each other cannot both hold the other's real ID for the same
reason, so neither edge lands and the record surfaces as an ordinary root fork,
visible in `Forks()` and resolvable by ADR-0032. Measured, not assumed:

```text
seal r1/"body A"            -> plain ID P
write r1/"body A" Retires=[P]  -> ID != P, and P is in no file
tip("r1") -> "body A"       (one healthy head; the retire is inert)

two roots retiring each other -> Forks() reports r1, tip refuses it
```

## Decision

**No runtime guard against a self- or cyclic edge is added, because the content
address forbids the edge rather than the code catching it.** The protection is a
property of the identity, not a check, and the right move is to record why the
absent check is correct and to pin the property it rests on, so a later reader
does not either add a pointless guard or remove the field that makes the guard
unnecessary.

The load-bearing fact is that `Retires` and `Supersedes` are part of `identity`.
That is what makes a self-reference unconstructable, and it is one deleted struct
field away from false: drop `Retires` from `identity` and a version that lists a
target keeps the ID it would have had without it, so it can name its own ID,
retire itself, and freeze the record with zero heads — a silent, invisible loss.
So the property is pinned by a test that constructs the attempt and asserts the
record stays readable (a self-retire) or surfaces as a visible fork (a cycle),
and that fails the moment the field leaves the identity.

## Consequences

A whole class of degenerate edge — self-retire, retire cycle, and by the same
argument self-supersede and supersession cycle — is closed with no code to
execute, maintain or get wrong. The supersession chain and the retirement set are
a DAG by construction: an edge can only point at an ID that already existed when
the edge's own ID was computed, so no edge can close a cycle.

The guarantee is exactly as strong as "the edge fields are part of the identity",
and no stronger. A future change that removes a field from `identity` to "simplify
the digest" would reopen this quietly, which is why the reason lives in a test
that breaks rather than in prose alone. A forged version *file* whose stored
`version_id` disagrees with its contents does not reach this at all: `Versions()`
reseals every file and refuses the whole read on a digest mismatch, so a
hand-edited ID is a loud read failure, not a silent freeze.

This ADR adds no production code. It is the record of a probe that cleared its
target and of the one assumption that clearance depends on, so the clearance is
not re-litigated and the assumption is not silently withdrawn.

## Discarded alternatives

**Add a runtime check that rejects a self- or cyclic edge in `headsByRecord` or
`Validate`.** Dead code: it defends against an input that cannot be constructed
while the identity covers the edge fields, so it would never fire, and a branch
that never fires is indistinguishable from one asserting nothing — the decoration
this project treats as worse than absence. If the identity ever stopped covering
the edge fields, that check would be the wrong fix anyway; restoring the field is.

**Say nothing and leave the safety implicit.** The safety is non-obvious and
sits on a single struct field whose loss is a plausible "cleanup". An implicit
guarantee with a one-line revert and no failing test is how the memory channel
survived four ADRs and how the header count drifted; the correction each time was
to derive or pin the fact, not to trust it.

## How it is verified

`internal/memorystore/degenerate_edge_test.go`:

- `TestASelfReferentialRetireCannotFreezeARecord` — a version naming the plain
  ID in `Retires` gets a different ID, so the retire is inert and the record
  keeps one healthy head; it fails if `Retires` leaves the identity.
- `TestMutuallyRetiringRootsAreAVisibleForkNotAFrozenRecord` — two roots that
  try to retire each other surface as a visible root fork rather than a
  zero-head freeze.

Both were confirmed to fail closed by mutation: removing `Retires` from
`identity` aligns the IDs, the edges land, and the record freezes — the first
test reports the unchanged ID and the second reports the fork that has vanished.
