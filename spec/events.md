# Event catalogue

The log is the source of truth. This document is the contract for what may appear
in it.

## Common shape

```json
{
  "seq": 42,
  "id": "e42",
  "ts": "2026-08-26T14:03:11Z",
  "type": "agent.blocked",
  "scope": "run:r1",
  "source": "runtime",
  "actor": "backend",
  "correlation_id": "e7",
  "caused_by": ["e41"],
  "depth": 2,
  "payload": {}
}
```

| field | why |
|---|---|
| `seq` | Order within the run. Assigned by the single writer, **never** by the reducer. |
| `type` | Hierarchical namespace with dots. Watchers match by prefix (`stage.*`), so the dot is not cosmetic. |
| `source` | `human`, `agent`, `runtime`, `trigger`. `runtime` (derived) events do **not** re-trigger watchers: if they did, a watcher on `stage.*` would loop on the `stage.advanced` it caused itself. |
| `correlation_id` | Groups the whole causal chain from the root cause. |
| `caused_by` | Direct parents. Together with `correlation_id` it lets `event trace` rebuild the tree. |
| `depth` | Causal depth. It is the brake on the watcher cascade (§`max_depth`). |

## Run lifecycle

| type | payload | notes |
|---|---|---|
| `run.started` | `run_id`, `actor`, `budget_usd`, `blueprint_sha`, `parent_run_id?`, `spawn_depth?` | `blueprint_sha` freezes the config: without it, a replay would use today's config. |
| `run.prompt` | `text`, `to?` | Injects a new cause into a live run. |
| `run.paused` | — | |
| `run.unpaused` | `budget_usd?` | A **raise** of the tree ceiling, honoured by the reducer, which also clears the block and the 80% warning. Absent means "resume, ceiling unchanged" — reading a missing field as `0` would give every plain resume an unsatisfiable limit. A value at or below the current ceiling is refused by the CLI and ignored by the reducer: one under the spend re-breaches on the next cost, which is the loop a raise exists to end. This is the payload behind the budget remedy below. |
| `run.cancelled` | `reason?` | |
| `run.expired` | — | |
| `run.quiescent` | **`diagnosis`** (required), `stage` | See below. |
| `run.result` | `summary`, `result_from?` | |

### `run.quiescent`

It is not a terminal state, it is a notice. `diagnosis` is **required**: an event
that only says "the run is idle" is useless to everybody. It has to name the
concrete cause — the advance rule that is not met, or who is waiting for what.

If nobody observes the event, the run fails carrying the diagnosis in `result`.

## Stages

| type | payload |
|---|---|
| `stage.entered` | `stage`, `index` |
| `stage.submitted` | — (the actor is whoever submitted) |
| `stage.advanced` | `from`, `to`, `to_index` |
| `stage.timeout` | `stage` |

`stage.advanced` **always** precedes the corresponding `stage.entered`. The order
between them is semantic, and that is why `orderEffects` uses a stable sort.

## Agents

| type | payload |
|---|---|
| `agent.activated` | — |
| `agent.steered` | `text`, `to?` |
| `agent.notified` | `text`, `to?` |
| `agent.turn_done` | — |
| `agent.blocked` | `blocked_on`, **`blocked_ref`** |
| `agent.unblocked` | — |
| `agent.failed` | `error` |

### The `blocked_ref` rule

**Every `agent.blocked` must bring `blocked_ref`: an object with the data needed
to unblock it.**

This is the rule that keeps `run why` free of hard-wired cases. Instead of a list
of `if`s for every known situation, `why` walks the reference and builds the
concrete command:

| `blocked_on` | `blocked_ref` | derived remedy |
|---|---|---|
| `approval` | `{inbox_id, tool, policy}` | `arxi inbox approve <inbox_id>` |
| `lock` | `{key, holder}` | `arxi state unlock <key>` |
| `peer` | `{peer}` | (informational: chained wait) |
| `budget` | `{}` | `arxi run unpause <run> --budget <higher>` |
| `timer` | `{timer_id}` | (informational) |
| `tool` | `{tool}` | (informational) |
| `workspace` | `{path}` | `arxi run show <run> --workspace` |

When a new blocking reason appears, it brings its reference and `why` shows it
with no code changes. If somebody emits a block with no reference, `why` reports
it explicitly instead of showing an empty line:

> blocked without a structured reference: this is a schema violation, every
> waiting:* must bring blocked_ref (see spec/events.md)

## Tools and model

| type | payload |
|---|---|
| `tool.call` | `tool`, `args?` |
| `tool.call_completed` | `tool`, `result?` |
| `tool.call_denied` | `tool`, `policy` |
| `llm.response` | `cost_usd`, `tokens_in?`, `tokens_out?`, `model?` |

