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

**Status:** implemented. `internal/runconfig.Publish` freezes the fully resolved
configuration into an artifact published by hard link, so a run cannot acquire a
different prompt, model or policy than the one it started under, and replay reads
the same bytes. Unsupported provider shapes fail explicitly rather than decoding
as empty text: `internal/provider/anthropic.go` rejects unknown content-block
types and turns an unrecognized stop reason into an explicit refusal.
`ReadConfirmed` in `internal/logstore` defines the confirmed-prefix boundary that
background consumers read, withholding in-flight batches instead of exposing a
partial one.

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

**Status:** implemented. `host/v1` is the versioned surface, with nine
capabilities — `job.submit`, `job.inspect`, `job.cancel`, `decision.approve`,
`decision.reject`, `decision.answer`, `job.wait`, `event.subscribe` and
`job.recover` — and public ports for providers, tools, job storage and workspace
provisioning that do not re-export kernel types. Handshakes advertise the
installed and authorized set rather than vocabulary alone.

One boundary is worth stating because it is load-bearing for Phase 8 and is not
visible from the capability list: the protocol is served over **stdio or a unix
socket** (`cmd/arxi/serve.go`). Stdio is the default — `arxi serve` with no
`--listen` speaks NDJSON over stdin/stdout — and the socket is the opt-in,
because stdio needs no path, no mode and no stale-file cleanup when a parent
process owns both ends.

Which one is the default matters for what can be built today. A local client
does not need a transport decision at all: it execs the binary and reads lines.
That makes a terminal UI reachable now against the ten methods the handshake
implements (`run.start`, `run.show`, `run.result`, `run.attach`, `run.cancel`,
`blueprint.validate`, `schema`, and `inbox.approve` / `inbox.reject` /
`inbox.reply` — the last three being the approvals an `ask`-policy tool will
request). A remote or mobile client still needs a transport that has not been
decided. The capability surface is not the constraint; the transport is, and
only for out-of-process consumers that are not local.

This paragraph replaces an earlier version of itself that said "unix socket"
and "five methods", both measured only against the single `net.Listen` call in
the tree. `serve.go` states the stdio default in a comment two functions above
that call, so the error was reading one site and generalizing rather than
missing information. It is recorded here instead of quietly fixed because it is
the same half-measured-claim shape the rest of this document keeps correcting,
and the distinction it got wrong is precisely the one that decides whether a
local client is blocked or unblocked.

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

**Status:** implemented. `internal/turn` holds the canonical representation:
roles, content blocks, media sources, tool definitions, tool calls and results,
refusals, finish reasons, usage and a stream event vocabulary
(`content_delta`, `usage`, `completed`, `canceled`). `turn.NewToolCall` binds a
provider call ID to canonical arguments so a result cannot be matched to the
wrong request. Native tool dispatch runs through `ClassifyToolDispatch` and
`ExecuteTurnToolDispatch` in `internal/exec`, which is what keeps a
model-requested tool inside the durable loop instead of beside it.

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

**Status:** partially implemented, and not exited. The store, retrieval and
deletion lineage exist in `internal/memorystore` (ADR-0027) and the eight
prerequisites below are settled. What remains is listed as the two deliberate
gaps at the end of this section plus item 7's record vocabulary, so this phase
must not be read as either finished or unstarted.

This line said "not started; the store, retrieval and deletion lineage do not
exist" for a full turn after ADR-0027 shipped all three, while the narration
below already reported the store as built. The two guards nearest to it could
not see the contradiction: one checks that a status marker exists rather than
what it claims, the other checks that the phase cites its enabling ADRs and was
satisfied by the very paragraph doing the refuting.
`TestNoPhaseDeclaresMachineryAbsentThatItAlsoDescribesAsBuilt` now fails when a
phase's headline contradicts its own status block.

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

The store now exists. ADR-0027 builds it, and it was reached by measuring the
seven prerequisites above rather than by planning from them: all seven are rules
about a receipt, and a probe of the production build found nothing that could
produce one. `KindApprovedMemoryRecord` had zero construction sites outside
tests, `MemoryReceipt.RecordID` was assigned in production zero times, and the
only receipt any code path emitted was `frozen_context_memory`. The channel was
a finished pipe with nothing flowing through it, so the version rule, the
vocabulary and the authority enumeration were reachable only from tests.

That measurement also explains the user-visible symptom that prompted it — an
agent remembers nothing between runs — and the cause is structural rather than a
missing feature. A run's truth is its event log (ADR-0002) and a log is per-run
by construction, so nothing inside a run directory can be read by a run that
does not exist yet. `ContextSpec.Memory` is frozen blueprint prose, identical
for every run of that blueprint; `ContextSpec.Shared` is within-run team
material; and `run fork` copies a parent's prefix, which is continuation of one
history rather than recall across histories. None of the three is a memory.

