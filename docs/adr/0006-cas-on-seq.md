# ADR-0006: Concurrency is resolved with CAS on `seq`; `turn_source` is retired

- Status: accepted
- Affects: `internal/kernel/config.go` (`Interaction`), `internal/surface/surface.go` (`--if-seq`)
- Depends on: ADR-0002
- Origin: review by AI B

## Context

The first draft of the blueprint had a `turn_source` field: a declaration of who
was allowed to open turns in a run (the coordinator, any member, only the human).
The intent was to prevent races: if only one source can open turns, there are not
two writers competing.

AI B pointed out that this does not solve the problem, and was right.

`turn_source` answers **who can speak**. The real race is a different question:
**two writers modify the state the other one read**. They are independent. With
`turn_source: coordinator`, two human operators who both speak through the
coordinator still produce the full race: both read `seq 40`, both send a
correction, and the second is applied on top of a state that is no longer the one
its author saw.

Worse: `turn_source` gives the *feeling* of having solved concurrency. It is a
visible constraint in the blueprint, it feels like a guarantee, and it is not one.

## Decision

`turn_source` is **retired**. `Interaction` keeps a single field, `SteerTarget`,
which is what was actually needed: who a message reaches when no addressee is
specified.

Concurrency is resolved with **compare-and-swap on `seq`**:

```
arxi run prompt <run> "..." --if-seq 40
```

It applies only if the run is at exactly `seq 40`. If another event arrived in
the meantime, the operation is rejected and the client reads again and decides
what to do with the new information.

It combines with `--on-busy` (`reject` | `queue` | `steer`), which is a different
and orthogonal decision: `if-seq` protects against writing over a stale state,
`on-busy` decides what to do when the addressee is busy (ADR-0005).

### Why `seq` and not the turn

`seq` **identifies a version of the state**. It is a monotonic integer assigned
by the single log writer (ADR-0002), and `State = fold(...up to seq N)` is a
function of it. That is exactly what a CAS needs: a version token.

A turn is not a version of the state. A turn spans many events and therefore many
states; two operations quoting the same turn may be looking at completely
different states. A CAS on the turn would be a CAS that sometimes passes when it
should fail, which is the worst thing a CAS can do.

## Discarded alternative

**Lock the run while it is being written.** Correct, and discarded because of the
usage mode: a live run can spend hours waiting for a human approval. A lock over
that window turns any concurrent interaction into an indefinite wait, and a lock
that has to be breakable by hand brings the race back through the side door. The
CAS is optimistic, which is the right choice when the conflict is rare and the
cost of retrying is reading again.

## Consequences

- `--if-seq` is optional. Without it the operation is "last write wins", which is
  fine for a human at a terminal and wrong for a script. The surface declares it
  on `run prompt` and `run steer` so a programmatic client can be correct.
- A client that gets an `if-seq` rejection has to re-read. That is a feature: the
  new state reaches it before it insists.
- There is no declaration in the blueprint about who can open turns. Controlling
  who can do what is authorization, and it belongs in the authorization layer,
  not in the execution model.
- Less config surface, and what remains promises nothing it does not deliver.

## How it is verified

`surface_test.go` verifies that `run prompt` and `run steer` declare `if-seq` and
`on-busy`. The comment in `config.go` about `Interaction` documents the removal at
the exact spot where somebody would be tempted to reintroduce the field.
