# ADR-0024: The preparer validates the receipts it emits, and evidence names content

- Status: accepted
- Affects: `internal/contextprep`, `docs/roadmap.md`
- Depends on: ADR-0013, ADR-0020, ADR-0021, ADR-0023
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0023 named its own weakness: `Presentable()` had no production caller, so
the candidate refusal was exercised only by tests. That admission was measured
before being built on, and the measurement found something larger than the
admission described.

A throwaway probe asked whether anything in the production path calls
`MemoryReceipt.Validate()`:

```text
approved-without-version -> Validate() = memory receipt of kind "approved_memory_record" ... has no version_id
candidate                -> Validate() = memory receipt of kind "proposed_memory_candidate" reached the preparer ...
Prepare() emitted 1 receipt(s), kind[0] = "frozen_context_memory"
artifact + invalid + candidate: marshal err = <nil>
committed bytes contain candidate kind = true
content digest over a candidate-bearing receipt set = "1d7dbde5..."
```

`Validate()` refuses correctly when it is called. **Nothing calls it.** Not
`Prepare`, not the adapter, not the barrier. An artifact carrying a
`proposed_memory_candidate` receipt marshals without error, computes a content
digest, and would be committed by ADR-0013's barrier exactly like a valid one.

So ADR-0021, ADR-0022 and ADR-0023 each added a rule to a function that no
production code path invokes. Three ADRs of enforcement reachable only from
tests. The repository's own standard names this failure: *"an ADR with no
associated test is an intention"* — and the inverse holds just as hard. A guard
with no caller is also an intention, and the tests passing is what disguises it.

Two more facts came from the same probe, and they are worse than the first
because the preparer produces them itself.

**The preparer emits a receipt whose evidence is empty.** With memory present
and no effective config SHA supplied:

```text
receipt = {Kind:frozen_context_memory EffectiveConfigSHA: ContentDigest:8f594241...}
Governed() = false, Presentable() = true, Validate() = <nil>
```

Memory was presented and the receipt names no blueprint version. For
`frozen_context_memory` the effective config SHA *is* the version identity —
ADR-0021 left `RecordID` and `VersionID` empty for exactly this kind, on the
grounds that a configuration field has no record identity. The config SHA was
the thing that made that acceptable. An empty one leaves the frozen kind with no
version identity of any sort, which is the state ADR-0021 exists to refuse for
governed records, reached from the other side.

The receipt is well-formed in practice only because `internal/exec/turn.go`
declines the durable path when `Context.EffectiveConfigSHA == ""`. That is a
guarantee held one layer away from the evidence — precisely the arrangement
ADR-0020 was written to end, where a boundary looks present in one layer and is
absent in the next. The legacy gate is a compatibility decision about which
runs record proofs, not an assertion about receipt integrity, and it is free to
change without anyone noticing it was load-bearing for this.

**A receipt with no content digest validates.** Measured across the evidence
fields:

```text
kind=frozen_context_memory   sha=""    digest=""  -> Validate() = <nil>
kind=approved_memory_record  sha=""    digest=""  -> Validate() = <nil>
```

`MemoryReceipt` is documented as *"evidence of what memory was presented"*. A
receipt naming no content is evidence of nothing. Phase 7's exit evidence
requires that "every influence identifies its source and version"; a receipt
with an empty `ContentDigest` identifies neither, and it satisfies every
assertion in the tree today.

## Decision

**The preparer validates every receipt it emits, before the artifact is
digested.** `Prepare` calls `Validate` on each receipt and returns an error if
any refuses. This gives ADR-0021's version rule, ADR-0023's enumeration and
ADR-0023's candidate refusal a production caller for the first time, and it
places the call before `PresentationDigest` and `ContentDigest` are computed, so
a refused receipt cannot reach the durable barrier at all.

The call belongs in `Prepare` rather than in the adapter or the barrier because
`Prepare` is where receipts are constructed, and because ADR-0013 makes the
prepared artifact immutable once committed. Validation after the digest would
describe bytes that were already frozen.

**Evidence is required, not optional.** `Validate` refuses:

- a receipt with an empty `ContentDigest`, for every kind — a receipt that names
  no content is not evidence of a presentation;
- a `frozen_context_memory` receipt with an empty `EffectiveConfigSHA` — the
  config SHA is that kind's only version identity, and ADR-0021 exempted it from
  `RecordID`/`VersionID` on the understanding that this field carried the
  version.

