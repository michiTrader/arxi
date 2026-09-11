package jobstore

import (
	"encoding/json"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

const (
	kindJobRegistered   = "job.registered"
	kindSubmission      = "submission.bound"
	kindOccurrence      = "occurrence.recorded"
	kindReservation     = "budget.reserved"
	kindExpired         = "attempt.expired"
	kindClaimed         = "attempt.claimed"
	kindHeartbeat       = "attempt.heartbeat"
	kindCheckpoint      = "attempt.checkpointed"
	kindDispatch        = "external.dispatch_registered"
	kindReceipt         = "external.receipt_recorded"
	kindSettlement      = "budget.settled"
	kindCancellation    = "job.cancel_requested"
	kindAttemptFinished = "attempt.finished"
	kindJobFinished     = "job.finished"
)

func (s *state) registerJob(expected Revision, value JobRegistration) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if value.JobID == "" {
		return nil, ErrConflict
	}
	if _, ok := s.view.Jobs[value.JobID]; ok {
		return nil, nil
	}
	return []record{{Kind: kindJobRegistered, Data: encodeData(value)}}, nil
}

func (s *state) bind(expected Revision, value Submission) ([]record, Submission, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, Submission{}, err
	}
	if value.Key == "" || value.RequestDigest == "" || value.JobID == "" {
		return nil, Submission{}, ErrNotFound
	}
	if old, ok := s.view.Submissions[value.Key]; ok {
		if old.RequestDigest == value.RequestDigest {
			return nil, old, nil
		}
		return nil, Submission{}, ErrConflict
	}
	return []record{{Kind: kindSubmission, Data: encodeData(value)}}, value, nil
}

func (s *state) recordOccurrence(expected Revision, value job.Occurrence) ([]record, job.Occurrence, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, job.Occurrence{}, err
	}
	if value.ID == "" || value.TriggerID == "" || value.NominalAt.IsZero() ||
		value.ID != job.OccurrenceIdentity(value.TriggerID, value.NominalAt) ||
		(value.State != job.OccurrencePending && value.State != job.OccurrenceSkipped) ||
		value.JobID != "" || value.ReservationID != "" ||
		(value.State == job.OccurrenceSkipped && value.SkipReason == "") ||
		(value.State == job.OccurrencePending && value.SkipReason != "") {
		return nil, job.Occurrence{}, ErrConflict
	}
	if old, ok := s.view.Occurrences[value.ID]; ok {
		if old == value || old.State == job.OccurrenceAdmitted {
			return nil, old, nil
		}
		return nil, job.Occurrence{}, ErrConflict
	}
	return []record{{Kind: kindOccurrence, Data: encodeData(value)}}, value, nil
}

func (s *state) admit(expected Revision, value Admission) ([]record, job.Occurrence, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, job.Occurrence{}, err
	}
	o := value.Occurrence
	if o.ID == "" || o.TriggerID == "" || o.JobID == "" || o.ReservationID == "" ||
		o.NominalAt.IsZero() || o.State != job.OccurrenceAdmitted ||
		value.Window.TriggerID == "" || value.Window.Period == "" || value.Window.StartsAt.IsZero() ||
		value.Window.TriggerID != o.TriggerID || !validPeriod(value.Window.Period) ||
		!value.Window.StartsAt.Equal(canonicalWindowStart(o.NominalAt, value.Window.Period)) ||
		value.Window.StartsAt.Location() != time.UTC || o.NominalAt.Location() != time.UTC ||
		o.ID != job.OccurrenceIdentity(o.TriggerID, o.NominalAt) ||
		!value.Ceiling.Canonical() || value.Ceiling.Coefficient == 0 ||
		!value.Reserved.Canonical() || value.Reserved.Coefficient == 0 {
		return nil, job.Occurrence{}, ErrConflict
	}
	if old, ok := s.view.Occurrences[o.ID]; ok {
		if old.State == job.OccurrenceAdmitted {
			reservation, exists := s.view.Reservations[o.ReservationID]
			if old == o && exists && reservation.ID == o.ReservationID && reservation.OccurrenceID == o.ID &&
				reservation.JobID == o.JobID && reservation.Window == value.Window && reservation.Reserved == value.Reserved {
				ledger, ledgerExists := s.view.Ledgers[windowKey(value.Window)]
				if ledgerExists && ledger.Window == value.Window && ledger.Ceiling == value.Ceiling {
					return nil, old, nil
				}
			}
			return nil, job.Occurrence{}, ErrConflict
		}
		if old.State != job.OccurrencePending || old.TriggerID != o.TriggerID || !old.NominalAt.Equal(o.NominalAt) {
			return nil, job.Occurrence{}, ErrConflict
		}
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
	records := []record{{Kind: kindOccurrence, Data: encodeData(o)}, {Kind: kindReservation, Data: encodeData(value)}}
	return records, o, nil
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

func canonicalWindowStart(nominal time.Time, period job.PeriodKind) time.Time {
	nominal = nominal.UTC()
	switch period {
	case job.PeriodHour:
		return nominal.Truncate(time.Hour)
	case job.PeriodDay:
		return time.Date(nominal.Year(), nominal.Month(), nominal.Day(), 0, 0, 0, 0, time.UTC)
	case job.PeriodWeek:
		day := time.Date(nominal.Year(), nominal.Month(), nominal.Day(), 0, 0, 0, 0, time.UTC)
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset)
	case job.PeriodMonth:
		return time.Date(nominal.Year(), nominal.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

func validPeriod(period job.PeriodKind) bool {
	switch period {
	case job.PeriodHour, job.PeriodDay, job.PeriodWeek, job.PeriodMonth:
		return true
	default:
		return false
	}
}

func encodeData(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return body
}
