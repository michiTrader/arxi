# Canonical transcript and prepared context

This specification implements ADR-0013 and ADR-0014. It defines provider-neutral
evidence recorded before a model call, the budgets and pressure measurement that
govern it, and the compaction artifact produced when a known limit is exceeded.
It does not define cross-run memory.

## Schemas

- `arxi.transcript/v1` is a deterministic projection of a confirmed run prefix.
- `arxi.prepared-context/v1` is the exact presentation selected for one model
  child.
- `arxi.context-prepare/v1` identifies one durable preparation request.
- `arxi.context-budget/v1` is the versioned layer-budget policy derived from a
  known input limit.
- `arxi.compaction/v1` is the verified lossy compaction produced when measured
  pressure exceeds a known limit under `on_overflow: summarize`.

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
- `arxi.context-presentation/v1`;
- `arxi.compaction-content/v1`.

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

Kind-specific identity is load-bearing for `model_output`: an item projected
from a native model child records the child work ID and the provider response
ID. Compaction ranges and omission ledgers bind to those durable records, so a
summary can always be audited against the execution that produced its sources.
Historical items without the binding replay unchanged.

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
argument digests. Producers of `tool.call` must record the digest the canonical
call already carries; a projector cannot preserve an identity the event never
wrote. Results preserve the exact committed text and call order. A
projector never manufactures a call ID for a legacy direct-tool event.

Human decisions preserve the authenticated principal, pending-item or action
identity, the decision verb and the exact answer text when one was given. The
verb alone is not the decision: an answer's substance is its text, and the
activation cause the reducer computes is an event ID, so dropping the text
would resume the member that asked without the answer it waited for. Asked,
granted, consumed and externally completed are distinct facts. Artifact references preserve immutable version or content digest,
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

The route records the non-secret destination: provider, protocol, model, base
URL, tool-schema version and context-policy version. It joins the content
digest, because the same messages sent to a different model under a different
tool schema are a different presentation. Before a committed presentation is
reused, its recorded provider, protocol and model must equal the route the turn
now resolves to; every other digest still verifies when only the destination
changed, so this comparison is the only thing standing between a frozen
presentation and a model it was never commissioned for.

The overflow decision records whether measured pressure exceeded a known input
limit, the mode that governed the outcome, and — when compaction ran — the
compaction artifact's identity and content digest. A prepared context over a
known limit without either a verified compaction artifact or a terminal
preparation failure must not exist.

## Measurements, memory and overflow

A token measurement records implementation and version, target model, tokens per
layer and total, input and output limits when known, and whether the result is
`exact` or `estimate`. Unknown values remain absent. A byte or rune estimate must
not be labelled exact.

Phase 5 records frozen `ContextSpec.Memory` as a receipt bound to its field identity
and effective-config digest. The receipt list is empty when no memory contributes.
Semantic retrieval, ranking, autonomous memory and cross-run scope belong to Phase
7 and are not performed here.

## Pressure, budgets and compaction

Layer budgets are the versioned policy `arxi.context-budget/v1`. When the input
limit is known, budgets are derived from it in fixed quarters — static 1/4,
summary 1/8, verbatim 1/2, input 1/8 — by integer division, with the remainder
left as headroom. The derivation is deterministic, and the budget values that
governed a preparation are recorded inside the artifacts that obeyed them. When
the input limit is unknown, pressure is unknown: no budget is derived, no
compaction is triggered, and no limit is invented.

Pressure is the measured total against the known input limit. A single layer
over its budget does not trigger anything by itself; the budgets are allocation
targets the compactor must respect once total pressure exists.

When pressure exceeds a known limit:

- `on_overflow: summarize` produces one compaction artifact under
  `arxi.compaction/v1`, embedded in the prepared context and committed in the
  same `context.prepared` batch.
- any other declared value fails preparation terminally through
  `context.prepare_failed`. Unknown modes never fall back to summarize.
- compaction that cannot bring the measured total within the limit — for
  example a static layer that alone exceeds it — fails visibly. Shrinking the
  window without an artifact is silent truncation and is forbidden.

## Compaction artifact

`arxi.compaction/v1` contains:

- schema, context ID, run ID, subject and source boundary;
- generator identity and version, and the budget policy values used;
- an ordered lossy summary whose every claim cites the transcript item IDs it
  was extracted from;
- the recent verbatim window as an ordered list of transcript item IDs;
- source ranges for the summarized material;
- retained critical items kept verbatim outside the window only where pair or
  anchor integrity requires it;
- an omission ledger recording, by item ID, kind and content digest, every
  item presented neither verbatim nor through a claim;
- before and after token counts under the same measurement identity.

The summary is extractive. A claim's text must be contained in the concatenated
text of exactly the items it cites; a claim that cannot be proven against its
citation fails closed before the artifact may exist. A claim shortened by the
summary budget is labelled incomplete rather than silently cut. Generated prose
that cannot meet this gate does not commit, whatever produced it.

Continuity anchors are user inputs, human decisions and the final model output
of each completed turn (the model output immediately preceding the next user
input, or the last item). Every anchor is inside the verbatim window or cited
by at least one claim; none may appear only in the omission ledger.

The verbatim window is a suffix of the item order and never splits a tool call
from its result. The presented conversation is therefore always provider-valid:
a tool result message never appears without its preceding call.

Compaction never modifies the transcript. The prepared context continues to
embed the full canonical transcript and its digest; only the presentation loses
material, and the omission ledger accounts for everything lost by identity.
The verifier is deterministic and needs only the committed artifact and the
transcript items, so audit and replay tooling can re-run it at any time;
between verifications the digest chain protects the bytes.

## Compaction failure

Generator errors, verification failures and budgets that cannot be satisfied
are preparation failures: terminal, classed `compaction`, and visible in the
event history. A compaction failure never produces a partial presentation, and
a model child never starts from an unverified summary.

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
- A valid `context.prepared` is loaded and verified; projector, memory, tokenizer,
  preparer and compaction generator are not called again. A committed compaction
  artifact is reused byte-for-byte with the rest of the preparation: recovery
  re-proves its digest and its agreement with the overflow decision, and the
  deterministic verifier remains available to audit and replay tooling at any
  time.
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
7. The canonical transcript remains intact: compaction changes only the
   presentation, and every transcript item is accounted for — verbatim in the
   window, cited by a claim, retained, or recorded in the omission ledger.
8. Public `host/v1` remains source-compatible unless a separate versioned contract
   is introduced.