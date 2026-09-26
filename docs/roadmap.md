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
prerequisites below are settled, as is item 7's record vocabulary (ADR-0042
through ADR-0047). What remains is the single deliberate gap at the end of this
section — retrieval is not wired into `internal/exec` — so this phase must not be
read as either finished or unstarted.

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

Probing that new edge in turn found the hole ADR-0033 closes. `Retires` removes a
head, and nothing checked that its targets belonged to the record declaring them:
a version of one record naming another record's head — in `Retires` or in the
pre-existing `Supersedes` — silently removed that head, leaving the victim with
zero current versions, unreadable, with no fork and no tombstone, and the victim
could be in another tenant. That is the cross-scope leakage the exit evidence
tests from the other direction, arriving as deletion rather than disclosure.
ADR-0033 honors an edge only within one record: a foreign edge is inert, the
target keeps its head, and a genuinely forked carrier surfaces through the
visible fork path instead of taking an unrelated record down silently.

Probing that same-record edge once more — for a self-retire or a retire cycle
that would leave a record with zero heads — found the case unconstructable rather
than unhandled, which ADR-0034 records. A version ID is content-addressed over
`Retires` and `Supersedes`, so a version can never name its own ID: an attempted
self-edge targets a version not in the store, ADR-0033 leaves it inert, and a
mutual-retire cycle surfaces as a visible fork instead of a silent freeze. No
runtime guard is added, because the content address forbids the edge; ADR-0034
pins the one assumption that clearance rests on — the edge fields are part of the
identity — with a test that fails the moment a field leaves it, so the safety
cannot be silently withdrawn by a digest "simplification".

Item 7's record vocabulary is no longer entirely deferred: ADR-0042 adds the
first of its dimensions, sensitivity, and adds it the way the scope dimension was
added — as an authorization applied before ranking, not a struct field. A record
carries a level from a closed ranked vocabulary (`public`, `internal`,
`confidential`, `secret`); `Validate` refuses a record that names none rather
than defaulting it to `public`, because the level most often left unset is the
sensitive one and a default of `public` would disclose exactly the records that
most needed a classification; and `Retrieve` withholds a record whose level
outranks the query's clearance in the same step that already filters by scope,
before the ranker sees it, on ADR-0027's own argument that a score computed over
disallowed material has already treated it as a candidate. The level is part of
the content-addressed version identity (ADR-0034), so a reclassification is a new
version a receipt can name rather than an in-place edit, and the existing
digest-immutability guard covers it with no new check. It was chosen first among
the item-7 dimensions because its omission is a disclosure — a record correctly
scoped but over-classified, handed to an under-cleared caller — which is the
leakage the exit evidence already tests, arriving through clearance rather than
through scope.

ADR-0043 adds the second dimension, purpose, the same way, and it is the second
worked example of a distinct authorization *relation*. Sensitivity is ranked, so
a clearance authorizes it against a ceiling; purpose has no order — `operate`,
`personalize` and `recommend` are incomparable uses — so it is authorized by set
membership, the shape scope already uses: a record is approved for one purpose,
a query holds the set of purposes it is authorized to serve, and `Retrieve`
withholds a record whose purpose is not in that set, in the same pre-ranking step
that filters by scope and clearance. `Validate` refuses a record that names no
purpose rather than defaulting it, because treating an unstated use as any use is
purpose creep by construction, and unlike sensitivity there is no least-privilege
member a default could safely pick. The purpose is part of the content-addressed
version identity (ADR-0034), so a re-purposing is a new version a receipt can
name; and an empty authorized set in a query authorizes nothing — the fail-closed
floor of an unranked dimension, mirroring the empty scope set rather than the
public floor an empty clearance falls back to. It was chosen second because its
omission is also a disclosure: a record the user approved for one use, surfaced
under another, is the leakage the exit evidence tests, arriving through purpose.

ADR-0044 adds the third dimension, evidence class, and it is the third worked
example of the membership relation — but the first whose argument is *why it is
not the ranked dimension it looks like*. A class describes what a record rests on
(`stated` by a user, `observed` from execution, `imported` from an external
source), and it is tempting to rank these by strength and authorize a floor the
way sensitivity authorizes a ceiling. The domain forbids it: for a stated
preference `stated` is authoritative and `observed` is the weaker guess, while for
an external fact `imported` outranks what a user `stated` from memory — the order
flips with the question, so any rank would be an order invented where the domain
has none, the failure ADR-0023 and ADR-0043 name. So a record carries one class, a
query holds the set of classes it accepts, and `Retrieve` withholds a record whose
class is not accepted, in the same pre-ranking step that filters by scope,
clearance and purpose. `Validate` refuses a record that names no class rather than
defaulting it, because — as with purpose — there is no least-privilege member to
fall back to; the class is part of the content-addressed version identity
(ADR-0034), so a reclassification is a new version a receipt can name; and an empty
accepted set accepts nothing, the fail-closed floor of an unranked dimension. It
was chosen third, over valid time, because its omission is a disclosure the exit
evidence tests — an inference surfaced where an assertion was asked for — and
because it is self-contained, where valid time is bitemporal and its as-of instant
is a clock value this store deliberately does not read.

