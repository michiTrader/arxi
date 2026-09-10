# Durable jobs, occurrences and attempts

This specification defines Phase 3 coordination records. It complements the
per-run event catalog in `spec/events.md`; it does not replace the kernel log.

## 1. Identity

| value | canonical identity |
|---|---|
| job/run | existing immutable job ID |
| trigger | stored immutable trigger ID; legacy records derive it from canonical immutable trigger fields |
| occurrence | SHA-256 of `arxi.occurrence/v1`, trigger ID and nominal UTC scheduled instant |
| attempt | SHA-256 of `arxi.attempt/v1`, job ID and monotonically increasing attempt number |
| submission key | caller-supplied opaque key bound to a canonical request digest |
| dispatch key | SHA-256 of `arxi.dispatch/v1`, job ID, work ID and request digest |

Timestamps are RFC3339Nano UTC. A scheduled occurrence uses the schedule's
nominal instant, not when a scheduler happened to wake. Repeating an identity
with different bound content is a conflict, never an update.

One job owns one kernel run and one frozen blueprint/effective configuration.
Attempts resume that run; they do not create child runs or change configuration.

## 2. State machines

A job is `accepted`, `running`, `succeeded`, `failed`, `cancelled`, or `unknown`.
Only the last four are terminal. Cancellation is intent until an attempt records a
terminal result; a cancellation request alone does not prove external work stopped.

An occurrence is `pending`, `admitted`, `skipped`, `completed`, or `unknown`.
`pending` may wait under overlap `queue`. `skipped` records the policy reason.
`admitted` binds exactly one job and one budget reservation.

An attempt is `claimed`, `running`, `succeeded`, `failed`, `cancelled`, `expired`,
or `unknown`. Every claim creates a fresh attempt. An expired attempt can never
return to a live state.

Legal terminal transitions require the current fence:

```text
accepted -> running -> succeeded | failed | cancelled | unknown
claimed  -> running -> succeeded | failed | cancelled | expired | unknown
pending  -> admitted | skipped
admitted -> completed | unknown
```

Repeated records with the same identity and identical content are idempotent.
Conflicting repeats are corruption or concurrency conflicts.

## 3. Claims

A claim contains `job_id`, `attempt_id`, `attempt_number`, `owner`, `fence`,
`claimed_at`, and `expires_at`. Fence values are positive and strictly increase
per job. Lease duration and heartbeat cadence are adapter configuration, not
persisted defaults.

Heartbeat, checkpoint, receipt, ledger settlement and terminal completion must
supply job, attempt and fence. The store rejects the operation when any differs,
when the attempt is terminal, or when its own clock is at or after `expires_at`.
Expiry judgment is appended before a replacement claim.

## 4. Checkpoints

A checkpoint contains:

- confirmed run-log revision;
- greatest contiguous `exec.step_completed` source cursor;
- optional current deterministic work ID;
- checkpoint creation instant.

A later checkpoint cannot move either revision or cursor backward. A checkpoint
never contains reducer state, credentials, live provider objects or workspace
process identifiers.

## 5. External dispatch

Before dispatch, the run log contains the exact prepared work and the coordination
journal binds its stable dispatch key. Immediately before calling outside the
process, the started boundary is durable.

`idempotent` work may retry only when no terminal outcome exists and the same key
is supplied to the external system. `non_idempotent` work may not retry after a
started boundary. It must either:

1. reuse an already committed canonical result;
2. reconcile a trustworthy external receipt and commit the established result; or
3. commit `unknown` and stop automatic continuation.

A receipt contains provider, external ID, dispatch key, observed instant, outcome
status and canonical outcome digest. Secrets and credential values are forbidden.

## 6. Occurrence admission and overlap

The pure trigger decision returns exact nominal UTC slots. For each slot, the
coordination transaction first checks occurrence identity and then applies policy:

| policy | durable result while another attempt is active |
|---|---|
| `skip` | append a `skipped` occurrence with reason; never create a job |
| `queue` | leave the occurrence `pending`; a later tick may admit the same identity |
| `parallel` | admit it if the ledger reservation succeeds |
| `cancel-previous` | append cancellation intent for active jobs, then admit the new occurrence if budget permits |

