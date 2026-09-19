# ADR-0028: Superseding a version claims it exclusively, and a fork that still arrives is contained to its own record

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0002, ADR-0006, ADR-0021, ADR-0027
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0027 decided that a forked supersession chain — two versions superseding
the same predecessor — is **refused rather than resolved by a tiebreak**, because
every available tiebreak (file order, digest order, mtime) is deterministic and
unrelated to which correction the user meant. That reasoning is right and is not
revisited here.

What was never measured is what "refused" costs, and who pays it. Following this
project's rule to probe the output of the last turn rather than read it, a
throwaway probe built one forked record in a store holding a second, unrelated
record belonging to a **different tenant**, and then asked what still worked:

| operation, after one record forks | result |
| --- | --- |
| `Retrieve` for the *other* tenant's scope | refused |
| `Delete` the forked record | refused |
| `Delete` the *unrelated* record | refused |
| `Correct` / `Promote` any record | refused |
| `Put` a version superseding one fork tip | accepted, and changes nothing |
| `Versions` | works — the only verb that does |

Every one of those refusals carries the same message, naming two version IDs of
a record the caller may not be authorized to see.

Two separate defects are visible in that table, and they compound.

**The refusal is global, not per record.** `tips()` walks every version in the
store and returns an error for the whole set, so `Retrieve` fails before
authorization runs. One corrupt record in one tenant denies memory to every
tenant in the store. ADR-0027 argues at length that authorization must strictly
precede ranking so that a bug leaks from one scope rather than from the corpus;
this is that argument's mirror image, and it was not made. A fault in one scope
becomes a denial of service across all of them, and the error text discloses
version IDs across the tenant boundary that the same ADR calls the one boundary
no retrieval crosses.

**The refusal is permanent.** `Correct`, `Delete` and `Promote` all resolve the
current version through `tip()`, which calls `tips()`, which is exactly the
function that fails. So the three verbs that could repair a fork are the three
the fork disables. `Put` still works, because it bypasses `tips()` — and
superseding one of the two tips does not help: the original predecessor still
has two successors, so the chain is still forked. The probe confirmed the store
is unrecoverable through its own API. Phase 7 promises "inspection, correction,
supersession, export and deletion controls"; after a fork, four of the five are
gone, and the remaining one is a raw version dump.

That is bad in proportion to how the fork arrives, and it does not require a
tampered disk or a hostile replica. A second probe reached it through the
supported API, with no `Put`:

```go
go s.Correct("r1", "correction A", "user:ana")
go s.Correct("r1", "correction B", "user:ana")
```

`Correct` reads the tip and then writes a version superseding it. Two callers
read the same tip and both write. Both return `nil`. The store is then bricked.
Run under `-count=5`, that raced three times out of five — and ADR-0027 states
that being "read and written by many runs" is the store's whole point, and cites
exactly that as why version IDs are content-addressed instead of allocated from a
counter. The concurrency the ID scheme was chosen to support is the concurrency
that destroys the store.

Nothing caught this because the existing guard,
`TestAForkedSupersessionChainIsRefused`, builds its fork with two deliberate
`Put` calls and asserts only that `Retrieve` returns an error. A test that asks
"does this fail?" cannot distinguish failing safely from failing catastrophically,
and the catastrophic reading is the one the code implements.

## Decision

Two changes, addressing prevention and containment separately. They are in one
record because each alone leaves the other's failure live: prevention cannot
help a fork that arrives from a replica, and containment alone would leave
`Correct` silently corrupting a record whenever two callers race.

**Superseding a version claims it exclusively, and the filesystem enforces it.**
Before a version naming `Supersedes: X` is written, the store creates a claim
file for `X` with `O_EXCL`. The claim holds the version ID that took it. Only
one successor can ever exist for one predecessor, and that is a property of the
filesystem rather than a check this code has to remember to perform — the same
mechanism and the same argument `Put` already uses to refuse overwriting a
version file.

A claim already held by the *same* successor is the benign case and returns
success: version IDs are content-addressed, so re-applying the identical
correction produces the identical version, and `Put` is idempotent by design. A
claim held by a *different* successor is the fork, and it is refused **before**
anything is written, so the second writer loses cleanly and the store is never
forked at all.

This is ADR-0006's compare-and-swap applied to the one field that identifies a
version of a record. That ADR settled that concurrency in this project is
resolved by CAS against a version token rather than by a lock, and rejected
`turn_source` precisely because restricting *who writes* does not stop two
writers from acting on state one of them read and the other replaced. `Correct`
reading a tip and then writing against it is that race exactly. The loser is
told to re-read and decide, which is what ADR-0006 prescribes, and not retried
automatically: a correction is a human assertion about content, and replaying it
onto a predecessor the author never saw would silently overwrite somebody else's
correction — the outcome the fork refusal exists to prevent.

