# Arxi implementation roadmap

## Status

This document turns the direction in [`design/30-vision.md`](design/30-vision.md)
into a proposed implementation sequence. It is a planning artifact, not a release
promise or an accepted architecture decision. Each phase still requires focused
ADRs or specifications before its contracts become stable.

The order is deliberate: Arxi must make accepted work durable and consequential
actions exact before Asha adds autonomous memory or write-capable personal tools.
A compelling voice demo is not evidence that the underlying execution is safe.

## Target product boundary

Asha owns personal-product behavior: voice and text UX, identity, authentication,
consent, device state, turn-taking, approval presentation and product policy.
Arxi owns reusable execution: runs, jobs, teams, providers, tools, authorization,
budgets, context, memory contracts, scheduling, workspaces and auditability.

Asha must consume a public Arxi host contract. It must not import `internal/`
packages, read `events.ndjson` directly or introduce voice-specific concepts into
the universal kernel.

## Delivery principles

1. Preserve the pure reducer and authoritative append-only event history.
2. Report accepted work only after its durable identity exists.
3. Never infer success or failure when a crash leaves an external outcome unknown.
4. Authorize the exact immutable action, not a later approximation of it.
5. Record every retrieved, summarized or generated input before it influences a
   model call.
6. Treat memory as untrusted data whose authority cannot exceed its provenance.
7. Make isolation names describe guarantees that adversarial tests can verify.
8. Project one capability implementation through CLI, protocol, SDK and triggers.
9. Introduce consequential personal tools only after recovery and authorization
   behavior is proven.

## Phase 0 — Contain current correctness gaps

Before expanding the public surface:

- refuse unsupported provider response shapes instead of decoding them as empty
  text, while routing supported native tool calls through the durable turn loop;
- route the first-party Anthropic preset through native Messages and reject
  unimplemented protocols explicitly;
- freeze the fully resolved effective configuration, including prompt, model,
  policies, defaults and capability versions;
- make fresh and resumed execution restore the same configuration and pending work;
- define and test the confirmed-log-prefix boundary used by background consumers;
- resolve the snapshot boundary ambiguity before extending snapshot use.

**Exit evidence:** fresh execution, resume and replay agree on effective
configuration; unsupported provider capabilities fail explicitly; crash-boundary
tests demonstrate that pending work is neither silently skipped nor invented.

## Phase 1 — Public host and common application services

Extract capability implementations from `cmd/arxi` behind a small versioned host
surface. The first lifecycle should cover job submission, inspection,
cancellation, exact approval decisions and filtered event subscriptions.

Define public contracts for model providers, tools, job storage and workspace
provisioning without publishing current kernel types unchanged. CLI, protocol,
triggers and future product adapters must call these common services. Protocol
handshakes advertise installed and authorized handlers, not vocabulary alone.

**Exit evidence:** an external text-only host submits and observes a run without
shelling out or importing `internal/`; the CLI and protocol exercise the same
application service in contract tests.

## Phase 2 — Provider-neutral turns and native tool loops

Introduce a canonical turn representation for text/content blocks, tool schemas,
tool requests, tool results, refusals, finish reasons, usage, streaming and
cancellation. Implement provider-specific adapters outside the kernel, beginning
with a deterministic fake provider and native Anthropic and OpenAI adapters.

Persist each call request before execution, preserve a stable call ID, persist its
result and reinject that exact result into the next model turn. Provider wire types
must not leak into reducer contracts.

**Exit evidence:** the fake provider requests one read-only tool, receives the
result under the same call ID and completes the turn; Anthropic and OpenAI produce
the same canonical events; denied calls never reach a runner.

## Phase 3 — Durable jobs, attempts and scheduling

Add durable jobs, stable trigger occurrences, attempts, leases, fencing tokens,
heartbeats, checkpoints, idempotency keys and external receipts. A scheduled
occurrence uses a stable identity such as `(trigger_id, scheduled_at)`. Replace
process-local in-flight truth with durable claims and enforce periodic budgets
through an atomic ledger.

