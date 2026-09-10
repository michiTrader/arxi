package jobstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

const journalFile = "coordination.ndjson"
const pendingFile = "pending.commit"
const lockFile = "writer.lock"

type pendingMarker struct {
	Version     int   `json:"version"`
	PriorOffset int64 `json:"prior_offset"`
}

type File struct {
	mu      sync.Mutex
	dir     string
	clock   Clock
	state   *state
	journal *os.File
	lock    *os.File
	size    int64
	closed  bool
}

func Open(dir string, clock Clock) (*File, error) {
	if clock == nil {
		return nil, ErrClockRequired
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jobstore: create directory: %w", err)
	}
	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}
	file := &File{dir: dir, clock: clock, state: newState(), lock: lock}
	failed := true
	defer func() {
		if failed {
			_ = file.releaseLock()
		}
	}()
	if err := file.rollback(); err != nil {
		return nil, err
	}
	if err := file.scan(); err != nil {
		return nil, err
	}
	journal, err := os.OpenFile(file.journalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("jobstore: open journal: %w", err)
	}
	file.journal = journal
	if err := syncDir(dir); err != nil {
		journal.Close()
		return nil, err
	}
	failed = false
	return file, nil
}

func (f *File) journalPath() string { return filepath.Join(f.dir, journalFile) }
func (f *File) pendingPath() string { return filepath.Join(f.dir, pendingFile) }
func (f *File) lockPath() string    { return filepath.Join(f.dir, lockFile) }

func (f *File) scan() error {
	journal, err := os.Open(f.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobstore: read journal: %w", err)
	}
	defer journal.Close()
	reader := bufio.NewReader(journal)
	var offset int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			if err := truncateSync(f.journalPath(), offset); err != nil {
				return err
			}
			f.size = offset
			return nil
		}
		if len(line) > 0 {
			entry, err := decodeRecord(bytes.TrimSuffix(line, []byte{'\n'}))
			if err != nil {
				return fmt.Errorf("jobstore: corrupt journal at offset %d: %w", offset, err)
			}
			if err := f.state.apply([]record{entry}); err != nil {
				return fmt.Errorf("jobstore: corrupt journal at offset %d: %w", offset, err)
			}
			offset += int64(len(line))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				f.size = offset
				return nil
			}
			return fmt.Errorf("jobstore: scan journal: %w", readErr)
		}
	}
}

func decodeRecord(body []byte) (record, error) {
	var entry record
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return record{}, err
	}
	if decoder.More() {
		return record{}, fmt.Errorf("record contains trailing JSON")
	}
	if entry.Revision == 0 || entry.Kind == "" || len(entry.Data) == 0 || bytes.Equal(entry.Data, []byte("null")) {
		return record{}, fmt.Errorf("record lacks revision, kind, or data")
	}
	return entry, nil
}

func (f *File) commit(records []record) (Revision, error) {
	if f.closed {
		return f.state.view.Revision, ErrClosed
	}
	if len(records) == 0 {
		return f.state.view.Revision, nil
	}
	records = numbered(records, f.state.view.Revision)
	candidate := cloneState(f.state)
	if err := candidate.apply(records); err != nil {
		return f.state.view.Revision, err
	}
	var body []byte
	for _, entry := range records {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return f.state.view.Revision, err
		}
		body = append(body, encoded...)
		body = append(body, '\n')
	}
	if err := f.writePending(); err != nil {
		return f.state.view.Revision, err
	}
	if _, err := f.journal.Write(body); err != nil {
		return f.state.view.Revision, fmt.Errorf("jobstore: append journal: %w", err)
	}
	if err := f.journal.Sync(); err != nil {
		return f.state.view.Revision, fmt.Errorf("jobstore: sync journal: %w", err)
	}
	if err := os.Remove(f.pendingPath()); err != nil {
		return f.state.view.Revision, fmt.Errorf("jobstore: clear pending marker: %w", err)
	}
	if err := syncDir(f.dir); err != nil {
		return f.state.view.Revision, err
	}
	f.state = candidate
	f.size += int64(len(body))
	return f.state.view.Revision, nil
}

func (f *File) writePending() error {
	body, _ := json.Marshal(pendingMarker{Version: 1, PriorOffset: f.size})
	body = append(body, '\n')
	marker, err := os.OpenFile(f.pendingPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("jobstore: create pending marker: %w", err)
	}
	if _, err := marker.Write(body); err != nil {
		marker.Close()
		return err
	}
	if err := marker.Sync(); err != nil {
		marker.Close()
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}
	return syncDir(f.dir)
}

