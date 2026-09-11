package jobstore

import (
	"fmt"

	"github.com/michiTrader/arxi/internal/job"
)

func (s *state) apply(records []record) error {
	for _, entry := range records {
		if entry.Revision != s.view.Revision+1 {
			return fmt.Errorf("journal revision %d follows %d", entry.Revision, s.view.Revision)
		}
		switch entry.Kind {
		case kindJobRegistered:
			var value JobRegistration
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Jobs[value.JobID] = job.Job{ID: value.JobID, State: job.JobAccepted}
		case kindSubmission:
			var value Submission
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Submissions[value.Key] = value
		case kindOccurrence:
			var value job.Occurrence
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Occurrences[value.ID] = value
			if value.JobID != "" {
				if _, exists := s.view.Jobs[value.JobID]; !exists {
					s.view.Jobs[value.JobID] = job.Job{ID: value.JobID, State: job.JobAccepted}
				}
			}
		case kindReservation:
			var value Admission
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			reservation := job.LedgerReservation{ID: value.Occurrence.ReservationID, Window: value.Window, OccurrenceID: value.Occurrence.ID, JobID: value.Occurrence.JobID, Reserved: value.Reserved, State: job.ReservationActive}
			s.view.Reservations[reservation.ID] = reservation
			key := windowKey(value.Window)
			ledger := s.view.Ledgers[key]
			ledger.Window = value.Window
			ledger.Ceiling = value.Ceiling
			ledger.Reservations = append(ledger.Reservations, reservation)
			s.view.Ledgers[key] = ledger
		case kindExpired:
			var claim job.Claim
			if err := decodeData(entry.Data, &claim); err != nil {
				return err
			}
			a := s.view.Attempts[claim.AttemptID]
			a.State = job.AttemptExpired
			s.view.Attempts[a.ID] = a
		case kindClaimed:
			var claim job.Claim
			if err := decodeData(entry.Data, &claim); err != nil {
				return err
			}
			s.view.Claims[claim.JobID] = claim
			s.view.Attempts[claim.AttemptID] = job.Attempt{ID: claim.AttemptID, JobID: claim.JobID, Number: claim.AttemptNumber, Fence: claim.Fence, State: job.AttemptRunning}
			j := s.view.Jobs[claim.JobID]
			j.State = job.JobRunning
			j.AttemptCount = claim.AttemptNumber
			j.CurrentAttempt = claim.AttemptID
			j.CurrentFence = claim.Fence
			s.view.Jobs[j.ID] = j
		case kindHeartbeat:
			var claim job.Claim
			if err := decodeData(entry.Data, &claim); err != nil {
				return err
			}
			s.view.Claims[claim.JobID] = claim
		case kindCheckpoint:
			var value job.Checkpoint
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Checkpoints[value.JobID] = value
		case kindDispatch:
			var value job.PreparedDispatch
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Dispatches[value.DispatchKey] = value
		case kindReceipt:
			var value job.Receipt
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Receipts[value.DispatchKey] = value
		case kindCancellation:
			var value Cancellation
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			s.view.Cancellations[value.JobID] = value
			j := s.view.Jobs[value.JobID]
			j.CancellationRequested = true
			s.view.Jobs[j.ID] = j
		case kindSettlement:
			var value Settlement
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			reservation := s.view.Reservations[value.ReservationID]
			switch value.Kind {
			case SettlementSpend:
				reservation.State = job.ReservationSettled
				reservation.Spent = value.Spent
			case SettlementRelease:
				reservation.State = job.ReservationReleased
			case SettlementUnknown:
				reservation.State = job.ReservationUnknown
			}
			s.view.Reservations[reservation.ID] = reservation
			key := windowKey(reservation.Window)
			ledger := s.view.Ledgers[key]
			for i := range ledger.Reservations {
				if ledger.Reservations[i].ID == reservation.ID {
					ledger.Reservations[i] = reservation
				}
			}
			s.view.Ledgers[key] = ledger
		case kindAttemptFinished:
			var value Completion
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			a := s.view.Attempts[value.AttemptID]
			a.State = value.AttemptState
			s.view.Attempts[a.ID] = a
		case kindJobFinished:
			var value Completion
			if err := decodeData(entry.Data, &value); err != nil {
				return err
			}
			j := s.view.Jobs[value.JobID]
			j.State = value.JobState
			s.view.Jobs[j.ID] = j
		default:
			return fmt.Errorf("unknown journal record kind %q", entry.Kind)
		}
		s.view.Revision = entry.Revision
	}
	return nil
}

func numbered(records []record, from Revision) []record {
	out := make([]record, len(records))
	for i, entry := range records {
		entry.Revision = from + Revision(i) + 1
		out[i] = entry
	}
	return out
}
