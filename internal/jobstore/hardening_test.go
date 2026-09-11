package jobstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

func TestFilesystemRecoversPendingMarkerAfterProcessExit(t *testing.T) {
	if os.Getenv("ARXI_JOBSTORE_CRASH_HELPER") == "1" {
		dir := os.Getenv("ARXI_JOBSTORE_CRASH_DIR")
		store, err := Open(dir, time.Now)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, _, err := store.Admit(0, admission("crash", "job-crash", 100)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		info, _ := os.Stat(filepath.Join(dir, journalFile))
		ghost := record{Revision: 3, Kind: kindSubmission, Data: encodeData(Submission{Key: "ghost", RequestDigest: "digest", JobID: "ghost-job"})}
		line, _ := json.Marshal(ghost)
		journal, _ := os.OpenFile(filepath.Join(dir, journalFile), os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = journal.Write(append(line, '\n'))
		_ = journal.Close()
		marker, _ := json.Marshal(pendingMarker{Version: 1, PriorOffset: info.Size()})
		_ = os.WriteFile(filepath.Join(dir, pendingFile), marker, 0o600)
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFilesystemRecoversPendingMarkerAfterProcessExit$")
	cmd.Env = append(os.Environ(), "ARXI_JOBSTORE_CRASH_HELPER=1", "ARXI_JOBSTORE_CRASH_DIR="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper failed: %v\n%s", err, output)
	}
	store, err := Open(dir, time.Now)
	if err != nil {
		t.Fatalf("reopen after helper exited without Close: %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending marker survived recovery: %v", err)
	}
	if view := store.View(); view.Revision != 2 || len(view.Submissions) != 0 {
		t.Fatalf("recovered revision = %d with submissions %#v, want revision 2 without ghost: pending bytes from a dead process must be rolled back", view.Revision, view.Submissions)
	}
}

func TestFilesystemAllowsIndependentClientsAndSerializesTheirCAS(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	first, err := Open(dir, clock.read)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer first.Close()
	second, err := Open(dir, clock.read)
	if err != nil {
		t.Fatalf("second independent Open: %v: host and scheduler must share one coordination truth", err)
	}
	defer second.Close()

	firstValue := admission("first", "job-first", 100).Occurrence
	firstValue.State, firstValue.JobID, firstValue.ReservationID = job.OccurrencePending, "", ""
	_, firstRevision, err := first.RecordOccurrence(0, firstValue)
	if err != nil {
		t.Fatalf("first client mutation: %v", err)
	}
	secondValue := admission("second", "job-second", 100).Occurrence
	secondValue.State, secondValue.JobID, secondValue.ReservationID = job.OccurrencePending, "", ""
	_, actual, err := second.RecordOccurrence(0, secondValue)
	var revisionErr *RevisionError
	if !errors.As(err, &revisionErr) || actual != firstRevision || revisionErr.Actual != firstRevision {
		t.Fatalf("stale independent client returned revision %d, error %v, want stale CAS at %d: each transaction must reload confirmed journal state", actual, err, firstRevision)
	}
	if view := second.View(); view.Revision != firstRevision || len(view.Occurrences) != 1 {
		t.Fatalf("refreshed second view = revision %d, occurrences %d: reads must observe the shared confirmed journal", view.Revision, len(view.Occurrences))
	}
}

func TestStaleAdvisoryLockFileDoesNotBlockOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFile), []byte("pid 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, time.Now)
	if err != nil {
		t.Fatalf("Open with an unlocked lock file: %v: lock-file existence must not turn process death into a permanent outage", err)
	}
	store.Close()
}

func TestOpenFailureDoesNotLeaveAHeldWriterLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, journalFile), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, time.Now); err == nil {
		t.Fatal("corrupt journal opened successfully: recovery would build an unjustified projection")
	}
	if err := os.WriteFile(filepath.Join(dir, journalFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, time.Now)
	if err != nil {
		t.Fatalf("Open after repairing journal: %v: a failed Open must release its advisory lock", err)
	}
	store.Close()
}

func TestNilClockFailsClosedInsteadOfPanicking(t *testing.T) {
	if store, err := Open(t.TempDir(), nil); !errors.Is(err, ErrClockRequired) || store != nil {
		t.Fatalf("Open with nil clock = %#v, %v, want ErrClockRequired: lease authority cannot depend on a missing clock", store, err)
	}
	store := NewMemory(nil)
	_, revision, err := store.Admit(0, admission("occurrence", "job", 100))
	if err != nil {
		t.Fatalf("clock-independent admission failed: %v", err)
	}
	if _, _, err := store.Claim(revision, "job", "worker", time.Minute); !errors.Is(err, ErrClockRequired) {
		t.Fatalf("Claim with nil memory clock returned %v, want ErrClockRequired: configuration mistakes must fail closed rather than panic", err)
	}
}

func TestAdmissionRejectsMalformedBindingsBeforeJournalWrite(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			cases := map[string]func(*Admission){
				"noncanonical ceiling":      func(value *Admission) { value.Ceiling = job.Amount{Coefficient: 100, Scale: 2} },
				"zero ceiling":              func(value *Admission) { value.Ceiling = job.Amount{} },
				"noncanonical reservation":  func(value *Admission) { value.Reserved = job.Amount{Coefficient: 100, Scale: 2} },
				"zero reservation":          func(value *Admission) { value.Reserved = job.Amount{} },
				"missing window":            func(value *Admission) { value.Window = job.LedgerWindow{} },
				"mismatched trigger":        func(value *Admission) { value.Window.TriggerID = "other" },
				"wrong occurrence identity": func(value *Admission) { value.Occurrence.ID = "invented" },
				"split daily window":        func(value *Admission) { value.Window.StartsAt = value.Window.StartsAt.Add(time.Hour) },
				"non-UTC nominal instant": func(value *Admission) {
					value.Occurrence.NominalAt = value.Occurrence.NominalAt.In(time.FixedZone("other", 3600))
					value.Occurrence.ID = job.OccurrenceIdentity(value.Occurrence.TriggerID, value.Occurrence.NominalAt)
				},
				"non-UTC window boundary": func(value *Admission) {
					value.Window.StartsAt = value.Window.StartsAt.In(time.FixedZone("other", 3600))
				},
			}
			for name, mutate := range cases {
				value := admission("occurrence", "job", 100)
				mutate(&value)
				if _, _, err := store.Admit(0, value); err == nil {
					t.Fatalf("%s admission succeeded: malformed accounting identity could be confirmed and poison every projection rebuild", name)
				}
				if view := store.View(); view.Revision != 0 || len(view.Occurrences) != 0 || len(view.Reservations) != 0 {
					t.Fatalf("%s admission mutated revision or projections: failed validation must leave no partial coordination fact", name)
				}
			}
		})
	}
}