`internal/memorystore` holds immutable content-addressed versions outside every
run, derives the current version by walking supersession rather than trusting a
flag, deletes by tombstone so deletion survives replication, and authorizes
before ranking because a relevance score computed across tenants is itself a
cross-tenant inference. `contextprep.Request` accepts the retrieved text and its
receipts, which pass the same validation the frozen receipt does. Phase 7's exit
evidence is now testable end to end: cross-scope leakage, correction
propagation, deletion without resurrection and per-influence identification each
have a witness that fails when its mechanism is removed.

Probing that store rather than reading it found the defect ADR-0028 corrects,
and it is worth recording because the guard that missed it looked adequate.
ADR-0027 decided a forked supersession chain is refused rather than resolved by
a tiebreak, which is right. What nothing measured is what "refused" cost: the
refusal was raised for the **whole store**, so one forked record denied
retrieval to every other principal — including other tenants — and the error
named version IDs across the boundary ADR-0027 calls the one no retrieval
crosses. It was also permanent, because `Correct`, `Delete` and `Promote` all
resolve a tip through the function the fork made fail, so the three verbs that
could repair a fork were the three a fork disabled. Four of the five controls
this phase promises were gone after one bad record.

Worse, the fork did not need a hostile replica to arrive. `Correct` reads the
tip and then writes against it, so two concurrent corrections both succeeded and
bricked the store — in three runs out of five — which is the concurrency
ADR-0027 explicitly says the store exists to support. The existing guard built
its fork with two deliberate `Put` calls and asserted only that retrieval then
failed, and a test asking "does this fail?" cannot tell a contained refusal from
a catastrophic one. ADR-0028 claims a predecessor exclusively so the fork cannot
be created, and contains an imported one to its own record so it cannot take the
store down.

Probing *that* record in turn found what its own argument had not measured, and
ADR-0029 corrects it. ADR-0028 preferred a claim file to a lock because a claim
"needs no release: it is the durable record of a fact that does not expire —
that predecessor now has a successor". True when the write succeeds; when the
write fails the fact never became true, and the surviving claim is exactly the
stale lock that reasoning rejected locking to avoid. An ordinary I/O failure —
no crash, no tampered file, no hostile replica — left a claim naming a version
that was never written, and because `Correct` and `Delete` both supersede the
tip, both were refused **permanently** for a record whose chain never forked,
while retrieval kept serving the pre-correction body as current. It was also
invisible: a fork is two versions and `Forks()` reports it, this is zero
versions, so no verb in the package could see the claim at all. The refusal even
named the absent successor as though the supersession had happened. ADR-0029
releases a claim whose write failed, scoping the release to the claim that call
created so it cannot steal an in-flight one, and adds `Claims()` and
`ReleaseClaim()` so the residue of a crash is diagnosable and repairable instead
of permanent and silent.

Probing ADR-0028's fork detector in turn found the fork it could not see, and
ADR-0031 corrects it. ADR-0028 defined a fork as two versions superseding one
predecessor and counted successors per predecessor to find it. Two `Approve`
calls for one record produce two versions that supersede *nothing* — two roots
that share no predecessor — so the count never rose and both were reported
current: retrieval returned two versions of one record and `tip` chose between
them by version-ID order, the silent loss the store exists to prevent, reached
through the gap in the fork test rather than the race the claim covers. ADR-0031
rephrases detection against the invariant it always meant — one current version
per record, and any record with more is forked, however the extra arose — which
subsumes the multi-successor case and catches the multi-root one. A root write
over a record that already has a different version is also refused at the source,
with `Correct` named as the verb that changes a record without forking it, while
an imported or concurrent root fork is still contained at read.

Probing that containment in turn found what ADR-0028 and ADR-0031 secured but did
not complete, and ADR-0032 corrects it. A fork was contained and visible but had
no way back: `Correct`, `Delete` and `Promote` all resolve through `tip`, which
refuses a forked record, and superseding a losing head by hand is a net-zero
operation on the head count — a one-parent supersession turns one head into a
non-head and adds a new one — so the fork was permanent, and `Fork.err` told the
operator to "supersede the ones that are wrong", a remedy a probe proved
impossible. ADR-0032 adds `Resolve(recordID, keepVersionID, origin)`: it appends
one version that supersedes the head the operator keeps and names every other
head in a new `Retires` field, which the single head definition treats as no
longer current, dropping the count to one. The store never picks the survivor —
that is the fact ADR-0027 and ADR-0031 refused to guess — so it is a required
argument, and the retired heads stay on disk so a receipt naming one still
resolves. This is the "supersession" control Phase 7's exit evidence promises,
made reachable for a record a fork had frozen.

Two gaps are deliberate rather than pending. **Retrieval is not wired into
`internal/exec`**: which principals a run is authorized for is an identity
question, and the boundary above assigns identity, authentication and consent to
Asha, so wiring it now would mean inventing a principal from whatever the run
happens to know — the class of guess ADR-0022 exists to stop. **Temporal
validity, evidence class, confidence, sensitivity, purpose and retention are not
implemented** and still need their own record (item 7 below); adding them as
struct fields with nothing authorizing them would be the "field nothing fails
on" defect this corpus has now recorded four times.

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
