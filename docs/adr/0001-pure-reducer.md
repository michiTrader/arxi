# ADR-0001: The reducer is pure and describes effects instead of running them

- Status: accepted
- Affects: `internal/kernel/decide.go`, `internal/kernel/effect.go`, `internal/arch_test.go`

## Context

An agent orchestrator has to do four things that look different: run a real
team, simulate it without spending money, replay an old run to understand what
happened, and explain why a run is stuck right now.

The obvious way to write it is a loop that decides and acts in the same place:
look at the state, call the model, write the file, update the state. It is the
shortest path to the first demo.

The problem shows up later. `--sim` needs the same loop without the real calls,
so you add a flag and an `if` at every point of contact with the world. `replay`
needs the same loop with *no* effects at all, so you add another flag and
another `if`. Every new point of contact has to be remembered in three places.
Whoever forgets one produces a `replay` that sends email.

## Decision

The core is a single function:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pure: it does not consult the clock, does not open sockets, does not read or
write files, does not use randomness. Everything it wants to happen in the world
it **describes** as a value of type `Effect` and returns. The caller decides what
to do with that list.

The four features stop being four programs:

| feature | what it is |
|---|---|
| `arxi run` | fold + real executor |
| `arxi run --sim` | fold + fake executor |
| `arxi run replay` | fold, with no executor |
| `arxi run why` | read the `State` that came out of the fold |

The decision logic exists **once**. There is no way for `replay` to drift away
from `run`, because there are not two implementations that could diverge.

## Discarded alternative

**A reducer allowed to call the world, with dependency injection.** Pass a
`Clock` and an `HTTPClient` as interfaces and mock them in tests. That is the
usual solution and it is worse, for two concrete reasons:

1. Purity stops being verifiable. With injection, "it is pure" depends on nobody
   calling `time.Now()` directly. Nothing checks that; you discover it when a
   replay comes out different.
2. The order of effects stays hidden inside control flow. When effects are
   returned data, the order is a list you can inspect, test and compare against
   a golden. When they are calls, the order is "whatever happened" and you have
   to read the whole reducer to know it.

## Consequences

- The reducer cannot make decisions that depend on real time. Timeouts arrive as
  events (`timer.fired`), not as comparisons against the clock. That is what
  makes the virtual clock of `--sim` possible.
- The reducer does not assign `seq` to the events it emits (see ADR-0002).
- There is an executor that *is* impure and has to be tested separately. It is
  more code in total, but the impure code stays concentrated in one small place
  instead of spread through all the logic.
- An effect that nobody implements in the executor does not break the reducer: it
  is decided and discarded. That is what lets us declare the surface before
  implementing it.

## How it is verified

`internal/arch_test.go` runs `go list -json` over the `kernel` package and fails
if `time`, `net`, `net/http`, `os`, `os/exec`, `math/rand`, `crypto/rand`,
`database/sql`, `io` or `bufio` appear in its own import closure. The error
message names the consequence: *"time.Now() inside the reducer breaks replay and
the virtual clock of --sim"*.

It is a guarantee about the import graph, not about the discipline of the next
commit.