func (f *File) rollback() error {
	body, err := os.ReadFile(f.pendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobstore: read pending marker: %w", err)
	}
	var marker pendingMarker
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil || marker.Version != 1 || marker.PriorOffset < 0 {
		return fmt.Errorf("jobstore: pending rollback marker is invalid; confirmed boundary is unknown")
	}
	if err := truncateSync(f.journalPath(), marker.PriorOffset); err != nil {
		return err
	}
	if err := os.Remove(f.pendingPath()); err != nil {
		return fmt.Errorf("jobstore: remove pending marker: %w", err)
	}
	return syncDir(f.dir)
}

func acquireLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, lockFile)
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		owner := "an unknown process"
		if body, readErr := os.ReadFile(path); readErr == nil && len(bytes.TrimSpace(body)) > 0 {
			owner = string(bytes.TrimSpace(body))
		}
		return nil, &LockedError{Dir: dir, Owner: owner}
	}
	if err != nil {
		return nil, fmt.Errorf("jobstore: acquire coordination writer lock: %w", err)
	}
	if _, err := fmt.Fprintf(lock, "pid %d\n", os.Getpid()); err != nil {
		lock.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("jobstore: record coordination lock owner: %w", err)
	}
	if err := lock.Sync(); err != nil {
		lock.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("jobstore: sync coordination writer lock: %w", err)
	}
	return lock, nil
}

func (f *File) releaseLock() error {
	if f.lock == nil {
		return nil
	}
	err := f.lock.Close()
	f.lock = nil
	if removeErr := os.Remove(f.lockPath()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		err = fmt.Errorf("jobstore: remove coordination writer lock: %w", removeErr)
	}
	return err
}

func truncateSync(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("jobstore: open journal for recovery: %w", err)
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("jobstore: truncate journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("jobstore: sync recovered journal: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	opened, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("jobstore: open directory for sync: %w", err)
	}
	defer opened.Close()
	if err := opened.Sync(); err != nil {
		return fmt.Errorf("jobstore: sync directory: %w", err)
	}
	return nil
}

func (f *File) View() View { f.mu.Lock(); defer f.mu.Unlock(); return cloneView(f.state.view) }
func (f *File) BindSubmission(expected Revision, value Submission) (Submission, Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, result, err := f.state.bind(expected, value)
	if err != nil {
		return Submission{}, f.state.view.Revision, err
	}
	revision, err := f.commit(records)
	return result, revision, err
}
func (f *File) RecordOccurrence(expected Revision, value job.Occurrence) (job.Occurrence, Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, result, err := f.state.recordOccurrence(expected, value)
	if err != nil {
		return job.Occurrence{}, f.state.view.Revision, err
	}
	revision, err := f.commit(records)
	return result, revision, err
}
func (f *File) Admit(expected Revision, value Admission) (job.Occurrence, Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, result, err := f.state.admit(expected, value)
	if err != nil {
		return job.Occurrence{}, f.state.view.Revision, err
	}
	revision, err := f.commit(records)
	return result, revision, err
}
func (f *File) Claim(expected Revision, id job.JobID, owner string, duration time.Duration) (job.Claim, Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, result, err := f.state.claim(expected, id, owner, duration, f.clock())
	if err != nil {
		return job.Claim{}, f.state.view.Revision, err
	}
	revision, err := f.commit(records)
	return result, revision, err
}
func (f *File) Heartbeat(expected Revision, id job.JobID, attempt job.AttemptID, fence job.Fence, duration time.Duration) (job.Claim, Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, result, err := f.state.heartbeat(expected, id, attempt, fence, duration, f.clock())
	if err != nil {
		return job.Claim{}, f.state.view.Revision, err
	}
	revision, err := f.commit(records)
	return result, revision, err
}
func (f *File) Checkpoint(expected Revision, value job.Checkpoint) (Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.state.checkpoint(expected, value, f.clock())
	if err != nil {
		return f.state.view.Revision, err
	}
	return f.commit(records)
}
func (f *File) RecordReceipt(expected Revision, value job.Receipt) (Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.state.receipt(expected, value, f.clock())
	if err != nil {
		return f.state.view.Revision, err
	}
	return f.commit(records)
}
func (f *File) Cancel(expected Revision, value Cancellation) (Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.state.cancel(expected, value)
	if err != nil {
		return f.state.view.Revision, err
	}
	return f.commit(records)
}
func (f *File) Settle(expected Revision, value Settlement) (Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.state.settle(expected, value, f.clock())
	if err != nil {
		return f.state.view.Revision, err
	}
	return f.commit(records)
}
func (f *File) Complete(expected Revision, value Completion) (Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.state.complete(expected, value, f.clock())
	if err != nil {
		return f.state.view.Revision, err
	}
	return f.commit(records)
}
func (f *File) Finalize(expected Revision, value Finalization) (Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.state.finalize(expected, value, f.clock())
	if err != nil {
		return f.state.view.Revision, err
	}
	return f.commit(records)
}
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	var firstErr error
	if f.journal != nil {
		firstErr = f.journal.Close()
	}
	if err := f.releaseLock(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

var _ Store = (*File)(nil)
