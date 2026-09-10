package jobstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/job"
)

type testClock struct{ now time.Time }

func (c *testClock) read() time.Time { return c.now }

type storeFactory struct {
	name string
	open func(*testing.T, Clock) Store
}

func factories() []storeFactory {
	return []storeFactory{
		{name: "memory", open: func(_ *testing.T, clock Clock) Store { return NewMemory(clock) }},
		{name: "filesystem", open: func(t *testing.T, clock Clock) Store {
			store, err := Open(filepath.Join(t.TempDir(), "journal"), clock)
			if err != nil {
				t.Fatalf("open filesystem store: %v", err)
			}
			return store
		}},
	}
}

func admission(id, jobID string, amount uint64) Admission {
	windowStart := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	var slotOffset int
	for _, value := range []byte(id) {
		slotOffset += int(value)
	}
	nominal := windowStart.Add(time.Duration(slotOffset) * time.Second)
	occurrenceID := job.OccurrenceIdentity("trigger", nominal)
	return Admission{
		Occurrence: job.Occurrence{ID: occurrenceID, TriggerID: "trigger", NominalAt: nominal, State: job.OccurrenceAdmitted, JobID: job.JobID(jobID), ReservationID: "reservation-" + id},
		Window:     job.LedgerWindow{TriggerID: "trigger", Period: job.PeriodDay, StartsAt: windowStart},
		Ceiling:    job.NewAmount(1000, 2), Reserved: job.NewAmount(amount, 2),
	}
}

func seedClaim(t *testing.T, store Store) (job.Claim, Revision) {
	t.Helper()
	_, revision, err := store.Admit(0, admission("occurrence-1", "job-1", 400))
	if err != nil {
		t.Fatalf("admit seed occurrence: %v", err)
	}
	claim, revision, err := store.Claim(revision, "job-1", "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("claim admitted job: %v", err)
	}
	return claim, revision
}

func TestStoreContractBindsIdempotencyAndRejectsConflicts(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			bound := Submission{Key: "request-1", RequestDigest: "digest-a", JobID: "job-a"}
			got, revision, err := store.BindSubmission(0, bound)
			if err != nil || got != bound || revision != 1 {
				t.Fatalf("first submission binding = %#v, revision %d, error %v: an accepted key must durably identify one request and job", got, revision, err)
			}
			got, repeated, err := store.BindSubmission(revision, bound)
			if err != nil || got != bound || repeated != revision {
				t.Fatalf("identical submission repeat advanced or failed at revision %d: retries must observe the original job without changing history: %v", repeated, err)
			}
			conflict := bound
			conflict.RequestDigest = "digest-b"
			if _, _, err := store.BindSubmission(revision, conflict); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicting key reuse returned %v, want ErrConflict: one idempotency key cannot authorize two canonical requests", err)
			}
		})
	}
}

func TestStoreContractFencesClaimsAtExpiry(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			first, revision := seedClaim(t, store)
			if _, _, err := store.Claim(revision, "job-1", "worker-b", time.Minute); !errors.Is(err, ErrConflict) {
				t.Fatalf("second claimant before expiry returned %v, want ErrConflict: two live owners could dispatch the same work", err)
			}
			clock.now = first.ExpiresAt
			second, revision, err := store.Claim(revision, "job-1", "worker-b", time.Minute)
			if err != nil || second.Fence <= first.Fence || second.AttemptID == first.AttemptID {
				t.Fatalf("reclaim after expiry = %#v, revision %d, error %v: takeover must create a fresh attempt and increasing fence", second, revision, err)
			}
			checkpoint := job.Checkpoint{JobID: first.JobID, AttemptID: first.AttemptID, Fence: first.Fence, RunRevision: 1, CompletedCursor: 1, CreatedAt: clock.now}
			if _, err := store.Checkpoint(revision, checkpoint); !errors.Is(err, job.ErrStaleClaim) {
				t.Fatalf("stale checkpoint returned %v, want ErrStaleClaim: the replaced worker could overwrite successor progress", err)
			}
		})
	}
}

