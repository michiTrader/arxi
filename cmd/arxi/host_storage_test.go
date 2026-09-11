package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	hostv1 "github.com/michiTrader/arxi/host/v1"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/jobstore"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
)

const filesystemStorageBlueprint = "name: example\nmembers:\n  - {name: worker}\nstages:\n  - {name: work, advance_when: all}\n"

func filesystemCreateRequest(t *testing.T, id string) hostv1.CreateJob {
	t.Helper()
	bp, err := blueprint.Load([]byte(filesystemStorageBlueprint))
	if err != nil {
		t.Fatal(err)
	}
	effective := runconfig.New(id, "sim", bp.SHA, "work", "", bp.Config, nil, nil)
	metadata, err := json.Marshal(filesystemStoredMetadata{Effective: effective, Simulated: true})
	if err != nil {
		t.Fatal(err)
	}
	start, err := json.Marshal(kernel.Event{ID: "start", Type: kernel.RunStarted,
		Source: kernel.SourceHuman, Scope: "run:" + id, Payload: map[string]any{
			"run_id": id, "actor": "example", "blueprint_sha": bp.SHA,
			"budget_usd": 1.0, "prompt": "work", "simulated": true,
		}})
	if err != nil {
		t.Fatal(err)
	}
	return hostv1.CreateJob{
		Record: hostv1.JobRecord{ID: hostv1.JobID(id), Data: metadata},
		Artifacts: []hostv1.Artifact{{Name: "blueprint", MediaType: "application/yaml",
			Digest: bp.SHA, Data: append([]byte(nil), bp.Raw...)}},
		Records: []hostv1.StoredRecord{{Data: start}},
	}
}