Idempotent effects may retry safely. Non-idempotent effects must reconcile through
provider receipts or end in an explicit `unknown` outcome rather than retrying or
guessing.

**Exit evidence:** restart tests at accept, claim, execute, checkpoint and complete
boundaries never lose an accepted job; expired workers cannot commit; one trigger
occurrence is not silently duplicated; unknown external outcomes remain visible.

## Phase 4 — Exact authorization and honest workspaces

**Status:** exact authorization is implemented: immutable grants bind principal,
call, canonical arguments, schema, policy, expiry and workspace profile, and one
writer CAS durably appends consumption with the matching work start before
dispatch. The honest provisioning framework is also implemented: requirements,
platform decisions, source identity and pre-accept prepared/started/finished
lifecycle fail closed and survive recovery.

Native availability remains deliberately narrower than the internal adapters and
tests. Windows advertises only `none` with `no-tools`. Linux advertises that pair
plus `direct-files`, but no native source-backed mode, so the profile cannot yet
form an accepted file-using combination. Native `shared`, `copy`, `worktree` and
`contained-process` are not advertised on either platform. Their internal
implementations and negative tests are evidence toward the contract, not a claim
that production provisioners are generally available. They remain preflight
unavailable until the production capability decision can guarantee source,
lifecycle, file, descendant, environment, filesystem and network behavior as one
complete platform contract.

Bind approvals to principal, call ID, canonical arguments or action digest, tool
schema version, policy version, expiry and single-use consumption. Validate that
the decision verb matches the pending item kind. Changing any bound field
invalidates the grant.

Complete production capability decisions for any source-backed mode selected for
support, including Git worktrees only where the full contract can be promised.
Unsupported guarantees continue to fail closed; an internal adapter does not by
itself make a mode supported.
Constrain process trees, filesystem reach, environment inheritance and network
access according to declared policy, with platform-specific negative tests.

**Exit evidence:** a grant cannot authorize changed arguments or be consumed twice;
self-approval is impossible; workspace escape, secret inheritance and descendant
process tests verify each advertised mode on every supported platform.

## Phase 5 — Canonical transcript and prepared context

Project confirmed events into transcript items covering user input, model output,
tool calls/results, human decisions and referenced artifacts. Add a durable
preparation barrier:

```text
context.prepare_requested -> context.prepared -> model.call_requested
```

The prepared artifact records source boundaries, ordered content, policy and model
versions, token measurements, memory-use receipts and content/presentation digests.
The reducer never reads transcript storage, memory, tokenizers, clocks, networks or
indexes directly.

**Exit evidence:** a later turn receives prior conversation and tool evidence; the
system can prove exactly what was presented; replay reuses the recorded prepared
artifact instead of rebuilding a potentially different prompt.

## Phase 6 — Measured context compaction

Implement context-pressure measurement, explicit layer budgets and a versioned
compaction artifact containing a lossy summary plus a recent verbatim window.
Preserve the canonical transcript and record source ranges, retained critical
artifacts, omitted branches, generator version and before/after token counts.
Compaction failure must be visible rather than silently truncating context.

**Exit evidence:** continuity probes preserve goals, constraints, decisions,
progress and next steps; summaries do not invent unsupported facts; source history
remains intact; replay does not invoke the summarizer again.

## Phase 7 — Read-only governed memory

Start with records supplied or explicitly approved by a user, operator or import.
Model-generated material may propose candidates but cannot create active memory.
Records carry stable and immutable version IDs, provenance, evidence class,
authority, confidence, sensitivity, purpose, retention, lifecycle and bitemporal
validity.

Authorization occurs before semantic ranking across tenant, subject, application,
project, team/agent, run, evidence class, sensitivity, purpose and valid time.
Retrieval receipts record exact selected versions and excerpts, ranking/index
versions and reasons. Provide inspection, correction, supersession, export and
deletion controls before enabling autonomous writes.

