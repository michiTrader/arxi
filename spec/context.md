# Canonical transcript and prepared context

This specification implements ADR-0013. It defines provider-neutral evidence
recorded before a model call. It does not define compaction or cross-run memory.

## Schemas

- `arxi.transcript/v1` is a deterministic projection of a confirmed run prefix.
- `arxi.prepared-context/v1` is the exact presentation selected for one model
  child.
- `arxi.context-prepare/v1` identifies one durable preparation request.

Unknown schemas fail closed. Historical runs without these records retain their
historical replay meaning and are classified as legacy; they do not acquire a
prepared-context proof retroactively.

## Canonical bytes and digests

Artifacts are JSON encoded from the versioned structures defined here. Arrays
retain semantic order. Object keys use the canonical encoder's stable ordering,
JSON numbers are not coerced through floating point, and insignificant whitespace
is absent.

Every digest is lowercase hexadecimal SHA-256. Composed identities use the named
domain followed by fixed-order fields encoded as an eight-byte big-endian length
and exact bytes. The domains are:

- `arxi.transcript-id/v1`;
- `arxi.transcript-content/v1`;
- `arxi.context-id/v1`;
- `arxi.context-content/v1`;
- `arxi.context-presentation/v1`.

Exact bytes remain the evidence. A digest is an integrity binding and index, not
a replacement for content.

## Transcript artifact

A transcript contains:

- schema, run ID, subject agent and projector version;
- `source_from_seq`, `source_through_seq` and the event ID at the upper boundary;
- the frozen effective-config schema and digest;
- ordered items;
- content digest.

Every item contains a deterministic item ID, kind, actor, audience, source
sequence, source event ID, within-source index, canonical content blocks and any
kind-specific identity. Kinds are `user_input`, `model_output`, `tool_call`,
`tool_result`, `human_decision` and `artifact_reference`.

Ordering is ascending source sequence and then within-source index. Projectors
must reject duplicate sequence positions or contradictory exact native and domain
records rather than reconciling them heuristically.

## Projection rules

Only confirmed events at or below `source_through_seq`, and immutable artifacts
bound by such events, may contribute.

User input includes the opening prompt bound by `run.started` to the frozen
effective-config artifact and later `run.prompt`, `agent.steered` and
`agent.notified` events. Target and provenance are retained. A current CLI flag or
blueprint file is never a source.

Native model output comes from completed model-child `result_json`, preserving
content blocks, response ID, finish reason, refusal and usage. `llm.response` is
the final domain and cost projection and a consistency check; it does not replace
intermediate native rounds. A historical text-only `llm.response` becomes an
explicit legacy item.

Tool calls preserve provider-issued call IDs, canonical argument bytes and
argument digests. Results preserve the exact committed text and call order. A
projector never manufactures a call ID for a legacy direct-tool event.

Human decisions preserve the authenticated principal, pending-item or action
identity and decision. Asked, granted, consumed and externally completed are
distinct facts. Artifact references preserve immutable version or content digest,
media metadata and provenance. A live path alone is not an artifact reference and
is never dereferenced during replay.

Visibility is deterministic for the frozen subject agent. It includes that
agent's own model/tool history, broadcast inputs, inputs addressed to the agent,
and exact causes commissioned for its turn. Current policy or membership cannot
change historical visibility.

## Prepared-context artifact

A prepared context contains:

- schema, context ID, run ID, parent work ID and subject agent;
- transcript artifact bytes and digest;
- source boundaries and effective-config schema/digest;
- context-policy, projector and preparer versions;
- provider, protocol, model and non-secret route version;
- tool-schema version;
- exact ordered provider-neutral messages;
- token measurement and configured limits;
- ordered memory-use receipts;
- overflow decision;
- content digest and presentation digest.

The stable layer order is identity, situation, frozen memory, shared context,
prior transcript, then current causes and input. Transcript content remains
role/content-block structured and is not flattened into prose. Provider adapters
translate this presentation at the edge but cannot select, reorder or reconstruct
it.

The content digest binds provider-neutral semantic material and source identity.
The presentation digest binds exact canonical message bytes, including order and
framing. The existing model-child request digest binds the complete
`arxi.turn/v1` request including route, tools and generation options. These three
digests are not interchangeable.

## Measurements, memory and overflow

A token measurement records implementation and version, target model, tokens per
layer and total, input and output limits when known, and whether the result is
`exact` or `estimate`. Unknown values remain absent. A byte or rune estimate must
not be labelled exact.

Phase 5 records frozen `ContextSpec.Memory` as a receipt bound to its field identity
and effective-config digest. The receipt list is empty when no memory contributes.
Semantic retrieval, ranking, autonomous memory and cross-run scope belong to Phase
7 and are not performed here.

Phase 5 performs no lossy selection. If a known limit is exceeded, preparation
records a visible failure. It does not honor `on_overflow: summarize` until Phase
6 supplies a versioned compaction artifact, and it never truncates silently.

## Durable events

`context.prepare_requested` records schema, context ID, parent work ID, subject
agent, source boundaries, effective-config digest and all projector/preparer policy
versions. It freezes the inputs a retry may use.

`context.prepared` repeats the identity and boundaries and carries the exact
transcript and prepared-context JSON, their artifact/content/presentation digests,
model and policy versions, measurement and receipt digests. The event commits only
a fully verifiable artifact.

`context.prepare_failed` repeats the immutable identity and records a stable
failure class and message. It is terminal for that preparation request and is not
evidence that a model call occurred.

All three event types are operational, reducer-inert and watcher-inert. They do
not create a source step, wake an agent or participate in quiescence.

## Recovery and replay

- Before `context.prepare_requested`, normal effect recovery may commission
  preparation.
- A request without a terminal event may retry only with its recorded prefix and
  versions.
- If exact artifact bytes exist without `context.prepared`, they are unconfirmed
  data and may be adopted only after byte equality is proven.
- A valid `context.prepared` is loaded and verified; projector, memory, tokenizer
  and preparer are not called again.
- Missing bytes, digest mismatch, unknown version or conflicting values for one
  context ID is corruption and fails before model dispatch.
- A prepared but unstarted model child dispatches its exact stored request. A
  started child follows receipt or explicit `unknown` semantics and is never made
  retryable by reconstructing context.
- Replay validates and displays recorded artifacts without rebuilding them.

## Invariants

1. Only confirmed evidence influences presentation.
2. Artifact bytes are durable before their prepared event is authoritative.
3. One context identity has one source boundary, version set and exact value.
4. No model child starts without a matching verified presentation.
5. Context selection cannot expand tool, workspace, artifact or memory authority.
6. Credentials and provider-native objects never enter these artifacts.
7. The canonical transcript remains intact; Phase 5 performs no compaction.
8. Public `host/v1` remains source-compatible unless a separate versioned contract
   is introduced.