func TestFilesystemJobStorageCreatesBoundRunAndTransfersWriter(t *testing.T) {
	root := t.TempDir()
	storage := newFilesystemJobStorage(root)
	created, err := storage.Create(context.Background(), filesystemCreateRequest(t, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	defer created.Writer.Close()
	if created.Record.Revision != "1" {
		t.Fatalf("created revision = %q, want 1", created.Record.Revision)
	}
	if _, err := os.Stat(filepath.Join(root, "r1", "blueprint.snapshot.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "r1", runconfig.FileName)); err != nil {
		t.Fatal(err)
	}
	batch, err := storage.ReadConfirmed(context.Background(), "r1", hostv1.ConfirmedRead{})
	if err != nil {
		t.Fatal(err)
	}
	var start kernel.Event
	if err := json.Unmarshal(batch.Records[0].Data, &start); err != nil {
		t.Fatal(err)
	}
	if start.Str("effective_config_schema") != runconfig.Schema ||
		start.Str("effective_config_path") != runconfig.FileName || start.Str("effective_config_sha") == "" {
		t.Fatalf("run.started binding = %#v", start.Payload)
	}
	if _, err := storage.OpenWriter(context.Background(), "r1"); !errors.Is(err, hostv1.ErrStorageConflict) {
		t.Fatalf("second writer error = %v, want storage conflict", err)
	} else {
		var locked *logstore.LockedError
		if !errors.As(err, &locked) {
			t.Fatalf("second writer error lost LockedError: %v", err)
		}
	}
}

func TestFilesystemJobStoragePreservesCASAndConfirmedPagination(t *testing.T) {
	storage := newFilesystemJobStorage(t.TempDir())
	created, err := storage.Create(context.Background(), filesystemCreateRequest(t, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	defer created.Writer.Close()
	event, _ := json.Marshal(kernel.Event{ID: "pause", Type: kernel.RunPaused, Source: kernel.SourceHuman})
	if _, err := created.Writer.Append(context.Background(), hostv1.AppendBatch{
		Expected: "0", Records: []hostv1.StoredRecord{{Data: event}},
	}); !errors.Is(err, hostv1.ErrStorageConflict) {
		t.Fatalf("stale append error = %v, want storage conflict", err)
	} else {
		var cas *logstore.CASError
		if !errors.As(err, &cas) || cas.Expected != 0 || cas.Actual != 1 {
			t.Fatalf("stale append error lost CAS detail: %v", err)
		}
	}
	if _, err := created.Writer.Append(context.Background(), hostv1.AppendBatch{
		Expected: created.Record.Revision, Records: []hostv1.StoredRecord{{Data: event}},
	}); err != nil {
		t.Fatal(err)
	}
	first, err := storage.ReadConfirmed(context.Background(), "r1", hostv1.ConfirmedRead{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.AfterSequence != 1 || first.End || first.Continuation == "" || len(first.Records) != 1 {
		t.Fatalf("first page = %#v", first)
	}
	last, err := storage.ReadConfirmed(context.Background(), "r1", hostv1.ConfirmedRead{
		AfterSequence: first.AfterSequence, Continuation: first.Continuation, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if last.AfterSequence != 2 || !last.End || last.Continuation != "" || len(last.Records) != 1 {
		t.Fatalf("last page = %#v", last)
	}
}

func TestFilesystemJobStorageWithholdsPendingBatch(t *testing.T) {
	root := t.TempDir()
	storage := newFilesystemJobStorage(root)
	created, err := storage.Create(context.Background(), filesystemCreateRequest(t, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Writer.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "r1")
	path := logstore.EventsPath(dir)
	confirmed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ghost, _ := json.Marshal(kernel.Event{Seq: 2, ID: "ghost", Type: kernel.RunPaused})
	if err := os.WriteFile(path, append(confirmed, append(ghost, '\n')...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending.commit"), []byte(fmt.Sprintf("%d\n", len(confirmed))), 0o644); err != nil {
		t.Fatal(err)
	}
	batch, err := storage.ReadConfirmed(context.Background(), "r1", hostv1.ConfirmedRead{})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Revision != "1" || batch.AfterSequence != 1 || len(batch.Records) != 1 {
		t.Fatalf("confirmed read exposed pending record: %#v", batch)
	}
}

func TestFilesystemJobStorageLoadsLegacyRunDirectory(t *testing.T) {
	workspace := t.TempDir()
	runAt(t, workspace, "legacy", "feature-team", 1, "")
	storage := newFilesystemJobStorage(filepath.Join(workspace, "runs"))
	record, err := storage.Load(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != "legacy" || record.Revision != "2" {
		t.Fatalf("legacy record = %#v", record)
	}
	var metadata filesystemStoredMetadata
	if err := json.Unmarshal(record.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Effective.RunID != "legacy" || metadata.Effective.BlueprintSHA == "" {
		t.Fatalf("legacy metadata = %#v", metadata)
	}
}

func TestFilesystemClaimedWriterRejectsStaleFence(t *testing.T) {
	root := t.TempDir()
	storage := newFilesystemJobStorage(root).(*filesystemJobStorage)
	created, err := storage.Create(context.Background(), filesystemCreateRequest(t, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Writer.Close(); err != nil {
		t.Fatal(err)
	}
	coordination := jobstore.NewMemory(nowFunc)
	if _, err := coordination.RegisterJob(0, jobstore.JobRegistration{JobID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordination.BindSubmission(coordination.View().Revision, jobstore.Submission{Key: "key", RequestDigest: "digest", JobID: "r1"}); err != nil {
		t.Fatal(err)
	}
	first, _, err := coordination.Claim(coordination.View().Revision, "r1", "first", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	claim := publicExecutionClaim(first)
	writer, err := storage.OpenWriter(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	fenced := &filesystemJobWriter{store: writer.(*filesystemJobWriter).store, record: created.Record, claim: &claim,
		validate: func(context.Context, hostv1.ExecutionClaim) error { return hostv1.ErrStorageConflict }}
	event, _ := json.Marshal(kernel.Event{ID: "pause", Type: kernel.RunPaused, Source: kernel.SourceHuman})
	if _, err := fenced.Append(context.Background(), hostv1.AppendBatch{Expected: created.Record.Revision,
		Records: []hostv1.StoredRecord{{Data: event}}}); !errors.Is(err, hostv1.ErrStorageConflict) {
		t.Fatalf("stale append error = %v, want storage conflict: a replaced host must not mutate the run log", err)
	}
	_ = fenced.Close()
}

func TestFilesystemStorageBacksProtocolInspectAndCancel(t *testing.T) {
	workspace := t.TempDir()
	runAt(t, workspace, "r1", "feature-team", 1, "")
	host := hostv1.New(hostv1.Options{Storage: newFilesystemJobStorage(filepath.Join(workspace, "runs"))})
	defer host.Close()
	session := newProtoSession(hostv1.Principal{ID: "local"}, host)

	shown := oneSession(t, session, `{"id":"show","type":"run.show","params":{"run":"r1"}}`)
	if !shown.OK {
		t.Fatalf("run.show failed: %+v", shown.Error)
	}
	cancelled := oneSession(t, session, `{"id":"cancel","type":"run.cancel","params":{"run":"r1","reason":"operator request"}}`)
	if !cancelled.OK {
		t.Fatalf("run.cancel failed: %+v", cancelled.Error)
	}
	batch, err := newFilesystemJobStorage(filepath.Join(workspace, "runs")).ReadConfirmed(
		context.Background(), "r1", hostv1.ConfirmedRead{})
	if err != nil {
		t.Fatal(err)
	}
	var event kernel.Event
	if err := json.Unmarshal(batch.Records[len(batch.Records)-1].Data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != kernel.RunCancelled || event.Str("reason") != "operator request" {
		t.Fatalf("last event = %#v", event)
	}
}
