# ADR-0010: Durable work uses occurrences, fenced attempts and explicit outcomes

- Status: accepted
- Affects: `internal/job`, `internal/jobstore`, `internal/scheduler`, `internal/app`, `internal/exec`, `internal/trigger`, `spec/jobs.md`, `spec/events.md`

## Context

A run is durable after `run.started`, but the process that executes it is not.
The host remembers workers in a map, and the scheduler remembers active executions
in another map. A restart therefore loses ownership information. The scheduler
also starts work before recording a firing; this deliberately prefers a visible
duplicate over an invisible skip, but it cannot prove exactly-once occurrence
admission.

Phase 2 made each external effect recoverable at prepared, started and finished
boundaries. That prevents an ambiguous started call from being retried, but it
does not let another process claim the containing job, reject an expired worker's
commit, enforce one periodic budget across concurrent firings, or reconcile an
external receipt.

## Decision

**One job owns one kernel run. Cross-job coordination is an append-only journal;
per-job event logs remain execution truth. Workers execute only through renewable,
fenced attempts.**

A retry never creates a different run or reads current configuration. It resumes
the job's frozen blueprint and effective configuration.

### Stable identities

A time-triggered occurrence is identified from an immutable trigger identity and
the nominal UTC scheduled instant. Scheduler wake time is not identity. Repeated
admission of the same occurrence returns the existing record and job rather than
creating another.

Submission idempotency binds a caller key to a canonical request digest. Reusing
the key with the same digest returns the original job; a different digest is a
conflict. Effect dispatch keys derive from job identity, deterministic work
identity and canonical request digest, so they survive attempts.

Every successful claim creates a new attempt and increments the job's fencing
token. Reclaim after expiry closes the prior attempt as expired; it never
continues under the old identity.

### Leases and fencing

A claim records owner, absolute expiry and fencing token. The coordination store,
using its injected clock, validates every heartbeat, checkpoint, receipt,
settlement and terminal commit. Matching an attempt ID is insufficient: the token
must still be current and the lease must not have expired. A stale worker cannot
commit even before another worker reclaims the job.

Clock judgments become durable journal records. The pure job model receives
instants as input and never reads a clock.

### Checkpoints and execution progress

An attempt checkpoint names a confirmed job-log revision and the greatest safe
`exec.step_completed` cursor. It is continuation evidence, not serialized kernel
state. Recovery validates it against confirmed history and falls back to folding
the log; snapshots and coordination projections remain disposable caches.

Existing `exec.work_prepared`, `exec.work_started`, `exec.work_finished` and
`exec.step_completed` records remain the effect-level protocol. Job attempts do
not duplicate or rename that state machine.

### Retry, receipts and unknown outcomes

Only work explicitly classified as idempotent may be redispatched after a
prepared or proven pre-dispatch failure, always with the same dispatch key.
Started non-idempotent work requires a trustworthy provider receipt and a
reconciler that can establish its outcome. Without that evidence it finishes as
`unknown`, remains visible and blocks automatic continuation. Absence of a local
terminal append never proves that an external action failed.

A receipt records provider, external identifier, dispatch key and canonical
outcome digest. Reconciliation adds evidence; it never rewrites an earlier
started boundary.

### Periodic budgets

Occurrence admission and a worst-case reservation share one journal transaction.
The ledger is scoped by trigger identity and UTC period window. Confirmed spend
settles a reservation. Capacity is released only with durable evidence that it
was not spent; an ambiguous outcome continues to reserve its ceiling. This may
underuse a budget temporarily, but cannot exceed the user's declared ceiling.

### Storage and compatibility

The coordination journal owns cross-job facts: submission keys, occurrences,
attempts, claims, fences, checkpoints, receipts and ledger entries. Atomic batches
use a durable rollback marker and revision compare-and-swap. A projection rebuilt
from the confirmed journal is cache, never truth.

`host/v1`, its `JobStorage`, and surface version 1 remain source-compatible. Phase
3 behavior is installed behind existing submit, inspect, wait, cancel and trigger
commands. Lease owners, fence tokens and storage paths are not public DTO fields.

## Discarded alternatives

**Keep process-local workers and record PIDs.** Rejected: a PID is not ownership,
can be reused, and says nothing after a machine restart. It cannot fence a stale
writer.

**Use the per-run event log as the global queue.** Rejected: occurrence uniqueness
and one periodic budget span jobs. Acquiring every run writer cannot form one
atomic admission decision and introduces lock ordering into ordinary scheduling.

**Treat optimistic revision as a fencing token.** Rejected: revision detects a
stale view of one stream. It does not prove that the writer's renewable authority
is current, and a previously opened writer may still hold a valid process-local
handle after its lease expires.

**Continue the same attempt after lease expiry.** Rejected: two processes could
then both claim to be one attempt. A new attempt and fence make the takeover
visible and make every late commit mechanically rejectable.

**Release budget when the worker disappears.** Rejected: disappearance does not
prove the provider did not charge. Releasing ambiguous spend permits the next
occurrence to exceed the declared period ceiling.

**Retry all unknown work with an idempotency key invented locally.** Rejected: a
local key protects nothing unless the external system binds and honors it. For
non-idempotent work, a missing result remains unknown without provider evidence.

**Add job-administration commands to surface v1.** Rejected: the declared surface
is frozen and raw worker coordination is an operator implementation detail. A new
public capability requires a separately versioned contract.

## Consequences

- A scheduler can run in more than one process without duplicating an occurrence.
- An accepted job can be resumed by a replacement worker after lease expiry.
- Heartbeats and checkpoints increase durable write volume.
- Conservative unknown reservations can require operator reconciliation before
  later scheduled work uses the remaining period budget.
- A coordination journal is a second authoritative log, but for disjoint facts:
  it cannot override kernel state or per-run execution history.
- Legacy triggers require deterministic identity derivation until they are written
  with an explicit immutable ID.

## How it is verified

Pure job-model tests cover canonical identity, legal transitions, checkpoint
monotonicity and retry classification. Store contract tests run against memory and
filesystem adapters and cover atomic admission, idempotency conflicts, competing
claims, expiry, fencing, heartbeat, checkpoint, receipt and ledger settlement.
Scheduler tests run two instances against one store and prove one nominal slot
produces one occurrence and job. Restart tests inject failure before and after
accept, claim, dispatch, checkpoint and complete boundaries. Execution tests keep
started non-idempotent work unknown unless a receipt reconciler supplies durable
evidence. Architecture tests keep the pure model away from clocks and I/O and keep
the scheduler behind its declared interfaces.