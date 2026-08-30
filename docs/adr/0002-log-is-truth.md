# ADR-0002: The log is the truth; snapshots are cache; the blueprint is frozen

- Status: accepted
- Affects: `internal/kernel/state.go` (`BlueprintSHA`), `internal/kernel/decide.go`
- Depends on: ADR-0001

## Context

With a pure reducer, state can be reconstructed:

```
State = fold(Decide, State0, events)
```

That opens a question that has to be answered once and not case by case: when
the stored snapshot and the log disagree, who wins?

The temptation is "the snapshot wins, it is faster". And it is faster. It is also
how you end up with a state nobody can explain: a `State` that says
`status: failed` with no failure event to justify it, because the snapshot was
written wrong once, three weeks ago.

## Decision

**The log wins, always.** Snapshots are exclusively a read optimization. You can
delete all of them and the system loses no information — it loses startup speed.

Two consequences with teeth:

### The blueprint is frozen at start

The run stores a `blueprint_sha` in its state (`State.BlueprintSHA`) and the
reducer **never** reads the live blueprint file. It reads the frozen copy in
`runs/<id>/blueprint.snapshot.yaml`.

Without this, replaying a run from last week would use today's config. If in the
meantime somebody changed `advance_when` from `all` to `quorum:2`, the replay
advances where the original run got stuck, and the diagnosis it gives you is
fiction. A replay that does not reproduce is worthless, and the worst part is
that it does not scream about it: it gives you a plausible, wrong answer.

This fix came out of the review by AI A.

### The reducer does not assign `seq`

The events the reducer returns via `Emit` carry `seq: 0`. The sequence number is
assigned by the single log writer, at write time.

The reducer does not know what global order what it emits will land in: between
deciding and writing, another event from another source may have arrived.
Pretending it does know is inventing a race that later shows up as a log with
duplicate `seq`, which is exactly what breaks the CAS of ADR-0006.

## Discarded alternative

**Authoritative snapshots with the log as an audit trail.** Faster, and it is
what most workflow systems do. Discarded because it turns diagnosis into
archaeology: when state and log differ there is no rule for deciding what really
happened, and `arxi run why` — which is the whole reason this tool exists — ends
up answering over data it cannot justify.

## Consequences

- Starting a long run from zero costs a full fold. Acceptable: the fold is pure
  and does no I/O per event.
- Changing the blueprint of a live run is impossible by design. That is what
  `run fork --at-seq` is for: you branch with the new config and the original run
  stays intact and reproducible.
- A corrupted log is an unrecoverable problem, so the log has to be append-only
  and written by a single writer.

## How it is verified

- The golden in `testdata/scenarios/` resets `seq = 0` before comparing,
  precisely so the test does not depend on assignment order — if the reducer
  started assigning `seq`, the comparison would fail.
- The tests in `decide_test.go` build the `Config` explicitly and pass it to
  `Decide`; there is no route by which the reducer could reach a file.
- `internal/arch_test.go` forbids `os` and `io` in the kernel, which is exactly
  what would be needed to read the live blueprint.
