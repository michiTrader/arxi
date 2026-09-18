# ADR-0023: Memory authority is enumerated, and an unknown kind is not authority

- Status: accepted
- Affects: `internal/contextprep`, `docs/roadmap.md`, `spec/context.md`
- Depends on: ADR-0020, ADR-0021, ADR-0022
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7 opens with a containment rule, not a feature:

> Start with records supplied or explicitly approved by a user, operator or
> import. **Model-generated material may propose candidates but cannot create
> active memory.**

ADR-0021 gave the receipt `RecordID` and `VersionID`, and `Governed()` to say
whether a receipt describes a stored record at all. So the question worth
measuring before designing a store is whether anything currently distinguishes a
*candidate* from an *approved record*. It was measured with a throwaway probe
rather than reasoned about:

```text
candidate.Governed() = true,  Validate() = <nil>
approved.Governed()  = true,  Validate() = <nil>
typo kind "governd_memory_recrd" -> Governed() = true, Validate() = <nil>
```

Three facts, and the third is the one that decides this ADR.

**A model-proposed candidate validates identically to an operator-approved
record.** Both name a record and a version, so both satisfy every assertion
ADR-0021 added. The containment rule above has no representation anywhere.

**A misspelled kind is accepted as authority.** `Governed()` is written as a
negation:

```go
func (r MemoryReceipt) Governed() bool {
	return r.Kind != "" && r.Kind != KindFrozenContextMemory
}
```

That is a blocklist with exactly one entry. Every string that is not
`"frozen_context_memory"` is authority — including typos, including kinds from a
future version of the store, including `"candidate"`. ADR-0020 already rejected
a blocklist once, for memory *content*: "a fence a record can contain is not a
fence". This is the same shape applied to the kind field, and it was introduced
by the ADR that fixed the previous gap.

**The failure is open, not closed.** An unrecognized kind gets more authority
than a recognized one, because the only recognized kind is the one restricted to
having no record identity. A store shipping a new kind, or shipping a bug, gets
authority by default.

This matters beyond tidiness because of what Phase 7 is for. The phase exists so
that autonomous writes stay disabled until inspection, correction and deletion
work. If a candidate is indistinguishable from an approved record at the receipt
layer, then "model-generated material cannot create active memory" is enforced
only by whatever the store remembers to do — which is the arrangement ADR-0020
was written to end, where a boundary looks present in one layer and is absent in
the next.

## Decision

Authority is **enumerated**. A receipt kind that is not in the enumeration is
not authority, and `Validate` refuses it.

Three kinds exist:

| kind | authority | record identity |
| --- | --- | --- |
| `frozen_context_memory` | presented as data; from the operator's own frozen blueprint | none, and naming one is refused |
| `approved_memory_record` | presented as data; supplied or explicitly approved by a user, operator or import | required |
| `proposed_memory_candidate` | **never presented**; may be stored, inspected and promoted | required |

`Governed()` keeps its meaning — "describes a stored record" — but is derived
from the enumeration rather than from a negation, so an unknown kind is not
governed and not frozen: it is invalid.

A new predicate carries the containment rule: `Presentable()` reports whether a
receipt may appear in a prepared context at all. It is true for
`frozen_context_memory` and `approved_memory_record`, and **false for
`proposed_memory_candidate`**. `Validate` refuses a candidate receipt that
reaches the preparer, because a candidate that can be presented is not a
candidate.

Adding a kind is therefore a deliberate edit to an enumeration in one place,
which is the property a blocklist cannot have. The failure direction inverts: an
unknown kind now fails closed.

This ADR governs **authority at the receipt boundary**. It decides nothing about
how candidates are proposed, stored, reviewed or promoted, and adds no store.
Promotion is Phase 7's own decision and needs the store to exist first.

## Consequences

The enumeration is a compatibility surface. A store that invents a kind string
now gets a refusal instead of silent authority, which is the point, but it means
the store and this enumeration must be changed together. That coupling is
deliberate and cheap today — there is no store — and it is exactly the coupling
that would be expensive to introduce later, once records had been authorized
under an unenumerated kind.

Phase 5 is unaffected. `frozen_context_memory` is in the enumeration, its
restriction against naming a record is unchanged, and the receipt encodes to the
same JSON. `presentation_digest` is untouched, so contexts committed under
ADR-0013's barrier still verify.

`Presentable()` is a predicate with no caller in production today, which is
worth naming honestly: the preparer never constructs a candidate receipt, so
nothing currently exercises the false branch outside tests. That is the one
weakness in this decision. It is accepted because the alternative — waiting for
the store — means the store defines the boundary, and a boundary defined by its
first consumer is one nobody reviewed. The verification below compensates by
asserting the refusal directly rather than relying on a caller to trigger it.

## Discarded alternatives

**Keep `Governed()` as a negation and document the expected kinds.** The
cheapest option and the one already in the tree. Rejected by measurement: a
typo'd kind validates as authority today, and documentation does not change
that. ADR-0021's own lesson applies — a rule nothing fails on is not a rule.

**Use a boolean `Approved` field instead of enumerated kinds.** Tempting because
it reads as exactly the roadmap's sentence. But a boolean cannot express the
frozen case, which is authoritative *and* has no approver, so it would need a
second field to disambiguate and the two could disagree. The kind already exists
and already distinguishes the frozen case; adding a parallel boolean creates two
sources of truth for one question, which ADR-0002 settles in general terms.

**Let the store enforce the candidate rule and leave the receipt permissive.**
The arrangement ADR-0020 exists to prevent. The receipt is what a prepared
context carries, so a permissive receipt means the guarantee lives only where
the store remembers it, and a second store — or an import path, which the
roadmap explicitly allows — re-decides it. The boundary belongs where the
evidence is.

**Enumerate authority now *and* add a candidate store.** Rejected for scope. A
store built alongside its own authority rules has no reviewer other than itself,
and Phase 7's remaining decisions (retention, ranking, deletion lineage) would
be made implicitly by whatever the store needed. This ADR deliberately stops at
the boundary that can be decided without a store.

## How it is verified

`internal/contextprep/memory_authority_test.go` asserts four things:

- **Each enumerated kind reports the authority it should.** `frozen_context_memory`
  and `approved_memory_record` are presentable; `proposed_memory_candidate` is
  not. Table-driven, so adding a kind without deciding its authority fails.

- **An unknown kind is refused, not treated as authority.** Includes a
  realistic typo of an enumerated kind, because the probe that motivated this
  ADR showed a typo validating cleanly. This is the assertion that inverts the
  failure direction.

- **A candidate receipt is refused by `Validate`.** Directly, not through a
  caller, since the preparer has no path that constructs one. Without this the
  candidate kind would be an enumeration entry that nothing enforces.

- **Phase 5's receipt is unchanged**: still validates, still presentable, still
  refuses to name a record, and still encodes without the version keys.

Mutation-verified rather than trusted from a pass: restoring the negation form
of `Governed()` must make the unknown-kind and candidate assertions fail, and a
`Validate` that refuses everything must fail the positive cases. Both are
recorded in the commit that lands this.
