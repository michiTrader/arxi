# ADR-0053: The retrieval selection witness spans every position, because the selection and reason guards each retrieved a single record and so proved only that the first selection describes the sole record present

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0049, ADR-0052
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0049 witnessed the structured `Selection` fields and ADR-0052 the free-text
`Reason`, and ADR-0052 concluded that with them "every content field of the
`Retrieval` receipt — structured and free-text alike — is defended". That claim
was verified at its narrowest point. Both guards approve one record and retrieve
it, so both assert against `Selections[0]` alone — the single selection a
one-record retrieval produces. Neither says anything about a second selection,
and neither can, because there is never a second record present.

A retrieval receipt is a slice. Its whole purpose beyond one record is to explain
an *ordering*: why this record was placed above that one. The one existing
multi-record test, `TestRankingPrefersTheMoreSpecificScope`, retrieves two
records and iterates every selection — but asserts only that each `Reason` is
non-empty, the same weak check ADR-0049 and ADR-0052 were written to replace one
selection over. So the correspondence between a selection's position and the
record it describes was the field nothing fails on: whether `Selections[i]`
actually describes the record ranked at position `i`, for any `i > 0`, was
unwitnessed.

Probing confirmed the gap is live and confirmed its width. Building every
selection from the winning record instead of from the record the loop is on —

```go
for range kept {
    r := kept[0] // every selection describes the winner
    ...
}
```

left the whole `internal/memorystore` suite green. Under that mutation a
retrieval of two records emits two selections that both name the winner's
identity, scope, sensitivity, purpose, evidence class, confidence, validity and
reason: an audit reading the receipt sees two records both claiming to be the
run-scoped record that actually ranked first, and the record that was really
placed second leaves no trace of its own classification. That is precisely the
misleading-but-coherent evidence ADR-0052 argued the reason must never produce,
reached through the selections the single-record guards never touch rather than
through the reason of the one they do.

**Why this is a different gap from the ones ADR-0049 and ADR-0052 closed.** Those
guards proved a selection *can* describe the record it names — that the fields
are sourced correctly for the one selection they inspect. This gap is that the
proof was never taken past the first position, so a receipt that describes its
first record perfectly and every later record as a copy of the first passes both.
The honest subject is not one more field on one selection; it is that the witness
must hold at every position a multi-record retrieval fills, which is the only
place the receipt's reason for being — explaining an ordering — is exercised.

## Decision

**One guard retrieves two records that differ in every ranking fact and asserts
that each selection, at each position, describes the record actually placed
there.** `TestEverySelectionWitnessesTheRecordAtItsOwnPosition` approves a
run-scoped record and a tenant-scoped record — distinct in scope specificity (so
they rank in a known order), principal, confidence and identity — retrieves both,
and for each returned record asserts that the selection at the same index names
that record's `RecordID`, `VersionID`, scope, and the four facts its reason
carries: the specificity rank read back from the record's own principal, that
principal, the record's confidence, and the exported `RetrievalVersion` constant.

The two records differ in confidence in the direction that makes a cross-wiring
visible from either side: the record that ranks first by specificity carries the
*lower* confidence, so a selection that borrowed the winner's confidence for the
loser would name the wrong level, and one that borrowed the loser's for the
winner would too. The expected values are derived from the returned record and
the exported constant rather than restated as literals — the same
derive-the-subject-from-the-corpus discipline ADR-0049 and ADR-0052 applied to a
single selection, extended to hold per position so the guard witnesses that each
selection describes the record at its index rather than matching a template.

No production code changes. `Retrieve` already builds each selection from the
record the ranking loop is on, and probing found no case where it mis-sources a
later one; this record closes a verification gap in the manner of ADR-0049
through ADR-0052 — a test that fails the moment the per-position correspondence
is withdrawn, added where the previous coverage stopped at the first selection.

**The guard's subject is the ordering, not one more field.** ADR-0049 and
ADR-0052 asked "does a selection describe its record?" and answered it for the
only selection a one-record retrieval has. This asks "does every selection
describe *its own* record when there is more than one?" — the question a receipt
that exists to justify a ranking has to answer, and the one the single-record
fixtures could not pose. A receipt whose second selection silently copies the
first reads as a coherent account of a ranking that never happened, passes every
existing check, and misleads the only reader that matters.

## Consequences

The retrieval receipt is now witnessed at every position a multi-record retrieval
fills, so a `Retrieve` that emitted a later selection describing the wrong record
— its identity, its classification or its reason — fails a test that names the
position and the audit consequence instead of passing silently. With ADR-0049
(the structured selection fields) and ADR-0052 (the reason), the earlier claim
that every content field of the receipt is defended now holds across the slice
rather than only at its first element.

No behavior changes. Each selection carried the same facts about its own record
before this record and carries them after; what changed is that the correspondence
is now verified past the first position, where before every selection but the
first was the field nothing fails on.

## Discarded alternatives

**Leave it: ADR-0049 and ADR-0052 already witness the selection and the reason.**
This is the argument the mutation refutes. Both guards witness `Selections[0]`
and only `Selections[0]`, because both retrieve one record; the mutation that
makes every selection describe the winner ships green today because no assertion
ever reads a second selection's content. A witness that holds at one position and
is generalised to a slice is the verified-at-its-narrowest-point mistake
`AGENTS.md` names, and the slice is exactly where the receipt does its job.

**Widen the existing single-record guards to two records instead of adding one.**
It would work, but it would conflate two properties in one test: that a selection
can describe its record (ADR-0049, ADR-0052) and that the correspondence holds
per position (this record). Keeping them separate means a future regression names
which property broke — a field mis-sourced versus a position mis-assigned — rather
than failing one overloaded test for either reason.

**Assert only that the second selection is not equal to the first.** It would
catch the specific mutation probed and nothing else: two records that happened to
share a confidence would make an inequality check pass while a real cross-wiring
of the differing fields went undetected. Deriving each expected value from the
record at that position witnesses the correspondence itself, not the accident
that the two records differ.

## How it is verified

`internal/memorystore/multi_selection_witness_test.go`:

- `TestEverySelectionWitnessesTheRecordAtItsOwnPosition` — approves a run-scoped
  and a tenant-scoped record differing in scope specificity, principal and
  confidence, retrieves both, and asserts for each returned record that the
  selection at the same index names its identity, its scope, and the specificity
  rank, principal, confidence and ranker version its reason carries, each derived
  from the returned record or the exported `RetrievalVersion` constant.

Confirmed to fail closed by mutation: building every selection from the winning
record (`for range kept { r := kept[0]; ... }`) leaves the record that ranked
second described as a copy of the first, which fails the position-1 assertions on
identity, principal, specificity and confidence — where before this record the
same mutation left the whole suite green; the baseline passes. Each message names
the position and the audit question the missing witness defeats — a selection
crediting a later record with the identity and ranking of the one above it.