func TestStoreContractEnforcesBudgetAndPreservesUnknownHolds(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			_, revision, err := store.Admit(0, admission("occurrence-1", "job-1", 600))
			if err != nil {
				t.Fatalf("admit first reservation: %v", err)
			}
			if _, _, err := store.Admit(revision, admission("occurrence-2", "job-2", 500)); !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("reservation above ceiling returned %v, want ErrBudgetExceeded: concurrent firings could spend more than the declared period maximum", err)
			}
			claim, revision, err := store.Claim(revision, "job-1", "worker", time.Minute)
			if err != nil {
				t.Fatalf("claim reserved job: %v", err)
			}
			revision, err = store.Settle(revision, Settlement{JobID: claim.JobID, AttemptID: claim.AttemptID, Fence: claim.Fence, ReservationID: "reservation-occurrence-1", Kind: SettlementUnknown})
			if err != nil {
				t.Fatalf("mark reservation unknown: %v", err)
			}
			if _, _, err := store.Admit(revision, admission("occurrence-2", "job-2", 500)); !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("unknown hold was released with error %v: ambiguous spend must retain its full reservation until evidence reconciles it", err)
			}
		})
	}
}

func TestStoreContractUsesRevisionCASForBudgetRaces(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			_, revision, err := store.Admit(0, admission("occurrence-1", "job-1", 500))
			if err != nil {
				t.Fatalf("first racing admission: %v", err)
			}
			if _, _, err := store.Admit(0, admission("occurrence-2", "job-2", 500)); !errors.Is(err, ErrRevision) {
				t.Fatalf("stale racing admission returned %v, want ErrRevision: both callers could reserve against the same old ledger view", err)
			}
			if _, _, err := store.Admit(revision, admission("occurrence-2", "job-2", 500)); err != nil {
				t.Fatalf("admission exactly at the ceiling failed: exact decimal arithmetic must allow the declared maximum: %v", err)
			}
		})
	}
}

func TestStoreContractRecordsReceiptsCancellationAndFencedCompletion(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			claim, revision := seedClaim(t, store)
			receipt := job.Receipt{JobID: claim.JobID, AttemptID: claim.AttemptID, Fence: claim.Fence, Provider: "provider", ExternalID: "external", DispatchKey: "dispatch", ObservedAt: clock.now, Status: job.OutcomeSucceeded, OutcomeDigest: "outcome"}
			var err error
			revision, err = store.RecordReceipt(revision, receipt)
			if err != nil {
				t.Fatalf("record receipt evidence: %v", err)
			}
			repeated, err := store.RecordReceipt(revision, receipt)
			if err != nil || repeated != revision {
				t.Fatalf("identical receipt repeat changed history at revision %d: reconciliation retries must be idempotent: %v", repeated, err)
			}
			revision, err = store.Cancel(revision, Cancellation{JobID: claim.JobID, Actor: "operator", Reason: "stop"})
			if err != nil {
				t.Fatalf("record cancellation intent: %v", err)
			}
			view := store.View()
			if !view.Jobs[claim.JobID].CancellationRequested || job.JobTerminal(view.Jobs[claim.JobID].State) {
				t.Fatal("cancellation intent terminated the job: a request cannot prove external work stopped")
			}
			revision, err = store.Complete(revision, Completion{JobID: claim.JobID, AttemptID: claim.AttemptID, Fence: claim.Fence, AttemptState: job.AttemptCancelled, JobState: job.JobCancelled})
			if err != nil {
				t.Fatalf("complete under current fence: %v", err)
			}
			if store.View().Jobs[claim.JobID].State != job.JobCancelled {
				t.Fatal("fenced terminal evidence did not finish the job: cancellation would remain intent forever")
			}
		})
	}
}

func TestStoreContractRejectsCheckpointRegressionAndExpiredHeartbeat(t *testing.T) {
	for _, factory := range factories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)}
			store := factory.open(t, clock.read)
			defer store.Close()
			claim, revision := seedClaim(t, store)
			checkpoint := job.Checkpoint{JobID: claim.JobID, AttemptID: claim.AttemptID, Fence: claim.Fence, RunRevision: 8, CompletedCursor: 5, CreatedAt: clock.now}
			var err error
			revision, err = store.Checkpoint(revision, checkpoint)
			if err != nil {
				t.Fatalf("record monotonic checkpoint: %v", err)
			}
			checkpoint.CompletedCursor--
			if _, err := store.Checkpoint(revision, checkpoint); !errors.Is(err, job.ErrCheckpointRegressed) {
				t.Fatalf("backward checkpoint returned %v, want ErrCheckpointRegressed: recovery could repeat already-confirmed execution", err)
			}
			clock.now = claim.ExpiresAt
			if _, _, err := store.Heartbeat(revision, claim.JobID, claim.AttemptID, claim.Fence, time.Minute); !errors.Is(err, job.ErrExpiredClaim) {
				t.Fatalf("heartbeat at expiry returned %v, want ErrExpiredClaim: a stale worker cannot renew authority after its deadline", err)
			}
		})
	}
}
