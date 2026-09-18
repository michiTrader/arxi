# ADR-0027: Governed memory is a store outside every run, and retrieval is authorized before it is ranked

- Status: accepted
- Affects: `internal/memorystore`, `internal/contextprep`
- Depends on: ADR-0002, ADR-0013, ADR-0020, ADR-0021, ADR-0022, ADR-0023, ADR-0024, ADR-0025
- Enables: Phase 7 (read-only governed memory)

## Context

Seven prerequisites for Phase 7 were recorded as settled before this record.
Every one of them is a rule about a `contextprep.MemoryReceipt`: which channel
it arrives on (ADR-0020), that it names a version (ADR-0021), which principals
may scope it (ADR-0022), which kinds carry authority (ADR-0023), that the
preparer validates it (ADR-0024), that every assembler uses its channel
(ADR-0025), and that the roadmap cites the decisions claiming to enable it
(ADR-0026).

Following this project's own rule — probe the output of the last turn rather
than read it — the production build was measured before designing on top of
those seven:

| measurement | count |
| --- | --- |
| production construction sites of `KindApprovedMemoryRecord` | 0 |
| production construction sites of `KindProposedMemoryCandidate` | 0 |
| production assignments of `MemoryReceipt.RecordID` | 0 |
| production `MemoryReceipt{...}` literals | 2, both `frozen_context_memory` |

The channel was a finished pipe with nothing flowing through it. The version
rule, the vocabulary and the authority enumeration were reachable only from
tests, because nothing in the system could produce a governed record for them to
govern. `Governed()` and `Presentable()` each had exactly one production caller,
both inside the validator ADR-0024 added.

That also answers the question this record exists to answer, which arrived as a
user report that an agent remembers nothing between runs. Three mechanisms look
like they might carry memory across runs and none does:

- **`ContextSpec.Memory`** is frozen blueprint prose. It is identical for every
  run of that blueprint, so it cannot carry anything a run learned. It is also
  the only memory any code path presents today.
- **`ContextSpec.Shared`** is within-run team material, assembled into the
  system message for members of one blueprint.
- **`run fork`** copies a parent's event prefix into a new run directory. That
  is continuation of one history, not recall across histories, and
  `cmd/arxi/fork.go` documents why it must not even be reported as lineage: the
  copied prefix already contains the parent's `llm.response` events, so summing
  them as a tree child would double-count every dollar spent before the fork.

The reason is structural rather than an omission. A run's truth is its event log
(ADR-0002), and a log is per-run by construction: `state = fold(events)` over
*this* run's events. Nothing living inside a run directory can be read by a run
that does not exist yet. Forgetting between runs is not a gap in the memory
channel; it is the absence of any store outside the run.

## Decision

Governed memory is an append-only store of immutable record versions, living
outside every run directory, and retrieval authorizes before it ranks.

**Versions are values.** Every version is its own file named by a
content-addressed version ID. Correction, promotion and deletion all append a
new version superseding the previous one; nothing is edited in place. ADR-0021's
guarantee — a receipt names the version it presented — is only true if a
version's bytes cannot change after the receipt was issued.

**The current version is derived, never flagged.** A version is current when
nothing supersedes it, computed by walking the chain on every read. A `current`
pointer or an `active` flag would be a cache, and ADR-0002 already settled this
project's position on caches that can disagree with the truth. A forked chain —
two versions superseding the same predecessor — is refused rather than resolved
by a tiebreak, because every available tiebreak (file order, digest order,
mtime) is deterministic and unrelated to which correction the user meant.

**Deletion is a tombstone.** Phase 7 requires deletion to propagate "without
resurrection". Unlinking files removes the record from one copy of the store and
from nothing else: any replica, backup or synced directory still holding the
bytes reintroduces it, and nothing in the data says it was deleted. A tombstone
is a positive assertion that travels with the data.

**A colliding version file is verified, not trusted.** Content addressing makes
an `O_EXCL` collision normally harmless — the same name means the same bytes —
which is exactly why the one harmful case had no witness. If a file was modified
after it was written, its name no longer describes its content, and the first
implementation returned success while the disk held a forged body, handing the
caller a receipt for content the store does not have. The existing file is
decoded and re-sealed, and a divergence is refused without repairing the file:
overwriting it would erase the only evidence that a version somebody holds a
receipt for was tampered with. This was found by a surviving mutation, not by
review.

