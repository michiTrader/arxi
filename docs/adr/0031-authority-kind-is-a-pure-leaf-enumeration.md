# ADR-0031: The authority kind is a pure-leaf enumeration the record and receipt share

- Status: accepted
- Affects: `internal/memory`, `internal/contextprep`
- Depends on: ADR-0023, ADR-0029
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0023 enumerated memory authority — `frozen_context_memory`,
`approved_memory_record`, `proposed_memory_candidate` — and put the enumeration,
its presentable table, and the `Governed`/`Presentable` predicates on
`MemoryReceipt` in `internal/contextprep`, because the receipt was the only thing
that carried a kind. ADR-0029 and ADR-0030 built the stored `Version` in the pure
leaf `internal/memory` and each noted, explicitly, that the version carried no
authority kind yet and that relocating the enumeration was "a package move worth
its own decision." This is that decision.

The store needs a version to carry its own authority. Phase 7 authorizes before
it ranks, across "evidence class, sensitivity, purpose" and the rest — and a
version whose authority lives only on a presentation receipt cannot be authorized
as a stored record. But the kind cannot simply be *copied* into `internal/memory`:
two enumerations of the same authority would drift, and a kind that is governed in
one and unknown in the other is exactly the divergence ADR-0023 exists to prevent.
The enumeration has to live in **one** place both the record and the receipt read.

`internal/contextprep` is not that place. It imports the kernel, the turn
contract and compaction; a `Version` defined there could not stay a clock-free
value, and the store, retrieval and deletion lineage that will all read the kind
would inherit those dependencies. The pure leaf is the only layer both a stored
record and a presentation receipt can depend on without a cycle.

## Decision

**The authority-kind enumeration lives in `internal/memory`, and the receipt
reads it rather than owning it.** `memory.Kind` is a typed string with the three
constants, the presentable table, and `Known`, `Governed`, `Presentable` and
`Validate`.

`contextprep.MemoryReceipt` keeps its `Kind` string field for wire
compatibility, keeps its three exported constants as aliases of the leaf's
values, and delegates `Governed`, `Presentable` and its unknown-kind refusal to
`memory.Kind`. So the receipt and the stored record answer "is this governed" and
"may this be presented" from one table, and cannot disagree.

The split of concern is deliberate. `memory.Kind.Validate` refuses only an
**unknown** kind — that is the record-layer rule, since a candidate is a
perfectly valid *stored* kind. The rule that a candidate must never be
**presented** stays in `MemoryReceipt.Validate`, because it is a property of a
presentation, not of the record: a candidate belongs in the store and is refused
only when something tries to show it to a model.

## Consequences

There is one enumeration. Adding or renaming a kind is one edit in the pure leaf,
and the receipt follows automatically; a kind cannot be governed for the store
and unknown for the receipt. This is the single-source property ADR-0023 argued
for, now that there are two readers of the vocabulary instead of one.

The `Version` still does not carry a `Kind` field yet — that is the next step,
the record struct that composes identity, authority and validity. This ADR moves
the vocabulary without changing what `Version` requires, so the existing version
and lineage tests are untouched. Deferring the field keeps this change a
relocation, reviewable on its own, rather than a relocation entangled with a new
required field and its test churn.

`contextprep`'s constants remain `string`-typed aliases, so every existing
`MemoryReceipt{Kind: KindApprovedMemoryRecord}` and every comparison against the
constants compiles and behaves identically. The one white-box test that poked the
old `memoryKindPresentable` map now asks `memory.Kind(kind).Known()`, following
the symbol to its new home rather than asserting a different thing.

## Discarded alternatives

**Copy the enumeration into `internal/memory` and leave the receipt's copy in
place.** The obvious low-friction move and the one ADR-0023 forbids: two tables
drift, and the first divergence is a kind that is authority in one and invalid in
the other, silently.

**Define `Version` in `internal/contextprep` beside the receipt so both share its
kind.** Reuses the existing enumeration and destroys the leaf: a `Version` in
`contextprep` is not clock-free, and everything that reads the record inherits
the kernel and turn dependencies.

**Move the "candidate is not presentable" rule into `memory.Kind.Validate` too.**
Tidier at first glance, and wrong: it would make a candidate an invalid *record*,
when a candidate is precisely a record the store must hold so it can be inspected
and promoted. The presentation rule belongs with the presentation.

**Add the `Kind` field to `Version` in the same change.** Larger and entangled:
requiring a governed kind on every version would churn every version and lineage
test written for ADR-0029 and ADR-0030, mixing a vocabulary relocation with a new
record invariant. The field is the next decision, made on its own.

## How it is verified

`internal/memory/kind_test.go`:

- `TestEachEnumeratedKindReportsItsAuthority` — the authority table at its new
  home, including a typo and the empty string reported as unknown, un-presentable
  and un-governed.
- `TestKindValidateRefusesTheUnenumerated` — every enumerated kind validates and
  an unenumerated one fails closed.

`internal/contextprep`'s existing memory tests continue to pass unchanged, which
is the evidence the relocation preserved behavior: the receipt's `Governed`,
`Presentable` and unknown-kind refusal now delegate to the leaf and every
assertion written against them still holds.
