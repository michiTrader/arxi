# Prompt: implement `internal/logstore` (the append-only event log)

Copy everything below the line into the other AI. It is written to be
self-contained: it does not assume the reader has seen this conversation.

Recommended model: **Opus 5**. Not because the code is long — it is maybe 400
lines — but because the concurrency and crash-safety decisions are the kind that
look fine in review and fail once a month in production.

---

## Task

Implement a new package `internal/logstore` in the Go repository
`github.com/michiTrader/iash` (Go 1.22). It is the append-only event log: the
component that owns durability and sequence-number assignment.

**Read `AGENTS.md` first.** It is binding. The two rules that will shape your
work most:

- **English only.** Identifiers, comments, tests, strings, commit messages.
- **Comments must explain why a decision is right and what breaks otherwise**,
  not what the code does. Tests must protect decisions and their failure
  messages must name the consequence and the remedy. A `t.Fatal("mismatch")`
  does not meet the contract of this project.

Then read, in this order: `docs/adr/0002-log-is-truth.md` (this is *your* ADR —
you are implementing it), `docs/adr/0006-cas-on-seq.md`, `spec/events.md`, and
`docs/design/10-execution.md` §10.2.

## Context: what this system is, in one paragraph

`iash` orchestrates teams of LLM agents. Its whole design rests on one pure
function in `internal/kernel`:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

The reducer is pure: no clock, no network, no disk, no randomness. It returns
effects as *data* and somebody else performs them. That is what makes `run`,
`--sim`, `replay` and `run why` the same machinery instead of four programs that
drift apart (ADR-0001). Purity is enforced by `internal/arch_test.go`, which
inspects the import graph with `go list -json`.

`State` is derived, never stored authoritatively:

```
State = fold(Decide, State0, events)
```

**You are building the thing that owns those events.** Everything else in the
system reads state through your package.

## Your scope, and what is explicitly not yours

**Yours:** appending events, assigning `seq`, reading ranges, compare-and-swap
on `seq`, snapshots as a pure cache, and rebuilding `State` by folding from
disk.

**Not yours, do not implement, do not stub speculatively:**

- Running effects, calling LLM providers, spawning agent turns. Another package
  (`internal/exec`) is being written in parallel by someone else. **Do not create
  `internal/exec` and do not import it.**
- Any change to `internal/kernel`. It is finished, 45 tests pass, and it is the
  one package that must stay pure. If you believe you need to change it, stop
  and say so in your final message with the reason — do not change it.
- CLI wiring in `cmd/iash`.

If you find yourself needing something from `internal/exec` to finish, that is a
signal the boundary is wrong. Say so rather than inventing an interface for it.

## The API you must provide

Design the details yourself, but these operations must exist and mean exactly
this:

```go
package logstore

// Store owns one run's log directory: runs/<run-id>/.
type Store struct { /* ... */ }

func Open(dir string) (*Store, error)

// Append assigns seq to each event and writes them durably, in order.
// Events arrive from the reducer with Seq == 0.
func (s *Store) Append(events []kernel.Event) ([]kernel.Event, error)

// AppendIfSeq is Append with a compare-and-swap precondition: it applies only
// if the log's current head is exactly expectedSeq. See ADR-0006.
func (s *Store) AppendIfSeq(expectedSeq int64, events []kernel.Event) ([]kernel.Event, error)

// Read returns events in [fromSeq, toSeq]. toSeq == 0 means "to the head".
func (s *Store) Read(fromSeq, toSeq int64) ([]kernel.Event, error)

func (s *Store) Head() int64

// Fold rebuilds state by replaying the log through kernel.Decide.
// untilSeq == 0 means "the whole log".
func (s *Store) Fold(c kernel.Config, untilSeq int64) (kernel.State, error)

func (s *Store) WriteSnapshot(st kernel.State, atSeq int64) error
```

Storage format: **NDJSON**, one `kernel.Event` per line, in
`runs/<run-id>/events.ndjson`. Snapshots in the same directory. Use
`encoding/json`; `kernel.Event` already has the JSON tags.

## The decisions that matter — get these right

These are not implementation details. Each one is a documented decision of the
system, and each has a specific way of failing that you should write a test for.

### 1. You assign `seq`. The reducer never does.

Events come out of the reducer with `Seq: 0`. That is deliberate, and the comment
on `kernel.Event.Seq` says why: the reducer cannot know what global order its
output will land in, because another event from another source may arrive between
deciding and writing. Pretending otherwise invents a race that surfaces later as
a log with duplicate `seq` — which breaks the CAS in point 3.

`seq` must be **strictly monotonic with no gaps**, starting at 1. A gap is
indistinguishable from a lost event, and `Fold` cannot tell the difference.

### 2. Single writer, and durability that survives a hard kill

