package app

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runread"
)

func mutationFixture(t *testing.T) (MutationServices, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		{ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"run_id": "r1", "actor": "team", "simulated": true}},
		{ID: "approval", Type: kernel.InboxCreated, Source: kernel.SourceRuntime, Payload: map[string]any{
			"inbox_id": "approval-1", "kind": "tool_approval", "question": "allow bash?", "agent": "worker",
		}},
		{ID: "question", Type: kernel.InboxCreated, Source: kernel.SourceRuntime, Payload: map[string]any{
			"inbox_id": "question-1", "kind": "question", "question": "which target?", "agent": "worker",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return MutationServices{RunsDir: root, Now: func() time.Time {
		return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	}}, dir
}

func appErrorKind(t *testing.T, err error, want ErrorKind) {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) || got.Kind != want {
		t.Fatalf("error = %v, want app kind %q", err, want)
	}
}

func TestLegacyPendingApprovalFailsClosedForLiveMutation(t *testing.T) {
	s, _ := mutationFixture(t)
	_, err := s.Approve(Decision{JobID: "r1", ItemID: "approval-1", Principal: "operator:alice"})
	appErrorKind(t, err, InvalidArgument)
	if !errors.Is(err, inbox.ErrAuthorizationBinding) {
		t.Fatalf("legacy approval error = %v: historical logs must remain readable but live mutation cannot invent exact authority; return the missing-binding refusal", err)
	}
}

func TestExactDecisionServicesValidateKindAndReturnRefold(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(MutationServices) (MutationResult, error)
	}{
		{"answer question", func(s MutationServices) (MutationResult, error) {
			return s.Answer(Decision{JobID: "r1", ItemID: "question-1", Text: "staging", Principal: "operator:alice"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := mutationFixture(t)
			result, err := tc.call(s)
			if err != nil {
				t.Fatal(err)
			}
			if result.State.Seq != 4 || !result.Simulated {
				t.Fatalf("updated result = seq %d simulated %v, want seq 4 simulated", result.State.Seq, result.Simulated)
			}
		})
	}

	for _, tc := range []struct {
		name string
		call func(MutationServices) error
	}{
		{"approve question", func(s MutationServices) error {
			_, err := s.Approve(Decision{JobID: "r1", ItemID: "question-1"})
			return err
		}},
		{"reject question", func(s MutationServices) error {
			_, err := s.Reject(Decision{JobID: "r1", ItemID: "question-1", Text: "no"})
			return err
		}},
		{"answer approval", func(s MutationServices) error {
			_, err := s.Answer(Decision{JobID: "r1", ItemID: "approval-1", Text: "yes"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := mutationFixture(t)
			appErrorKind(t, tc.call(s), WrongDecisionKind)
		})
	}
}

func TestHeldWriterAndConcurrentDecisionsAreClassified(t *testing.T) {
	t.Run("held writer", func(t *testing.T) {
		s, dir := mutationFixture(t)
		held, err := logstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		_, err = s.Approve(Decision{JobID: "r1", ItemID: "approval-1"})
		appErrorKind(t, err, Conflict)
	})

	t.Run("concurrent exact decisions", func(t *testing.T) {
		s, _ := mutationFixture(t)
		start := make(chan struct{})
		errs := make(chan error, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		for i := 0; i < 2; i++ {
			go func() {
				ready.Done()
				<-start
				_, err := s.Approve(Decision{JobID: "r1", ItemID: "approval-1"})
				errs <- err
			}()
		}
		ready.Wait()
		close(start)
		var success, refused int
		for i := 0; i < 2; i++ {
			err := <-errs
			if err == nil {
				success++
				continue
			}
			var appErr *Error
			if !errors.As(err, &appErr) || (appErr.Kind != Conflict && appErr.Kind != AlreadyDecided) {
				t.Fatalf("concurrent refusal = %v, want conflict or already_decided", err)
			}
			refused++
		}
		if success != 1 || refused != 1 {
			t.Fatalf("success/refused = %d/%d, want 1/1", success, refused)
		}
	})
}

func TestStoreMutationsPreserveSemanticsWithHeldWriter(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(MutationServices, *logstore.Store) (MutationResult, error)
	}{
		{"cancel", func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.CancelStore(store, "r1", "resident cancel")
		}},
		{"approve", func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.ApproveStore(store, Decision{JobID: "r1", ItemID: "approval-1"})
		}},
		{"reject", func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.RejectStore(store, Decision{JobID: "r1", ItemID: "approval-1", Text: "unsafe"})
		}},
		{"answer", func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.AnswerStore(store, Decision{JobID: "r1", ItemID: "question-1", Text: "staging"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := mutationFixture(t)
			store, err := logstore.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			result, err := tc.call(s, store)
			if err != nil {
				t.Fatal(err)
			}
			if result.State.Seq != 4 {
				t.Fatalf("result seq = %d, want 4", result.State.Seq)
			}
		})
	}

	s, dir := mutationFixture(t)
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = s.AnswerStore(store, Decision{JobID: "r1", ItemID: "approval-1", Text: "yes"})
	appErrorKind(t, err, WrongDecisionKind)
}

func TestCancelUsesConfirmedCLIEventShapeAndReturnsRefold(t *testing.T) {
	s, dir := mutationFixture(t)
	result, err := s.Cancel("r1", "  requirement moved  ")
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Status != kernel.StatusCancelled || result.State.Seq != 4 {
		t.Fatalf("cancel result = status %q seq %d", result.State.Status, result.State.Seq)
	}
	run, err := runread.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev := run.Events[len(run.Events)-1]
	if ev.Type != kernel.RunCancelled || ev.Source != kernel.SourceHuman || ev.Scope != "run:r1" || ev.Str("reason") != "requirement moved" {
		t.Fatalf("cancel event = %+v", ev)
	}
	_, err = s.Cancel("r1", "again")
	appErrorKind(t, err, AlreadyTerminal)
}

func TestExactDecisionServiceErrors(t *testing.T) {
	s, dir := mutationFixture(t)
	_, err := s.Reject(Decision{JobID: "r1", ItemID: "approval-1", Text: " \n"})
	appErrorKind(t, err, InvalidArgument)
	_, err = s.Answer(Decision{JobID: "r1", ItemID: "question-1", Text: "\t"})
	appErrorKind(t, err, InvalidArgument)
	_, err = s.Approve(Decision{JobID: "missing", ItemID: "approval-1"})
	appErrorKind(t, err, NotFound)
	_, err = s.Approve(Decision{JobID: "r1", ItemID: "missing"})
	appErrorKind(t, err, NotFound)
	_, err = s.Approve(Decision{JobID: "../r1", ItemID: "approval-1"})
	appErrorKind(t, err, InvalidArgument)

	if _, err = s.Approve(Decision{JobID: "r1", ItemID: "approval-1"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Approve(Decision{JobID: "r1", ItemID: "approval-1"})
	appErrorKind(t, err, AlreadyDecided)

	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Append([]kernel.Event{{ID: "cancel", Type: kernel.RunCancelled}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	_, err = s.Answer(Decision{JobID: "r1", ItemID: "question-1", Text: "staging"})
	appErrorKind(t, err, AlreadyTerminal)
}