ADR-0045 adds confidence, and it is the first dimension on the ranking side of the
line ADR-0043 drew: the four before it decide whether a record may be seen at all,
confidence decides only where it ranks among the records already authorized. So it
is the first that cannot borrow the authorization seam's witness — no query
withholds a record for its confidence, so there is no cross-confidence leakage test
— and its check is a *reordering* rather than a leakage one: a record carries a
level from a closed ranked vocabulary (`low`, `medium`, `high`), and `Retrieve`
orders the authorized set by it, highest first, after scope specificity and before
the record-ID tie-break, so removing the clause presents a guess ahead of a
vouched-for fact. It is the honest counterweight to evidence class: where that
dimension *looked* ranked and was not, confidence genuinely is ranked, because a
degree of belief in a record's own correctness does not invert with the question
asked. There is deliberately no confidence field on the query — filtering by it
would withhold material the caller may see merely because the writer was unsure,
turning a ranking dimension into an authorization one. `Validate` still refuses a
record that names no confidence, because an unstated confidence is no assessment
rather than a low one and defaulting it would launder a missing judgment into a
stated one; and the level is part of the content-addressed version identity
(ADR-0034), so a re-rating is a new version a receipt can name. It was chosen
before retention because retention feeds lifecycle rather than ranking and needs a
clock this store does not read, the same reason valid time is last.

ADR-0046 adds retention, the first dimension on the lifecycle side of ADR-0043's
line: the four authorization dimensions decide whether a record may be seen,
confidence decides where it ranks, and retention decides neither — it decides when
a record stops being kept. So its check is neither a leakage test nor a reordering
one but an *expiry* test: a record carries a policy from a closed lifecycle
vocabulary (`permanent`, `ephemeral`), and an `Expire` sweep tombstones the
ephemeral records while leaving the permanent ones, so removing the permanent guard
expires a record a user committed to keep. There is deliberately no retention field
on the query — a stronger version of confidence's reason, because retention does
not even change the order, so filtering by it would withhold a record the caller
may see merely because it is expirable. `Validate` still refuses a record that
names no retention, because an unstated retention is no lifecycle decision and both
defaults are dishonest — `permanent` hoards a record meant to be transient,
`ephemeral` expires one meant to be kept; and the policy is part of the
content-addressed version identity (ADR-0034), so a re-tiering is a new version a
receipt can name rather than an in-place edit a sweep could act on. It met the
clock constraint that still defers valid time not by reading a clock but by being a
policy the caller times: `Expire` takes no as-of instant — the store decides what
may expire and the caller decides when. Expiry is a tombstone, not an unlink, so an
expired record inherits the deletion lineage's no-resurrection guarantee.

ADR-0047 adds valid time, the fifth and last authorizing dimension and the store's
second time axis: the version chain records transaction time — when the store came
to believe a thing — and this records when the thing is true in the world, which
the chain cannot, so carrying both makes the store bitemporal. A record carries a
half-open interval `[From, To)` of canonical UTC instants, and `Retrieve` withholds
a record whose interval does not contain the query's `AsOf` instant, in the same
pre-ranking step that filters by scope, clearance, purpose and evidence class — so
its witness is a leakage test: remove the filter and a fact true only in 2025 is
presented as current in 2026. The load-bearing move is that the store reads no
clock — the as-of instant is carried on the query, supplied by the caller, exactly
as the reducer receives the clock as an event rather than calling `time.Now()`. The
one clock read an as-of query needs by nature happens in the caller, and the store
only compares canonical strings, which sort chronologically because every instant
is the one fixed-width UTC spelling. An empty as-of is the timeless floor rather
than a wildcard, mirroring the clearance floor: a caller naming no as-of sees only
records that declared no window. Both bounds are optional and an empty bound is a
stated claim ("no known end") rather than an unknown to refuse — requiring a `To`
would force a writer to invent an expiry nobody knows, the fabrication the
confidence and retention records already refuse — so `Validate` refuses only a
non-canonical instant or an interval no instant falls inside, and the guard that
keeps valid time from being decoration is the as-of filter, which fails on a bounded
record rather than a rule that every record be bounded. The interval is part of the
content-addressed version identity (ADR-0034), so a re-dating is a new version a
receipt can name rather than an in-place edit that could narrow a window a receipt
was issued under.

Probing ADR-0047's own carry-forward claim found the gap ADR-0048 closes, and it is
the failure shape this file keeps recording: a claim verified once and generalised
to a whole capability. ADR-0047's Decision says all four derived verbs carry the
window forward — `Correct`, `Delete`, `Promote`, `Resolve` — and its verification
listed one guard, over `Correct` alone. The three unguarded verbs were not merely
untested: removing the valid-time carry-forward from `Delete`, from `Promote`, or
from `Resolve`, each in isolation, left the whole suite green. The reason is that
valid time is the one identity dimension whose empty value is *legal* — `Validity{}`
is the timeless record ADR-0047 admits on purpose — so a dropped window passes
`Validate`, where the other six dimensions are caught incidentally because an empty
one of them is refused at the write. `Promote` and `Resolve` append retrievable
records, so a dropped window is the stale-fact disclosure the headline test prevents,
one verb over; `Delete` appends a tombstone, so its dropped window is a lineage loss.
ADR-0048 adds a guard per verb and records the asymmetry, so the other dimensions are
not each given a redundant test asserting what `Validate` already enforces.

