package jobstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

type record struct {
	Revision Revision        `json:"revision"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
}

type state struct{ view View }

func newState() *state {
	return &state{view: View{
		Submissions: make(map[string]Submission), Jobs: make(map[job.JobID]job.Job),
		Occurrences: make(map[job.OccurrenceID]job.Occurrence), Attempts: make(map[job.AttemptID]job.Attempt),
		Claims: make(map[job.JobID]job.Claim), Checkpoints: make(map[job.JobID]job.Checkpoint),
		Receipts: make(map[job.DispatchKey]job.Receipt), Reservations: make(map[string]job.LedgerReservation),
		Ledgers: make(map[string]job.Ledger), Cancellations: make(map[job.JobID]Cancellation),
	}}
}

func (s *state) checkRevision(expected Revision) error {
	if s.view.Revision != expected {
		return &RevisionError{Expected: expected, Actual: s.view.Revision}
	}
	return nil
}

func (s *state) active(jobID job.JobID, attemptID job.AttemptID, fence job.Fence, now time.Time) (job.Job, job.Attempt, job.Claim, error) {
	j, ok := s.view.Jobs[jobID]
	if !ok {
		return job.Job{}, job.Attempt{}, job.Claim{}, ErrNotFound
	}
	a, ok := s.view.Attempts[attemptID]
	if !ok {
		return job.Job{}, job.Attempt{}, job.Claim{}, ErrNotFound
	}
	claim, ok := s.view.Claims[jobID]
	if !ok {
		return job.Job{}, job.Attempt{}, job.Claim{}, ErrNotFound
	}
	if claim.Fence != fence || claim.AttemptID != attemptID {
		return job.Job{}, job.Attempt{}, job.Claim{}, job.ErrStaleClaim
	}
	if err := job.ValidateActiveFence(j, a, claim, now); err != nil {
		return job.Job{}, job.Attempt{}, job.Claim{}, err
	}
	return j, a, claim, nil
}

func cloneState(in *state) *state {
	return &state{view: cloneView(in.view)}
}

func cloneView(in View) View {
	out := newState().view
	out.Revision = in.Revision
	for k, v := range in.Submissions {
		out.Submissions[k] = v
	}
	for k, v := range in.Jobs {
		out.Jobs[k] = v
	}
	for k, v := range in.Occurrences {
		out.Occurrences[k] = v
	}
	for k, v := range in.Attempts {
		out.Attempts[k] = v
	}
	for k, v := range in.Claims {
		out.Claims[k] = v
	}
	for k, v := range in.Checkpoints {
		out.Checkpoints[k] = v
	}
	for k, v := range in.Receipts {
		out.Receipts[k] = v
	}
	for k, v := range in.Reservations {
		out.Reservations[k] = v
	}
	for k, v := range in.Ledgers {
		v.Reservations = append([]job.LedgerReservation(nil), v.Reservations...)
		out.Ledgers[k] = v
	}
	for k, v := range in.Cancellations {
		out.Cancellations[k] = v
	}
	return out
}

func decodeData(data json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("record data contains trailing JSON")
	}
	return nil
}

func requireDuration(duration time.Duration) error {
	if duration <= 0 {
		return fmt.Errorf("lease duration must be positive")
	}
	return nil
}
