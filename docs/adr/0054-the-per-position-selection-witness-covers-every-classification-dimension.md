# ADR-0054: The per-position selection witness covers every classification dimension, because ADR-0053's two records differed only in ranking facts and so left the non-ranking classification unwitnessed past the first selection

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0049, ADR-0053
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0053 widened the selection witness past the first position, and concluded
that with it "the receipt is defended across the slice rather than only at its
first element". That claim was verified at its widest-looking point but its
narrowest real one: the guard it added,
`TestEverySelectionWitnessesTheRecordAtItsOwnPosition`, retrieves two records
that "differ in every ranking fact — scope specificity, principal, confidence
and identity", and asserts each selection describes the record at its position
using exactly those facts. `record_id`, `version_id`, `scope`, `confidence` and
the four facts the reason carries are witnessed per position. The four remaining
`Selection` fields are not: `sensitivity`, `purpose`, `evidence_class` and the
`valid_from`/`valid_to` bounds.

They are not witnessed per position because ADR-0053's two fixture records are
*identical* in exactly those dimensions. Both are approved `Public`, `Operate`,
`Stated`, with an empty `Validity{}`. The fixture was built to differ in the
ranking facts so the order would be determinate and a cross-wiring of those
facts visible — and it succeeds at that — but a field the two records share
cannot expose a selection that copied it from the wrong record, because the
wrong record's value is the same value. So the per-position correspondence
ADR-0053 established holds for the ranking facts and identity, and for the
non-ranking classification it still rests on ADR-0049 alone, which asserts those
four dimensions at `Selections[0]` and only there.

This is the same shape ADR-0053 itself closed one layer down, recurring one
layer up. ADR-0049 witnessed the structured fields at position 0; ADR-0053 said
"across the slice" but carried only the fields its fixture happened to vary to
position 1. The dimensions ADR-0042 (sensitivity), ADR-0043 (purpose), ADR-0044
(evidence class) and ADR-0047 (valid time) each added to `Selection` — each
justified as "recorded so an audit can answer under what classification the
record was presented" — are exactly the ones the multi-record fixture holds
constant, so their per-position correspondence is the field nothing fails on for
any `i > 0`.

Probing confirmed the gap is live and confirmed its width. Sourcing those five
fields from the winning record instead of the record the loop is on —

```go
for _, r := range kept {
    ...
    Sensitivity: string(kept[0].Sensitivity), Purpose: string(kept[0].Purpose),
    EvidenceClass: string(kept[0].EvidenceClass), Confidence: string(r.Confidence),
    ValidFrom: string(kept[0].Validity.From), ValidTo: string(kept[0].Validity.To),
    ...
}
```

left the whole `internal/memorystore` suite green — including ADR-0053's own
guard, which reads `confidence` (sourced correctly above) but never the four
classification fields. Under that mutation a retrieval of two records that
differ in sensitivity, purpose, evidence class or validity emits a second
selection describing the *winner's* classification: an audit reading the receipt
sees the record ranked second disclosed under a clearance, approved for a use,
backed by an evidence class and valid across a window that belong to a different
record — the precise disclosure ADR-0042 through ADR-0047 each added their field
to make visible, made invisible again for every position but the first.

**Why this is a different gap from the one ADR-0053 closed.** ADR-0053 proved
that a selection's *identity and ranking facts* correspond to the record at its
position. This is that the selection's *authorization classification* does too.
The honest subject is not one more position — ADR-0053 reached position 1 — it
is that "across the slice" must hold for every field a selection carries, and
ADR-0053's fixture could only exercise the fields it varied. A receipt whose
second selection names the right record and confidence but the first record's
sensitivity, purpose, class and window reads as a coherent classification of a
record that was never authorized that way, and passes ADR-0053.

## Decision

**One guard retrieves two records that differ in the non-ranking classification
dimensions as well as the ranking ones, and asserts that each selection, at each
position, names its own record's sensitivity, purpose, evidence class and
valid-time bounds.**
`TestEverySelectionWitnessesTheNonRankingClassificationAtItsOwnPosition`
approves a run-scoped record and a tenant-scoped record that differ in scope
specificity — so they rank in a known order and the positions the assertions key
on mean what they say — and additionally in sensitivity (`confidential` vs
`public`), purpose (`operate` vs `personalize`), evidence class (`stated` vs
`observed`) and validity (`[2025, 2027)` vs `[2026, unbounded)`). It retrieves
both under a query that authorizes both records' values in every dimension, and
for each returned record asserts that the selection at the same index names that
record's `Sensitivity`, `Purpose`, `EvidenceClass`, `ValidFrom` and `ValidTo`,
each read back from the returned record rather than restated as a literal.

