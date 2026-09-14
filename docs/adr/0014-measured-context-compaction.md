# ADR-0014: Context pressure is measured against explicit budgets and compaction is a verified lossy artifact beside an intact transcript

- Status: accepted
- Affects: `internal/contextprep`, `internal/compaction`, `internal/transcript`, `internal/exec`, `internal/contextruntime`, `spec/context.md`, `spec/events.md`
- Depends on: ADR-0013

## Context

Phase 5 freezes exactly what a model sees but performs no lossy selection: the
prepared presentation carries the whole canonical transcript, and a run that
grows long enough either gets rejected by the provider or gets shortened by
whoever is least careful. `on_overflow: summarize` has been declared in
`ContextSpec` since the kernel first assembled context, and `spec/context.md`
explicitly deferred honoring it until a versioned compaction artifact existed.

Two prerequisites are missing. First, nothing measures pressure: the Phase 5
token measurement records one total and never compares it against the limit, so
"overflow" is not a fact the runtime can act on. Second, intermediate native
rounds inside one tool loop became transcript items in Phase 5, but the items
carry no binding to the child records they came from, so any Phase 6 range or
omission that leans on them would not be auditable against durable execution
records.

## Decision

**Preparation measures every stable layer against an explicit versioned budget,
and when measured pressure exceeds a known input limit the preparer produces one
verified compaction artifact. The canonical transcript is never modified; only
the presentation loses material, and everything the presentation loses is
recorded by identity.**

### Pressure and budgets

Measurement is per layer (static, summary, verbatim, input) plus total, with
the input and output limits recorded when known and absent when unknown. An
unknown limit means pressure is unknown: no compaction is triggered and nothing
is guessed. Layer budgets are the versioned policy `arxi.context-budget/v1`,
derived deterministically from the known limit in fixed quarters (static 1/4,
summary 1/8, verbatim 1/2, input 1/8) and recorded inside the artifacts that
obeyed them. Total pressure, not a single layer, is the trigger.

### Overflow behavior

Over a known limit with `on_overflow: summarize`, preparation compacts. Over a
known limit with any other declared value, preparation fails visibly through
the existing terminal `context.prepare_failed`. If compaction cannot bring the
measured total within the limit — the static layer alone can exceed it — the
failure is visible, never a silent trim.

### The compaction artifact

`arxi.compaction/v1` contains a lossy summary plus a recent verbatim window,
and records source ranges over transcript item identities, the retained
critical items, the omission ledger, the generator identity and version, the
budget policy used, and before/after token counts.

The default generator is deterministic and extractive. Every summary claim
cites the item IDs it was extracted from, and the artifact is verified before
it may exist: each claim's text must be contained in the concatenated text of
exactly its cited items. A claim that cannot be proven against its citation
fails closed. This makes "summaries do not invent unsupported facts" a
structural property of the committed artifact rather than a hope about model
behavior.

Continuity anchors — user inputs, human decisions, and the final model output
of each completed turn — are always either inside the verbatim window or cited
by a claim. They cannot be silently dropped. The verbatim window is a suffix of
the item order that never splits a tool call from its result.

### Placement

Compaction rides inside the existing one-batch ADR-0013 barrier. There is no
`context.compacted` event: one preparation identity has one committed value,
and the compaction bytes, digest and overflow decision travel in the
`context.prepared` payload beside the transcript and prepared-context bytes.
The preparer version advances to `arxi.context-preparer/v2` because the
prepared-context artifact gains the overflow decision, per-layer measurement
and the embedded compaction. Recovery of a committed preparation reuses the
bytes and never invokes the generator; containment is a generation-time gate
protected afterward by the digest chain.

### Transcript item identity

`model_output` items projected from native children now carry the child work ID
and the provider response ID as kind-specific identity, so compaction ranges
and omission ledgers bind to durable execution records. Historical artifacts
without the binding replay unchanged.

## Discarded alternatives

**Compact the transcript itself.** Rejected: the transcript is the evidence
from which the summary is proven. Rewriting it would let a derived artifact
silently replace the source it summarizes, violating ADR-0013's invariant that
the log and its lossless projection remain authoritative.

**A separate `context.compacted` event.** Rejected: it would create two records
that can disagree about one preparation identity, the exact drift ADR-0013
refused for `model.call_requested`.

**A generative summarizer as the default.** Rejected: nothing structural stops
a generative summary from inventing facts, and a wrong summary is worse than a
long prompt because it is trusted. The generator port permits one later, but
the containment gate stays: generated prose that cannot be proven against its
citations does not commit.

**Provider-side truncation or prompt caching as relief.** Rejected: both are
silent, provider-specific and invisible to replay; the system could no longer
prove what the model saw.

**Shrink the window until it fits, without an artifact.** Rejected: silent
truncation with extra steps. Anything not presented must be recorded by
identity or not dropped.

## Consequences

- Long runs present bounded input while durable records keep growing; storage
  cost is the price of provable history.
- Summary quality is bounded by extraction; the artifact proves provenance of
  every claim, not judgment.
- Preparation-time verification cost grows with history length; recovery pays
  only digest checks.
- A model-backed generator remains future work behind the same verification
  gate; this decision does not add one.
- Runs prepared under v1 replay unchanged; v2 artifacts appear only for new
  preparations.

## How it is verified

Budget and pressure tests pin the derivation, the unknown-limit behavior and
the fail-closed path for non-summarize overflow. Compaction tests require
deterministic bytes, containment rejection of an invented claim, anchor
survival, window pair integrity, omission-ledger completeness and before/after
accounting that actually relieves pressure. Transcript tests prove the
projected artifact is byte-identical before and after compaction and that
native items carry child identity. Barrier tests prove the compaction bytes
commit in the same batch as the preparation, that recovery and replay reuse the
committed value with a generator call count of zero, and that tampered
compaction bytes are refused before model dispatch. The legacy path and the
reducer/loop inertness of `context.*` records remain pinned by their Phase 5
tests.
