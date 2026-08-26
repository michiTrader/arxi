# ADR-0003: Effects are classified into control and independent

- Status: accepted
- Affects: `internal/kernel/effect.go` (`EffectClass`), `internal/kernel/decide.go` (`orderEffects`)
- Depends on: ADR-0001
- Origin: review by AI A

## Context

ADR-0001 says `Decide` returns `[]Effect`. AI A pointed out the obvious hole
that leaves open: **the executor receives a list — does it run them in order or
in parallel?**

This is not a performance question. The two simple answers break different
things:

- **All in parallel** breaks correctness. `Emit` and `SetTimer` change what the
  rest of the system is going to see. If an `Emit` of `stage.advanced` runs after
  a `SpawnTurn`, the turn starts by reading a stage that is no longer current.
- **All in order** breaks the reason the tool exists. Three agent turns that know
  nothing about each other get serialized, and then orchestrating a team costs
  the same as running them one by one by hand.

A tempting third path is to let the executor decide case by case with a `switch`
over the concrete type. That moves policy into the executor and guarantees that
the next `Effect` variant runs with whatever default policy it happens to get,
without anybody having thought about it.

## Decision

The class is a property **of the effect**, declared in the kernel:

```go
type Effect interface {
	isEffect()
	Class() EffectClass
}
```

Two classes, and the policy is a single rule:

| class | variants | how it is executed |
|---|---|---|
| `ClassControl` | `Emit`, `SetTimer`, `CancelTimer`, `Snapshot` | sequential, in the exact order of the list |
| `ClassIndependent` | `SpawnTurn`, `CallTool`, `AskHuman` | concurrent among themselves |

`Decide` sorts the list before returning it (`orderEffects`), so the executor
only needs to know this: **run the control prefix one by one, then the rest in
parallel.** There is no `switch` over concrete types in the executor, and a new
variant has to declare its class to compile.

### Why `SliceStable` and not `Slice`

```go
sort.SliceStable(fx, func(i, j int) bool { return fx[i].Class() < fx[j].Class() })
```

The relative order **among** the `Emit`s is semantic: `stage.advanced` has to
come out before `stage.entered`, because the second describes the state the first
one left. An unstable sort reorders them depending on input size and pivot
implementation — that is, it breaks intermittently and unreproducibly, which is
the worst possible way to break in a system whose selling point is faithful
replay.

## Discarded alternative

**A DAG of explicit dependencies between effects.** More expressive: it would let
us parallelize two `Emit`s that genuinely do not touch each other. Discarded
because the cost is not paid back by the gain: you have to declare the dependency
at every emission site, and one forgotten dependency produces exactly the race
bug we wanted to avoid, now with more ceremony. Two classes are coarse and they
are correct by construction.

## Consequences

- An `Emit` never runs in parallel with another `Emit`, even when it sometimes
  could. Accepted: `Emit`s are cheap, the expensive ones are the `SpawnTurn`s and
  those do go in parallel.
- Adding an `Effect` variant requires deciding its class (it is a method on the
  interface). There is no silent default.
- The executor has no policy of its own about ordering. The whole decision lives
  in the kernel, where it is pure and testable.

## How it is verified

- `TestEffectExhaustive` walks `EffectVariants()` and fails if a variant is not
  registered — see ADR-0007.
- The ordering tests in `decide_test.go` verify that control effects come before
  the independent ones and that the relative order of the `Emit`s is preserved.
- The golden in `testdata/scenarios/` pins the full effect list: an accidental
  ordering change makes the comparison fail.