**The log is the truth** (ADR-0002). Snapshots are cache — deleting every
snapshot must lose no information, only startup speed. So a torn write is the one
unrecoverable failure in this system.

Requirements:

- Exactly one writer per run directory. Enforce it, do not document it —
  an `O_EXCL` lock file or equivalent. Two processes appending concurrently is
  how you get duplicate `seq`.
- A partial line at the end of the file (killed mid-write) must be **detected**
  on `Open` and must not silently truncate history or produce a corrupt `Fold`.
  Decide whether you recover by discarding the trailing partial line or by
  refusing to open, then **write down in a comment why that choice is right and
  what the other one would break.** Both answers are defensible; an
  undocumented answer is not.
- Batches must be atomic. If `Append` is given 3 events, a crash must not leave
  1 or 2 of them visible. Explain in a comment how you get that.

Think about `fsync` explicitly. State your policy and its cost in a comment. "I
never called fsync" is a decision too, and an indefensible one for a log that
claims to be the source of truth.

### 3. CAS is on `seq`, never on the turn

Read ADR-0006 before writing `AppendIfSeq`. The short version: `seq` identifies a
*version of the state*, and `State = fold(...up to seq N)` is a function of it.
That is what a CAS needs. A turn spans many events and therefore many states, so a
CAS on a turn would sometimes *pass when it should fail* — the worst possible
behaviour for a CAS.

A rejected `AppendIfSeq` is **not an error condition to log and swallow**. The
caller must be able to distinguish "the CAS failed, re-read and retry" from "the
disk is broken". Return a typed error carrying the actual head so the caller can
act without a second round trip.

### 4. `Fold` must be order-independent and must not mutate its input

`Fold` is the whole reason the log exists. Two properties:

- Folding the same log twice must give identical states. If it does not, the
  reducer mutated its input through an aliased map — `kernel.State` has maps and
  slices and `Clone()` exists for exactly this reason. There is already a test
  for this in `internal/kernel/decide_test.go`; add the equivalent for folding
  *from disk*.
- Folding to `untilSeq` must give exactly the state at that point, which is what
  makes `run replay --until-seq` honest.

### 5. Snapshots are a cache and must be provably droppable

Write a test that: builds a log, folds it, writes a snapshot, **deletes the
snapshot file**, folds again, and asserts the state is identical. If that test
does not exist, nothing stops a future change from making snapshots load-bearing,
and then a stale snapshot becomes a state nobody can explain — a run reporting
`failed` with no failure event to justify it.

Do **not** implement snapshot-based fast loading yet. Write and verify snapshots;
reading them as an optimization is a later change, and doing it now means
shipping the cache before the thing it caches is trusted.

## Testing bar

Match the existing style — read `internal/kernel/decide_test.go` and
`internal/surface/surface_test.go` first. Table-driven where it fits, and every
failure message names the consequence and the remedy. Look at how the existing
tests phrase it:

```
"two folds of the same log gave different states: replay is worthless"
```

That is the register: what broke, and why the reader should care.

Cover at minimum:

- `seq` monotonic, no gaps, starts at 1
- concurrent `Append` from goroutines produces no duplicate `seq` (run it under
  `-race`)
- a second `Open` on the same directory fails while the first is held
- a truncated final line is handled per your documented policy
- `AppendIfSeq` succeeds at the right head, fails at a stale one, and the
  failure is distinguishable from an I/O error
- double `Fold` gives identical state
- `Fold(untilSeq)` matches a fold of the truncated event slice
- snapshot deleted → `Fold` unchanged

## Verification before you finish

```bash
export PATH=$PATH:/usr/local/go/bin   # if go is not on PATH
gofmt -l .            # must print nothing
go vet ./...          # must be clean
go test -race -count=1 ./...   # everything must pass, including the 48 existing tests
```

`-race` is not optional here; you are writing the only concurrent component in
the repository so far.

Also confirm you did not break purity:

```bash
go test -run TestKernel -v ./internal/
```

`internal/kernel` must still import nothing from the project and nothing from
`os`/`io`. **Your package imports `kernel`; `kernel` must never import yours.**
If you ever feel the urge to have the kernel read a file, that is the design
telling you the data belongs in `Config` or in an `Event`.

## Commit and hand off

Work on a branch off `main` named `feat/logstore`. Commit in focused steps with
conventional-commit messages (`feat(logstore): ...`, `test(logstore): ...`).

In your final message, report:

1. Your durability policy — fsync strategy, and what a hard kill can and cannot
   lose.
2. Your torn-write recovery choice and why the alternative is worse.
3. Anything in `kernel` or in the ADRs you think is wrong or ambiguous. You will
   be the first person to implement against ADR-0002; if it does not survive
   contact with reality, that is a finding worth more than the code. Do not
   silently work around it.
