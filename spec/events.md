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
| `run.started` | `run_id`, `actor`, `budget_usd`, `blueprint_sha`, `effective_config_schema`, `effective_config_path`, `effective_config_sha`, `parent_run_id?`, `spawn_depth?` | The blueprint and effective-config bindings freeze the complete accepted input. Historical events without effective-config fields remain inspectable but cannot resume through the modern runtime. |
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
| `approval` | Legacy: `{inbox_id, tool, policy}`. Exact authorization: `{inbox_id, authorization_id, action_digest, tool, policy}`. | `arxi inbox approve <inbox_id>` |
| `lock` | `{key, holder}` | `arxi state unlock <run> <key>` |
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
| `tool.call` | `tool`, `call_id`, `args?` |
| `tool.call_completed` | `tool`, `call_id`, `result?` |
| `tool.call_denied` | `tool`, `call_id`, `policy` |
| `llm.response` | `cost_usd`, `tokens_in?`, `tokens_out?`, `model?`, `ok?`, `error?`, `status?`, `code?`, `retryable?`, `response_id?`, `finish_reason?`, `text?`, `coalesced?` |

A provider-native tool request enters the durable canonical turn loop. Each call
emits `tool.call` with the provider-issued `call_id` and canonical `args`. An
allowed call then emits `tool.call_completed` with the same `call_id` and its
exact text result; that exact result is reinjected under the same ID before the
next model request. Calls from one response and their results preserve provider
order. The final `llm.response` aggregates input and output tokens across every
model round and carries the final response ID, finish reason, refusal and text.

Policy is resolved before the tool runner. `tool.call_denied` with
`policy: "ask"` is **not an error**: it is a question. Exact authorization does
not ask the model to recreate that call. The runtime persists the canonical
continuation and `authorization.requested`, then leaves an authorization-bound
`blocked_ref`. `deny` is recorded without running the tool. Legacy logs keep the
old stop-and-reprompt meaning, but an unanswered legacy mutation cannot be
upgraded into an exact grant.

## Exact authorization

Authorization records are reducer-visible domain facts. The suspended canonical
continuation they reference is reducer-inert execution data: replay can decide
that an authorization may resume without reading provider or tool state.

| type | payload | notes |
|---|---|---|
| `authorization.requested` | `schema`, `authorization_id`, `inbox_id`, `requester_principal`, `suspension_id`, `parent_work_id`, `provider_call_id`, `tool`, `argument_digest`, `action_digest`, `tool_schema_version`, `policy_version`, `workspace_profile_id`, `expires_at`, `after_ms` | Every binding is immutable. The matching continuation and exact canonical call must already be durable. `after_ms` arms the authorization timer; `expires_at` is the recorded absolute judgment used by mutation adapters. |
| `authorization.granted` | `schema`, `authorization_id`, `action_digest`, `approver_principal`, `grant_event_id`, `expires_at` | The approver is authenticated by the adapter and must differ from the requester. A grant does not run or consume the action. |
| `authorization.denied` | `schema`, `authorization_id`, `action_digest`, `principal`, `reason?` | Terminal decision. It never resumes the suspended call. |
| `authorization.expired` | `schema`, `authorization_id`, `action_digest`, `expired_at` | A clock-owning runtime records this judgment; the reducer never compares wall-clock time. |
| `authorization.consumed` | `schema`, `authorization_id`, `action_digest`, `grant_event_id`, `work_id` | Appended in one writer compare-and-swap batch with the matching `exec.work_started`, immediately before external dispatch. There is at most one consumption. |

`schema` is `arxi.authorization/v1`. An action digest is computed over job and
run identity, requester principal, suspended parent work, provider call ID, tool
name, canonical argument digest, tool-schema version, frozen policy version and
workspace-profile identity. The digest uses a domain-separated, length-framed
encoding. It is an integrity binding; the exact persisted bytes remain the
evidence of what was authorized.

Approval and rejection are valid only for an unanswered `tool_approval` item;
answer is valid only for a question. The reply append records the authenticated
principal. Empty principals, self-approval, mismatched authorization IDs or
digests, changed bindings, elapsed grants and a second consumption fail closed.