**Exit evidence:** cross-user and cross-project leakage tests return zero records;
every influence identifies its source and version; stale versions stop appearing
after correction; deletion propagates through content, indexes, summaries, caches
and unused prepared contexts without resurrection.

## Phase 8 — First useful Asha vertical slice

Build Asha as an external product adapter with authenticated text and push-to-talk.
Limit the first tools to read-only notes, tasks and calendar access, durable
reminders, explicitly confirmed memory and draft generation. Do not enable
payments, autonomous sends, destructive operations or automatic memory promotion.

**Exit evidence:** Asha can submit a durable job, observe its event stream, survive
an Arxi restart, execute one read-only model-requested tool, continue from its
result, explain which memory influenced the answer and let the user correct or
delete that memory.

## Phase 9 — Provider-neutral realtime voice

Add a media gateway and turn manager above Arxi. Support both native duplex
providers and composed `speech-to-text -> model -> text-to-speech` pipelines behind
one lifecycle. Keep provider session state as a cache, never as the product's
canonical conversation.

Normalize generation IDs, playback cursors, endpointing, interruption,
cancellation, tool calls and reconnect/resume. Correct barge-in must silence and
flush playback, invalidate the old generation, cancel safe work, record what was
heard and ignore late events. Consent for microphone access, transmission,
recording, audio retention, transcript retention, review, training and third-party
tools remains explicit and separate.

**Exit evidence:** changing realtime providers does not alter the kernel; interrupted
generations cannot apply late audio or tool results; durable conversation survives
provider-session loss; latency and false-interruption thresholds are established by
product evals rather than assumed.

## Phase 10 — Reflection and governed consolidation

Reflection creates evidence-linked candidate claims. Consolidation separately
plans promotion, merge, supersession, quarantine, expiry or rejection and commits
through a revision CAS with idempotency and independent authorization. Generated
prose never becomes trusted user truth merely because it was repeated or
summarized.

**Exit evidence:** candidates can be reviewed and rejected; retries do not duplicate
records; contradictions preserve temporal history; rollback works; poisoning,
correction, deletion and backup-restore tests show that rejected or erased content
cannot silently return.

## Phase 11 — Consequential personal tools

Expand gradually from reads to reversible writes, drafts, exact-confirmation sends
and only then irreversible actions. Each integration defines idempotency,
reconciliation, receipt, cancellation, compensation and unknown-outcome behavior.
Voice is never the sole authentication factor for sensitive actions.

**Exit evidence:** calendar writes and messages consume exact one-shot approvals;
external receipts support recovery; irreversible tools fail closed when safe retry
or reconciliation is unavailable.

## First implementation milestone

The first milestone is **Arxi Host v0: public lifecycle, durable job and tool loop**:

1. contain Phase 0 provider and resume defects;
2. define durable identities and states for jobs, attempts, tool calls and approvals;
3. extract shared application services from the CLI composition root;
4. expose the smallest public host lifecycle and honest protocol capabilities;
5. prove a restart-safe read-only tool loop with a fake provider.

The milestone is complete when a text-only Asha submits a durable job, a model
requests one read-only tool, Arxi persists and executes it, reinjects the result,
survives restart and exposes the same event stream through its public host and
protocol surfaces.

Voice, write-capable personal tools, autonomous reflection and memory promotion are
explicitly outside this first milestone.

## Decisions and specifications required first

Before implementation stabilizes contracts, write focused records for:

1. public host lifecycle and compatibility policy;
2. canonical provider turn and tool-call representation;
3. durable jobs, lease/fencing and unknown-outcome semantics;
4. exact approval grants and consumption;
5. workspace guarantee levels by platform;
6. confirmed transcript and prepared-context artifacts;
7. memory identity, scope, temporal validity, authority and deletion lineage;
8. realtime session lifecycle, generation identity and interruption semantics.

Each accepted ADR must identify the tests that make reverting it fail. Event names,
Go interfaces, schemas and quantitative service thresholds belong in their focused
specifications and evals, not in this roadmap.
