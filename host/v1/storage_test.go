package v1

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
)

type memoryStorage struct {
	mu   sync.Mutex
	jobs map[JobID]*memoryJob
}

type memoryJob struct {
	record    JobRecord
	artifacts []Artifact
	records   []StoredRecord
	snapshot  Snapshot
	owned     bool
}

type memoryWriter struct {
	storage *memoryStorage
	id      JobID
	closed  bool
}

func newMemoryStorage() *memoryStorage { return &memoryStorage{jobs: map[JobID]*memoryJob{}} }

func (s *memoryStorage) Create(_ context.Context, req CreateJob) (CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs[req.Record.ID] != nil {
		return CreateResult{}, ErrStorageConflict
	}
	job := &memoryJob{record: cloneJobRecord(req.Record), artifacts: cloneArtifacts(req.Artifacts), owned: true}
	job.record.Revision = "1"
	job.records = assignRecords(req.Records, 0)
	s.jobs[req.Record.ID] = job
	return CreateResult{Record: cloneJobRecord(job.record), Writer: &memoryWriter{storage: s, id: req.Record.ID}}, nil
}

func (s *memoryStorage) List(context.Context) ([]JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobRecord, 0, len(s.jobs))
	for _, job := range s.jobs {
		out = append(out, cloneJobRecord(job.record))
	}
	return out, nil
}

func (s *memoryStorage) Load(_ context.Context, id JobID) (JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return JobRecord{}, ErrJobNotFound
	}
	return cloneJobRecord(job.record), nil
}

func (s *memoryStorage) ReadConfirmed(_ context.Context, id JobID, read ConfirmedRead) (ConfirmedBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return ConfirmedBatch{}, ErrJobNotFound
	}
	start := int(read.AfterSequence)
	if start < 0 || start > len(job.records) {
		return ConfirmedBatch{}, ErrStorageConflict
	}
	end := len(job.records)
	if read.Limit > 0 && end-start > read.Limit {
		end = start + read.Limit
	}
	return ConfirmedBatch{Records: cloneStoredRecords(job.records[start:end]), AfterSequence: int64(end),
		Revision: job.record.Revision, End: end == len(job.records)}, nil
}

func (s *memoryStorage) OpenWriter(_ context.Context, id JobID) (JobWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return nil, ErrJobNotFound
	}
	if job.owned {
		return nil, ErrStorageConflict
	}
	job.owned = true
	return &memoryWriter{storage: s, id: id}, nil
}

func (w *memoryWriter) Append(_ context.Context, batch AppendBatch) (AppendResult, error) {
	w.storage.mu.Lock()
	defer w.storage.mu.Unlock()
	job := w.storage.jobs[w.id]
	if w.closed || job == nil || batch.Expected != job.record.Revision {
		return AppendResult{}, ErrStorageConflict
	}
	written := assignRecords(batch.Records, len(job.records))
	job.records = append(job.records, written...)
	if batch.Data != nil {
		job.record.Data = append(json.RawMessage(nil), batch.Data...)
	}
	job.record.Revision = Revision(strconv.Itoa(len(job.records)))
	return AppendResult{Revision: job.record.Revision, Records: cloneStoredRecords(written)}, nil
}

func (w *memoryWriter) WriteSnapshot(_ context.Context, snapshot Snapshot) error {
	w.storage.mu.Lock()
	defer w.storage.mu.Unlock()
	if w.closed {
		return ErrStorageConflict
	}
	w.storage.jobs[w.id].snapshot = Snapshot{AtSequence: snapshot.AtSequence, Data: append(json.RawMessage(nil), snapshot.Data...)}
	return nil
}

func (w *memoryWriter) Close() error {
	w.storage.mu.Lock()
	defer w.storage.mu.Unlock()
	if !w.closed {
		w.closed = true
		w.storage.jobs[w.id].owned = false
	}
	return nil
}

func assignRecords(records []StoredRecord, prior int) []StoredRecord {
	out := cloneStoredRecords(records)
	for i := range out {
		out[i].Sequence = int64(prior + i + 1)
	}
	return out
}
func cloneStoredRecords(in []StoredRecord) []StoredRecord {
	out := make([]StoredRecord, len(in))
	for i := range in {
		out[i] = StoredRecord{Sequence: in[i].Sequence, Data: append(json.RawMessage(nil), in[i].Data...)}
	}
	return out
}
func cloneJobRecord(in JobRecord) JobRecord {
	in.Data = append(json.RawMessage(nil), in.Data...)
	return in
}
func cloneArtifacts(in []Artifact) []Artifact {
	out := make([]Artifact, len(in))
	copy(out, in)
	for i := range out {
		out[i].Data = append([]byte(nil), out[i].Data...)
	}
	return out
}

var _ JobStorage = (*memoryStorage)(nil)