`authorization.granted` causes a resume effect for the recorded suspension, not a
fresh model turn. Immediately before dispatch the fenced worker re-folds the
confirmed prefix and verifies the grant and continuation. It atomically appends
`authorization.consumed` and the exact child's `exec.work_started`; only then may
the runner see the call. The consume/start pair is therefore the durable point of
no return, not evidence that the external action completed. A crash after that
boundary is handled as started work: a trustworthy receipt may reconcile it,
otherwise a non-idempotent outcome is `unknown`. Recovery never restores the grant
or guesses that the external action did not happen.

Authorization expiry uses timer id `authorization:<authorization_id>`. A grant
remains subject to the timer until consumption. `timer.tick` records
`authorization.expired`; replay uses that event and never consults the current
clock. A mutation adapter with a wall clock must materialize an already-due
expiry before accepting or consuming a grant, so worker downtime cannot extend
its authority.

Historical events remain valid. A legacy `tool.call_denied` and
`inbox.replied` pair replays with its historical behavior. Live recovery refuses
to execute an unanswered legacy mutating approval because it lacks immutable
action, schema, policy and principal bindings.

## Context preparation

The canonical transcript and prepared-context schemas, digest domains and
recovery rules are defined by [`context.md`](context.md).

| type | payload | notes |
|---|---|---|
| `context.prepare_requested` | `schema`, `context_id`, `parent_work_id`, `agent`, `source_from_seq`, `source_through_seq`, `source_through_event_id`, `effective_config_schema`, `effective_config_sha`, `projector_version`, `preparer_version`, `policy_version` | Freezes the confirmed prefix and versions from which one presentation may be prepared. |
| `context.prepared` | All request bindings plus `transcript_schema`, `transcript_json`, `transcript_digest`, `prepared_context_schema`, `prepared_context_json`, `prepared_context_digest`, `content_digest`, `presentation_digest`, `model_version`, `token_measurement`, `memory_receipt_digests` | Exact bytes and digests commit before any model child may start. |
| `context.prepare_failed` | Request bindings plus `failure_class`, `error` | Terminal preparation failure; no model call is implied. |

These records are reducer- and watcher-inert execution metadata. A valid
`context.prepared` is reused by recovery and replay; it is never rebuilt from
current data.

## Durable execution progress

Execution metadata records what the runtime did with the effects decided from a
domain event. These events are reducer- and watcher-inert: they reconstruct safe
continuation boundaries, but do not themselves cause more effects.

| type | payload | notes |
|---|---|---|
| `exec.work_prepared` | Top-level: `work_id`, `source_seq`, `source_event_id`, `effect_index`, `effect_kind`, `effect_class`, `effect_digest`; external top-level work also carries `work_class`, `dispatch_key`, `request_digest`, `provider`. Native child: `work_id`, `parent_work_id`, `work_scope: "turn_child"`, `source_seq`, `child_kind`, `child_slot`, `request_json`, `work_class`, `dispatch_key`, `request_digest`, `provider`; model children prepared under Phase 5 also carry `context_id` and `presentation_digest`. | The full top-level manifest is committed before external dispatch. Child request JSON is the exact provider-neutral model request or tool call that may dispatch. A model child's context bindings must match a verified `context.prepared`. `dispatch_key` derives from stable job/work/request identity and is independent of attempts. `work_class: "idempotent"` is valid only when the concrete adapter declares and actually honors that key; generic tools and production OpenAI/Anthropic adapters remain `non_idempotent`. |
| `exec.work_started` | `work_id`; native children also carry `parent_work_id`, `work_scope: "turn_child"` | Durable boundary immediately before an independent external dispatch. A native parent marker starts coordination; ambiguity is tracked by its model and tool children. |
| `exec.work_finished` | `work_id`, `status`, `error?`; native children also carry `parent_work_id`, `work_scope: "turn_child"`, `result_json?` | `status` is exactly `completed`, `failed`, or `unknown`. A completed native child stores the exact canonical outcome used by recovery. |
| `exec.step_completed` | `source_seq`, `source_event_id`, `work_ids` | Commits that every effect of the source event has a durable terminal outcome. `work_ids` preserves effect-list order. |