`tool.call_denied` with `policy: "ask"` is **not an error**: it is a question. It
creates an inbox item and leaves `blocked_ref` so the remedy is automatic.

## Resources

| type | payload |
|---|---|
| `lock.acquired` | `key`, `expires_at?` |
| `lock.released` | `key`, `previous_holder?`, `reason?`, `expired_at?` |
| `resource.conflict` | `path`, `agents?` |

`resource.conflict` does not fail the run. It wakes whoever observes it; if
nobody observes, it stays recorded and quiescence detects it later. Failing here
would let a trivial merge conflict kill half an hour of work.

A lock is held by `actor`, or by `source` when no actor signed for it — the way
`arxi state lock` leaves it, since naming a member there would disable that
member's own watcher on `lock.*`. One key has one holder: a second `lock.acquired`
from somebody else is **dropped**, and one from the holder is a **renewal** that
moves `expires_at` in place rather than a release followed by a re-take. A
renewal is how a member whose turn outlasts its lease keeps the key, and a
handover written into the log would read as that member losing it.

`expires_at` is an **absolute** RFC3339 instant, never a duration, and the
reducer only carries it. Judging a lease lapsed needs a `now`, so it is a
reading rather than a fold: a `Decide` that dropped an expired lock would make
the same log answer differently in the morning and in the afternoon, and
reproducible replay is the property the rest of this spec depends on. The field
is optional, and **absent is not expired** — it means held until the run ends.

So the reader that has a clock **records its judgement**. `arxi state lock`
steals a lapsed key by appending a `lock.released` carrying `previous_holder`,
`reason: "expired"` and `expired_at`, immediately followed by the
`lock.acquired`, in one batch. The next fold reproduces the steal without a
clock, and the log says who decided the lease was dead and on what evidence.
That release is `SourceRuntime` and is honoured **whatever** it comes from: a
release accepted only from its own holder could never reclaim a key whose holder
crashed mid-turn, and the only way around it would be for a shell to write an
event claiming to be that agent.

An acquire with no `key` is dropped rather than stored, on the same ground as
`state.set`: a row keyed on nothing is one no release can name, and the next
such event would add another.

## Shared state

| type | payload |
|---|---|
| `state.set` | `key`, `value` |

The run's key/value store is folded from the log like everything else, and that
is the whole reason it is an event. A KV file living beside the log would make
`state = fold(decide, state0, events)` false: the fold would rebuild every
member, lock and inbox item from August and then read **today's** value for a
key an agent set last Tuesday, so a replay would not be a replay.

The last write wins and the state keeps no history of a key. That is not a loss
of information — `arxi event log <run> --type state.set` **is** the history, and
a second copy of it inside the state is a copy that can disagree with the log.

There is deliberately no `state.get`: a read changes nothing, so an event for it
would be a row nothing can fold. There is no delete either — no verb declares
one, and a key that vanished from the fold could not be told from a key nobody
ever set.

`state.set` is **not** runtime-derived, so watchers fire on it. That is the
point: a blueprint declaring `watchers: [{agent: backend, pattern: state.*}]`
gets a turn when the contract it was waiting for lands, instead of somebody
paying for a turn to say so.

## Budget

| type | payload |
|---|---|
| `budget.warning` | `tree_spent_usd`, `budget_usd`, `pct` |
| `budget.exceeded` | `tree_spent_usd`, `budget_usd` |

Both report the spending of the **tree**, not of the run: with nested spawn, the
spending of one run alone is a misleading fraction.

`budget.warning` is emitted **once** (marked in `State.BudgetWarned`). A notice
that repeats on every call is a notice the user learns to ignore.

## Human in the loop

| type | payload |
|---|---|
| `inbox.created` | `inbox_id`, `kind`, `question`, `agent?`, `on_timeout` |
| `inbox.replied` | `inbox_id`, `text` |
| `inbox.timeout` | `inbox_id` |

`on_timeout` is decided **when the question is created**, not when it expires: at
the moment of the timeout there is nobody watching anymore.

## Clock

| type | payload |
|---|---|
| `timer.tick` | `timer_id` |

Timers are armed with relative offsets in milliseconds, not absolute timestamps.
That is what lets the virtual clock of `--sim` run the same fold without waiting
half an hour of real time.

## User events

`custom.*` is reserved for events emitted by agents via `arxi event emit`. Agents
can **only** emit in that namespace: if they could emit `stage.advanced`, they
could skip the advance rule of their own blueprint.
