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

**Status:** implemented for coordinated storage. Durable jobs use stable trigger
occurrences, fenced attempts, leases, heartbeats, checkpoints, dispatch
registrations, external receipts and an atomic periodic ledger. Process-local
storage remains a supported reduced-capability fallback and does not advertise
coordination or restart guarantees.

Idempotent effects may retry safely only when the concrete adapter honors the
prepared key. Non-idempotent effects reconcile through trustworthy provider
receipts or end in an explicit `unknown` outcome rather than retrying or
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
tests. Windows advertises only `none` with `no-tools`. Linux advertises that
pair plus `shared` with the read-only `direct-files-read` profile (ADR-0017)
and `copy` with the write-capable `direct-files` profile (ADR-0019), so
read/grep and write/edit members both form accepted file-using combinations.
The layout is what separates them: reads may see the operator's frozen tree,
writes land in a per-member snapshot and never reach it. That separation is a
stated pairing rather than a consequence of which profiles happen to be
advertised.

The line is file access, not write access. `bash` is refused on both counts —
it resolves to `worktree`, unadvertised because its root holds a `gitdir:`
pointer into the operator's repository (ADR-0018), and to
`contained-process`, which no platform advertises while descendants,
filesystem, environment and network remain unavailable. Native `worktree` and
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

**Status:** implemented. `internal/transcript` projects the confirmed prefix
into ordered items; `internal/contextprep` turns one projection into an
immutable presentation. The barrier below is committed as real events
(`kernel.ContextPrepareRequested`, `ContextPrepared`, `ContextPrepareFailed`,
`ExecWorkPrepared`), specified in [`spec/events.md`](../spec/events.md), and
enforced in `internal/exec`: a model child cannot start without a verified
`context.prepared`, and `loadPreparedContext` reuses the committed bytes
byte-for-byte rather than rebuilding them. Two values for one context ID, a
digest that disagrees with its bytes, a changed source boundary, or a terminal
`prepare_failed` are each refused rather than retried. Phase 6 builds directly
on this barrier, so it could not have been implemented without it.

The original phase text follows.

Project confirmed events and immutable artifacts bound by them into transcript
items covering user input, model output, tool calls/results, human decisions and
referenced artifacts. Add a durable preparation barrier:

```text
context.prepare_requested -> context.prepared
  -> exec.work_prepared (turn_child/model) -> exec.work_started
```

The prepared artifact records source boundaries, ordered content, policy and model
versions, token measurements, memory-use receipts and content/presentation digests.
The reducer never reads transcript storage, memory, tokenizers, clocks, networks or
indexes directly.

**Exit evidence:** a later turn receives prior conversation and tool evidence; the
system can prove exactly what was presented; replay reuses the recorded prepared
artifact instead of rebuilding a potentially different prompt.

## Phase 6 — Measured context compaction

**Status:** implemented. Preparation measures per-layer pressure against the
versioned budget policy `arxi.context-budget/v1`, derived deterministically
from a known input limit. Measured overflow under `on_overflow: summarize`
produces a verified extractive compaction artifact (`arxi.compaction/v1`) —
lossy summary plus recent verbatim window — committed inside the same
`context.prepared` batch; every claim must be contained in the transcript items
it cites, anchors are never silently dropped, and the omission ledger records
everything the presentation dropped by identity and content digest. Unknown
limits mean unknown pressure; other overflow modes and uncompactable contexts
fail visibly as terminal `context.prepare_failed` records of class
`compaction`. The canonical transcript is never modified. A model-backed
generator remains future work behind the same containment gate.

The original phase text follows.

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

Authorization occurs before semantic ranking across tenant, user, application,
project, team, agent, run, evidence class, sensitivity, purpose and valid time.
Retrieval receipts record exact selected versions and excerpts, ranking/index
versions and reasons. Provide inspection, correction, supersession, export and
deletion controls before enabling autonomous writes.

**Exit evidence:** cross-user and cross-project leakage tests return zero records;
every influence identifies its source and version; stale versions stop appearing
after correction; deletion propagates through content, indexes, summaries, caches
and unused prepared contexts without resurrection.

