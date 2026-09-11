package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	hostv1 "github.com/michiTrader/arxi/host/v1"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/runread"
)

const hostStorageRecordFile = ".host-job.v1.json"

type filesystemJobStorage struct {
	root         string
	coordination hostv1.Coordination
}

type filesystemJobWriter struct {
	store    *logstore.Store
	record   hostv1.JobRecord
	claim    *hostv1.ExecutionClaim
	validate func(context.Context, hostv1.ExecutionClaim) error
	closed   bool
}

type filesystemJobEnvelope struct {
	Record    hostv1.JobRecord  `json:"record"`
	Artifacts []hostv1.Artifact `json:"artifacts,omitempty"`
}

type filesystemStoredMetadata struct {
	Effective runconfig.Artifact `json:"effective"`
	Simulated bool               `json:"simulated"`
}

func newFilesystemJobStorage(root string) hostv1.JobStorage {
	return &filesystemJobStorage{root: root}
}

func (s *filesystemJobStorage) Create(ctx context.Context, req hostv1.CreateJob) (result hostv1.CreateResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	dir, err := s.jobDir(req.Record.ID)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return result, fmt.Errorf("create jobs root: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return result, hostv1.ErrStorageConflict
		}
		return result, err
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = os.RemoveAll(dir)
		}
	}()

	metadata, err := decodeFilesystemMetadata(req.Record.Data)
	if err != nil {
		return result, err
	}
	if metadata.Effective.RunID != string(req.Record.ID) {
		return result, fmt.Errorf("effective config belongs to job %q, not %q", metadata.Effective.RunID, req.Record.ID)
	}
	if err := publishFilesystemArtifacts(dir, req.Artifacts, metadata.Effective); err != nil {
		return result, err
	}
	record := cloneFilesystemRecord(req.Record)
	record.Revision = "0"
	if err := writeFilesystemEnvelope(dir, filesystemJobEnvelope{Record: record, Artifacts: cloneFilesystemArtifacts(req.Artifacts)}); err != nil {
		return result, err
	}
	store, err := logstore.Open(dir)
	if err != nil {
		return result, adaptFilesystemStorageError(err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = store.Close()
		}
	}()
	events, err := filesystemEvents(req.Records)
	if err != nil {
		return result, err
	}
	if len(events) == 0 || events[0].Type != kernel.RunStarted {
		return result, errors.New("initial records must begin with run.started")
	}
	_, configDigest, err := runconfig.Encode(metadata.Effective)
	if err != nil {
		return result, err
	}
	bindFilesystemStartEvent(&events[0], metadata.Effective, configDigest)
	if _, err := store.Append(events); err != nil {
		return result, adaptFilesystemStorageError(err)
	}
	record.Revision = filesystemRevision(store.Head())
	if err := writeFilesystemEnvelope(dir, filesystemJobEnvelope{Record: record, Artifacts: cloneFilesystemArtifacts(req.Artifacts)}); err != nil {
		return result, err
	}
	writer := &filesystemJobWriter{store: store, record: record}
	keepOpen, accepted = true, true
	return hostv1.CreateResult{Record: cloneFilesystemRecord(record), Writer: writer}, nil
}

func (s *filesystemJobStorage) List(ctx context.Context) ([]hostv1.JobRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return []hostv1.JobRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]hostv1.JobRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := hostv1.JobID(entry.Name())
		record, loadErr := s.Load(ctx, id)
		if loadErr == nil {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *filesystemJobStorage) Load(ctx context.Context, id hostv1.JobID) (hostv1.JobRecord, error) {
	if err := ctx.Err(); err != nil {
		return hostv1.JobRecord{}, err
	}
	dir, err := s.existingJobDir(id)
	if err != nil {
		return hostv1.JobRecord{}, err
	}
	record, err := loadFilesystemRecord(dir, id)
	if err != nil {
		return hostv1.JobRecord{}, err
	}
	head, _, err := readFilesystemRecords(dir)
	if err != nil {
		return hostv1.JobRecord{}, err
	}
	record.Revision = filesystemRevision(head)
	return record, nil
}

