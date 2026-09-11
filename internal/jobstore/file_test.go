package jobstore

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

func TestAmountArithmeticIsExactAndOverflowSafe(t *testing.T) {
	total, err := addAmounts(job.NewAmount(1, 1), job.NewAmount(2, 1))
	if err != nil || total != job.NewAmount(3, 1) {
		t.Fatalf("0.1 + 0.2 = %#v, error %v, want exact 0.3: binary floating-point cannot guard an accounting ceiling", total, err)
	}
	if _, err := addAmounts(job.NewAmount(math.MaxUint64, 0), job.NewAmount(1, 0)); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("coefficient overflow returned %v, want ErrAmountOverflow: wrapping could admit spend above the ceiling", err)
	}
	if _, err := compareAmounts(job.NewAmount(math.MaxUint64, 0), job.NewAmount(1, 1)); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("scale overflow returned %v, want ErrAmountOverflow: an unrepresentable comparison must fail closed", err)
	}
}

func TestFilesystemRecoveryRollsBackAWholePendingTransaction(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
	store, err := Open(dir, clock.read)
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.Admit(0, admission("occurrence-1", "job-1", 400))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	ghost := record{Revision: revision + 1, Kind: kindSubmission, Data: encodeData(Submission{Key: "ghost", RequestDigest: "digest", JobID: "ghost-job"})}
	line, err := json.Marshal(ghost)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, journalFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	file.Close()
	marker, _ := json.Marshal(pendingMarker{Version: 1, PriorOffset: info.Size()})
	if err := os.WriteFile(filepath.Join(dir, pendingFile), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, clock.read)
	if err != nil {
		t.Fatalf("recover pending transaction: %v", err)
	}
	defer reopened.Close()
	view := reopened.View()
	if view.Revision != revision || len(view.Submissions) != 0 {
		t.Fatalf("recovered revision %d with submissions %#v: pending bytes must never become confirmed projection state", view.Revision, view.Submissions)
	}
}

func TestFilesystemPoisonsClientAfterUncertainPostMarkerFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	originalRemove := store.ops.remove
	store.ops.remove = func(path string) error {
		if path == store.pendingPath() {
			return errors.New("injected marker removal failure")
		}
		return originalRemove(path)
	}
	if _, err := store.RegisterJob(0, JobRegistration{JobID: "job"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("post-marker failure returned %v, want ErrPoisoned: the caller cannot know whether revision one reached durable storage", err)
	}
	if _, err := store.RegisterJob(0, JobRegistration{JobID: "job"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("retry on poisoned client returned %v, want ErrPoisoned: repeating an uncertain revision can duplicate journal records", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, time.Now)
	if err != nil {
		t.Fatalf("reopen must recover the pending transaction: %v", err)
	}
	defer reopened.Close()
	if view := reopened.View(); view.Revision != 0 || len(view.Jobs) != 0 {
		t.Fatalf("recovered view = revision %d, jobs %#v: the uncertain transaction must roll back before retries resume", view.Revision, view.Jobs)
	}
	if _, err := reopened.RegisterJob(0, JobRegistration{JobID: "job"}); err != nil {
		t.Fatalf("retry after close and recovery: %v", err)
	}
}

func TestFilesystemProjectionRebuildMatchesConfirmedJournal(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
	store, err := Open(dir, clock.read)
	if err != nil {
		t.Fatal(err)
	}
	claim, revision := seedClaim(t, store)
	revision, err = store.Checkpoint(revision, job.Checkpoint{JobID: claim.JobID, AttemptID: claim.AttemptID, Fence: claim.Fence, RunRevision: 4, CompletedCursor: 3, CreatedAt: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	before := store.View()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, clock.read)
	if err != nil {
		t.Fatalf("rebuild projection from journal: %v", err)
	}
	defer reopened.Close()
	if after := reopened.View(); !reflect.DeepEqual(after, before) {
		t.Fatalf("rebuilt projection differs from confirmed state:\n before %#v\n after  %#v: deleting process memory must not change coordination truth", before, after)
	}
}

func TestFilesystemJournalRejectsUnknownKindsFieldsAndTypes(t *testing.T) {
	for name, line := range map[string]string{
		"unknown kind":           `{"revision":1,"kind":"future.record","data":{}}`,
		"unknown envelope field": `{"revision":1,"kind":"submission.bound","data":{},"future":true}`,
		"wrong revision type":    `{"revision":"one","kind":"submission.bound","data":{}}`,
		"unknown data field":     `{"revision":1,"kind":"submission.bound","data":{"key":"key","request_digest":"digest","job_id":"job","future":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, journalFile), []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(dir, time.Now); err == nil {
				store.Close()
				t.Fatal("invalid journal record was accepted: silently ignoring schema drift makes rebuilt projections depend on reader version")
			}
		})
	}
}