**Status:** not started; the store, retrieval and deletion lineage do not exist.
One prerequisite is already settled: ADR-0020 decided the channel retrieved
memory arrives on, and the preparer presents it as a user-role message rather
than folding it into the system message. That was done ahead of the phase on
purpose — the presentation shape is the expensive thing to change once prepared
contexts have been committed against it, and settling it first is what lets the
retrieval decision be about retrieval.

A second prerequisite is now settled, and it was found by measuring the first
rather than by planning: ADR-0020's Decision section claimed the memory receipt
already recorded "the record identities and version IDs it carries", and no
field of `MemoryReceipt` held either. The exit evidence above requires that
"every influence identifies its source and version", so a retrieval design
reading ADR-0020 would have inherited a receipt that cannot tell version 1 of a
record from version 2 — and correction propagation is unverifiable against
that. ADR-0021 adds `RecordID` and `VersionID`, keeps them empty for frozen
configuration memory because a config field has no record identity, and makes a
governed receipt without a version **fail** rather than merely be documented.

A third prerequisite is settled, found the same way — by measuring the previous
one instead of designing on top of it. The scope list above is what a store must
implement first, since authorization runs before ranking, so it was compared
against the two other places this project names scopes. All three disagreed:
`docs/design/30-vision.md` said `user` and had no `tenant`; this roadmap said
`tenant` and `subject`; and the code already commits `Subject` as
`subject_agent` in five artifact schemas, meaning the subject *agent*. A reader
implementing `subject` scoping would have wired it to the field that already
exists and shipped a store whose subject scope was its agent scope, silently.
ADR-0022 removes `subject` from the vocabulary, adds `tenant` to the vision, and
pins the agreement with a test that reads both documents.

A fourth prerequisite is settled, found by probing the third rather than by
planning. The phase opens with a containment rule — model material "may propose
candidates but cannot create active memory" — and nothing represented it: a
throwaway probe showed a candidate receipt validating identically to an approved
one, and a misspelled kind validating too, because `Governed()` was a negation
and therefore a blocklist with one entry. Every string that was not
`frozen_context_memory` was authority. ADR-0023 enumerates the three kinds, adds
`Presentable()` so a candidate is never presented, and makes an unknown kind fail
closed.

A fifth prerequisite is settled, and it is the one that made the previous three
enforceable. ADR-0023 recorded its own weakness — `Presentable()` had no
production caller — and probing that admission found it understated: nothing
called `MemoryReceipt.Validate()` at all. The version rule of ADR-0021, the
vocabulary of ADR-0022 and the enumeration of ADR-0023 were each reachable only
from tests, so an artifact carrying a candidate receipt marshalled cleanly,
computed a content digest and would have been committed by the durable barrier.
The same probe found two receipts the preparer itself emits that prove nothing:
one with an empty effective config SHA, which is the only version identity
frozen memory has, and one with no content digest at all. ADR-0024 calls
`Validate` from `Prepare` before either digest is computed, and requires the
evidence fields to hold evidence, so "every influence identifies its source and
version" is refused rather than merely documented.

A sixth prerequisite is settled, and it corrects what the first one above
claimed. With five prerequisites recorded as closed, the earliest — the
channel — was measured before building on it, and the sentence "the preparer
presents it as a user-role message" turned out to describe one assembler of
three. ADR-0020 named `internal/provider` among its affected packages and only
`internal/contextprep` had adopted the decision: `provider.buildMessages` and
`host/v1.textSystem` both still folded memory into the system message, which is
verbatim the defect ADR-0020 quotes in its own Context as the thing it exists to
remove. The suite did not see it because the channel was asserted at the wire
against hand-built `turn.Request` literals — the mapping was proven and the code
that assembles the messages in production was never touched.

That exposure was not theoretical: `PrepareTurn` is the fallback taken whenever
durable preparation is not in force, and `SpawnTurn` builds and dispatches in one
step, never reaching `internal/contextprep`. Because `turn.Request` has no
receipt field, ADR-0021's version rule, ADR-0022's vocabulary, ADR-0023's
enumeration and ADR-0024's validation are not merely unenforced on that path —
there is no receipt for them to attach to, and the channel is its only
guarantee. ADR-0025 moves memory to its own user-role message in the provider
assembler, adds an additive `Memory` field to `host/v1.TextRequest` so the port
can express the separation at all, and asserts the channel through the real
assembler entry points rather than against literals.