func (s *filesystemJobStorage) ReadConfirmed(ctx context.Context, id hostv1.JobID, read hostv1.ConfirmedRead) (hostv1.ConfirmedBatch, error) {
	if err := ctx.Err(); err != nil {
		return hostv1.ConfirmedBatch{}, err
	}
	dir, err := s.existingJobDir(id)
	if err != nil {
		return hostv1.ConfirmedBatch{}, err
	}
	if read.AfterSequence < 0 {
		return hostv1.ConfirmedBatch{}, hostv1.ErrStorageConflict
	}
	if read.Continuation != "" && read.Continuation != filesystemContinuation(read.AfterSequence) {
		return hostv1.ConfirmedBatch{}, hostv1.ErrStorageConflict
	}
	head, records, err := readFilesystemRecords(dir)
	if err != nil {
		return hostv1.ConfirmedBatch{}, err
	}
	if read.AfterSequence > head {
		return hostv1.ConfirmedBatch{}, hostv1.ErrStorageConflict
	}
	start := int(read.AfterSequence)
	end := len(records)
	if read.Limit > 0 && end-start > read.Limit {
		end = start + read.Limit
	}
	after := int64(end)
	continuation := hostv1.Continuation("")
	if after < head {
		continuation = filesystemContinuation(after)
	}
	return hostv1.ConfirmedBatch{
		Records: records[start:end], AfterSequence: after,
		Continuation: continuation, Revision: filesystemRevision(head), End: after == head,
	}, nil
}

func (s *filesystemJobStorage) OpenWriter(ctx context.Context, id hostv1.JobID) (hostv1.JobWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.existingJobDir(id)
	if err != nil {
		return nil, err
	}
	record, err := loadFilesystemRecord(dir, id)
	if err != nil {
		return nil, err
	}
	store, err := logstore.Open(dir)
	if err != nil {
		return nil, adaptFilesystemStorageError(err)
	}
	record.Revision = filesystemRevision(store.Head())
	return &filesystemJobWriter{store: store, record: record}, nil
}

func (s *filesystemJobStorage) OpenClaimedWriter(ctx context.Context, claim hostv1.ExecutionClaim) (hostv1.JobWriter, error) {
	writer, err := s.OpenWriter(ctx, claim.JobID)
	if err != nil {
		return nil, err
	}
	value := writer.(*filesystemJobWriter)
	value.claim = &claim
	value.validate = func(ctx context.Context, current hostv1.ExecutionClaim) error {
		if s.coordination == nil {
			return errors.New("filesystem job storage has no coordination validator")
		}
		return s.coordination.Validate(ctx, current)
	}
	return value, nil
}

func (w *filesystemJobWriter) Append(ctx context.Context, batch hostv1.AppendBatch) (hostv1.AppendResult, error) {
	if err := w.validClaim(ctx); err != nil {
		return hostv1.AppendResult{}, err
	}
	if w.closed || w.store == nil {
		return hostv1.AppendResult{}, hostv1.ErrStorageConflict
	}
	expected, err := strconv.ParseInt(string(batch.Expected), 10, 64)
	if err != nil || expected < 0 {
		return hostv1.AppendResult{}, hostv1.ErrStorageConflict
	}
	if batch.Data != nil && !bytes.Equal(batch.Data, w.record.Data) {
		return hostv1.AppendResult{}, errors.New("filesystem job storage does not support mutable job metadata")
	}
	events, err := filesystemEvents(batch.Records)
	if err != nil {
		return hostv1.AppendResult{}, err
	}
	written, err := w.store.AppendIfSeq(expected, events)
	if err != nil {
		return hostv1.AppendResult{}, adaptFilesystemStorageError(err)
	}
	records := make([]hostv1.StoredRecord, len(written))
	for i, event := range written {
		body, encodeErr := json.Marshal(event)
		if encodeErr != nil {
			return hostv1.AppendResult{}, encodeErr
		}
		records[i] = hostv1.StoredRecord{Sequence: event.Seq, Data: body}
	}
	w.record.Revision = filesystemRevision(w.store.Head())
	return hostv1.AppendResult{Revision: w.record.Revision, Records: records}, nil
}

func (w *filesystemJobWriter) WriteSnapshot(ctx context.Context, snapshot hostv1.Snapshot) error {
	if err := w.validClaim(ctx); err != nil {
		return err
	}
	if w.closed || w.store == nil {
		return hostv1.ErrStorageConflict
	}
	var state kernel.State
	if err := json.Unmarshal(snapshot.Data, &state); err != nil {
		return fmt.Errorf("decode lifecycle snapshot: %w", err)
	}
	return w.store.WriteSnapshot(state, snapshot.AtSequence)
}

func (w *filesystemJobWriter) validClaim(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.claim != nil {
		if w.validate == nil {
			return hostv1.ErrStorageConflict
		}
		if err := w.validate(ctx, *w.claim); err != nil {
			return fmt.Errorf("%w: claimed writer is stale: %v", hostv1.ErrStorageConflict, err)
		}
	}
	return nil
}