`on-missed=run-all` produces one occurrence per selected nominal slot in ascending
order. `run-once` selects the most recent owed slot. Skipped backlog is recorded so
it does not silently reappear. Duplicate scheduler ticks observe existing records.

## 7. Periodic budget ledger

A ledger window is keyed by trigger ID, period kind and UTC window start. Amounts
use the repository's canonical decimal representation; binary floating-point
comparison is not an accounting boundary.

Admission atomically appends the occurrence, job binding and maximum reservation.
It fails without a partial occurrence when:

```text
settled + active reservations + requested reservation > configured ceiling
```

Settlement replaces reserved capacity with confirmed spend. A proven
pre-dispatch failure may release its reservation. `unknown` spend keeps the full
reservation until reconciliation adds durable evidence. No heartbeat, expiry or
cancellation request releases money by itself.

## 8. Coordination journal

The journal is append-only and revisioned. One atomic transaction either publishes
all records or none. A durable rollback marker contains the prior confirmed byte
offset; recovery truncates a batch whose marker survived a crash. Confirmed reads
never expose pending bytes.

The filesystem adapter serializes transactions between processes on one local
machine with an OS advisory lock (`flock` on Unix and `LockFileEx` on Windows).
The lock is acquired only for a transaction and is released automatically by the
OS when a process exits, including hard termination. This is not a distributed
lock and makes no multi-host guarantee for network/shared filesystems; deployments
requiring that topology need a coordination backend with distributed transaction
semantics.

Cross-job record kinds are:

| kind | required identity and purpose |
|---|---|
| `submission.bound` | idempotency key, request digest and job ID |
| `occurrence.recorded` | occurrence identity, trigger, nominal slot and state |
| `budget.reserved` | occurrence/job, window, ceiling and reserved amount |
| `attempt.claimed` | job, fresh attempt, owner, expiry and fence |
| `attempt.expired` | prior attempt, fence and observed expiry |
| `attempt.heartbeat` | active attempt, fence and new absolute expiry |
| `attempt.checkpointed` | active attempt, fence, run revision and safe cursor |
| `external.receipt_recorded` | active attempt, dispatch key and receipt evidence |
| `budget.settled` | reservation, confirmed spend or evidenced release |
| `job.cancel_requested` | durable intent, actor and reason |
| `attempt.finished` | fenced terminal attempt outcome |
| `job.finished` | fenced terminal job outcome |

Unknown record kinds or fields are rejected. A projection may index the journal,
but deleting it and replaying confirmed records must produce the same answer.

## 9. Acceptance and recovery boundaries

`run.started` remains the durable job-acceptance record. Submission does not report
success before immutable artifacts and that event are confirmed. A scheduled job
uses a deterministic job ID bound in its occurrence transaction. If a process dies
between reservation and per-run publication, recovery retries publication under
the same ID; an already-published matching job is adopted, while conflicting
artifacts are corruption.

Crash tests cover both sides of:

```text
accept -> occurrence/admission -> claim -> dispatch -> checkpoint -> receipt -> complete
```

The required post-restart properties are:

- every accepted job remains inspectable and claimable;
- one nominal trigger slot has one occurrence and at most one bound job;
- only the current unexpired fence can mutate coordination state;
- confirmed work is never repeated;
- prepared idempotent work retries only with its original dispatch key;
- ambiguous non-idempotent work remains visible as `unknown`;
- cancellation remains intent until terminal evidence;
- reservations cannot be double-spent or silently released.

## 10. Projection and compatibility

Existing job inspection may expose selected status, attempt count, scheduled
origin and whether reconciliation is required. It must not expose lease owner,
fence token, journal location, credential references or raw checkpoint records.

`host/v1.JobStorage` and surface version 1 remain source-compatible. Optional
coordination and reconciliation ports install Phase 3 behavior; capability
advertisement must still reflect only installed, authorized and safely adapted
operations.