The two records differ in each of these dimensions so a cross-wiring is visible
from either side: the winner carries the *higher* sensitivity, the *earlier*
lower bound and a *bounded* upper end, so a selection that borrowed the winner's
classification for the loser names the wrong level, use, class and window, and
one that borrowed the loser's for the winner does too. The as-of instant
(`2026-01-01T00:00:00Z`) falls inside both intervals, so both records are
authorized on valid time while their recorded bounds still differ — which is
what lets a mis-sourced bound be caught rather than filtered out before it can
be.

No production code changes. `Retrieve` already builds each selection from the
record the ranking loop is on for every field, and probing found no case where
it mis-sources a later one; this record closes a verification gap in the manner
of ADR-0049 through ADR-0053 — a test that fails the moment the per-position
correspondence of the classification fields is withdrawn, added where ADR-0053's
coverage stopped at the fields its fixture varied.

**The guard's subject is the classification, not the ranking.** ADR-0053 asked
"does every selection describe its own record's identity and ranking?" and
answered it with a fixture that varied only the ranking. This asks "does every
selection describe its own record's *authorization classification*?" — the
dimensions an audit reads to confirm each presented record was within its
clearance, approved for its use, backed by its class and valid at the as-of, per
position rather than only for the record that happened to rank first.

## Consequences

The retrieval receipt is now witnessed at every position for the classification
dimensions as well as the ranking ones, so a `Retrieve` that emitted a later
selection naming the wrong record's sensitivity, purpose, evidence class or
valid-time window fails a test that names the position, the dimension and the
audit consequence instead of passing silently. With ADR-0049 (the structured
fields at the first position), ADR-0052 (the reason) and ADR-0053 (identity and
ranking facts across the slice), the claim that every content field of the
receipt is defended now holds across the slice for *every* field a selection
carries, not only those a two-record fixture happened to vary.

No behavior changes. Each selection carried its own record's classification
before this record and carries it after; what changed is that the per-position
correspondence of the four classification dimensions is now verified, where
before it was witnessed only at the first selection and generalised to the
slice.

## Discarded alternatives

**Leave it: ADR-0053 already witnesses the slice.** This is the argument the
mutation refutes. ADR-0053's guard varies only the ranking facts, so the
classification fields are equal across its two records, and a selection that
copies them from the wrong record names the same value the right one would —
undetectable. ADR-0049 covers those fields only at `Selections[0]`. A witness
that holds for the fields one fixture varied and is generalised to every field a
selection carries is the verified-at-its-narrowest-point mistake `AGENTS.md`
names, one field-group over from where ADR-0053 found it.

**Widen ADR-0053's guard to vary the classification dimensions too.** It would
work, but it would conflate two properties in one test: that identity and
ranking correspond per position (ADR-0053) and that the classification does
(this record). Keeping them separate means a future regression names which
correspondence broke — a ranking fact mis-assigned versus a classification
dimension mis-sourced — rather than failing one overloaded test for either
reason, the same separation ADR-0053 kept from ADR-0049.

**Assert the second selection's classification differs from the first's.** It
would catch the specific mutation probed and nothing else: two records that
happened to share a sensitivity or a window would make an inequality check pass
while a real cross-wiring of the differing dimensions went undetected. Deriving
each expected value from the record at that position witnesses the
correspondence itself, not the accident that the two records differ — the same
reason ADR-0053 rejected an inequality check.

## How it is verified

`internal/memorystore/nonranking_selection_witness_test.go`:

- `TestEverySelectionWitnessesTheNonRankingClassificationAtItsOwnPosition` —
  approves a run-scoped and a tenant-scoped record differing in scope
  specificity, sensitivity, purpose, evidence class and valid-time window,
  retrieves both under a query that authorizes both records' values and an as-of
  inside both intervals, and asserts for each returned record that the selection
  at the same index names its sensitivity, purpose, evidence class, valid-from
  and valid-to, each derived from the returned record.

Confirmed to fail closed by mutation: sourcing the five classification fields
from the winning record (`Sensitivity: string(kept[0].Sensitivity), ...`) leaves
the record that ranked second described under the first record's classification,
which fails the position-1 assertions on sensitivity, purpose, evidence class
and both valid-time bounds — where before this record the same mutation left the
whole suite green, including ADR-0053's guard; the baseline passes. Each message
names the position, the dimension and the audit question the missing witness
defeats — a selection crediting a later record with the classification of the
one ranked above it.
