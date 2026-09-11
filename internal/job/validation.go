package job

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrIllegalTransition   = errors.New("illegal state transition")
	ErrStaleClaim          = errors.New("claim is not the active fence")
	ErrExpiredClaim        = errors.New("claim lease has expired")
	ErrTerminalAttempt     = errors.New("attempt is terminal")
	ErrCheckpointRegressed = errors.New("checkpoint moved backward")
)

func JobTerminal(state JobState) bool {
	switch state {
	case JobSucceeded, JobFailed, JobCancelled, JobUnknown:
		return true
	default:
		return false
	}
}

func AttemptTerminal(state AttemptState) bool {
	switch state {
	case AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptExpired, AttemptUnknown:
		return true
	default:
		return false
	}
}

func OccurrenceTerminal(state OccurrenceState) bool {
	switch state {
	case OccurrenceSkipped, OccurrenceCompleted, OccurrenceUnknown:
		return true
	default:
		return false
	}
}

func ValidateJobTransition(from, to JobState) error {
	if from == to {
		return nil
	}
	legal := from == JobAccepted && to == JobRunning ||
		from == JobRunning && JobTerminal(to)
	if !legal {
		return fmt.Errorf("%w: job %q to %q", ErrIllegalTransition, from, to)
	}
	return nil
}

func ValidateAttemptTransition(from, to AttemptState) error {
	if from == to {
		return nil
	}
	legal := from == AttemptClaimed && to == AttemptRunning ||
		from == AttemptRunning && AttemptTerminal(to)
	if !legal {
		return fmt.Errorf("%w: attempt %q to %q", ErrIllegalTransition, from, to)
	}
	return nil
}

func ValidateOccurrenceTransition(from, to OccurrenceState) error {
	if from == to {
		return nil
	}
	legal := from == OccurrencePending && (to == OccurrenceAdmitted || to == OccurrenceSkipped) ||
		from == OccurrenceAdmitted && (to == OccurrenceCompleted || to == OccurrenceUnknown)
	if !legal {
		return fmt.Errorf("%w: occurrence %q to %q", ErrIllegalTransition, from, to)
	}
	return nil
}

// ValidateActiveFence rejects a stale worker as soon as its supplied instant
// reaches expiry; waiting for a replacement claim would leave a commit window.
func ValidateActiveFence(job Job, attempt Attempt, claim Claim, now time.Time) error {
	if claim.JobID != job.ID || attempt.JobID != job.ID ||
		claim.AttemptID != attempt.ID || job.CurrentAttempt != attempt.ID ||
		claim.Fence == 0 || claim.Fence != attempt.Fence || claim.Fence != job.CurrentFence ||
		claim.AttemptNumber != attempt.Number {
		return ErrStaleClaim
	}
	if AttemptTerminal(attempt.State) {
		return ErrTerminalAttempt
	}
	if !now.Before(claim.ExpiresAt) {
		return ErrExpiredClaim
	}
	return nil
}

func ValidateCheckpoint(previous *Checkpoint, next Checkpoint) error {
	if next.JobID == "" || next.AttemptID == "" || next.Fence == 0 {
		return fmt.Errorf("checkpoint requires job, attempt and fence")
	}
	if previous == nil {
		return nil
	}
	if previous.JobID != next.JobID || previous.AttemptID != next.AttemptID || previous.Fence != next.Fence {
		return ErrStaleClaim
	}
	if next.RunRevision < previous.RunRevision || next.CompletedCursor < previous.CompletedCursor {
		return ErrCheckpointRegressed
	}
	return nil
}

// RetryAllowed requires both a work classification that an external provider
// can honor and the absence of terminal evidence. A local key cannot make an
// already-started non-idempotent action safe to repeat.
func RetryAllowed(class WorkClass, started, terminal bool) bool {
	if terminal {
		return false
	}
	if class == WorkIdempotent {
		return true
	}
	return class == WorkNonIdempotent && !started
}
