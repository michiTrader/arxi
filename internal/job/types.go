// Package job defines pure durable job coordination records and decisions.
package job

import (
	"encoding/json"
	"time"
)

type JobID string
type TriggerID string
type OccurrenceID string
type AttemptID string
type DispatchKey string
type Digest string
type WorkID string
type Fence uint64

// Amount is a canonical non-negative decimal expressed as an integer coefficient
// and a base-10 scale. Equal values must use the same normalized representation.
type Amount struct {
	Coefficient uint64 `json:"coefficient"`
	Scale       uint8  `json:"scale"`
}

func NewAmount(coefficient uint64, scale uint8) Amount {
	if coefficient == 0 {
		return Amount{}
	}
	for coefficient%10 == 0 && scale > 0 {
		coefficient /= 10
		scale--
	}
	return Amount{Coefficient: coefficient, Scale: scale}
}

func (a Amount) Canonical() bool {
	return a.Scale == 0 || a.Coefficient == 0 || a.Coefficient%10 != 0
}

type JobState string

const (
	JobAccepted  JobState = "accepted"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	JobUnknown   JobState = "unknown"
)

type OccurrenceState string

const (
	OccurrencePending   OccurrenceState = "pending"
	OccurrenceAdmitted  OccurrenceState = "admitted"
	OccurrenceSkipped   OccurrenceState = "skipped"
	OccurrenceCompleted OccurrenceState = "completed"
	OccurrenceUnknown   OccurrenceState = "unknown"
)

type AttemptState string

const (
	AttemptClaimed   AttemptState = "claimed"
	AttemptRunning   AttemptState = "running"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptCancelled AttemptState = "cancelled"
	AttemptExpired   AttemptState = "expired"
	AttemptUnknown   AttemptState = "unknown"
)

type Job struct {
	ID                    JobID     `json:"id"`
	State                 JobState  `json:"state"`
	AttemptCount          uint64    `json:"attempt_count"`
	CurrentAttempt        AttemptID `json:"current_attempt,omitempty"`
	CurrentFence          Fence     `json:"current_fence,omitempty"`
	CancellationRequested bool      `json:"cancellation_requested,omitempty"`
}

type Occurrence struct {
	ID            OccurrenceID    `json:"id"`
	TriggerID     TriggerID       `json:"trigger_id"`
	NominalAt     time.Time       `json:"nominal_at"`
	State         OccurrenceState `json:"state"`
	JobID         JobID           `json:"job_id,omitempty"`
	ReservationID string          `json:"reservation_id,omitempty"`
	SkipReason    string          `json:"skip_reason,omitempty"`
}

type Attempt struct {
	ID     AttemptID    `json:"id"`
	JobID  JobID        `json:"job_id"`
	Number uint64       `json:"number"`
	Fence  Fence        `json:"fence"`
	State  AttemptState `json:"state"`
}

type Claim struct {
	JobID         JobID     `json:"job_id"`
	AttemptID     AttemptID `json:"attempt_id"`
	AttemptNumber uint64    `json:"attempt_number"`
	Owner         string    `json:"owner"`
	Fence         Fence     `json:"fence"`
	ClaimedAt     time.Time `json:"claimed_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type Checkpoint struct {
	JobID           JobID     `json:"job_id"`
	AttemptID       AttemptID `json:"attempt_id"`
	Fence           Fence     `json:"fence"`
	RunRevision     uint64    `json:"run_revision"`
	CompletedCursor uint64    `json:"completed_cursor"`
	CurrentWorkID   WorkID    `json:"current_work_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type OutcomeStatus string

const (
	OutcomeSucceeded OutcomeStatus = "succeeded"
	OutcomeFailed    OutcomeStatus = "failed"
	OutcomeCancelled OutcomeStatus = "cancelled"
	OutcomeUnknown   OutcomeStatus = "unknown"
)

type PreparedDispatch struct {
	JobID         JobID       `json:"job_id"`
	AttemptID     AttemptID   `json:"attempt_id"`
	Fence         Fence       `json:"fence"`
	Provider      string      `json:"provider"`
	DispatchKey   DispatchKey `json:"dispatch_key"`
	WorkID        WorkID      `json:"work_id"`
	RequestDigest Digest      `json:"request_digest"`
	WorkClass     WorkClass   `json:"work_class"`
}

type Receipt struct {
	JobID            JobID           `json:"job_id"`
	AttemptID        AttemptID       `json:"attempt_id"`
	Fence            Fence           `json:"fence"`
	Provider         string          `json:"provider"`
	ExternalID       string          `json:"external_id"`
	DispatchKey      DispatchKey     `json:"dispatch_key"`
	WorkID           WorkID          `json:"work_id"`
	RequestDigest    Digest          `json:"request_digest"`
	ObservedAt       time.Time       `json:"observed_at"`
	Status           OutcomeStatus   `json:"status"`
	OutcomeDigest    Digest          `json:"outcome_digest"`
	CanonicalOutcome json.RawMessage `json:"canonical_outcome"`
}

type PeriodKind string

const (
	PeriodHour  PeriodKind = "hour"
	PeriodDay   PeriodKind = "day"
	PeriodWeek  PeriodKind = "week"
	PeriodMonth PeriodKind = "month"
)

type LedgerWindow struct {
	TriggerID TriggerID  `json:"trigger_id"`
	Period    PeriodKind `json:"period"`
	StartsAt  time.Time  `json:"starts_at"`
}

type ReservationState string

const (
	ReservationActive   ReservationState = "active"
	ReservationSettled  ReservationState = "settled"
	ReservationReleased ReservationState = "released"
	ReservationUnknown  ReservationState = "unknown"
)

type LedgerReservation struct {
	ID           string           `json:"id"`
	Window       LedgerWindow     `json:"window"`
	OccurrenceID OccurrenceID     `json:"occurrence_id"`
	JobID        JobID            `json:"job_id"`
	Reserved     Amount           `json:"reserved"`
	Spent        Amount           `json:"spent"`
	State        ReservationState `json:"state"`
}

type Ledger struct {
	Window       LedgerWindow        `json:"window"`
	Ceiling      Amount              `json:"ceiling"`
	Reservations []LedgerReservation `json:"reservations"`
}

type WorkClass string

const (
	WorkIdempotent    WorkClass = "idempotent"
	WorkNonIdempotent WorkClass = "non_idempotent"
)
