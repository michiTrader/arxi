# ADR-0009: Native model/tool loops use one durable provider-neutral turn contract

- Status: accepted
- Affects: `internal/turn`, `internal/exec/turn.go`, `internal/provider`, `internal/runconfig`, `cmd/arxi/runtime.go`, `spec/events.md`

## Context

OpenAI Chat Completions and Anthropic Messages describe the same model/tool
conversation with different wire objects. Letting either vocabulary enter the
kernel would make replay and policy depend on the selected provider. Treating a
tool request as empty assistant text is worse: the run advances after work that
never happened.

A native loop also crosses crash boundaries. The model request may have reached a
remote service; a tool may have changed the workspace; or the result may have
been committed before the next model request. Process-local transcript state
cannot distinguish these cases after restart, and blindly retrying started work
can duplicate paid or mutating effects.

## Decision

**One internal canonical contract describes native turns, and `internal/exec`
durably coordinates every model and tool child.** Provider adapters translate at
the edge. `internal/kernel` and the public `host/v1.TextProvider` remain free of
provider wire types and are not replaced by this contract.

### Canonical identity

The schema is `arxi.turn/v1`. It defines roles, content blocks, tool definitions,
tool calls and results, refusals, finish reasons, usage, streaming events and
cancellation outcomes. Streaming is canonical vocabulary for future adapters;
the current OpenAI and Anthropic transports reject streaming before dispatch
rather than feeding an SSE response into their non-streaming decoders. Requests
freeze the non-secret route needed to repeat the same dispatch: provider,
protocol, base URL, model and API-key environment variable name. Credential
values are resolved only at dispatch and are never persisted.

A provider owns its tool-call ID. Arxi preserves that string exactly through
canonical decoding, durable child records, tool execution, domain-event
projection and result reinjection. A result whose `call_id` differs from its call
is refused. Reusing one ID for a different tool name or argument digest is also
refused.

Tool arguments are exactly one JSON object. Decoding uses JSON numbers without
float coercion; re-encoding produces compact canonical JSON with stable object
key ordering. SHA-256 of those bytes is stored as `argument_digest`.
Noncanonical bytes, arrays, scalars, `null`, trailing values and digest mutation
are refused before a runner is called.

Multiple calls keep their provider order. Their result blocks are reinjected in
that same order and with their exact committed text. No adapter may synthesize a
new call ID, reorder results or reconstruct a result from projected domain
events.

### Durable loop boundary

A top-level `SpawnTurn` remains one durable work item. Each model round and tool
call is a child identified by its parent, kind, deterministic slot and prepared
request bytes. Before dispatch, `exec.work_prepared` stores `request_json`; then
`exec.work_started` marks the external boundary; finally `exec.work_finished`
stores the exact `result_json` and terminal status.

Recovery reconstructs the transcript only from committed child outcomes. The
started top-level turn marker performs no provider or tool work, so a restart
before its first child is prepared resumes safely at child preparation:

- prepared but unstarted children may dispatch;
- completed children are decoded and reused, so committed tools never rerun;
- started children without a terminal outcome become `ErrUnknownWork` and are not
  guessed or automatically redispatched;
- a committed model or tool result is the only value eligible for later
  processing or reinjection.

The loop is bounded at 64 model rounds. Cancellation is represented canonically
as `canceled`; context cancellation during an ambiguous dispatch follows the
same unknown-outcome rule as other transport failures.

### Policy, projection and protocols

Policy is resolved before invoking the tool runner. `allow` executes and records
the exact result. `ask` and `deny` never reach the runner; both record a denial,
and `ask` projects the existing inbox/waiting behavior. A stopped policy outcome
ends the native loop without pretending that a tool ran.

Adapters currently implement `openai-chat-completions/v1` and
`anthropic-messages/v1`. Both map native text, tool calls/results, refusals,
finish reasons and usage to the canonical contract. Provider usage is summed
across model rounds for the final `llm.response`; canonical cache-read and
cache-write counts are retained even though current cost projection prices only
input and output tokens.

OpenAI legacy `function_call` is rejected because it has no provider call ID that
can satisfy the recovery contract. Unsupported content blocks are rejected
explicitly rather than flattened or silently discarded.

The canonical package is internal. `host/v1.TextProvider` remains source- and
behavior-compatible for text-only embedding. If external native-turn providers
are needed, they require a separate versioned public DTO/interface family rather
than exposing `internal/turn` or destructively changing v1.

Simulation semantics are frozen with the run configuration. Version 1 keeps the
legacy direct fake path; version 2 opts the production fake into the canonical
durable loop. New simulation artifacts use version 2, and unknown versions are
refused.

## Discarded alternatives

**Keep provider-native transcript objects in the executor or kernel.** Rejected:
it makes durable replay provider-specific and leaks wire-version changes into the
domain model.

**Resume from projected `tool.call_completed` events.** Rejected: projections are
for domain observation and do not retain the complete canonical child outcome.
They cannot prove the exact bytes sent to the next provider call.

**Retry every started child after restart.** Rejected: a missing terminal append
does not prove the external effect failed. Retrying can duplicate cost or mutate
the workspace twice.

**Change `host/v1.TextProvider` to canonical turns.** Rejected: it would break the
published text-only embedding surface. New capability belongs in a new version.

## Consequences

- Provider adapters can differ at the wire while producing equivalent canonical
  traces and domain events.
- Native tool loops survive restart without rerunning committed tools.
- Exact request/result JSON increases execution-log volume; that is the price of
  an auditable continuation boundary.
- Image, document and provider-specific thinking blocks remain canonical
  vocabulary but are rejected by adapters that do not implement them.
- Leasing, fencing, receipts, broad retries and argument-bound authorization
  remain later phases; this ADR does not claim those guarantees.

## How it is verified

`internal/turn/canonical_test.go` verifies object canonicalization and digest
binding. `internal/exec/turn_test.go` covers exact reinjection, multiple-call
ordering and prepared/started/completed crash boundaries. Production-fake tests
in `internal/exec/exec_test.go` prove deterministic canonical execution and
policy stops. `internal/provider/native_test.go` runs native OpenAI and Anthropic
loops and compares their projected events. `internal/runconfig/config_test.go`
and `cmd/arxi/model_cli_test.go` freeze protocol and simulation-version
compatibility. `internal/arch_test.go` keeps provider wire types out of the
kernel and execution coordinator.