func (w *filesystemJobWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.store == nil {
		return nil
	}
	return w.store.Close()
}

func (s *filesystemJobStorage) jobDir(id hostv1.JobID) (string, error) {
	value := strings.TrimSpace(string(id))
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value {
		return "", fmt.Errorf("invalid job id %q", id)
	}
	if strings.TrimSpace(s.root) == "" {
		return "", errors.New("filesystem job storage has no root")
	}
	return filepath.Join(s.root, value), nil
}

func (s *filesystemJobStorage) existingJobDir(id hostv1.JobID) (string, error) {
	dir, err := s.jobDir(id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(logstore.EventsPath(dir)); err != nil {
		if os.IsNotExist(err) {
			return "", hostv1.ErrJobNotFound
		}
		return "", err
	}
	return dir, nil
}

func filesystemEvents(records []hostv1.StoredRecord) ([]kernel.Event, error) {
	events := make([]kernel.Event, len(records))
	for i, record := range records {
		if record.Sequence != 0 {
			return nil, fmt.Errorf("record %d already has storage sequence %d", i, record.Sequence)
		}
		if err := json.Unmarshal(record.Data, &events[i]); err != nil {
			return nil, fmt.Errorf("decode record %d: %w", i, err)
		}
		if events[i].Seq != 0 {
			return nil, fmt.Errorf("record %d event already has sequence %d", i, events[i].Seq)
		}
	}
	return events, nil
}

func bindFilesystemStartEvent(event *kernel.Event, effective runconfig.Artifact, digest string) {
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	event.Payload["effective_config_schema"] = effective.Schema
	event.Payload["effective_config_path"] = runconfig.FileName
	event.Payload["effective_config_sha"] = digest
}

func readFilesystemRecords(dir string) (int64, []hostv1.StoredRecord, error) {
	read, err := logstore.ReadConfirmed(dir, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, hostv1.ErrJobNotFound
		}
		return 0, nil, err
	}
	var records []hostv1.StoredRecord
	body := read.Bytes
	var expected int64 = 1
	for len(body) > 0 {
		index := bytes.IndexByte(body, '\n')
		if index < 0 {
			break
		}
		line := bytes.TrimSpace(body[:index])
		body = body[index+1:]
		if len(line) == 0 {
			continue
		}
		var header struct {
			Sequence int64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return 0, nil, err
		}
		if header.Sequence != expected {
			return 0, nil, fmt.Errorf("confirmed sequence %d, want %d", header.Sequence, expected)
		}
		records = append(records, hostv1.StoredRecord{Sequence: header.Sequence, Data: append(json.RawMessage(nil), line...)})
		expected++
	}
	return expected - 1, records, nil
}

func loadFilesystemRecord(dir string, id hostv1.JobID) (hostv1.JobRecord, error) {
	body, err := os.ReadFile(filepath.Join(dir, hostStorageRecordFile))
	if err == nil {
		var envelope filesystemJobEnvelope
		if decodeErr := json.Unmarshal(body, &envelope); decodeErr != nil {
			return hostv1.JobRecord{}, decodeErr
		}
		if envelope.Record.ID != id {
			return hostv1.JobRecord{}, fmt.Errorf("stored job id %q disagrees with %q", envelope.Record.ID, id)
		}
		return cloneFilesystemRecord(envelope.Record), nil
	}
	if !os.IsNotExist(err) {
		return hostv1.JobRecord{}, err
	}
	return reconstructFilesystemRecord(dir, id)
}

func reconstructFilesystemRecord(dir string, id hostv1.JobID) (hostv1.JobRecord, error) {
	run, err := runread.Open(dir)
	if err != nil {
		return hostv1.JobRecord{}, err
	}
	effective, _, loadErr := runconfig.Load(dir)
	if loadErr != nil && !os.IsNotExist(loadErr) {
		return hostv1.JobRecord{}, loadErr
	}
	if os.IsNotExist(loadErr) {
		sha := strings.Repeat("0", sha256.Size*2)
		if raw, readErr := os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml")); readErr == nil {
			sum := sha256.Sum256(raw)
			sha = hex.EncodeToString(sum[:])
		}
		mode := "live"
		if run.Simulated {
			mode = "sim"
		}
		prompt := ""
		if len(run.Events) > 0 {
			prompt = run.Events[0].Str("prompt")
		}
		effective = runconfig.New(string(id), mode, sha, prompt, "", run.Config, nil, nil)
	}
	metadata, err := json.Marshal(filesystemStoredMetadata{Effective: effective, Simulated: run.Simulated})
	if err != nil {
		return hostv1.JobRecord{}, err
	}
	return hostv1.JobRecord{ID: id, Data: metadata}, nil
}

