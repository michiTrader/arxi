# 30. Product vision

## 30.0 Purpose and status

This document records where Arxi is intended to go and the constraints that must
survive the journey. It is not a roadmap, release promise, wire specification or
substitute for an architecture decision record.

Every capability statement uses one of these labels:

- **Current** — behavior that can be checked against the repository today.
- **Proposed** — a product direction that has not necessarily been implemented.
- **Not decided** — a question intentionally left open until evidence justifies a
  narrower contract.

[`10-execution.md`](10-execution.md) describes the execution machinery that exists.
[`20-use-cases.md`](20-use-cases.md) describes current user-facing scenarios.
The [ADRs](../adr/) record accepted decisions, and [`spec/events.md`](../../spec/events.md)
defines current event contracts. This document instead says what those mechanisms
should eventually make possible and which boundaries they must not cross.

## 30.1 Enduring vision

**Current.** Arxi is built around a pure reducer and an append-only event history.
Live execution, simulation, replay and diagnosis derive from the same decisions.
Blueprints describe individual agents and teams with explicit models, tools,
policies, stages, budgets and activation rules.

**Proposed.** Arxi should become a portable, lightweight and efficient agent
runtime that can support both complete products and applications embedding only
the engine. It should:

- coordinate one agent or a deeply customized team without changing runtimes;
- make costly or consequential behavior inspectable, reproducible and bounded;
- expose modular providers, tools, storage and host integrations through public
  contracts;
- preserve a small core and make optional capabilities pay only for themselves;
- support long-lived work through durable execution, context continuity and
  scoped memory;
- let agents help improve software without letting them redefine their own
  authority.

Portability and efficiency are product requirements, not afterthoughts. The
standard-library-only static binary is a useful current property; future extension
points must justify any cost they add to deployment size, startup, memory, latency
or operational complexity.

## 30.2 Product boundaries

### Arxi Core

**Current.** The engine is modular inside the repository, but its reusable packages
live under `internal/` and the composition root lives in `cmd/arxi`. External
applications can use the CLI and the partially implemented NDJSON service; there
is no supported in-process SDK.

**Proposed.** Arxi Core should expose a small, versioned host surface for execution,
providers, tools, storage, memory and observability. Hosts should be able to embed
the runtime without invoking the CLI, while the kernel and concrete adapters can
remain private implementation details. Publishing the current internal types
unchanged would freeze too much machinery and is not the intended shortcut.

### Arxi CLI

**Current.** This repository provides a broad CLI over the runtime. A separate CLI
experience is also being developed for later integration.

**Proposed.** The Arxi CLI should be a general-purpose agent environment capable of
serving the jobs for which users choose tools such as Claude Code, Pi or Gemini
CLI. Its advantage should come from portable execution, configurable teams,
provider choice, efficient context handling and auditable autonomy—not from
copying another product's interface. A separately shipped CLI should consume a
public SDK or protocol instead of importing `internal/` packages or duplicating
the runtime.

### Asha and other product adapters

**Proposed.** Asha should be a voice-first application built on Arxi, not a fork or
an application-specific mutation of its kernel. Asha owns the personal experience:
voice transport, user-facing identity, interface state, consent flows and product
policies. Arxi owns general execution: teams, jobs, tools, memory contracts,
scheduling, budgets and auditability.

OpenAI Realtime is a viable first voice backend, not a dependency of Arxi Core.
Asha should place realtime speech behind a replaceable provider boundary and expose
its interface through explicit capabilities. It may assemble specialist roles or
teams for personal organization, research, communication and other domains, but a
single permanent team topology should not be baked into the engine. The product
chooses and evolves that composition under user-controlled policy.

**Not decided.** Repository topology, packaging, process boundaries and whether a
given host uses in-process calls, IPC or a remote worker remain open.

## 30.3 Architectural invariants

Future work must preserve the decisions already defended by
[`10-execution.md`](10-execution.md) and the [ADRs](../adr/):

1. The pure reducer decides; effects perform external work.
2. The event log is authoritative. Snapshots, indexes and summaries are derived
   artifacts and cannot silently rewrite history.
3. Live execution, replay, simulation and diagnosis retain compatible semantics.
4. A run uses frozen configuration. Later configuration changes do not alter its
   past.
5. Cost, depth, authority and side effects remain explicit and bounded.
6. Optional capability does not imply ambient authority.
7. Products such as Asha consume public host contracts rather than reaching into
   engine internals.
8. An agent cannot expand its own permissions, replace its governing policy or
   promote its own runtime changes.

The event log need not contain every private datum forever, but it must retain
enough immutable identity, provenance and outcome information to explain the
behavior that depended on that datum. Privacy and auditability must be designed
together rather than solved by copying sensitive content indiscriminately.

## 30.4 Control plane and data plane

Arxi should distinguish the work an authorized run performs from the rules that
make that work permissible.

**Proposed runtime data plane:**

