package jobstore

import (
	"reflect"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

func (s *state) checkpoint(expected Revision, value job.Checkpoint, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if _, _, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now); err != nil {
		return nil, err
	}
	if old, ok := s.view.Checkpoints[value.JobID]; ok {
		if reflect.DeepEqual(old, value) {
			return nil, nil
		}
		if err := job.ValidateCheckpoint(&old, value); err != nil {
			return nil, err
		}
	}
	return []record{{Kind: kindCheckpoint, Data: encodeData(value)}}, nil
}

func (s *state) receipt(expected Revision, value job.Receipt, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if _, _, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now); err != nil {
		return nil, err
	}
	if old, ok := s.view.Receipts[value.DispatchKey]; ok {
		if reflect.DeepEqual(old, value) {
			return nil, nil
		}
		return nil, ErrConflict
	}
	return []record{{Kind: kindReceipt, Data: encodeData(value)}}, nil
}

func (s *state) cancel(expected Revision, value Cancellation) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if _, ok := s.view.Jobs[value.JobID]; !ok {
		return nil, ErrNotFound
	}
	if old, ok := s.view.Cancellations[value.JobID]; ok {
		if old == value {
			return nil, nil
		}
		return nil, ErrConflict
	}
	return []record{{Kind: kindCancellation, Data: encodeData(value)}}, nil
}

func (s *state) settle(expected Revision, value Settlement, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if _, _, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now); err != nil {
		return nil, err
	}
	reservation, ok := s.view.Reservations[value.ReservationID]
	if !ok || reservation.JobID != value.JobID {
		return nil, ErrNotFound
	}
	wanted := job.ReservationSettled
	switch value.Kind {
	case SettlementSpend:
		comparison, err := compareAmounts(value.Spent, reservation.Reserved)
		if err != nil {
			return nil, err
		}
		if comparison > 0 {
			return nil, ErrBudgetExceeded
		}
	case SettlementRelease:
		wanted = job.ReservationReleased
		if value.Spent.Coefficient != 0 {
			return nil, ErrConflict
		}
	case SettlementUnknown:
		wanted = job.ReservationUnknown
		if value.Spent.Coefficient != 0 {
			return nil, ErrConflict
		}
	default:
		return nil, ErrConflict
	}
	if reservation.State != job.ReservationActive {
		if reservation.State == wanted && reservation.Spent == value.Spent {
			return nil, nil
		}
		return nil, ErrConflict
	}
	return []record{{Kind: kindSettlement, Data: encodeData(value)}}, nil
}

func (s *state) complete(expected Revision, value Completion, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	j, a, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now)
	if err != nil {
		return nil, err
	}
	if !job.AttemptTerminal(value.AttemptState) || !job.JobTerminal(value.JobState) {
		return nil, job.ErrIllegalTransition
	}
	if a.State == value.AttemptState && j.State == value.JobState {
		return nil, nil
	}
	return []record{
		{Kind: kindAttemptFinished, Data: encodeData(value)},
		{Kind: kindJobFinished, Data: encodeData(value)},
	}, nil
}
