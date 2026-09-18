# ADR-0021: A memory receipt names the record version it presented

- Status: accepted
- Affects: `internal/contextprep`, `spec/context.md`, `docs/adr/0020-retrieved-memory-is-data-not-instruction.md`
- Depends on: ADR-0013, ADR-0020
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7's exit evidence contains a requirement that reads as one sentence and
is really two:

> every influence identifies its source **and version**; stale versions stop
> appearing after correction

Identifying a source is a retrieval concern. Identifying a *version* is a
receipt concern, and the receipt already exists — which is why it is worth
checking what it actually holds before designing anything on top of it.

`contextprep.MemoryReceipt` today:

```go
type MemoryReceipt struct {
	Kind               string `json:"kind"`
	EffectiveConfigSHA string `json:"effective_config_sha"`
	ContentDigest      string `json:"content_digest"`
}
```

Three fields, none of which is a record identity or a version ID. `Kind` is the
constant `"frozen_context_memory"`. `EffectiveConfigSHA` identifies the
*configuration* the memory came from, not the record. `ContentDigest` is a hash
of the presented bytes.

That is exactly right for Phase 5, and `spec/context.md` describes it exactly:

> Phase 5 records frozen `ContextSpec.Memory` as a receipt bound to its **field
> identity and effective-config digest**.

`ContextSpec.Memory` is a single `string` on a frozen blueprint. It has no
record identity to record, because it is not a record — it is a field. A
receipt cannot name a version that the data model does not have.

**But ADR-0020 claims it already does.** Its Decision section states:

> The presentation records, for each memory message, the record identities and
> version IDs it carries, so the existing `MemoryReceipt` continues to bind
> what was presented. The receipt mechanism is unchanged and was never the gap.

The second sentence is true. The first is not: no field of `MemoryReceipt`
holds a record identity or a version ID, and none did when that line was
written. This was found by reading the struct while preparing Phase 7's design,
not by review of the prose.

The error is small in bytes and specific in consequence. ADR-0020 is the record
a Phase 7 retrieval design reads *first*, precisely because it settled the
channel so the retrieval decision could be about retrieval. A reader taking
that sentence at face value concludes version identity is solved and builds
ranking, correction and deletion lineage on top of a receipt that cannot
distinguish version 1 of a record from version 2 — both hash their own bytes
and neither says which record it is. The correction-propagation requirement
("stale versions stop appearing after correction") is unverifiable against such
a receipt: with no version named, there is nothing to compare a correction to.

So the gap is not the receipt. The gap is that **a decision record describes a
receipt the code does not implement**, in the one document Phase 7 is meant to
build from.

## Decision

A memory receipt names the record version it presented, and the receipt is
**honest about which of those it cannot name.**

`MemoryReceipt` gains two fields, both optional and both empty for frozen
blueprint memory:

- `RecordID` — the stable identity of the record, across all its versions.
- `VersionID` — the immutable identity of *this* version of that record.

For Phase 5's frozen `ContextSpec.Memory` these stay empty, because the source
is a configuration field with no record identity. `Kind` already distinguishes
that case as `"frozen_context_memory"`, and it keeps doing so. Emptiness here
is a measured fact about the source, not a missing implementation.

Phase 7's store, when it exists, populates both for every record it returns. A
receipt from a governed record with an empty `VersionID` is malformed, and the
verification below asserts that — because an optional field that is *silently*
optional would let Phase 7's store ship without version identity while the
receipt still looked well-formed. That is the ADR-0020 failure repeated one
layer down: a field present in the struct and absent in the data.

ADR-0020's Decision section is corrected in place to describe the receipt as it
is, with a note recording what it had claimed. It is not rewritten to look as
though it never overstated: the overstatement is why this record exists, and a
reader who was misled by it deserves to find that out where they were misled.

This ADR governs **version identity in the receipt**. It deliberately says
nothing about how records are stored, ranked, scoped or deleted. Those remain
Phase 7's own decisions.

## Consequences

`MemoryReceipt` grows two fields. Both are `omitempty`, so a Phase 5 receipt
encodes to the same JSON it encoded to before, and `presentation_digest` is
unaffected — the receipt is not part of the presentation, only of the artifact.
Prepared contexts committed before this change still verify against ADR-0013's
barrier. This is a deliberately cheaper change than ADR-0020's, and for a
structural reason: ADR-0020 moved bytes the model sees, this moves evidence
about those bytes.

Phase 7 inherits a receipt that can express version identity and a test that
fails if its store omits it. It does not inherit a solved problem — populating
the fields correctly is retrieval's job, and correction propagation still has
to be built and verified on top.

`spec/context.md` needs no correction. It was already accurate, and that is
worth stating rather than passing over: the spec described the receipt as bound
to "field identity and effective-config digest" while the ADR described record
and version IDs. **Two documents disagreed and the more precise one was right.**
The disagreement was visible in the repository the whole time and went unread
until someone compared both against the struct.

## Discarded alternatives

**Fix the sentence in ADR-0020 and write no new record.** The cheapest option,
and it loses the finding. A one-line edit to an accepted ADR leaves no trace of
*why* the line was wrong, and the reason — that version identity was assumed
solved because a decision record said so — is the part Phase 7 needs. It would
also silently revise an accepted decision, which is the practice ADR-0002
exists to prevent.

**Add the fields without asserting they are populated for governed records.**
Then Phase 7's store can return records with empty `VersionID` and every
existing test still passes. The struct would advertise version identity and the
data would not carry it: the ADR-0020 defect, reproduced exactly, one layer
down. A field is not a guarantee until something fails when it is empty.

**Make `RecordID` and `VersionID` required, and give frozen memory synthetic
values.** Superficially tidier — no empty fields, no conditional assertion. But
it would mint an identity for something that has none, and a synthetic version
ID is indistinguishable from a real one downstream. Correction propagation
would then have a version to compare against that no store can correct. Empty
is the honest encoding of "this source has no record identity", and `Kind`
already says which case a reader is in.

**Defer all of this to Phase 7's retrieval ADR.** The same argument ADR-0020
rejected for the channel, and it fails the same way: the retrieval ADR would
inherit a receipt shape it never examined, having been told by ADR-0020 that
version identity was handled. The reason to fix a false claim before building
on it is that the building is what makes it expensive.

## How it is verified

`internal/contextprep/memory_receipt_test.go` asserts three things, and the
third is the one that makes the other two more than documentation:

- **Frozen blueprint memory leaves `RecordID` and `VersionID` empty**, and the
  receipt still carries `Kind`, `EffectiveConfigSHA` and `ContentDigest`. This
  pins that the new fields cost Phase 5 nothing.

- **A Phase 5 receipt encodes to JSON without the new keys**, so the artifact
  wire form is unchanged for every configuration that exists today. Asserted
  against the encoded bytes, not the struct, because `omitempty` is a claim
  about encoding and checking the struct would not test it.

- **A receipt whose `Kind` marks it as coming from a governed record is
  rejected when `VersionID` is empty.** This is the assertion that keeps the
  fields from being decoration. Without it, Phase 7 could ship a store that
  never sets a version and nothing would fail — which is precisely how
  ADR-0020's sentence survived: nothing was measuring it.

The verification deliberately does not include a test that a governed record
*round-trips* its version through retrieval. There is no store to retrieve
from; such a test would have to construct the receipt it then inspects, and
would pass against a store that never populates the field. Phase 7 owns that
test when Phase 7 owns a store.
