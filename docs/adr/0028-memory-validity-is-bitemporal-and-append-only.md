# ADR-0028: Governed memory validity is bitemporal and append-only

- Status: accepted
- Affects: `internal/memory`, `internal/arch_test.go`, `docs/roadmap.md`
- Depends on: ADR-0002, ADR-0021
- Enables: Phase 7 (read-only governed memory)

## Context

The seven prerequisites recorded before this one (ADR-0020 through ADR-0027)
settled the memory *receipt* — the evidence of what was presented to a model —
and the integrity of the documents Phase 7 is planned from. None of them touched
the memory *record*: the stored thing a receipt names a version of. The roadmap
lists what a record must carry — "stable and immutable version IDs, provenance,
evidence class, authority, confidence, sensitivity, purpose, retention,
lifecycle and bitemporal validity" — and marks "temporal validity, ranking and
deletion lineage" as still undecided (item 7). The store does not exist.

Two pieces of Phase 7's exit evidence rest entirely on the temporal model, and
neither is expressible without it:

- "stale versions stop appearing after correction";
- and, from Phase 10 which this feeds, "contradictions preserve temporal
  history".

A single timestamp per record cannot satisfy both at once. "Stops appearing"
demands that a corrected version be excluded from a present query; "preserves
history" demands that the same version stay visible to a query about the past.
One axis cannot both hide a version and keep it.

## Decision

**Validity is bitemporal, and its decision axis is append-only.** This ADR
introduces `internal/memory` as a pure leaf — the sense in which `internal/job`
and `internal/workspace` are pure — holding the `Validity` contract and nothing
that reaches a clock, store or network.

Two independent half-open intervals:

- **Valid time** `[ValidFrom, ValidTo)` — when the described fact holds in the
  world. A person's address is true from the day they moved, whatever day we
  were told.
- **Decision time** `[RecordedAt, SupersededAt)` — when the store believed this
  version. It opens when the version is recorded and closes when a correction
  supersedes it.

`LiveAt(decisionAsOf, validAt)` selects a version iff the decision instant lies
in the belief interval and the world instant lies in the valid interval. A
correction closes the old version's belief interval and appends a new version;
it never mutates the old version's bytes. So a present query (`decisionAsOf` =
now) no longer selects the corrected version — it stopped appearing — while a
historical query (`decisionAsOf` before the correction) still selects it — the
history is preserved. That append-only rule is ADR-0002's "the log is truth"
applied to the belief axis: correction is a new fact, not an edit of an old one.

Both axes are **half-open** so adjacent versions tile the timeline: the
successor's lower bound equals the predecessor's upper bound, and the switchover
instant belongs to exactly the successor. Closed-closed intervals would hand
that instant to both versions, and a query at the switchover would see the
record twice.

`Validate` refuses an instant that is absent or unparseable and an interval that
runs backwards, rather than repairing it: an unparseable instant compares equal
to nothing and would silently fall outside every as-of window instead of failing
where it was written, which is the same "fail closed" reasoning ADR-0023 gives
for unknown receipt kinds.

## Consequences

The record type is not built here, and that is deliberate. Provenance, evidence
class, authority, confidence, sensitivity, purpose, retention and lifecycle are
each their own decision, and several depend on ranking and deletion lineage that
remain undecided. Validity is settled first because it is the axis the exit
evidence names and the hardest to change once records are committed against it —
the same reason ADR-0020 settled the presentation channel before the store.

`internal/memory` is guarded by `TestMemoryModelIsPureAndIndependent`, which
permits `time` (a record represents instants) and forbids every runtime import
and every project import, exactly as the job and workspace guards do. If `LiveAt`
ever read the clock, a version excluded from an as-of query on the live fold
could be included on replay, and "stale versions stop appearing" would be true
one run and false the next.

The times are RFC3339Nano strings, matching event `Ts` and authorization
`ExpiresAt`, because a record's instants are data carried in events, not a
`time.Time` a store minted. The package parses them; it does not produce them.

## Discarded alternatives

**One timestamp (`updated_at`) per record.** The obvious model and the one that
makes the two exit-evidence clauses mutually exclusive: a single axis cannot both
exclude a corrected version from the present and retain it in the past.

**Mutate the record in place on correction.** Simpler storage, and it erases
history. A query about what was believed last month would return this month's
correction, so an audit of a contradiction — the Phase 10 evidence — is
impossible. Append-only is the price of a defensible history.

**Closed intervals `[from, to]`.** Reads more naturally and overlaps at every
boundary. Two adjacent versions would both claim the switchover instant, so a
query there sees the record twice; the deletion-lineage evidence forbids exactly
that duplication.

**Store `time.Time` and let the package stamp `RecordedAt` itself.** Convenient
and fatal to replay: the package would read the clock, and the arch guard exists
to keep it from doing so. `RecordedAt` is the caller's fact, supplied as a value,
for the same reason every `internal/trigger` function takes `now` as a parameter.

**Build the whole record struct now.** Larger and premature: authority is
already enumerated (ADR-0023) but confidence, sensitivity, retention and
lifecycle interact with ranking and deletion lineage that have no decision yet.
Settling validity alone keeps this reviewable and does not smuggle in the
undecided parts.

## How it is verified

`internal/memory/validity_test.go`:

- `TestValidateRefusesIllFormedValidity` — a table pinning that absent, backwards
  and unparseable instants are refused and well-formed ones accepted.
- `TestCorrectionHidesTheStaleVersionButKeepsTheHistory` — the exit-evidence
  scenario: after a correction the stale version is gone from a present query and
  still visible to a historical one.
- `TestHalfOpenIntervalsTileWithoutOverlapAtTheBoundary` — at the supersession
  instant exactly one version is live, and it is the successor.
- `TestValidTimeExcludesInstantsOutsideTheFactWindow` — belief and validity stay
  independent: a current version does not answer a world query outside its valid
  window.
- `TestLiveAtRequiresBothQueryInstants` — an as-of query with a missing instant
  fails rather than defaulting to now, keeping the package off the wall clock.

`internal/arch_test.go`'s `TestMemoryModelIsPureAndIndependent` keeps the package
a pure leaf, so the temporal judgment stays a function of supplied values.
