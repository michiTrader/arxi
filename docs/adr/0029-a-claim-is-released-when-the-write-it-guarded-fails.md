# ADR-0029: A supersession claim is released when the write it guarded fails, and an unfulfilled one is visible and releasable

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0006, ADR-0021, ADR-0027, ADR-0028
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0028 replaced a lock with a claim file, and argued the choice explicitly:

> The claim file needs no release because it is the durable record of a fact
> that does not expire: that predecessor now has a successor.

Following this project's rule to probe the output of the last turn rather than
read it, a throwaway probe asked the claim the same question that ADR asked of
the lock it rejected — what does a failure between taking the exclusion and
completing the write leave behind, and who pays for it?

The sentence above is true only when the write succeeds. When it fails, the
fact the claim records never became true: no successor exists. The claim that
outlives it is precisely the stale exclusion that ADR rejected locking to
avoid, reintroduced under another name and with no way to break it.

The probe approved a record, obstructed the path its correction would be
written to, and then asked what still worked:

| operation, after one failed correction | result |
| --- | --- |
| `Correct` the same record, any text | refused, permanently |
| `Delete` the same record | refused, permanently |
| `Retrieve` | works — serves the *pre-correction* body as current |
| `Forks()` | reports nothing |
| `Versions()` | reports nothing wrong |
| `Correct` an unrelated record | works |

Three separate defects are visible there.

**The freeze is permanent, and it needs no crash.** `Correct` and `Delete` both
resolve a tip and then write a version superseding it, so both meet the leftover
claim and both are refused, forever, for a record whose supersession chain never
forked and is entirely healthy. Reaching it took no hostile replica, no tampered
file and no kill signal — only a write that did not finish, which is the failure
mode any store on a real disk must survive. A transient I/O error became
permanent, irreversible loss of two of the five controls Phase 7 promises, for
that record.

**The freeze is invisible.** A fork is two versions and `Forks()` reports it;
this is *zero* versions, so `Forks()` cannot see it. `Versions()` skips the
claim because it does not end in `.json` — deliberately, so a claim cannot break
the record set — and `Retrieve` goes on serving the stale body as current with
no evidence that anything is withheld. No verb in the package could see the
claim at all, so a record could be frozen against every repair with nothing able
to say why. ADR-0028 argued that excluding a record with no trace makes a lost
correction indistinguishable from one never written; this is that same failure
one layer down, in the mechanism that ADR added.

**The refusal asserts something false.** The message read `memory version X is
already superseded by Y`, naming a version `Y` that is on no disk anywhere. It
describes a supersession that never happened and sends an operator looking for a
version that does not exist, then prescribes a remedy — "re-read the current
version and decide whether this correction still applies" — which loops forever,
because re-reading returns the same tip and the next attempt hits the same
claim. The only escape was to guess the exact body of the correction that was
lost, reproducing its content-addressed ID.

Nothing caught this because ADR-0028's guards all assert the *presence* of the
exclusion — that a second successor is refused. None asked what happens when the
write behind the first one never lands, which is the question that separates an
exclusion from a leak.

## Decision

**A write that fails releases the claim it created.** The claim is still taken
before the version file is written, for ADR-0028's reason: ordering it after
would persist a losing version and then report failure. What changes is the
unwind. Every failure path from the claim onward releases it, so an abandoned
write leaves no exclusion behind, and the record stays correctable and
deletable. This is the release ADR-0028 said a claim would never need; the
analysis that concluded it was unnecessary considered only the successful write.

**Only the creator may release.** `claimSupersession` now reports whether this
call created the claim, and the rollback is scoped to that. `O_EXCL` makes the
creator the exclusive holder, so a claim this call created is one no other
writer can be acting on. A claim found already held is never released, because
it belongs to somebody else's in-flight write — and releasing that would let two
writers supersede one predecessor, which is the fork ADR-0028 exists to prevent.

**A successful write never releases.** The idempotent re-`Put` path — the
version file already exists holding identical bytes — is a success, so the
claim it created stays. That claim is the exclusion protecting a predecessor
that genuinely does have a successor. This boundary is not hypothetical: claims
do not travel with replicated bytes, so a successor imported from a replica
arrives with no claim beside it, and re-`Put`ting that content is exactly how a
claim gets created for an already-present version.

**An unfulfilled claim is visible and releasable.** No in-process unwind can
cover a process that stops existing, so a crash between the claim and the write
still strands one. That residue is no longer permanent or invisible.
`Store.Claims()` reports every claim, the record it belongs to, and whether its
successor was actually written. `Store.ReleaseClaim(predecessor)` removes one
whose successor never landed, and **refuses** one that is fulfilled — releasing
that would allow a second successor and fork the chain by hand. The refusal for
an unfulfilled claim now says so in those terms and names `ReleaseClaim` as the
remedy, instead of claiming a supersession that never happened.

