package v1_test

import (
	"context"
	"encoding/json"
	host "github.com/michiTrader/arxi/host/v1"
	"strconv"
	"sync"
)

type memoryStorage struct {
	mu   sync.Mutex
	jobs map[host.JobID]*memoryJob
}
type memoryJob struct {
	record  host.JobRecord
	records []host.StoredRecord
	owned   bool
}
type memoryWriter struct {
	storage *memoryStorage
	id      host.JobID
	closed  bool
}

func newMemoryStorage() *memoryStorage { return &memoryStorage{jobs: map[host.JobID]*memoryJob{}} }

func (s *memoryStorage) Create(_ context.Context, req host.CreateJob) (host.CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs[req.Record.ID] != nil {
		return host.CreateResult{}, host.ErrStorageConflict
	}
	job := &memoryJob{record: cloneJob(req.Record), records: assign(req.Records, 0), owned: true}
	job.record.Revision = "1"
	s.jobs[req.Record.ID] = job
	return host.CreateResult{Record: cloneJob(job.record), Writer: &memoryWriter{storage: s, id: req.Record.ID}}, nil
}
func (s *memoryStorage) List(context.Context) ([]host.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]host.JobRecord, 0, len(s.jobs))
	for _, job := range s.jobs {
		out = append(out, cloneJob(job.record))
	}
	return out, nil
}
func (s *memoryStorage) Load(_ context.Context, id host.JobID) (host.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return host.JobRecord{}, host.ErrJobNotFound
	}
	return cloneJob(job.record), nil
}
func (s *memoryStorage) ReadConfirmed(_ context.Context, id host.JobID, read host.ConfirmedRead) (host.ConfirmedBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return host.ConfirmedBatch{}, host.ErrJobNotFound
	}
	start, end := int(read.AfterSequence), len(job.records)
	if start < 0 || start > end {
		return host.ConfirmedBatch{}, host.ErrStorageConflict
	}
	if read.Limit > 0 && end-start > read.Limit {
		end = start + read.Limit
	}
	return host.ConfirmedBatch{Records: cloneRecords(job.records[start:end]), AfterSequence: int64(end), Revision: job.record.Revision, End: end == len(job.records)}, nil
}
func (s *memoryStorage) OpenWriter(_ context.Context, id host.JobID) (host.JobWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return nil, host.ErrJobNotFound
	}
	if job.owned {
		return nil, host.ErrStorageConflict
	}
	job.owned = true
	return &memoryWriter{storage: s, id: id}, nil
}
func (w *memoryWriter) Append(_ context.Context, batch host.AppendBatch) (host.AppendResult, error) {
	w.storage.mu.Lock()
	defer w.storage.mu.Unlock()
	job := w.storage.jobs[w.id]
	if w.closed || batch.Expected != job.record.Revision {
		return host.AppendResult{}, host.ErrStorageConflict
	}
	written := assign(batch.Records, len(job.records))
	job.records = append(job.records, written...)
	job.record.Revision = host.Revision(strconv.Itoa(len(job.records)))
	return host.AppendResult{Revision: job.record.Revision, Records: cloneRecords(written)}, nil
}
func (w *memoryWriter) WriteSnapshot(context.Context, host.Snapshot) error { return nil }
func (w *memoryWriter) Close() error {
	w.storage.mu.Lock()
	defer w.storage.mu.Unlock()
	if !w.closed {
		w.closed = true
		w.storage.jobs[w.id].owned = false
	}
	return nil
}
func assign(records []host.StoredRecord, prior int) []host.StoredRecord {
	out := cloneRecords(records)
	for i := range out {
		out[i].Sequence = int64(prior + i + 1)
	}
	return out
}
func cloneRecords(in []host.StoredRecord) []host.StoredRecord {
	out := make([]host.StoredRecord, len(in))
	for i := range in {
		out[i] = host.StoredRecord{Sequence: in[i].Sequence, Data: append(json.RawMessage(nil), in[i].Data...)}
	}
	return out
}
func cloneJob(in host.JobRecord) host.JobRecord {
	in.Data = append(json.RawMessage(nil), in.Data...)
	return in
}

var _ host.JobStorage = (*memoryStorage)(nil)
