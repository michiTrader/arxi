# ADR-0013: Confirmed history is projected once and prepared context is immutable before model dispatch

- Status: accepted
- Affects: `internal/exec`, `internal/provider`, `internal/transcript`, `internal/contextprep`, `internal/runconfig`, `spec/context.md`, `spec/events.md`
- Depends on: ADR-0002, ADR-0009, ADR-0010

## Context

A top-level turn currently presents the frozen opening prompt and the causes that
opened that turn, while prior conversation survives only inside one native
tool-call loop. A later turn does not receive earlier user input, model output,
tool evidence or human decisions. Reconstructing that material immediately before
a provider call would create a second problem: recovery could observe changed
configuration, indexes, tokenizers or memory and send different bytes for work
that already had one durable identity.

The reducer cannot solve either problem. Reading logs, artifacts, memory,
tokenizers or provider metadata there would make the fold depend on today's
external state and destroy replay.

## Decision

**The runtime projects a canonical transcript from a confirmed log prefix, then
records one immutable prepared-context artifact before preparing model work. A
committed prepared context is reused byte-for-byte by recovery and replay.**

The successful barrier is:

```text
context.prepare_requested -> context.prepared
  -> exec.work_prepared (turn_child/model) -> exec.work_started
```

`exec.work_prepared` already records the exact `arxi.turn/v1` request and is the
model-call request boundary. A second `model.call_requested` event would create
two records that could disagree about whether and what the runtime commissioned.

Transcript projection and preparation live outside `internal/kernel`. The reducer
continues to return `SpawnTurn` as pure intent; storage-derived content never
enters the effect or its digest.

### Canonical transcript

`arxi.transcript/v1` is a deterministic, lossless projection of confirmed events
and immutable artifacts cryptographically bound by them. Items retain source
sequence, event ID, within-source order, actor and audience. Native model and tool
items retain exact canonical child results, provider call IDs and argument bytes.
A path or current file is not an artifact identity.

The log remains authoritative. The transcript cannot suppress, reinterpret or
replace source events, and historical events without modern identity remain
explicitly legacy rather than receiving invented bindings.

### Prepared context

`arxi.prepared-context/v1` records exact ordered provider-neutral content, source
boundaries, frozen policy and model identities, token-measurement identity and
result, memory-use receipts, and distinct content and presentation digests. Exact
artifact bytes remain evidence; a digest is not a substitute for them.

Phase 5 does not compact or summarize. Unknown limits remain unknown and measured
estimates are labelled estimates. Overflow cannot cause silent truncation.

### Recovery

A preparation request freezes its source boundary and versions. An interrupted
request may be retried against those same inputs. Once `context.prepared` commits,
recovery verifies and loads it without calling the projector, memory, tokenizer or
preparer again. Missing bytes, a digest mismatch, an unknown schema or a second
value for one context identity is corruption and fails closed.

Started model work retains ADR-0009 and ADR-0010 semantics. Prepared context never
makes a non-idempotent provider call safe to redispatch.

## Discarded alternatives

**Rebuild context immediately before every provider call.** Rejected: a restart
could read newer events, memory, policies or tokenizer behavior and silently
change an already commissioned call.

**Store only a prompt digest.** Rejected: a digest proves equality only when the
exact candidate bytes still exist. It cannot show what the model saw or restore
those bytes after a crash.

**Put transcript state in the reducer.** Rejected: provider rounds, artifact
reads, tokenization and future memory retrieval are external observations. Folding
them from mutable stores would make replay time-dependent.

**Use provider-native message history as the transcript.** Rejected: it would
leak provider formats into durable runtime contracts and make equivalent OpenAI,
Anthropic and fake turns produce different histories.

**Treat snapshots or summaries as source history.** Rejected: snapshots are cache
and summaries are intentionally lossy. Either choice would let a derived artifact
silently rewrite the evidence from which it was made.

## Consequences

- Later turns can receive prior conversation and tool evidence without moving I/O
  into the kernel.
- Durable records grow because exact transcript and presentation bytes are kept.
- Context selection and measurement versions become compatibility contracts.
- Old runs remain replayable with their historical behavior but cannot claim a
  prepared-context proof they never recorded.
- Phase 6 may add compaction artifacts without changing or deleting the canonical
  transcript.
- Phase 7 may add memory receipts without granting memory authority to Phase 5.

## How it is verified

Projector tests require stable ordering, exact source references, audience rules,
native call/result identity and deterministic bytes. Preparation tests require
stable layer order, distinct content and presentation digests, honest token
measurement and no silent overflow behavior. Crash tests stop after request,
artifact publication and model-child boundaries; recovery must reuse the committed
artifact and must not invoke a deliberately failing preparer. Corruption tests
alter bytes, versions, boundaries and identities and require refusal before model
dispatch. Integration tests require a later turn to receive prior user, model and
tool evidence exactly once. Architecture tests continue to reject storage,
network, clock and tokenizer imports from the kernel.