**Recovery is an operator act, not a sweep.** A claim with no successor file is
usually abandoned and sometimes is a writer microseconds into its own `Put`.
Nothing on disk distinguishes them, and a probe confirmed that the naive rule —
"no successor file, take the claim" — reclaims a live in-flight claim and forks
the chain. So the store reports the evidence and a human decides, on the same
reasoning ADR-0028 gives for not retrying a losing correction automatically: the
machine cannot know the fact, and guessing it silently destroys somebody's work.

## Alternatives rejected

**Write the version first, then the claim.** Removes the stranded claim by
removing the window, and reintroduces the fork: two writers would both write
their version files before either claimed, so both succeed and the chain forks.
ADR-0028 ordered the claim first for exactly this reason.

**A timestamp or PID in the claim, expired by a sweep.** This is the stale-lock
problem ADR-0028 rejected locking to avoid, restated. Every rule for expiring it
is a guess about whether the holder is alive: too short and it reclaims a slow
but live writer, forking the chain; too long and the record stays frozen for the
duration. A PID is worse, because it is meaningless across the replicas and
synced directories ADR-0027 expects to move these bytes between machines.

**Let retrieval or `tip()` ignore an unfulfilled claim.** Tempting, because it
would unfreeze the record with no new verb. It puts the fork back: `tip()`
ignoring the claim means a second correction supersedes a predecessor another
writer may be mid-write on, which is the race the claim exists to lose cleanly.
The claim must stay authoritative for writes; what was missing is a way to see
and retire it.

**Report unfulfilled claims through `Forks()`.** One verb, fewer concepts, and
it conflates two different faults with different remedies. A fork is two
versions and is unresolvable by design — ADR-0027 refuses to pick a winner. An
unfulfilled claim is zero versions and there is nothing to choose between; it is
safely releasable once no writer is in flight. Merging them would put an
actionable fault behind a message that says the situation is unresolvable.

## Consequences

A failed correction now costs the correction and nothing else. Before, it cost
the record: every repair verb was refused permanently, and the pre-correction
body kept being served as current with no evidence anywhere that a write had
been lost.

The store grows two operator verbs, `Claims` and `ReleaseClaim`, and neither is
on the retrieval path. `tips()` still derives the current version by walking
supersession and never reads a claim, so ADR-0028's load-bearing distinction
holds: the claim remains a write-side exclusion and never a second source of
truth about what is current.

A crash between claim and write is still possible and still freezes one record.
That is now a diagnosable, repairable state rather than a permanent and silent
one, which is the honest shape — an in-process rollback cannot cover a process
that ceases to exist, and claiming otherwise would be the unearned guarantee
this corpus keeps finding in its own documents.

`internal/memorystore` still has no importer outside its own tests. This record
hardens the store; it does not wire it. That wiring stays blocked on the
identity boundary ADR-0027 describes, which is unchanged here.

## How it is verified

Every mechanism below was mutated in isolation and confirmed to fail naming its
consequence. Two of these tests exist **only because a mutation survived** the
suite as first written — the ownership scope and the idempotent-re-`Put`
boundary were both untested until their mutants passed.

- `TestAFailedWriteReleasesTheClaimItTook` obstructs the version write and
  asserts the claim is gone and the record is correctable again. Before this
  record, the correction was refused forever.
- `TestAFailedWriteDoesNotFreezeDeletion` is the deletion half, because
  `Correct` and `Delete` are separate verbs and deletion is the control Phase 7
  requires to always work.
- `TestAFailedWriteNeverReleasesAClaimItDidNotCreate` pins the ownership scope.
  It needs three writers: content addressing makes two writers produce identical
  bytes, so only a third, differing correction exposes the fork a stolen claim
  would allow. Added after a mutation dropping `mine` survived everything else.
- `TestAnIdempotentRePutKeepsTheClaimItCreated` pins the other boundary: a
  successful re-`Put` must keep its claim. Added after a mutation releasing it
  survived everything else.
- `TestAnUnfulfilledClaimIsVisibleAndReleasable` asserts `Claims()` reports the
  stranded claim with its record ID and `Fulfilled: false`, that the refusal
  does **not** say "already superseded by" a version that was never written,
  that it names `ReleaseClaim`, and that releasing it restores the record.
- `TestAFulfilledClaimIsNeverReleased` asserts `ReleaseClaim` refuses a
  fulfilled claim and says that releasing it would fork the chain.
- `TestReleasingAClaimThatIsNotHeldIsRefused` keeps the repair verb from
  reporting a repair that did not happen.
- `TestTheRollbackDoesNotStealAnotherWritersClaim` races a failing correction
  against a succeeding one ten times and asserts the store never forks.
