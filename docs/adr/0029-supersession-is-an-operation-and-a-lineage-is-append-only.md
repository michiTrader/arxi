# ADR-0029: Supersession is an operation, and a lineage is an append-only chain

- Status: accepted
- Affects: `internal/memory`
- Depends on: ADR-0021, ADR-0028
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0028 introduced `Validity` and proved, in `internal/memory`, that half-open
bitemporal intervals tile the timeline: at the supersession instant exactly one
version is live. That proof was built on two hand-written validities whose bounds
happened to line up. Nothing in the package *produced* a correction, so nothing
guaranteed that a real correction lines the bounds up — the tests asserted the
property against literals, not against the code a store would call.

That is the same shape ADR-0025 recorded one layer out: an assertion with no
subject. There the memory channel was asserted against hand-built `turn.Request`
literals while the code that assembles messages went untouched; here the tiling
was asserted against hand-built validities while the code that performs a
correction did not exist. A store written against ADR-0028 would have aligned
`SupersededAt` and `RecordedAt` by hand at every call site, and the first one
that got it wrong — a gap of one nanosecond — would pass every existing test.

`Validity` also says nothing about identity. ADR-0021 gave the presentation
receipt a `RecordID` and `VersionID`; the stored version had neither, so two
validities could not be told apart as versions of the same fact, and "lineage"
was a word with no type behind it.

## Decision

**A correction is an operation that tiles by construction, and a lineage is a
validated append-only chain.** `internal/memory` gains a `Version` (RecordID +
VersionID + Validity) and two functions.

`Version.Supersede(newVersionID, at, validFrom, validTo)` returns the closed
predecessor and the new current successor. It sets the predecessor's
`SupersededAt` and the successor's `RecordedAt` to the **same** instant, so the
belief axis tiles by construction and no caller can introduce a gap or an
overlap. It copies rather than mutates the predecessor — a correction appends a
new fact, it does not edit the old one (ADR-0002, ADR-0028) — and it refuses to
supersede a version that is already superseded (a fork has no single answer to
"what was believed next"), to reuse or omit the successor's version identity, or
to correct a version at an instant before it was recorded.

`ValidateLineage(versions)` refuses any ordered chain that is not append-only and
contiguous: every version but the last is closed, each closed interval ends
exactly where the next begins, version identities are unique, all versions share
one record, and at most one version — the last — is current. A gap leaves an
instant with no belief; an overlap leaves one with two; a non-final current
version is a fork. The slice is read in the order given rather than sorted,
because the caller holds the history and silently re-sorting would hide a chain
stored out of order, which is itself the defect worth failing on.

A lineage **may** end closed, with no current version — a fact recorded and then
fully retracted. That tombstone is the shape a deletion-lineage decision will
build on, so this function pins the chain and deliberately does not decide what a
closed tail *means* for a query. Deletion lineage remains undecided (roadmap
item 7).

The `Version` carries no authority kind, provenance or evidence class yet. The
kind enumeration lives on the receipt in `internal/contextprep` (ADR-0023);
moving it into this pure leaf so the record can carry it is a package relocation
worth its own decision, not a side effect of settling lineage.

## Consequences

Correction has one implementation, and the tiling boundary is unfakeable at the
call site: a store that wants to correct a version calls `Supersede` and cannot
produce a discontinuous chain. `ValidateLineage` is the independent check a store
runs over a reassembled history, so a chain corrupted in storage — a dropped
version, a duplicated identity, a reordered pair — fails at read rather than
answering a query wrong.

The package stays a pure leaf. `Supersede` and `ValidateLineage` are functions of
the versions and the instants handed in; neither reads a clock, so a lineage
validated on the live fold validates identically on replay, which
`TestMemoryModelIsPureAndIndependent` continues to guarantee.

## Discarded alternatives

**Leave supersession to the caller and keep only `Validity`.** The status quo
after ADR-0028, and the reason this ADR exists: the tiling guarantee was proved
on literals and every call site would have re-implemented the boundary
alignment, one of them eventually wrong in a way no test caught.

**Mutate the predecessor in place instead of returning a closed copy.** Smaller
signature, and it destroys the history ADR-0028 was written to preserve: the old
version's bytes must not change, or a query about the past returns the present.

**Let `ValidateLineage` sort the versions.** More forgiving of a caller that
passes them out of order, and it hides exactly the storage defect worth
surfacing. Recorded order is part of the history; a chain that arrives scrambled
is a chain something stored wrong.

**Require every lineage to end with a current version.** Simpler invariant, and
it makes a fully-retracted fact unrepresentable — pre-deciding that deletion
leaves a live tail, which is a deletion-lineage question this ADR has no mandate
to answer.

**Put `Version` in `internal/contextprep` beside the receipt.** That package is
not a pure leaf — it imports the kernel and the turn contract — so a `Version`
there could not stay a clock-free value, and the store, retrieval and deletion
lineage that will all share `Version` would inherit `contextprep`'s dependencies.

## How it is verified

`internal/memory/version_test.go`:

- `TestVersionValidateRequiresBothIdentities` — a version missing either identity
  is refused.
- `TestSupersedeTilesTheBeliefAxisByConstruction` — the operation shares the
  boundary instant, preserves the identities, does not mutate the receiver, and
  yields a chain `ValidateLineage` accepts.
- `TestSupersedeRefusesAnAlreadySupersededVersion`,
  `TestSupersedeRefusesReusedOrMissingSuccessorIdentity`,
  `TestSupersedeRefusesACorrectionBeforeTheRecordedInstant` — the fork, identity
  and backwards-instant refusals.
- `TestValidateLineageAcceptsAContiguousAppendOnlyChain` — a three-version history
  built through the operation validates, with exactly the last current.
- `TestValidateLineageRejectsAGapBetweenVersions`,
  `...RejectsANonFinalCurrentVersion`, `...RejectsAMixedRecord`,
  `...RejectsAnEmptyChain` — the four ways a chain is not an append-only history.
- `TestValidateLineageAcceptsAFullyRetractedChain` — a tombstone is a legitimate
  lineage, so the validator does not pre-decide deletion semantics.