- execute frozen blueprints and already approved capabilities;
- call models and tools within granted policy and budget;
- read or write memory only within explicit scopes;
- schedule and resume approved jobs;
- inspect evidence, generate candidate changes, run tests and evaluate results;
- record causal outcomes and resource usage.

**Proposed operator-owned control plane:**

- manage credentials, models, prices, policies and approval gates;
- publish versioned agents, teams and capabilities;
- configure memory privacy, retention and deletion policy;
- define evaluation and promotion criteria;
- build, sign, stage, revoke and roll back executable artifacts.

The data plane may request a control-plane action and supply evidence for it. It
must not perform the approval through another route. This boundary preserves the
current principle that commands withheld from agents cannot be obtained
transitively through automation.

## 30.5 Capability horizon

These are coherent capability groups, not implementation phases or release dates.

### Embeddability and extension

**Proposed.** Provide a public runtime lifecycle, provider adapters, host tools,
storage adapters and end-to-end model tool calling. CLI, protocol, triggers and
agent tools should invoke common capability implementations rather than advertise
parallel surfaces with different wiring.

Optional third-party capabilities should be versioned, attributable and governed
by declared inputs, outputs, privileges, side effects and resource limits. Whether
they execute as compiled adapters, isolated subprocesses, WASM components or
remote workers is not decided. Runtime mutation by an untrusted plugin is not a
requirement.

### Durable execution

**Current.** Coordinated storage records durable jobs, stable trigger
occurrences, fenced attempts, heartbeats, checkpoints, dispatch registrations,
external receipts and periodic ledger settlement. Under that installed
contract, accepted work survives process restarts and stale workers cannot
commit. Process-local storage remains a supported reduced-capability fallback;
it must not advertise coordination or restart guarantees it cannot provide.
Workspace modes provide only the isolation their names claim, with
platform-specific behavior tested negatively. The service surface exposes only
operations that have real, authorized handlers.

**Proposed.** Multi-host coordination backends and additional source-backed or
contained-process profiles remain future capabilities until their complete
operational and platform guarantees are implemented and verified.

### Context continuity

**Proposed.** Context management should support compaction under token pressure
while leaving the source transcript intact. A compacted context should combine an
incremental summary of older work with a recent verbatim window. It should preserve
goals, constraints, progress, decisions, next steps and critical artifacts, and it
should summarize abandoned branches when navigation would otherwise lose useful
work.

These principles are informed by Prime Agent and Pi, but they do not commit Arxi
to either project's formats, hooks or implementation.

### Cross-run memory

**Proposed.** Persistent memory should be an optional capability with explicit
scopes such as user, application, project, team, agent and run. A record must carry
provenance, time, confidence and retention information sufficient to inspect why
it exists and where it may be used.

User-provided facts, preferences, observed events and model-derived conclusions
are different classes of evidence. Retrieval must respect authority and token
limits, and the exact retrieved material that influenced a run must be auditable.
Users and authorized hosts need mechanisms to inspect, correct, supersede, export
and delete memory. Memory content is data, not trusted instructions.

### Reflection and consolidation

**Proposed.** Reflection should turn run evidence into candidate lessons, errors,
decisions and follow-up work. Candidates remain claims until checked against their
sources and policy.

A background consolidation lifecycle may use a Light/REM/Deep-inspired structure:
collect new evidence, identify recurrence or contradiction, then critically decide
what to promote, merge, supersede, retain temporarily or discard. Only the final,
governed stage changes durable memory. The process must be idempotent, budgeted,
observable and safe to retry after interruption.

### Product adapters

**Proposed.** The same core should support a rich coding CLI, Asha's voice and UI
integration, and applications that provide their own models, tools or user
experience. Product-specific identity and policy remain outside the universal
kernel.

### Governed evolution

**Proposed.** A request to "evolve" should initiate a controlled improvement
pipeline, not unrestricted self-modification:

```text
proposal -> validation and tests -> eval comparison -> security review
         -> human approval -> build and signing -> staged promotion
         -> observation -> retain or roll back
```

An agent may inspect code, produce a patch, explain trade-offs, execute authorized
tests and gather reproducible evidence. It may not approve its own proposal,
elevate its privileges, alter the evaluation gate, sign the result or replace the
active runtime. Promotion and rollback belong to principals outside the candidate
run.

## 30.6 Four different continuity mechanisms

Compaction, memory, reflection and dreaming solve different problems. Implementing
one does not imply the others.

| Mechanism | Purpose | Typical lifetime | Treatment of information |
|---|---|---|---|
| Compaction | Keep an active task coherent inside a finite model context | Conversation or run | Intentionally lossy summary plus recent verbatim context |
| Cross-run memory | Retrieve useful information in later work | Across runs | Selective, scoped and provenance-bearing |
| Reflection | Derive candidate lessons from outcomes | Post-run or background | Revisable claims tied to evidence |
| Dream consolidation | Maintain the quality of persistent memory | Background lifecycle | Policy-governed promotion, merging, supersession and forgetting |