**A fork that still arrives is contained to its own record.** Exclusive claiming
removes the fork this store can create; it cannot remove one that arrives with
the bytes, and ADR-0027 chose tombstones over unlinking precisely because
replicas, backups and synced directories are expected to move records between
stores. So a fork remains representable and must not be trusted — but it is now
a property of one record rather than of the store. `tips()` partitions: healthy
records resolve normally, forked records are collected and excluded. Retrieval
of every other record proceeds, authorization still runs before ranking, and the
forked record is never presented, because which of its two versions is current
is still unknowable.

The fork is **reported rather than dropped silently**. `Retrieval.Forked` names
the affected record IDs in the evidence the caller commits, and `Store.Forks()`
returns the conflicting version IDs for an operator. Excluding a record with no
trace would make a correction that raced look like a record that was never
written, which is the silent-loss failure the whole store exists to avoid.
Version IDs are disclosed by `Forks()`, an explicit local inspection verb, and
not by a retrieval error crossing a scope boundary.

Consequently the repair verbs work again: `tip()` fails only for a record that
is itself forked, so a forked record can no longer take the rest of the store
down with it, and `Delete` on any healthy record succeeds. A forked record
itself stays unrepairable through `Correct`/`Delete`, and deliberately so —
both need a single current version to supersede, and inventing one is the
tiebreak ADR-0027 rejected. `Forks()` gives an operator the two version IDs, and
resolution is an explicit act with the evidence in hand.

## Alternatives rejected

**Resolve the fork by picking a winner.** Rejected by ADR-0027 and not reopened.
Nothing changes here: containment excludes the record, it does not choose for the
user.

**A lock file around read-modify-write in `Correct`.** It would close the race
inside one process and is weaker than what it replaces: a lock must be released,
so a crash between claim and write leaves a stale lock that a later run must
decide whether to break — and every rule for breaking it is a guess about
whether the holder is alive. The claim file needs no release because it is the
durable record of a fact that does not expire: that predecessor now has a
successor. ADR-0006 rejected locking for this project's concurrency on
neighbouring grounds.

**Make `Correct` take the expected version ID from the caller.** A real CAS
token, and it does not prevent this fork: two callers who both read version 1
both pass version 1, and both writes are still valid against what they read. The
exclusion has to happen at the write, which is where the claim is. The claim
subsumes the parameter without making every caller carry one.

**Let `Retrieve` skip forked records without reporting them.** Simpler, and it
converts a loud failure into a silent one: a user whose correction lost a race
would see their memory quietly absent with nothing anywhere saying why. Phase 7's
exit evidence requires that "every influence identifies its source and version";
a record that vanishes without evidence is the same gap seen from the other side.

## Consequences

One fork no longer denies memory to unrelated principals, and the cross-scope
disclosure in the refusal message is gone with it. Concurrent corrections of the
same record now resolve to one winner and one explicit refusal, instead of two
successes and a store nobody can read.

Superseding writes cost one extra file and one extra `fsync`d directory entry.
Accepted: a correction is rare relative to retrieval, and retrieval does not
read claims — `tips()` still derives the current version by walking supersession,
so the claim is a write-side exclusion mechanism and never a second source of
truth about what is current. That distinction is load-bearing. A claim file that
retrieval trusted would be the `current` pointer ADR-0027 rejected, and it would
be able to disagree with the chain.

Two versions superseding the same predecessor is now unreachable through this
store's own API, so the containment path is exercised only by forks that arrive
with imported bytes. It is tested by writing the colliding version files
directly, which is what an imported fork looks like on disk.

## How it is verified

- `TestConcurrentCorrectionsCannotForkTheChain` runs the exact race from the
  probe above: two goroutines correcting one record. One wins, one is refused,
  and the store is readable afterwards. Before the claim, this raced to a bricked
  store.
- `TestASupersededVersionCannotBeClaimedTwice` asserts the refusal names the
  predecessor and tells the caller to re-read the tip, and that nothing was
  written.
- `TestReapplyingAnIdenticalCorrectionIsIdempotent` pins the benign collision, so
  the claim cannot be tightened into something that breaks re-`Put`.
- `TestAForkedRecordDoesNotDenyRetrievalToOtherRecords` imports a fork by writing
  version files directly and asserts an unrelated scope still retrieves, that the
  forked record is absent, and that `Retrieval.Forked` names it.
- `TestAForkedRecordDoesNotDisableRepairOfHealthyRecords` asserts `Delete` still
  works on a healthy record while a fork sits in the store — the permanence half
  of the defect.
- `TestAForkedSupersessionChainIsRefused` keeps ADR-0027's rule: the forked
  record itself is never presented.
