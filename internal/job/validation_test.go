package job

import (
	"errors"
	"testing"
	"time"
)

func TestStateTransitionsAcceptOnlyTheSpecifiedEdges(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"job accepted to running", ValidateJobTransition(JobAccepted, JobRunning)},
		{"job running to unknown", ValidateJobTransition(JobRunning, JobUnknown)},
		{"attempt claimed to running", ValidateAttemptTransition(AttemptClaimed, AttemptRunning)},
		{"attempt running to expired", ValidateAttemptTransition(AttemptRunning, AttemptExpired)},
		{"occurrence pending to skipped", ValidateOccurrenceTransition(OccurrencePending, OccurrenceSkipped)},
		{"occurrence admitted to completed", ValidateOccurrenceTransition(OccurrenceAdmitted, OccurrenceCompleted)},
	}
	for _, test := range tests {
		if test.err != nil {
			t.Errorf("%s was rejected: %v: recovery would be unable to record a legal durable boundary; add only the transition specified in spec/jobs.md", test.name, test.err)
		}
	}

	illegal := []struct {
		name string
		err  error
	}{
		{"job accepted directly to succeeded", ValidateJobTransition(JobAccepted, JobSucceeded)},
		{"terminal job back to running", ValidateJobTransition(JobFailed, JobRunning)},
		{"attempt claimed directly to succeeded", ValidateAttemptTransition(AttemptClaimed, AttemptSucceeded)},
		{"expired attempt back to running", ValidateAttemptTransition(AttemptExpired, AttemptRunning)},
		{"pending occurrence directly to completed", ValidateOccurrenceTransition(OccurrencePending, OccurrenceCompleted)},
		{"skipped occurrence to admitted", ValidateOccurrenceTransition(OccurrenceSkipped, OccurrenceAdmitted)},
	}
	for _, test := range illegal {
		if !errors.Is(test.err, ErrIllegalTransition) {
			t.Errorf("%s returned %v, want ErrIllegalTransition: invalid history could be persisted and replayed as fact; reject every edge absent from spec/jobs.md", test.name, test.err)
		}
	}
}

func TestRepeatedStateIsIdempotent(t *testing.T) {
	if err := ValidateJobTransition(JobRunning, JobRunning); err != nil {
		t.Fatalf("repeated identical job state was rejected: duplicate journal delivery would turn a valid replay into a conflict; accept identical repeats: %v", err)
	}
	if err := ValidateAttemptTransition(AttemptExpired, AttemptExpired); err != nil {
		t.Fatalf("repeated identical terminal attempt was rejected: recovery could not replay an already-confirmed expiry; accept identical repeats: %v", err)
	}
	if err := ValidateOccurrenceTransition(OccurrenceSkipped, OccurrenceSkipped); err != nil {
		t.Fatalf("repeated identical skipped occurrence was rejected: duplicate ticks would fail instead of observing the existing record; accept identical repeats: %v", err)
	}
}

func activeFixture() (Job, Attempt, Claim, time.Time) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	attempt := Attempt{ID: "attempt-2", JobID: "job-1", Number: 2, Fence: 7, State: AttemptRunning}
	job := Job{ID: "job-1", State: JobRunning, AttemptCount: 2, CurrentAttempt: attempt.ID, CurrentFence: 7}
	claim := Claim{JobID: job.ID, AttemptID: attempt.ID, AttemptNumber: attempt.Number, Owner: "worker-a", Fence: 7, ClaimedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}
	return job, attempt, claim, now
}

func TestActiveFenceRequiresEveryBoundIdentityAndUnexpiredLease(t *testing.T) {
	job, attempt, claim, now := activeFixture()
	if err := ValidateActiveFence(job, attempt, claim, now); err != nil {
		t.Fatalf("current unexpired fence was rejected: a healthy worker could not checkpoint or finish; accept matching job, attempt, number and fence before expiry: %v", err)
	}

	stale := claim
	stale.Fence--
	if err := ValidateActiveFence(job, attempt, stale, now); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale fence returned %v, want ErrStaleClaim: a replaced worker could commit over its successor; compare the supplied fence with both job and attempt", err)
	}

	wrongAttempt := claim
	wrongAttempt.AttemptID = "attempt-1"
	if err := ValidateActiveFence(job, attempt, wrongAttempt, now); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("wrong attempt returned %v, want ErrStaleClaim: matching a fence alone could authorize unrelated work; bind job, attempt and fence together", err)
	}

	if err := ValidateActiveFence(job, attempt, claim, claim.ExpiresAt); !errors.Is(err, ErrExpiredClaim) {
		t.Fatalf("operation at lease expiry returned %v, want ErrExpiredClaim: a stale worker retained a commit window; reject when now is at or after expires_at", err)
	}

	attempt.State = AttemptSucceeded
	if err := ValidateActiveFence(job, attempt, claim, now); !errors.Is(err, ErrTerminalAttempt) {
		t.Fatalf("terminal attempt returned %v, want ErrTerminalAttempt: completed work could mutate coordination state; reject every terminal attempt", err)
	}
}

func TestCheckpointCannotMoveEitherProgressMeasureBackward(t *testing.T) {
	previous := Checkpoint{JobID: "job", AttemptID: "attempt", Fence: 3, RunRevision: 12, CompletedCursor: 8}
	for name, next := range map[string]Checkpoint{
		"run revision":     {JobID: "job", AttemptID: "attempt", Fence: 3, RunRevision: 11, CompletedCursor: 8},
		"completed cursor": {JobID: "job", AttemptID: "attempt", Fence: 3, RunRevision: 12, CompletedCursor: 7},
	} {
		if err := ValidateCheckpoint(&previous, next); !errors.Is(err, ErrCheckpointRegressed) {
			t.Errorf("backward %s returned %v, want ErrCheckpointRegressed: recovery could repeat confirmed work; require both revision and cursor to be monotonic", name, err)
		}
	}

	advanced := previous
	advanced.RunRevision++
	if err := ValidateCheckpoint(&previous, advanced); err != nil {
		t.Fatalf("monotonic checkpoint was rejected: a worker could not publish safe continuation evidence; allow either measure to stay equal or increase: %v", err)
	}

	wrongFence := advanced
	wrongFence.Fence++
	if err := ValidateCheckpoint(&previous, wrongFence); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("checkpoint under a different fence returned %v, want ErrStaleClaim: progress from separate owners could be merged; require one job-attempt-fence identity", err)
	}
}

func TestRetryClassificationStopsAmbiguousNonIdempotentWork(t *testing.T) {
	if !RetryAllowed(WorkIdempotent, true, false) {
		t.Fatal("started idempotent work was denied retry without a terminal result: durable provider idempotency could not recover a lost response; allow it with the original dispatch key")
	}
	if RetryAllowed(WorkIdempotent, true, true) {
		t.Fatal("terminal idempotent work was allowed to retry: confirmed external work could execute again; terminal evidence must stop every retry")
	}
	if RetryAllowed(WorkNonIdempotent, true, false) {
		t.Fatal("started non-idempotent work was allowed to retry: an ambiguous external action could be duplicated; require receipt reconciliation or finish unknown")
	}
	if !RetryAllowed(WorkNonIdempotent, false, false) {
		t.Fatal("non-idempotent work proven not dispatched was denied retry: safe pre-dispatch recovery would be impossible; permit retry only before the started boundary")
	}
}