**Current.** `ContextSpec.Memory` is static prose from the frozen blueprint.
`ContextSpec.OnOverflow` defaults to `summarize`, but no runtime path currently
implements that policy. The event log preserves execution evidence, but provider
calls do not reconstruct a Prime/Pi-style conversation from the old transcript.
Snapshots cache workflow configuration or state; they are not context summaries.

**Proposed.** Compaction should react to measured model-context pressure, retain the
original history and persist enough boundary metadata to reconstruct what the
model saw. Persistent memory should be queried through an external effect or
preparation step whose returned IDs and exact content are recorded before they
influence a decision. Reflection and dream consolidation should never silently
turn generated prose into trusted user truth.

## 30.7 Current baseline and gaps

| Area | Current | Proposed direction |
|---|---|---|
| Runtime modularity | Internal packages have strong boundaries, but the runtime is compile-time closed | A small public embedding surface and governed capability adapters |
| SDK and plugins | No supported in-process SDK or dynamic plugin contract | Versioned host, provider, tool and storage contracts |
| Model tool use | OpenAI Chat Completions and Anthropic Messages share a durable provider-neutral loop with exact call-ID/result reinjection | Extend canonical content support and expose a separate versioned public turn contract only when external native providers require it |
| Prompt memory | `ContextSpec.Memory` is frozen blueprint text | Optional scoped cross-run memory with provenance and user control |
| Context overflow | `on_overflow: summarize` is declared but not executed | Tested compaction with an intact source transcript |
| Scheduling | Coordinated deployments have durable occurrences, fenced attempts, checkpoints, receipts and ledgers; process-local storage remains an explicitly reduced-capability fallback | Extend the specified guarantees to additional coordination topologies without weakening fencing or unknown-outcome semantics |
| Service integration | The declared protocol surface is broader than its implemented handlers | One honest capability implementation projected across adapters |
| Workspace isolation | Exact requirements, platform decisions and pre-accept lifecycle are implemented and fail closed. Native Windows advertises only `none`/`no-tools`; native Linux adds `direct-files` but advertises no source-backed mode. Internal shared/copy/worktree provisioners and process containment components are not production availability. | Advertise source-backed and contained-process profiles only after the production capability decision can prove the complete contract on each supported platform |

This table is deliberately about architectural gaps rather than a count of
commands or tools. Volatile counts belong beside the tests that keep them current.

## 30.8 Explicit non-goals

This vision does not:

- define new event names, commands, Go interfaces, database schemas or plugin ABI;
- turn Asha into a fork of Arxi;
- make OpenAI Realtime or any provider a core dependency;
- treat context compaction as personal memory;
- expose every memory scope to every agent;
- allow self-approval, self-escalation or unreviewed runtime replacement;
- promise command-level compatibility with another CLI;
- replace ADRs, specifications or tests with aspirations;
- promise dates or an implementation order.

## 30.9 Open decisions

The following remain **Not decided**:

- the first stable shape and compatibility policy of the public SDK;
- in-process embedding versus local IPC or remote execution for each host;
- compiled adapters versus a capability package or plugin boundary;
- provider capability negotiation and realtime voice abstractions;
- memory backend, encryption, synchronization, portability and deletion model;
- consolidation cadence, scoring and promotion policy;
- durable scheduler backend and multi-instance coordination;
- artifact signing, attestation and promotion infrastructure;
- repository and release relationships among Core, the CLI and Asha;
- quantitative success thresholds for quality, cost, latency and memory accuracy.

Each decision should become a focused ADR or specification only when the trade-off
can be defended and verified.

## 30.10 Success criteria

The direction in this document is realized when evidence can demonstrate that:

- an application embeds Arxi without shelling out to the CLI;
- an external provider completes model and tool calls through public contracts;
- one-agent and team runs remain replayable and explainable from durable artifacts;
- process restarts do not silently lose accepted background or scheduled work;
- workspace isolation claims survive adversarial tests on supported platforms;
- compaction preserves task continuity without altering the source transcript;
- memory retrieval explains provenance, obeys scope and supports correction and
  deletion;
- Asha can change realtime providers without changing the kernel;
- a candidate evolution cannot reach an active release without independent
  evaluation, authorization and a traceable promoted artifact;
- every promoted runtime has a tested rollback path.

These criteria intentionally omit target numbers. Evals and operating evidence
must establish those thresholds before they become promises.

## 30.11 Document responsibilities

- This vision says **where Arxi is going and which boundaries constrain it**.
- Design documents say **how the current system behaves**.
- ADRs say **what was accepted, what was rejected and what verifies the choice**.
- Specifications say **which contracts implementations must obey**.
- Use cases say **what a user can complete with the system today**.

When an item moves from **Proposed** to **Current**, its implementation, tests and
appropriate ADR or specification must land before this document changes its label.