Probing ADR-0048's delivery in turn found the same failure shape one artifact over, in
the retrieval evidence ADR-0049 closes. The `Retrieval.Selection` records, per returned
record, its identity and the full classification it was admitted under — scope,
sensitivity, purpose, evidence class, confidence and valid-time interval — the audit
trail this phase's exit evidence rests on, and the field ADR-0042 through ADR-0047 each
added claiming an audit could read it. No test asserted any of those structured fields:
blanking all nine of them at once left the whole suite green, so the witness was the
field nothing fails on, across every dimension. This is deliberately wider than valid
time — asserting only the valid-time bounds would repeat the verified-at-its-narrowest
error one more time — so ADR-0049 adds a single guard that reads the expected values off
the returned record and asserts every `Selection` field describes the record it names.
Like ADR-0048 it changes no production code: the witness was built correctly and only
unverified.

Probing ADR-0049's delivery found the companion gap one level up, which ADR-0050
closes. ADR-0049 witnessed each returned record; the `Retrieval` header — the
authorization envelope the whole retrieval ran under: `authorized_scopes`, `clearance`,
`authorized_purposes`, `authorized_evidence_classes` and `as_of`, the fields ADR-0027,
ADR-0042, ADR-0043, ADR-0044 and ADR-0047 each added claiming an audit could read them —
was almost entirely unasserted. Dropping the three set fields to nil or cross-wiring
them passed, and forcing clearance to the public floor passed because the one prior
assertion covers only the empty-query floor a floored bug satisfies; only `as_of` was
defended. So three of five envelope dimensions had no coverage and a fourth only its
narrowest point — the failure shape this repository keeps finding. ADR-0050 adds one
guard over the whole envelope, under a query with a non-public clearance and
multi-valued, duplicated, unsorted sets so a dropped, mis-sourced, undeduplicated,
unsorted or floored field is caught, with expectations derived from the query values.
Like ADR-0049 it changes no production code.

Probing ADR-0050's delivery closed the last unwitnessed content on the receipt, which
ADR-0051 guards. ADR-0049 witnessed each returned record and ADR-0050 the query
envelope; what remained was the receipt's own provenance — `schema`, the tag an audit
tool parses it by, and `retrieval_version`, the ranker identity Phase 7's exit evidence
names as the "ranking version". Blanking `schema` in the header left the whole suite
green, and the one prior check on `retrieval_version` asserts only non-emptiness, so a
wrong non-empty ranker name — a ranking credited to code that never ran — passed too;
only an empty version was caught. So one provenance field had no coverage and the other
only its floor, the same shape one field over. ADR-0051 adds one guard pinning both to
the package constants they must reflect, derived from the constants rather than
hand-copied so a const bump moves output and expectation together. Like ADR-0049 and
ADR-0050 it changes no production code, and with it every *structured* content
field of the `Retrieval` receipt — selected records, authorizing query, and the
evidence's own identity — is defended.

Probing ADR-0051's delivery found that "every content field is defended" had itself
outrun its evidence by one field: the receipt's one free-text field, the `reason`
on each selection, was still witnessed only at a single substring — the shape this
project keeps recording, a claim verified in one place and generalised to a whole
capability. ADR-0052 closes it. The exit evidence names "ranking/index versions and
reasons"; ADR-0051 pinned the versions, and the reason is the *reasons*. Each reason
carries four facts — the specificity rank, the principal, the tie-breaking
confidence, and the ranker version — but only the `confidence high` substring was
checked. With that substring left intact, crediting the ordering to a ranker that
never ran, printing the wrong specificity, and naming the wrong principal each left
the suite green: three of four facts unwitnessed and the fourth only a substring.
ADR-0052 adds one guard over every fact the reason names, each derived from the
returned record or the exported ranker constant so it witnesses the reason
describes the record it explains rather than matching a template a reword could
drift from. Like ADR-0049 through ADR-0051 it changes no production code, and with
it every content field of the `Retrieval` receipt — structured and free-text alike
— is defended.

One gap remains, and it is deliberate rather than pending. **Retrieval is not wired
into `internal/exec`**: which principals a run is authorized for is an identity
question, and the boundary above assigns identity, authentication and consent to
Asha, so wiring it now would mean inventing a principal from whatever the run
happens to know — the class of guess ADR-0022 exists to stop. Item 7's record
vocabulary is otherwise complete: ADR-0042 through ADR-0047 settled sensitivity,
purpose, evidence class, confidence, retention and valid time, the store is
bitemporal, and what is left of Phase 7 is that one wiring, not another dimension.


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