func publishFilesystemArtifacts(dir string, artifacts []hostv1.Artifact, effective runconfig.Artifact) error {
	seenBlueprint, seenEffective := false, false
	for _, artifact := range artifacts {
		sum := sha256.Sum256(artifact.Data)
		if artifact.Digest != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("artifact %q digest does not match its data", artifact.Name)
		}
		switch artifact.Name {
		case "blueprint":
			if seenBlueprint {
				return errors.New("duplicate blueprint artifact")
			}
			seenBlueprint = true
			if artifact.Digest != effective.BlueprintSHA {
				return errors.New("blueprint artifact digest disagrees with effective config")
			}
			if _, err := blueprint.Load(artifact.Data); err != nil {
				return fmt.Errorf("validate blueprint artifact: %w", err)
			}
			if err := writeExclusiveSynced(filepath.Join(dir, "blueprint.snapshot.yaml"), artifact.Data, 0o644); err != nil {
				return err
			}
		case runconfig.FileName:
			if seenEffective {
				return errors.New("duplicate effective config artifact")
			}
			seenEffective = true
			encoded, digest, err := runconfig.Encode(effective)
			if err != nil || digest != artifact.Digest || !bytes.Equal(encoded, artifact.Data) {
				if err == nil {
					err = errors.New("effective config artifact disagrees with job metadata")
				}
				return err
			}
		default:
			return fmt.Errorf("unsupported artifact %q", artifact.Name)
		}
	}
	if !seenBlueprint {
		return errors.New("filesystem job storage requires a blueprint artifact")
	}
	if !seenEffective {
		encoded, _, err := runconfig.Encode(effective)
		if err != nil {
			return err
		}
		if err := writeExclusiveSynced(filepath.Join(dir, runconfig.FileName), encoded, 0o600); err != nil {
			return err
		}
	} else {
		for _, artifact := range artifacts {
			if artifact.Name == runconfig.FileName {
				if err := writeExclusiveSynced(filepath.Join(dir, runconfig.FileName), artifact.Data, 0o600); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeFilesystemMetadata(raw json.RawMessage) (filesystemStoredMetadata, error) {
	var metadata filesystemStoredMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return metadata, fmt.Errorf("decode job metadata: %w", err)
	}
	if err := runconfig.Validate(metadata.Effective); err != nil {
		return metadata, fmt.Errorf("validate effective job config: %w", err)
	}
	return metadata, nil
}

func writeFilesystemEnvelope(dir string, envelope filesystemJobEnvelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := filepath.Join(dir, hostStorageRecordFile)
	tmp, err := os.CreateTemp(dir, ".host-job-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncFilesystemDirectory(dir)
}

func writeExclusiveSynced(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func syncFilesystemDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func adaptFilesystemStorageError(err error) error {
	var locked *logstore.LockedError
	var cas *logstore.CASError
	if errors.As(err, &locked) || errors.As(err, &cas) {
		return fmt.Errorf("%w: %w", hostv1.ErrStorageConflict, err)
	}
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %w", hostv1.ErrJobNotFound, err)
	}
	return err
}

func filesystemRevision(sequence int64) hostv1.Revision {
	return hostv1.Revision(strconv.FormatInt(sequence, 10))
}

func filesystemContinuation(sequence int64) hostv1.Continuation {
	return hostv1.Continuation("fs1:" + strconv.FormatInt(sequence, 10))
}

func cloneFilesystemRecord(record hostv1.JobRecord) hostv1.JobRecord {
	record.Data = append(json.RawMessage(nil), record.Data...)
	return record
}

func cloneFilesystemArtifacts(artifacts []hostv1.Artifact) []hostv1.Artifact {
	out := make([]hostv1.Artifact, len(artifacts))
	copy(out, artifacts)
	for i := range out {
		out[i].Data = append([]byte(nil), out[i].Data...)
	}
	return out
}

var _ hostv1.JobStorage = (*filesystemJobStorage)(nil)
var _ hostv1.CoordinatedJobStorageV1 = (*filesystemJobStorage)(nil)
var _ hostv1.JobWriter = (*filesystemJobWriter)(nil)
