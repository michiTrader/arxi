package jobstore

import (
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

type Memory struct {
	mu     sync.Mutex
	clock  Clock
	state  *state
	closed bool
}

func NewMemory(clock Clock) *Memory {
	return &Memory{clock: clock, state: newState()}
}

func (m *Memory) now() (time.Time, error) {
	if m.clock == nil {
		return time.Time{}, ErrClockRequired
	}
	return m.clock(), nil
}

func (m *Memory) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneView(m.state.view)
}

func (m *Memory) commit(records []record) (Revision, error) {
	if m.closed {
		return m.state.view.Revision, ErrClosed
	}
	if len(records) == 0 {
		return m.state.view.Revision, nil
	}
	records = numbered(records, m.state.view.Revision)
	candidate := cloneState(m.state)
	if err := candidate.apply(records); err != nil {
		return m.state.view.Revision, err
	}
	m.state = candidate
	return m.state.view.Revision, nil
}

func (m *Memory) BindSubmission(expected Revision, value Submission) (Submission, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, result, err := m.state.bind(expected, value)
	if err != nil {
		return Submission{}, m.state.view.Revision, err
	}
	revision, err := m.commit(records)
	return result, revision, err
}
func (m *Memory) RecordOccurrence(expected Revision, value job.Occurrence) (job.Occurrence, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, result, err := m.state.recordOccurrence(expected, value)
	if err != nil {
		return job.Occurrence{}, m.state.view.Revision, err
	}
	revision, err := m.commit(records)
	return result, revision, err
}
func (m *Memory) Admit(expected Revision, value Admission) (job.Occurrence, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, result, err := m.state.admit(expected, value)
	if err != nil {
		return job.Occurrence{}, m.state.view.Revision, err
	}
	revision, err := m.commit(records)
	return result, revision, err
}
func (m *Memory) Claim(expected Revision, id job.JobID, owner string, duration time.Duration) (job.Claim, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, clockErr := m.now()
	if clockErr != nil {
		return job.Claim{}, m.state.view.Revision, clockErr
	}
	records, result, err := m.state.claim(expected, id, owner, duration, now)
	if err != nil {
		return job.Claim{}, m.state.view.Revision, err
	}
	revision, err := m.commit(records)
	return result, revision, err
}
func (m *Memory) Heartbeat(expected Revision, id job.JobID, attempt job.AttemptID, fence job.Fence, duration time.Duration) (job.Claim, Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, clockErr := m.now()
	if clockErr != nil {
		return job.Claim{}, m.state.view.Revision, clockErr
	}
	records, result, err := m.state.heartbeat(expected, id, attempt, fence, duration, now)
	if err != nil {
		return job.Claim{}, m.state.view.Revision, err
	}
	revision, err := m.commit(records)
	return result, revision, err
}
func (m *Memory) Checkpoint(expected Revision, value job.Checkpoint) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, clockErr := m.now()
	if clockErr != nil {
		return m.state.view.Revision, clockErr
	}
	records, err := m.state.checkpoint(expected, value, now)
	if err != nil {
		return m.state.view.Revision, err
	}
	return m.commit(records)
}
func (m *Memory) RecordReceipt(expected Revision, value job.Receipt) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, clockErr := m.now()
	if clockErr != nil {
		return m.state.view.Revision, clockErr
	}
	records, err := m.state.receipt(expected, value, now)
	if err != nil {
		return m.state.view.Revision, err
	}
	return m.commit(records)
}
func (m *Memory) Cancel(expected Revision, value Cancellation) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.state.cancel(expected, value)
	if err != nil {
		return m.state.view.Revision, err
	}
	return m.commit(records)
}
func (m *Memory) Settle(expected Revision, value Settlement) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, clockErr := m.now()
	if clockErr != nil {
		return m.state.view.Revision, clockErr
	}
	records, err := m.state.settle(expected, value, now)
	if err != nil {
		return m.state.view.Revision, err
	}
	return m.commit(records)
}
func (m *Memory) Complete(expected Revision, value Completion) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, clockErr := m.now()
	if clockErr != nil {
		return m.state.view.Revision, clockErr
	}
	records, err := m.state.complete(expected, value, now)
	if err != nil {
		return m.state.view.Revision, err
	}
	return m.commit(records)
}
func (m *Memory) Close() error { m.mu.Lock(); defer m.mu.Unlock(); m.closed = true; return nil }

var _ Store = (*Memory)(nil)
