# 10. The execution model

## 10.1 A single function decides

All of iash turns around one function:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pure. It does not look at the clock, does not touch the network, does not write
anything. Everything it wants to happen in the world it **describes** as an
`Effect` and returns; somebody else runs it.

That restriction is not aesthetic. It is what makes four features be the same
feature instead of four programs that have to be kept in sync:

| feature | what it is |
|---|---|
| `iash run` | fold + real executor |
| `iash run --sim` | fold + fake executor |
| `iash run replay` | fold over an old log, with no executor |
| `iash run why` | read the `State` that came out of the fold |

In a design where the reducer calls the network, `replay` is a separate program
that reimplements the logic of the main one. It is always out of date and nobody
notices until they need it.

Purity is verified, not promised: `internal/arch_test.go` runs `go list` over the
package and fails if the kernel imports `time`, `net`, `os`, `math/rand` or
anything in that family. The error message explains why it is wrong and what to
do instead.

## 10.2 The log is the truth

```
State = fold(Decide, State0, events)
```

Snapshots are cache. If a snapshot and the log disagree, **the log wins**.

Two consequences with teeth:

**The blueprint is frozen at start.** The run stores `blueprint_sha` and the
reducer never reads the live file, but the copy in
`runs/<id>/blueprint.snapshot.yaml`. Without this, replaying a run from last week
would use today's config and give a different result — which is the same as
having no replay.

**The reducer does not assign `seq`.** The events it returns via `Emit` carry
`seq: 0`. The sequence number is assigned by the single log writer. The reducer
does not know what global order what it emits will land in, and pretending it
does is inventing a race.

## 10.3 Two effect classes

If `Decide` returns `[]Effect`, does the executor run them in order or in
parallel?

Neither simple answer works. "All in parallel" breaks correctness: `Emit` and
`SetTimer` change what the rest of the system is going to see. "All in order"
serializes three agent turns that know nothing about each other, and that is
where the reason the tool exists gets lost.

So each effect declares its class:

- **`ClassControl`** — changes the observable state of the run or the clock.
  `Emit`, `SetTimer`, `CancelTimer`, `Snapshot`. They run **in the exact order of
  the list**, one after another.
- **`ClassIndependent`** — only affects itself. `SpawnTurn`, `CallTool`,
  `AskHuman`. They can run **in parallel** with each other.

`Decide` sorts the list before returning it (`orderEffects`), so the executor
needs only one rule: run the control prefix sequentially, then the rest
concurrently.

The sort uses `sort.SliceStable`, not `sort.Slice`. The relative order among the
`Emit`s is semantic — `stage.advanced` has to go before `stage.entered` — and an
unstable sort would break it intermittently, which is the worst way to break.

## 10.4 Quiescence: the failure mode nobody specifies

The most expensive failure mode of a multi-agent system is not the crash. It is
silence: the run does not fail, does not finish, does not advance. Things stop
happening. The user finds out the next morning that they spent forty dollars on
nothing.

`Decide` checks quiescence **at the end of every step**. If nobody is busy,
nobody is wakeable, there is no armed timer, there are no unanswered questions
and no pending effect is going to generate an event, then it emits
`run.quiescent`.

Three decisions that matter:

**It is an event, not a terminal state.** Turning it into `StatusQuiescent` was
the obvious temptation and would have been the worst mistake in the design. The
event wakes the observer and the run recovers. Only if **nobody** observes it does
the run fail — and it fails carrying the diagnosis with it.

**It carries a required diagnosis.** "The run is idle" is useless to everybody.
The payload says which rule is not met:

```
stage review advances with quorum:3 and it is not met; everyone who could has
already submitted: the rule is unsatisfiable with this blueprint
```

That case — the rule asks for three submissions and only two members can submit —
is the hardest to debug by eye, because everybody "complied" and the blueprint
looks correct.

**`submitted` is not `runnable`.** The subtlety that made the first
implementation fail. A member who already submitted looks available (not
thinking, not waiting) but has nothing to do. Counting them as runnable means
quiescence is **never** detected, and the bug is invisible: the system looks
eternally healthy while being stuck forever.

## 10.5 One mechanism, three features

`run.prompt`, `agent.steered` and `agent.notified` are the same mechanism with
different provenance: text arrives for somebody.

If the addressee is busy, the text is neither lost nor does it open a parallel
turn. It accumulates in `PendingCauses` and is drained **all together** in the
next turn.

That IS `on_busy: queue`. And `queue` IS follow-up. And the draining IS
coalescing. Three features from the requirements document, one twenty-line
machine.

The saving is direct: if five events wake the same agent while it is busy,
**one** turn is opened with five causes in the context, not five turns.
Literally 5x on the invoice. The `Coalesced` field of the effect records how many
were merged, so the saving is auditable and not a marketing claim.

## 10.6 The two filters that run before spending

A watcher over `agent.*` that reacts to its own events is an infinite loop with a
credit card. Two cheap filters run **before** generating a single expensive
effect:

1. **Self-exclusion** (`include_self: false` by default). A watcher is not woken
   by its own events. This is a default, not a prohibition: there are legitimate
   patterns that need to see themselves, and `include_self: true` enables it
   explicitly.
2. **Depth limit** (`max_depth: 12`). Every derived event increments `depth`.
   Without this, a watcher reacting to what another watcher caused has no floor.

## 10.7 The budget belongs to the tree, not to the run

`State.TreeSpentUSD` accumulates the spending of the whole subtree. A child run
consumes from the parent's pool.

Without this, N levels of spawn multiply the ceiling by N and the `--budget` of
the root run is decorative. With this, `--budget 10` means ten dollars, no matter
how many levels of delegation show up.

And when it runs out: **block and ask, do not kill**. The work done up to that
point is worth real money that has already been spent. The human decides whether
to raise the ceiling or stop.

## 10.8 The defaults are security decisions

| default | why |
|---|---|
| `workspace: worktree` if anybody has `write`/`bash` | Two agents writing the same directory overwrite each other, and the KV store lock does not prevent it. The lock coordinates *intent*; real isolation comes from the filesystem. |
| `on_timeout: escalate` | A timeout almost never means "impossible", it means "something got stuck, go look". Failing by default trains the user to set absurdly long timeouts, which is worse than having none. |
| `activation: coalesce` | The alternative multiplies the invoice in exchange for nothing. |
| `include_self: false` | See §10.6. |
| undeclared tool policy → `deny` | A permissive default turns every oversight into a silent hole. |
| `--budget` with no default, mandatory | An invisible ceiling is a surprise invoice. Making the user type the number is the only way to be sure they know it exists. |

## 10.9 What the compiler guarantees and what the tests guarantee

Go has no exhaustive enums. `Effect` is a sealed interface (unexported method
`isEffect()`), so nobody from outside can add variants, but the compiler does not
require a `switch` to cover them all.

The replacement is mechanical: `allEffectVariants` registers the seven, and
`TestEffectExhaustive` fails if the count does not match — with a message that
says exactly what to do:

```
registered variants = 8, expected 7.
If you added an Effect variant, add it to allEffectVariants and review ALL the
switches over Effect (grep 'case SpawnTurn').
```

It is worse than a Rust `match`. The compensation is in the other direction:
`go list` lets us verify the import graph, and `TestKernelIsPure` turns "the
kernel is pure" into something CI checks instead of something you remember. See
ADR-0007.