Two facts to carry into the store work rather than rediscover. **The channel is
a property of every assembler, not of the preparer** — routing the legacy paths
through `internal/contextprep` is the right long-term answer and is deliberately
not done, because `contextprep` freezes a durable artifact under ADR-0013's
barrier and a single-turn request is a different contract. Until that is
decided, a retrieval design must assume more than one presentation path.
**Receipt guarantees reach only the durable path**, so the exit evidence that
"every influence identifies its source and version" holds for prepared contexts
and has no representation on `SpawnTurn` at all; extending it there needs a
receipt on the turn request, which is undecided.

A seventh prerequisite is settled, and it is about this paragraph's own
reliability. Probing the artifact the store work would be planned *from* rather
than the code found that six ADRs declared `Enables: Phase 7` and this status
cited five. ADR-0025 was missing — so the first prerequisite above still read
"the preparer presents it as a user-role message", preserving as current
guidance the exact belief under which the channel defect survived four ADRs.
Nothing connected an ADR's `Enables:` header to the phase it names, and
`TestEveryImplementedPhaseSaysSo` deliberately checks only that a status line
exists, never what it says. ADR-0026 derives the pairs from the corpus on every
run and fails when a phase omits a decision that claimed it, so the citation
cannot silently age again.

An eighth prerequisite is settled, and like the seventh it is about the
reliability of this section rather than the store. ADR-0026 added the citation
check by enumerating the ADR header vocabulary, and justified that set in prose
as "the measured vocabulary of all 25 records — `Depends on` in 16, `Enables`
in 6". Measured, the corpus held 26 records with those fields appearing 17 and 7
times: the counts described the corpus without the record stating them, the same
off-by-one the total-count check already guards, one field-set deeper. ADR-0027
derives the vocabulary from the corpus on every run and asserts the map equals it
exactly in both directions, so a field the map lists that no record uses now
fails too — the vacuity that direction left open. The frozen per-field counts are
removed rather than corrected, because a hand-kept count drifts again on the next
record.

A ninth prerequisite is settled, and it is the first that touches the record
rather than the receipt or the documents around it. Every prerequisite before it
concerned what memory *presented* (the channel, the receipt, its version
identity, its authority) or the reliability of this plan; none touched the
stored thing a receipt names a version of. ADR-0028 settles the hardest part of
that record — its temporal validity — ahead of the rest, for the reason ADR-0020
settled the channel first: it is the shape most expensive to change once records
are committed against it. Validity is bitemporal (a valid-time axis for when the
fact holds in the world, a decision-time axis for when the store believed it)
and its decision axis is append-only, so a correction closes the old version's
belief interval and appends a new one rather than mutating it. That is exactly
what makes "stale versions stop appearing after correction" and "contradictions
preserve temporal history" both true at once — a single timestamp cannot satisfy
both, because one axis cannot both hide a version from the present and keep it in
the past. `internal/memory` holds the contract as a pure leaf, guarded like
`internal/job`, so the temporal judgment never reads a clock and an as-of query
cannot answer differently on replay.

A tenth prerequisite is settled, and it is what made the ninth enforceable.
ADR-0028 proved its tiling property on two hand-built validities whose bounds
happened to align — the same "assertion with no subject" shape ADR-0025
recorded, because nothing in the package actually *produced* a correction, so
nothing guaranteed a real one lines the bounds up. ADR-0029 makes supersession an
operation: `Version.Supersede` closes the predecessor and opens the successor at
the same instant, so the belief axis tiles by construction and no call site can
introduce a gap or an overlap. It also gives the stored version the `RecordID` and
`VersionID` ADR-0021 put on the receipt, and adds `ValidateLineage`, which refuses
any chain that is not append-only and contiguous. A lineage may end closed — a
fully retracted fact — which is the tombstone shape the deletion decision will
build on, so the validator pins the chain without deciding what a closed tail
means for a query.

