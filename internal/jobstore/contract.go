// Package jobstore owns durable cross-job coordination and its projections.
package jobstore

import (
	"errors"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

type Revision uint64

type Clock func() time.Time

var (
	ErrConflict       = errors.New("bound identity conflicts with existing content")
	ErrRevision       = errors.New("coordination revision changed")
	ErrNotFound       = errors.New("coordination record does not exist")
	ErrBudgetExceeded = errors.New("periodic budget ceiling exceeded")
	ErrAmountOverflow = errors.New("amount arithmetic overflow")
	ErrClockRequired  = errors.New("coordination store requires a clock")
	ErrClosed         = errors.New("coordination store is closed")
)

type LockedError struct {
	Dir   string
	Owner string
}

func (e *LockedError) Error() string {
	return "coordination directory " + e.Dir + " is already open for writing by " + e.Owner +
		": exactly one writer may assign journal revisions; if that writer is certainly dead, remove " +
		e.Dir + "/writer.lock after confirming no process is running"
}

type RevisionError struct {
	Expected Revision
	Actual   Revision
}

func (e *RevisionError) Error() string { return "coordination revision changed" }
func (e *RevisionError) Unwrap() error { return ErrRevision }

type Submission struct {
	Key           string     `json:"key"`
	RequestDigest job.Digest `json:"request_digest"`
	JobID         job.JobID  `json:"job_id"`
}

type JobRegistration struct {
	JobID job.JobID `json:"job_id"`
}

type Admission struct {
	Occurrence job.Occurrence   `json:"occurrence"`
	Window     job.LedgerWindow `json:"window"`
	Ceiling    job.Amount       `json:"ceiling"`
	Reserved   job.Amount       `json:"reserved"`
}

type SettlementKind string

const (
	SettlementSpend   SettlementKind = "settled"
	SettlementRelease SettlementKind = "released"
	SettlementUnknown SettlementKind = "unknown"
)

type Settlement struct {
	JobID         job.JobID      `json:"job_id"`
	AttemptID     job.AttemptID  `json:"attempt_id"`
	Fence         job.Fence      `json:"fence"`
	ReservationID string         `json:"reservation_id"`
	Kind          SettlementKind `json:"kind"`
	Spent         job.Amount     `json:"spent"`
}

type Cancellation struct {
	JobID  job.JobID `json:"job_id"`
	Actor  string    `json:"actor"`
	Reason string    `json:"reason"`
}

type Completion struct {
	JobID        job.JobID        `json:"job_id"`
	AttemptID    job.AttemptID    `json:"attempt_id"`
	Fence        job.Fence        `json:"fence"`
	AttemptState job.AttemptState `json:"attempt_state"`
	JobState     job.JobState     `json:"job_state"`
}

// Finalization is the indivisible terminal truth for a scheduled job. Keeping
// settlement and occurrence completion in the same journal batch prevents a
// crash from publishing a finished job whose reserved budget still looks live.
type Finalization struct {
	Completion Completion          `json:"completion"`
	Settlement Settlement          `json:"settlement"`
	Occurrence job.OccurrenceID    `json:"occurrence_id"`
	State      job.OccurrenceState `json:"occurrence_state"`
}

// Reconciler is optional because a local response ID is not evidence that a
// provider supports trustworthy lookup. Installations expose it only when the
// external system can establish the canonical outcome behind a dispatch key.
type Reconciler interface {
	Reconcile(job.DispatchKey) (job.Receipt, bool, error)
}

type View struct {
	Revision      Revision
	Submissions   map[string]Submission
	Jobs          map[job.JobID]job.Job
	Occurrences   map[job.OccurrenceID]job.Occurrence
	Attempts      map[job.AttemptID]job.Attempt
	Claims        map[job.JobID]job.Claim
	Checkpoints   map[job.JobID]job.Checkpoint
	Receipts      map[job.DispatchKey]job.Receipt
	Reservations  map[string]job.LedgerReservation
	Ledgers       map[string]job.Ledger
	Cancellations map[job.JobID]Cancellation
}

// Store applies every mutation through revision CAS. Its injected clock is the
// sole authority for lease expiry, so callers cannot extend stale ownership by
// supplying a favorable instant.
// Store coordinates processes on one local machine through an advisory file
// lock acquired for each transaction. It does not claim distributed locking or
// cache-coherence guarantees across machines sharing a network filesystem.
type Store interface {
	View() View
	RegisterJob(Revision, JobRegistration) (Revision, error)
	BindSubmission(Revision, Submission) (Submission, Revision, error)
	RecordOccurrence(Revision, job.Occurrence) (job.Occurrence, Revision, error)
	Admit(Revision, Admission) (job.Occurrence, Revision, error)
	Claim(Revision, job.JobID, string, time.Duration) (job.Claim, Revision, error)
	Heartbeat(Revision, job.JobID, job.AttemptID, job.Fence, time.Duration) (job.Claim, Revision, error)
	Checkpoint(Revision, job.Checkpoint) (Revision, error)
	RecordReceipt(Revision, job.Receipt) (Revision, error)
	Cancel(Revision, Cancellation) (Revision, error)
	Settle(Revision, Settlement) (Revision, error)
	Complete(Revision, Completion) (Revision, error)
	Finalize(Revision, Finalization) (Revision, error)
	Close() error
}
