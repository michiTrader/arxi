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

func (s *state) dispatch(expected Revision, value job.PreparedDispatch, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if value.JobID == "" || value.AttemptID == "" || value.Fence == 0 || value.Provider == "" ||
		value.DispatchKey == "" || value.WorkID == "" || value.RequestDigest == "" || !validWorkClass(value.WorkClass) {
		return nil, ErrConflict
	}
	if _, _, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now); err != nil {
		return nil, err
	}
	if old, ok := s.view.Dispatches[value.DispatchKey]; ok {
		if old == value {
			return nil, nil
		}
		return nil, ErrConflict
	}
	return []record{{Kind: kindDispatch, Data: encodeData(value)}}, nil
}

func (s *state) receipt(expected Revision, value job.Receipt, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	if value.JobID == "" || value.AttemptID == "" || value.Fence == 0 || value.Provider == "" || value.ExternalID == "" ||
		value.DispatchKey == "" || value.WorkID == "" || value.RequestDigest == "" || value.ObservedAt.IsZero() ||
		value.OutcomeDigest == "" || len(value.CanonicalOutcome) == 0 ||
		value.OutcomeDigest != job.CanonicalOutcomeDigest(value.CanonicalOutcome) || !validOutcome(value.Status) {
		return nil, ErrConflict
	}
	if _, _, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now); err != nil {
		return nil, err
	}
	prepared, ok := s.view.Dispatches[value.DispatchKey]
	if !ok || prepared.JobID != value.JobID || prepared.AttemptID != value.AttemptID || prepared.Fence != value.Fence ||
		prepared.Provider != value.Provider || prepared.WorkID != value.WorkID || prepared.RequestDigest != value.RequestDigest {
		return nil, ErrConflict
	}
	if old, ok := s.view.Receipts[value.DispatchKey]; ok {
		if reflect.DeepEqual(old, value) {
			return nil, nil
		}
		return nil, ErrConflict
	}
	return []record{{Kind: kindReceipt, Data: encodeData(value)}}, nil
}

func validWorkClass(value job.WorkClass) bool {
	return value == job.WorkIdempotent || value == job.WorkNonIdempotent
}

func validOutcome(value job.OutcomeStatus) bool {
	switch value {
	case job.OutcomeSucceeded, job.OutcomeFailed, job.OutcomeCancelled, job.OutcomeUnknown:
		return true
	default:
		return false
	}
}

func completionCompatible(attempt job.AttemptState, result job.JobState) bool {
	switch attempt {
	case job.AttemptSucceeded:
		return result == job.JobSucceeded
	case job.AttemptFailed:
		return result == job.JobFailed
	case job.AttemptCancelled:
		return result == job.JobCancelled
	case job.AttemptUnknown:
		return result == job.JobUnknown
	default:
		return false
	}
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
		return nil, ErrConflict
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
	return s.completionRecords(value, now)
}

func (s *state) completionRecords(value Completion, now time.Time) ([]record, error) {
	j, a, _, err := s.active(value.JobID, value.AttemptID, value.Fence, now)
	if err != nil {
		return nil, err
	}
	if !completionCompatible(value.AttemptState, value.JobState) ||
		job.ValidateAttemptTransition(a.State, value.AttemptState) != nil ||
		job.ValidateJobTransition(j.State, value.JobState) != nil {
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

func (s *state) finalize(expected Revision, value Finalization, now time.Time) ([]record, error) {
	if err := s.checkRevision(expected); err != nil {
		return nil, err
	}
	completion := value.Completion
	if value.Settlement.JobID != completion.JobID || value.Settlement.AttemptID != completion.AttemptID ||
		value.Settlement.Fence != completion.Fence {
		return nil, ErrConflict
	}
	occurrence, ok := s.view.Occurrences[value.Occurrence]
	if !ok || occurrence.JobID != completion.JobID || occurrence.State != job.OccurrenceAdmitted {
		return nil, ErrNotFound
	}
	if err := job.ValidateOccurrenceTransition(occurrence.State, value.State); err != nil {
		return nil, err
	}
	if value.State == job.OccurrenceUnknown {
		if completion.JobState != job.JobUnknown || completion.AttemptState != job.AttemptUnknown || value.Settlement.Kind != SettlementUnknown {
			return nil, ErrConflict
		}
	} else if completion.JobState == job.JobUnknown || completion.AttemptState == job.AttemptUnknown || value.Settlement.Kind != SettlementSpend {
		return nil, ErrConflict
	}
	settlementRecords, err := s.settle(expected, value.Settlement, now)
	if err != nil {
		return nil, err
	}
	completionRecords, err := s.completionRecords(completion, now)
	if err != nil {
		return nil, err
	}
	terminalOccurrence := occurrence
	terminalOccurrence.State = value.State
	return append(append(settlementRecords, completionRecords...), record{Kind: kindOccurrence, Data: encodeData(terminalOccurrence)}), nil
}