An eleventh prerequisite is settled, and it closes the meaning ADR-0029 left
open. A supersession and a deletion produce the same bytes — both close the
current version's belief interval — so a closed tail was indistinguishable from a
correction awaiting its successor, and nothing could forbid appending a
contiguous successor after a deletion. That is the resurrection this phase's exit
evidence rules out, passing every check. ADR-0030 makes deletion a distinct
terminal operation: `Version.Retract` closes the belief and marks the closure
terminal, opening no successor, and both `Supersede` and `ValidateLineage` refuse
any version that follows a retraction. The temporal semantics need no new query
logic — a retracted version is closed, so the present sees nothing and a
historical as-of query still sees it, deletion closing the interval rather than
erasing it. This decides deletion within one record's lineage; propagation across
indexes, summaries, caches and unused prepared contexts is a store concern that
needs the store to exist, and the lineage rule here is the invariant those paths
must preserve.

A twelfth prerequisite is settled, and it is the relocation ADR-0029 and ADR-0030
each flagged and deferred. Authority was enumerated (ADR-0023) but the
enumeration lived on the presentation receipt in `internal/contextprep`, the only
thing that carried a kind. A stored version needs its own authority — the store
authorizes before it ranks — and copying the enumeration into the pure leaf would
have created two tables that drift, the exact failure ADR-0023 forbids. ADR-0031
moves the enumeration to `internal/memory` as `memory.Kind`, and the receipt now
delegates its `Governed`, `Presentable` and unknown-kind refusal to it, so the
record and the receipt read one vocabulary. The record-layer rule refuses only an
unknown kind; the rule that a candidate is never *presented* stays with the
receipt, because it is a property of a presentation, not of a stored record. The
`Version` does not yet carry a kind field — that is the record struct, the next
step — so this is a relocation reviewable on its own.

A thirteenth prerequisite is settled, composing the previous ones. ADR-0032 puts
a governed authority kind on the stored `Version`, so identity (ADR-0021),
authority (ADR-0023/0031) and validity (ADR-0028) now sit on one unit — the
record struct the store holds and authorizes before it ranks. `Validate` refuses
a kind that is not governed, which excludes frozen configuration memory: a
blueprint field has no record identity, so it is presented from the blueprint and
never stored as a version. `Supersede` propagates the kind, so a correction keeps
a record's authority rather than laundering a candidate into an approved record;
promotion is a separate Phase 10 operation, deliberately not expressible through a
correction.

A fourteenth prerequisite is settled: the authorization that runs before ranking.
ADR-0022 fixed the scope vocabulary and declined to add a type, since a scope with
no store to authorize would be a field nothing fails on. ADR-0033 adds the type
together with the operation that gives it teeth — `Scope` and
`record.AuthorizedFor(principal)` — so it is a primitive, not decoration. The
tenant fails closed: a record with no tenant is visible to nobody, and the tenant
must match exactly, which is ADR-0022's named asymmetry made executable. Every
other axis narrows only when the record sets it, so a record scoped to a user or
project is invisible to a different one (the leakage exit evidence, in ADR-0022's
vocabulary) while a tenant-wide record is visible to any principal in the tenant.
Like the descriptive attributes, `Scope` is not yet attached to the `Version`; the
authorization primitive is what needed deciding, and wiring it onto the record
belongs with the store.

A fifteenth prerequisite is settled: the retrieval audit trail. The record
contract satisfies the exit evidence about correction and leakage, but a second
clause — "retrieval receipts record exact selected versions and excerpts,
ranking/index versions and reasons" — had nowhere to live, because the only
receipt was `internal/contextprep`'s, which records what was *presented to a
model*, not what a *retrieval selected from the store*. ADR-0034 adds
`memory.RetrievalReceipt` and `Selection`: the retrieval-side companion to the
presentation receipt. `Validate` refuses a receipt that cannot account for what
it selected — a tenant-less principal, no ranking version, a selection missing its
record or version or reason, or the same version counted twice — so "every
influence identifies its source and version" is executable on the retrieval path.
An empty receipt is valid: a retrieval that matched nothing, including one refused
across a tenant, proves nothing influenced the turn, which is the leakage evidence
itself. `RankingVersion` is opaque here, so the receipt has a field to record a
ranking decision in without a schema change when ranking is decided.

Ranking remains undecided and still needs its own record (item 7 below), as do
the record's remaining descriptive attributes — provenance, evidence class,
confidence, sensitivity, purpose, retention and lifecycle — which are left off the
version because several interact with ranking and committing their shape now would
pre-decide it. The store itself does not exist.

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