Governed kinds are not required to carry a config SHA: they name `RecordID` and
`VersionID`, which is their version identity, and a store record does not
belong to the blueprint.

This ADR adds no store, no candidate path and no retrieval. It makes the three
receipt ADRs enforceable from production and requires the evidence fields to
hold evidence. Retention, ranking and deletion lineage remain undecided.

## Consequences

`Prepare` gains a failure mode it did not have: a caller that supplies memory
without an effective config SHA now gets an error instead of a receipt nobody
can trace. Production is unaffected, because `internal/exec/turn.go` already
routes such turns through the legacy path that records no context events — but
the reason is now asserted where the evidence is built instead of inferred from
a compatibility gate two packages away. If that gate is ever relaxed, this
refusal is what turns a silent loss of provenance into a failed preparation.

The refusal is a preparation error, not a panic or a dropped receipt. Dropping
the receipt would present memory and record nothing, which is the worse of the
two failures: the model would see the content and the artifact would deny it.

ADR-0023's named weakness is now closed for `Validate` as a whole, including the
candidate refusal, which `Prepare` will reject if a future store ever routes one
here. `Presentable()` still has no caller other than `Validate`, which is the
correct shape — the predicate is the rule and `Validate` is the enforcement
point.

Phase 5 is unaffected in behavior. The frozen receipt the preparer builds
carries both a config SHA and a content digest whenever the durable path runs,
so `presentation_digest` and `content_digest` are byte-identical for every
context already committed. No test fixture changed its expected digests.

## Discarded alternatives

**Validate in the adapter or at the barrier.** Closer to the durable boundary
and wrong for it. The adapter marshals an artifact that `Prepare` already
digested, so a refusal there rejects bytes whose digests were computed over
invalid evidence — and ADR-0013's recovery path loads the committed artifact
without re-preparing, so a second consumer of `Prepare` would bypass the check
entirely. Validation belongs with construction.

**Require a config SHA on every kind, uniformly.** Simpler to state, and it
contradicts ADR-0021. A governed record is not part of the blueprint; binding it
to a config SHA would make a stored record's identity depend on the configuration
that happened to retrieve it, so the same record version would carry different
evidence in two runs. The kinds differ in what identifies them, which is why the
enumeration exists.

**Leave `Prepare` permissive and rely on the `exec` gate.** The cheapest option
and the one in the tree. Rejected by the same argument ADR-0020 used against
delimiter marking: the guarantee would live wherever the caller remembers it. It
also fails the stated purpose of the `exec` gate, which is backward
compatibility for runs that recorded no context proofs — not integrity of the
proofs that are recorded.

**Drop an unvalidatable receipt instead of failing the preparation.** Rejected
because it inverts the guarantee. Memory would still be presented to the model
while the artifact recorded no receipt for it, so the presentation would be
un-auditable and would look clean. Phase 7's exit evidence requires every
influence to identify its source; silence is not identification.

## How it is verified

`internal/contextprep/memory_validation_test.go` asserts four things:

- **`Prepare` refuses a receipt that `Validate` refuses.** Driven through the
  real preparer with memory supplied and no effective config SHA, which is the
  case the probe found the preparer emitting. This is the assertion that gives
  the three prior ADRs a production caller.

- **The refusal happens before the artifact is digested.** The returned artifact
  is the zero value, so no `presentation_digest` or `content_digest` exists for
  a presentation that was refused. Without this the check could pass while still
  committing bytes.

- **A receipt with no content digest is refused**, for a governed kind and the
  frozen kind alike, since "evidence of what memory was presented" that names no
  content is the emptiest form of the decoration ADR-0021 set out to prevent.

- **Phase 5's committed digests are unchanged.** The frozen receipt built on the
  normal path still validates, and the presentation and content digests for a
  fixture with memory match the values recorded before this change, so contexts
  committed under ADR-0013's barrier still verify.

Mutation-verified rather than trusted from a pass, and recorded in the commit
that lands this: removing the `Validate` call from `Prepare` must fail the
refusal and pre-digest assertions; dropping the `ContentDigest` requirement must
fail the evidence assertions; and making `Validate` refuse everything must fail
the Phase 5 positive case, so the refusals are not passing vacuously.
