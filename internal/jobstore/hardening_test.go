package jobstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

func TestFilesystemRefusesASecondWriterAndReopensAfterClose(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	first, err := Open(dir, clock.read)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_, err = Open(dir, clock.read)
	var locked *LockedError
	if !errors.As(err, &locked) {
		first.Close()
		t.Fatalf("second independent Open returned %v, want *LockedError: two instances could assign the same revision and corrupt the coordination journal", err)
	}
	if locked.Owner == "" {
		first.Close()
		t.Fatal("lock conflict omitted its owner: an operator cannot distinguish a live writer from a stale lock")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first writer: %v", err)
	}
	reopened, err := Open(dir, clock.read)
	if err != nil {
		t.Fatalf("Open after Close: %v: a clean restart must release and reacquire coordination ownership", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened writer: %v", err)
	}
}

func TestOpenFailureReleasesItsWriterLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, journalFile), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, time.Now); err == nil {
		t.Fatal("corrupt journal opened successfully: recovery would build an unjustified projection")
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Open left writer lock behind: fixing storage would still require unrelated manual lock deletion: %v", err)
	}
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
