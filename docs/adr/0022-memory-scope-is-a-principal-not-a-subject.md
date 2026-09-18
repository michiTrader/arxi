# ADR-0022: Memory scope names a principal, and `subject` is not one

- Status: accepted
- Affects: `docs/roadmap.md`, `spec/context.md`, `internal/contextprep`
- Depends on: ADR-0020, ADR-0021
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7's scope list is the first thing a memory store has to implement, because
authorization runs before ranking and a store cannot authorize across a
dimension it has no field for. So the list was compared against the two other
places this project names scopes, before designing anything that consumes it.

The three disagree.

**`docs/design/30-vision.md`** — the product constraint:

> Persistent memory should be an optional capability with explicit scopes such
> as **user, application, project, team, agent and run**.

**`docs/roadmap.md`** — Phase 7's authorization dimensions:

> Authorization occurs before semantic ranking across **tenant, subject,
> application, project, team/agent, run**, evidence class, sensitivity, purpose
> and valid time.

**The code** — five artifacts already commit a field named `Subject` to the
wire as `subject_agent`, in `internal/transcript`, `internal/contextprep`,
`internal/compaction` and `internal/exec`. In every one of them it is populated
from `effect.Agent`:

```go
Subject: effect.Agent,
```

Three problems, in increasing order of cost.

**`user` became `tenant` and `subject` with no record saying so.** The vision
says `user`. The roadmap says `tenant` and `subject`. Nothing decided that
split, and the two are not the same shape: a tenant is an isolation boundary, a
user is a person, and a single tenant holds many users. A store built from the
roadmap gets tenant isolation; one built from the vision gets per-user
isolation. The leakage test in the exit evidence — "cross-user and
cross-project leakage tests return zero records" — is written in the vision's
vocabulary, so it would be testing a boundary the roadmap's store does not
necessarily have.

**`subject` collides with an existing committed identifier.** In the roadmap,
`subject` and `team/agent` are separate dimensions. In the code, `Subject` *is*
the agent. Anyone implementing memory scoping reads `subject` in the roadmap,
finds `Subject` in `contextprep.Artifact`, and wires the scope to the field that
already exists. They then have a store whose `subject` scope is its agent scope,
silently, with the roadmap's separate `team/agent` dimension unimplemented
and nothing failing.

**The vision omits `tenant` entirely**, which is the one dimension whose absence
is a containment bug rather than a missing feature. Every other scope narrows
retrieval within a trust boundary; a tenant *is* the trust boundary.

This is the same class of defect as ADR-0021, one level up. There the gap was a
decision record describing a field the struct did not have. Here it is three
documents describing scopes with different names and no record reconciling them,
in the list a store must implement first.

## Decision

Memory scope is a set of **principals**, and `subject` is not one of them.

The canonical scope vocabulary for Phase 7 is:

| scope | what it isolates |
| --- | --- |
| `tenant` | the trust boundary; no retrieval ever crosses it |
| `user` | a person within a tenant |
| `application` | an external product adapter (Asha is one) |
| `project` | a body of work |
| `team` | a blueprint with several members |
| `agent` | one member of a blueprint |
| `run` | a single execution |

`subject` is **removed** from the vocabulary rather than defined. It has no
meaning that `user` or `agent` does not already carry, and it already denotes
the subject agent in five committed artifacts. Keeping it would mean one word
naming two things in the same system — and the reader who conflates them gets a
store that authorizes the wrong dimension while looking correct.

`tenant` is **added** to the vision's list, because an isolation boundary that
only the roadmap mentions is not a stated product constraint.

The code keeps `Subject`/`subject_agent` unchanged. It is correct in its own
context — a transcript does have a subject agent — and renaming it would
rewrite five artifact schemas to fix a documentation collision. The collision is
resolved by the scope vocabulary not using the word, not by the code
surrendering it.

This ADR governs **the vocabulary**. It deliberately decides nothing about how
records are stored, ranked, retained or deleted, and it does not add a scope
field to any struct: there is no store yet, and a scope type with no store to
authorize would be a field nothing fails on, which is exactly what ADR-0021 was
written about.

## Consequences

Phase 7's retrieval design starts from one scope list instead of reconciling
three, and the leakage test in the exit evidence is now written in the same
vocabulary the store will implement. "Cross-user" means `user`, and `user` is a
scope.

`tenant` becoming a stated scope has a consequence worth naming: it is the only
scope where the correct behaviour on a missing value is refusal, not a wider
search. A record with no `project` may legitimately be visible across projects;
a record with no `tenant` must not be visible at all. That asymmetry is why it
belongs in the vocabulary before the store exists rather than being discovered
while writing the query.

Nothing in the running system changes. No struct gains a field, no artifact
changes shape, and `presentation_digest` is untouched. This ADR is cheaper than
ADR-0021, which was already cheaper than ADR-0020, and the descent is not a
coincidence: each one moved a boundary that had not yet been built against.
Deciding the vocabulary is free now and expensive once a store has authorized
records under it.

## Discarded alternatives

**Define `subject` as "the principal a record is about", distinct from the agent
that retrieves it.** This is a real and useful distinction — a record about
Alice retrieved by the backend agent — and it was the most attractive option,
because the roadmap's phrasing suggests exactly this reading. It is rejected on
the collision alone: `Subject` already means the subject agent in five committed
wire schemas, so the word would denote the record's topic in the scope
vocabulary and the retrieving agent in every artifact beside it. The distinction
survives without the word — a record *about* a user is expressed by its `user`
scope, which is what that scope is for.

**Keep all three lists and reconcile them in Phase 7's retrieval ADR.** The
argument ADR-0020 rejected for the channel, failing the same way. The retrieval
ADR would have to choose a vocabulary silently while appearing to be about
retrieval, and the choice would land in code before anyone noticed a choice had
been made.

**Rename the code's `Subject` to `SubjectAgent` so `subject` is free.** Honest
but disproportionate: five artifact schemas, each with a committed JSON field,
changed to free up a word for a store that does not exist. The wire form
`subject_agent` is already unambiguous; only the Go identifier is short, and it
is short inside packages where nothing else is a subject.

**Adopt the vision's list unchanged, since the vision is the product
constraint.** Rejected for one word: it has no `tenant`. Every other difference
between the lists is vocabulary, but omitting the trust boundary is a
containment gap, and a scope list without it would authorize correctly within a
tenant and say nothing about crossing one.

## How it is verified

A vocabulary decision has no runtime behaviour to pin, and inventing one would
produce exactly the decoration ADR-0021 forbids. What can be verified is that
the documents no longer disagree, and that is what
`internal/contextprep/memory_scope_vocabulary_test.go` asserts, against the
committed files rather than against a copy of the list:

- **Every scope in the vocabulary appears in both `docs/roadmap.md` and
  `docs/design/30-vision.md`.** This fails if a future edit adds a scope to one
  document and not the other, which is the defect this ADR corrects.

- **The word `subject` does not appear in either document's memory scope
  list.** Asserted against the specific lines, not the whole file, because both
  documents legitimately use "subject agent" elsewhere. This is the assertion
  that would have caught the original collision.

- **`tenant` appears in both.** Stated separately from the first assertion
  because it is the scope whose omission was a containment gap rather than a
  naming inconsistency, and a test that only compared the two lists to each
  other would pass on two lists that both omitted it.

The test reads the repository's own documents, so it fails when the documents
drift — which is the only failure mode a vocabulary can have.