**Authorization strictly precedes ranking.** The candidate set is built from the
caller's authorized scopes and the ranker never receives anything else. Ranking
first and filtering after means the ranker has seen the whole corpus, so a bug
leaks from everywhere rather than from one scope — and a relevance score
computed across tenants is itself a cross-tenant inference even when the record
is dropped afterwards.

**Scope matching is exact.** A record scoped `project:arxi` is not returned to a
caller holding `tenant:acme`, even if that project belongs to that tenant.
Implying containment would require this package to know the tenant of every
project and the membership of every team, which lives in whatever product owns
identity. Prefix matching would authorize `project:arxi-secret` for a caller
holding `project:arxi`.

**There is no semantic ranker, and the code says so.** Retrieval orders by scope
specificity — what this run established outranks what the tenant believes in
general — and every selection states that rule as its reason. Calling it
semantic would be the unearned claim this project keeps finding in its own
documents.

**ADR-0022's vocabulary becomes a type.** That ADR deliberately added no scope
field anywhere, on the grounds that "a scope type with no store to authorize
would be a field nothing fails on". The store exists now, so the type exists
now. `subject` is refused with a message naming `user` and `agent` as the
replacements, because the roadmap itself said `subject` before ADR-0022 and a
reader working from a stale copy will type it.

**Retrieved memory reaches the preparer as data.** `contextprep.Request` gains
rendered text plus receipts rather than records: contextprep owns the receipt
vocabulary the store depends on, so the reverse import would be a cycle, and
deciding what memory is relevant is not the preparer's job. Those receipts pass
through the same validation loop as the frozen one, so the barrier cannot be
reached with an unvalidated receipt by any route — which is also the second of
two independent refusals of a proposed candidate, because one point of
enforcement is one point of regression. Memory without receipts, or receipts
without memory, fails the preparation: the first presents an influence the
artifact cannot attribute, the second attests to one that never reached the
model.

## Alternatives rejected

**A memory event in the run log.** Rejected because it puts memory back inside
the one boundary that cannot cross runs, and because the reducer is pure:
retrieval is I/O against a store, which is an effect, and ADR-0001 is explicit
that the kernel describes effects rather than performing them.

**Mutable records with a version counter.** Simpler to read and it breaks
ADR-0021: a counter needs a writer with exclusive state to allocate from, so
concurrent runs either serialize on a lock or mint colliding IDs, and an
in-place edit makes every issued receipt describe bytes that have since changed.

**Hierarchical scope expansion.** Letting `tenant:acme` retrieve everything
beneath it. Rejected as above — the containment mapping is not this package's to
know, and string-prefix containment is a leak.

**One mutable row for the candidate and the approved record.** Rejected because
an audit asking whether a presented record was originally model-generated must
be able to answer yes. Promotion appends and keeps the candidate version.

## Consequences

The three `MemoryReceipt` kinds now all have production meaning, and
`RecordID`/`VersionID` are populated by something other than a test for the
first time. Phase 7's exit evidence is testable: cross-scope leakage, correction
propagation, deletion without resurrection and per-influence source
identification each have a witness that fails when the mechanism is removed.

Reading a record costs a directory scan and a chain walk. Accepted at this size
and honest in shape; if it becomes the bottleneck, the answer is an index whose
staleness is detectable, not a mutable pointer.

Retrieval is not wired into `internal/exec` by this record. The preparer accepts
retrieved memory and the store produces it; which principals a given run is
authorized for is an identity question, and the roadmap assigns identity,
authentication and consent to Asha rather than to Arxi. Wiring it before that
boundary is decided would mean inventing a principal from whatever the run
happens to know, which is the class of guess ADR-0022 was written to stop.

Temporal validity, evidence class, confidence, sensitivity, purpose and
retention are not implemented. They are enumerated in Phase 7's own list and
still need their own record; adding them as struct fields with nothing
authorizing them would be the "field nothing fails on" defect this corpus has
now recorded four times.
