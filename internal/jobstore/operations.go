package jobstore

import (
	"encoding/json"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

const (
	kindSubmission      = "submission.bound"
	kindOccurrence      = "occurrence.recorded"
	kindReservation     = "budget.reserved"
	kindExpired         = "attempt.expired"
	kindClaimed         = "attempt.claimed"
	kindHeartbeat       = "attempt.heartbeat"
	kindCheckpoint      = "attempt.checkpointed"
	kindReceipt         = "external.receipt_recorded"
	kindSettlement      = "budget.settled"
	kindCancellation    = "job.cancel_requested"
	kindAttemptFinished = "attempt.finished"
	kindJobFinished     = "job.finished"
)

func (s *state) bind(expected Revision, value Submission) ([]record, Submission, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, Submission{}, err
	}
	if value.Key == "" || value.RequestDigest == "" || value.JobID == "" {
		return nil, Submission{}, ErrNotFound
	}
	if old, ok := s.view.Submissions[value.Key]; ok {
		if old == value {
			return nil, old, nil
		}
		return nil, Submission{}, ErrConflict
	}
	return []record{{Kind: kindSubmission, Data: encodeData(value)}}, value, nil
}

func (s *state) admit(expected Revision, value Admission) ([]record, job.Occurrence, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, job.Occurrence{}, err
	}
	o := value.Occurrence
	if o.ID == "" || o.JobID == "" || o.ReservationID == "" || o.State != job.OccurrenceAdmitted || value.Reserved.Coefficient == 0 {
		return nil, job.Occurrence{}, ErrNotFound
	}
	if old, ok := s.view.Occurrences[o.ID]; ok {
		if old == o {
			return nil, old, nil
		}
		return nil, job.Occurrence{}, ErrConflict
	}
	ledger := s.view.Ledgers[windowKey(value.Window)]
	if len(ledger.Reservations) > 0 && ledger.Ceiling != value.Ceiling {
		return nil, job.Occurrence{}, ErrConflict
	}
	total := job.Amount{}
	var err error
	for _, reservation := range ledger.Reservations {
		amount := reservation.Reserved
		if reservation.State == job.ReservationSettled {
			amount = reservation.Spent
		}
		if reservation.State == job.ReservationReleased {
			continue
		}
		total, err = addAmounts(total, amount)
		if err != nil {
			return nil, job.Occurrence{}, err
		}
	}
	total, err = addAmounts(total, value.Reserved)
	if err != nil {
		return nil, job.Occurrence{}, err
	}
	if comparison, err := compareAmounts(total, value.Ceiling); err != nil {
		return nil, job.Occurrence{}, err
	} else if comparison > 0 {
		return nil, job.Occurrence{}, ErrBudgetExceeded
	}
	return []record{
		{Kind: kindOccurrence, Data: encodeData(o)},
		{Kind: kindReservation, Data: encodeData(value)},
	}, o, nil
}

func (s *state) claim(expected Revision, jobID job.JobID, owner string, duration time.Duration, now time.Time) ([]record, job.Claim, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, job.Claim{}, err
	}
	if err := requireDuration(duration); err != nil {
		return nil, job.Claim{}, err
	}
	j, ok := s.view.Jobs[jobID]
	if !ok {
		return nil, job.Claim{}, ErrNotFound
	}
	if job.JobTerminal(j.State) {
		return nil, job.Claim{}, job.ErrIllegalTransition
	}
	var records []record
	if old, ok := s.view.Claims[jobID]; ok {
		attempt := s.view.Attempts[old.AttemptID]
		if !job.AttemptTerminal(attempt.State) {
			if now.Before(old.ExpiresAt) {
				return nil, job.Claim{}, ErrConflict
			}
			records = append(records, record{Kind: kindExpired, Data: encodeData(old)})
		}
	}
	number, fence := j.AttemptCount+1, j.CurrentFence+1
	claim := job.Claim{JobID: jobID, AttemptID: job.AttemptIdentity(jobID, number), AttemptNumber: number, Owner: owner, Fence: fence, ClaimedAt: now.UTC(), ExpiresAt: now.Add(duration).UTC()}
	records = append(records, record{Kind: kindClaimed, Data: encodeData(claim)})
	return records, claim, nil
}

func (s *state) heartbeat(expected Revision, jobID job.JobID, attemptID job.AttemptID, fence job.Fence, duration time.Duration, now time.Time) ([]record, job.Claim, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, job.Claim{}, err
	}
	if err := requireDuration(duration); err != nil {
		return nil, job.Claim{}, err
	}
	_, _, claim, err := s.active(jobID, attemptID, fence, now)
	if err != nil {
		return nil, job.Claim{}, err
	}
	claim.ExpiresAt = now.Add(duration).UTC()
	return []record{{Kind: kindHeartbeat, Data: encodeData(claim)}}, claim, nil
}

func encodeData(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return body
}
