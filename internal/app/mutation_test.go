package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if err := os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), []byte("name: team\nmembers:\n  - name: worker\nstages:\n  - name: work\n    advance_when: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		{ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"run_id": "r1", "actor": "team", "simulated": true}},
		exactAuthorizationRequest("worker", "approval-1"),
		{ID: "approval", Type: kernel.InboxCreated, Source: kernel.SourceRuntime, Payload: map[string]any{
			"inbox_id": "approval-1", "kind": "tool_approval", "question": "allow bash?", "agent": "worker",
			"authorization_id": "authorization-1", "action_digest": strings.Repeat("b", 64),
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

func exactAuthorizationRequest(actor, inboxID string) kernel.Event {
	return kernel.Event{ID: "authorization-request", Type: kernel.AuthorizationRequested, Source: kernel.SourceRuntime, Actor: actor, Payload: map[string]any{
		"schema": "arxi.authorization/v1", "authorization_id": "authorization-1", "inbox_id": inboxID,
		"requester_principal": "agent:" + actor, "suspension_id": "suspension-1", "parent_work_id": "parent-1",
		"provider_call_id": "call-1", "tool": "bash", "argument_digest": strings.Repeat("a", 64),
		"action_digest": strings.Repeat("b", 64), "tool_schema_version": "arxi.tool.bash/v1", "policy_version": "policy-1",
		"workspace_profile_id": "workspace-1", "expires_at": "2026-09-12T00:00:00Z", "after_ms": int64(60000),
	}}
}

func legacyMutationFixture(t *testing.T) MutationServices {
	t.Helper()
	s, dir := mutationFixture(t)
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]kernel.Event{{ID: "legacy-approval", Type: kernel.InboxCreated, Source: kernel.SourceRuntime, Payload: map[string]any{
		"inbox_id": "legacy-approval-1", "kind": "tool_approval", "question": "allow legacy bash?", "agent": "worker",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

func appErrorKind(t *testing.T, err error, want ErrorKind) {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) || got.Kind != want {
		t.Fatalf("error = %v, want app kind %q", err, want)
	}
}

func TestMutationServicesMaterializeApprovalExpiryBeforeFailure(t *testing.T) {
	observed := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	for _, decision := range []struct {
		name string
		call func(MutationServices) (MutationResult, error)
	}{
		{"approve", func(s MutationServices) (MutationResult, error) {
			return s.Approve(Decision{JobID: "r1", ItemID: "approval-1", Principal: "operator:alice"})
		}},
		{"reject", func(s MutationServices) (MutationResult, error) {
			return s.Reject(Decision{JobID: "r1", ItemID: "approval-1", Text: "unsafe", Principal: "operator:alice"})
		}},
	} {
		t.Run(decision.name, func(t *testing.T) {
			s, dir := mutationFixture(t)
			s.Now = func() time.Time { return observed }
			_, err := decision.call(s)
			appErrorKind(t, err, InvalidArgument)
			if !errors.Is(err, inbox.ErrAuthorizationExpired) {
				t.Fatalf("due %s error = %v: callers need the stable expiry refusal after its event commits", decision.name, err)
			}
			run, readErr := runread.Open(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			last := run.Events[len(run.Events)-1]
			if last.Type != kernel.AuthorizationExpired || last.Str("expired_at") != "2026-09-12T00:00:00Z" {
				t.Fatalf("due %s persisted %+v: application mutations must leave replayable expiry evidence", decision.name, last)
			}
			if a := run.State.Authorization("authorization-1"); a == nil || a.Decision != "expired" || a.ConsumingWorkID != "" {
				t.Fatalf("due %s replay = %+v: expiry must neither grant nor consume the action", decision.name, a)
			}
		})
	}
}

func TestLegacyPendingApprovalFailsClosedForLiveMutation(t *testing.T) {
	s := legacyMutationFixture(t)
	before, err := s.Inspect("r1")
	if err != nil || before.State.InboxItem("legacy-approval-1") == nil {
		t.Fatalf("legacy approval did not survive replay before mutation: historical logs would become unreadable; preserve the unbound item for inspection while refusing new authority: state=%#v err=%v", before.State.Inbox, err)
	}
	_, err = s.Approve(Decision{JobID: "r1", ItemID: "legacy-approval-1", Principal: "operator:alice"})
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
			if result.State.Seq != 5 || !result.Simulated {
				t.Fatalf("updated result = seq %d simulated %v, want seq 5 simulated: exact authorization adds one immutable request event; keep result assertions aligned with the structurally valid fixture", result.State.Seq, result.Simulated)
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
				_, err := s.Approve(Decision{JobID: "r1", ItemID: "approval-1", Principal: "operator:alice"})
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
		name    string
		wantSeq int64
		call    func(MutationServices, *logstore.Store) (MutationResult, error)
	}{
		{"cancel", 5, func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.CancelStore(store, "r1", "resident cancel")
		}},
		{"approve", 6, func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.ApproveStore(store, Decision{JobID: "r1", ItemID: "approval-1", Principal: "operator:alice"})
		}},
		{"reject", 6, func(s MutationServices, store *logstore.Store) (MutationResult, error) {
			return s.RejectStore(store, Decision{JobID: "r1", ItemID: "approval-1", Text: "unsafe", Principal: "operator:alice"})
		}},
		{"answer", 5, func(s MutationServices, store *logstore.Store) (MutationResult, error) {
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
			if result.State.Seq != tc.wantSeq {
				t.Fatalf("result seq = %d, want %d: exact approval decisions append an authorization record plus the linked reply, while other mutations append one event; preserve the atomic decision shape in the refold", result.State.Seq, tc.wantSeq)
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
	if result.State.Status != kernel.StatusCancelled || result.State.Seq != 5 {
		t.Fatalf("cancel result = status %q seq %d: cancellation must remain terminal and preserve the exact authorization event in the refold; want cancelled at seq 5", result.State.Status, result.State.Seq)
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

	if _, err = s.Answer(Decision{JobID: "r1", ItemID: "question-1", Text: "staging", Principal: "operator:alice"}); err != nil {
		t.Fatalf("a valid question answer failed: generic questions must keep legacy single-reply semantics; append the answer without requiring authorization: %v", err)
	}
	_, err = s.Answer(Decision{JobID: "r1", ItemID: "question-1", Text: "production", Principal: "operator:bob"})
	appErrorKind(t, err, AlreadyDecided)

	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Append([]kernel.Event{{ID: "cancel", Type: kernel.RunCancelled}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	_, err = s.Approve(Decision{JobID: "r1", ItemID: "approval-1", Principal: "operator:alice"})
	appErrorKind(t, err, AlreadyTerminal)
}