A prepared work item may be dispatched after restart. Coordinated workers first
register its provider/work/request binding under the active fence, then commit the
started boundary. A started top-level native turn with no child may resume because
its parent marker performs no external work. Completed local outcomes always win.
Otherwise, a committed trustworthy receipt may supply the exact canonical outcome;
its digest and provider/work/request binding are verified before that outcome is
committed locally. A started idempotent item may retry only when its concrete adapter
both declares and honors external key semantics, and it reuses the prepared key.
Any other started item without a terminal record is materialized as `unknown` and is
never automatically redispatched. A durable `unknown` blocks continuation until
trustworthy reconciliation establishes the external outcome. Finished work is not
repeated, even when the process stopped before `exec.step_completed`; recovery
closes the source step after validating its deterministic manifest.

The resume cursor is the greatest contiguous source-event frontier proven by
`exec.step_completed`. The physical log head is not a cursor: domain outcomes and
execution metadata can exist above an unfinished source event.

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
is optional, and **absent is not expired** — no clock ever reclaims such a key,
so it stands until somebody hands it back by name with `arxi state unlock`. A
caller who says nothing therefore gets a lease, not eternity: the reclaim that
needs nobody's attention.

So the reader that has a clock **records its judgement**. `arxi state lock`
steals a lapsed key by appending a `lock.released` carrying `previous_holder`,
`reason: "expired"` and `expired_at`, immediately followed by the
`lock.acquired`, in one batch. The next fold reproduces the steal without a
clock, and the log says who decided the lease was dead and on what evidence.
That steal is `SourceRuntime` because the `lock.acquired` batched behind it
carries the wake, and a `lock.*` watcher fired twice bills two turns for one
handover.

A release is honoured **whatever** it comes from, and the reducer does not check
the holder: a release accepted only from its own holder could never reclaim a key
whose holder crashed mid-turn, and the only way around it would be for a shell to
write an event claiming to be that agent. So who MAY release is the writer's
judgement, and `reason` is where that judgement is recorded:

| `reason` | what the writer decided |
|---|---|
| `released` | the holder handed the key back — no ceremony |
| `expired` | the lease had run out, and `expired_at` is the evidence |
| `forced` | the lease had **not** run out and was ended anyway |

`arxi state unlock <run> <key>` writes the first two with no flag, and `forced`
needs `--force`, since it is the only one that ends work in flight. Its release
is `SourceHuman`, unlike the steal above, because here the release IS the news:
`wakeWatchers` is skipped outright for `SourceRuntime`, so a runtime hand-back
would leave a member watching `lock.*` waiting for a key that is already free.

Freeing a key does **not** unblock the member waiting on it. The `lock.released`
arm removes the row and stops there, and nothing in the tree emits
`agent.unblocked` — so a lock-blocked member is moved by the turn a `lock.*`
watcher opens, or not at all.

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
| `inbox.created` | `inbox_id`, `kind`, `question`, `agent?`, `on_timeout`, `authorization_id?`, `action_digest?` |
| `inbox.replied` | `inbox_id`, `decision`, `text?`, `principal?`, `authorization_id?`, `action_digest?` |
| `inbox.timeout` | `inbox_id` |

`on_timeout` is decided **when the question is created**, not when it expires: at
the moment of the timeout there is nobody watching anymore.

## Clock

| type | payload | notes |
|---|---|---|
| `timer.scheduled` | `timer_id`, `after_ms`, `deadline_ms` | Durable arming record. `deadline_ms` is absolute on the run's clock timeline. |
| `timer.cancelled` | `timer_id` | Durable disarm; a cancelled timer is never restored. |
| `timer.fired` | `timer_id`, `fired_at_ms` | Operational firing record, appended atomically with its `timer.tick`. |
| `timer.tick` | `timer_id` | Reducer-facing delivery of the elapsed timer. |

`SetTimer` still receives a relative offset in milliseconds. The runtime records
its exact absolute deadline so restart does not recompute it from a later
instant. Live deadlines and firing instants are Unix milliseconds; simulated
ones are logical milliseconds beginning at zero, and downtime never advances
that logical clock.

The first three records are operational and reducer/watcher-inert. On restart,
the clock restores schedules minus cancellations and firings (a legacy
`timer.tick` also counts as fired). A live deadline that elapsed during downtime
is delivered immediately; a future one preserves its original deadline.
`timer.fired` and `timer.tick` share one confirmed append, so a crash cannot
record consumption without delivery or deliver a confirmed timer twice.

## User events

`custom.*` is reserved for events emitted by agents via `arxi event emit`. Agents
can **only** emit in that namespace: if they could emit `stage.advanced`, they
could skip the advance rule of their own blueprint.
