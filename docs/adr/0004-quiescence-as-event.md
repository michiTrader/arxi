# ADR-0004: Quiescence is an event with a diagnosis, not a terminal state

- Status: accepted
- Affects: `internal/kernel/decide.go` (`checkQuiescence`), `internal/kernel/state.go` (`RunStatus`)

## Context

The failure modes of an agent orchestrator that everybody specifies are the ones
that scream: an agent fails, the budget runs out, a timeout expires. They are
easy to detect because something happens.

The failure mode nobody specifies is the one that does **not** scream: the run
does not fail, does not finish, does not advance. Things simply stop happening.
Nobody is thinking, nobody can start, there is no timer armed, there is no
pending question. The system is silent.

That is the more expensive of the two. A run that fails is discovered
immediately; a silent run is discovered the next morning, after having spent the
budget on the turns that did run and having consumed the working day of the
person who was waiting for it.

## Decision

The reducer detects the silence and emits it as an **event**, `run.quiescent`,
with a required `diagnosis` field.

**It is not a terminal state.** `RunStatus` deliberately has no `quiescent`
value; `Terminal()` covers `succeeded`, `failed`, `cancelled` and `expired`, and
nothing else.

The reason is that quiescence is *recoverable by definition*: a `run prompt`
breaks it, an `inbox approve` breaks it, changing a tool policy breaks it. If it
were a terminal state, the user would have to `fork` to continue a run that only
needed a nudge, and would lose log continuity exactly when they need it most to
understand what happened.

### The detection is conservative

It is not emitted if there is **any** reason to believe something is going to
happen:

- any pending effect that will produce an event (`SpawnTurn`, `CallTool`,
  `SetTimer`, `AskHuman`, `Emit`)
- somebody busy (`anyBusy`) or somebody wakeable (`anyRunnable`)
- an armed timer (`ActiveTimer`)
- an unanswered inbox item

The bias is intentional. A false positive — telling a run "you are stuck" when it
was going to continue — destroys trust in the signal, and a signal you do not
trust is worse than no signal, because it gets ignored exactly when it is true.
`QuiescentEmitted` additionally guarantees it is emitted only once.

### The diagnosis always names the advance rule

This came out of a test that failed and was right. The first version said
"nobody is working and nobody can start", which is true and useless.

The hard case is this: the stage advances with `quorum:3` and there are only two
members who can submit. Both submitted. Nobody is missing. The rule is not met
and never will be. A diagnosis that only said "nobody can start" leaves the user
staring at a blueprint that looks fine at a glance.

So the diagnosis names the rule, and distinguishes the two cases:

```
stage review advances with quorum:3 and it is not met; missing the submit of: backend
stage review advances with quorum:3 and it is not met; everyone who could has
already submitted: the rule is unsatisfiable with this blueprint
```

The second line is a diagnosis pointing at the blueprint, not at the run. That is
the difference between "wait a bit longer" and "this will never finish, fix the
config".

## Discarded alternative

**A global inactivity timeout in the executor.** Simpler: if nothing happened in
N minutes, raise a warning. Discarded for two reasons. First, it requires the
clock, and the clock does not exist inside the reducer (ADR-0001), so detection
would live outside replay: you could not replay the diagnosis. Second, a timeout
does not know **why** it is silent; it gives you the alarm without the cause, and
the cause is the entire value.

## Consequences

- Detection is synchronous with the event that produced the silence, so `replay`
  reproduces the `run.quiescent` at the same point in the log. The diagnosis is
  reproducible, not an observation of one particular run.
- The run stays alive and keeps accepting injections after quiescence.
- A blueprint with an unsatisfiable rule is reported as such instead of
  presenting itself as a slow run.

## How it is verified

`decide_test.go` has the hard case built on purpose: `quietCfg` builds a
`quorum:3` with only two eligible members, all of them submit, and the test
requires the diagnosis to contain the advance rule and the word `unsatisfiable`.
Other tests verify it is not emitted while there is a pending effect, an armed
timer or an unanswered inbox.
