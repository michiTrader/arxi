package v1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/runconfig"
)

func exactHostStorage(t *testing.T) (*memoryStorage, JobID) {
	t.Helper()
	storage := newMemoryStorage()
	id := JobID("r1")
	records := []kernel.Event{
		{ID: "start", Type: kernel.RunStarted, Payload: map[string]any{"run_id": "r1"}},
		{ID: "activate", Type: kernel.AgentActivated, Actor: "worker", Payload: map[string]any{"agent": "worker"}},
		{ID: "request", Type: kernel.AuthorizationRequested, Actor: "worker", Payload: map[string]any{
			"schema": "arxi.authorization/v1", "authorization_id": "authorization-1", "inbox_id": "approval-1",
			"requester_principal": "agent:worker", "suspension_id": "suspension-1", "parent_work_id": "parent-1",
			"provider_call_id": "call-1", "tool": "bash", "argument_digest": strings.Repeat("a", 64),
			"action_digest": strings.Repeat("b", 64), "tool_schema_version": "bash/v1", "policy_version": "policy-1",
			"workspace_profile_id": "workspace-1", "expires_at": "2026-09-12T00:00:00Z", "after_ms": int64(60000),
		}},
		{ID: "question", Type: kernel.InboxCreated, Payload: map[string]any{"inbox_id": "question-1", "kind": "question", "question": "which target?"}},
	}
	stored := make([]StoredRecord, len(records))
	for i, event := range records {
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		stored[i] = StoredRecord{Data: body}
	}
	blueprint := "name: team\nmembers:\n  - name: worker\nstages:\n  - name: work\n    advance_when: all\n"
	effective := runconfig.New("r1", "live", "sha", "work", "host", kernel.Config{Members: []kernel.MemberConfig{{Name: "worker"}}}, nil, nil)
	metadata, err := json.Marshal(storedJobMetadata{Effective: effective})
	if err != nil {
		t.Fatal(err)
	}
	created, err := storage.Create(context.Background(), CreateJob{Record: JobRecord{ID: id, Data: metadata}, Artifacts: []Artifact{{Name: "blueprint", Data: []byte(blueprint)}}, Records: stored})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Writer.Close(); err != nil {
		t.Fatal(err)
	}
	return storage, id
}

func hostAuthorizationEvents(t *testing.T, storage *memoryStorage, id JobID) []kernel.Event {
	t.Helper()
	storage.mu.Lock()
	records := cloneStoredRecords(storage.jobs[id].records)
	storage.mu.Unlock()
	events := make([]kernel.Event, len(records))
	for i, record := range records {
		if err := json.Unmarshal(record.Data, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

func TestHostDueApprovalAndRejectPersistExpiryBeforeStableFailure(t *testing.T) {
	for _, decision := range []struct {
		name string
		call func(*Host, JobID) error
	}{
		{"approve", func(h *Host, id JobID) error {
			_, err := h.Approve(context.Background(), ApproveRequest{Principal: Principal{ID: "operator:alice"}, JobID: id, ItemID: "approval-1"})
			return err
		}},
		{"reject", func(h *Host, id JobID) error {
			_, err := h.Reject(context.Background(), RejectRequest{Principal: Principal{ID: "operator:alice"}, JobID: id, ItemID: "approval-1", Reason: "unsafe"})
			return err
		}},
	} {
		t.Run(decision.name, func(t *testing.T) {
			storage, id := exactHostStorage(t)
			h := New(Options{Storage: storage, Now: func() time.Time { return time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) }})
			defer h.Close()
			err := decision.call(h, id)
			if !IsCode(err, CodeInvalidArgument) || !errors.Is(err, errAuthorizationExpired) {
				t.Fatalf("due host %s error = %v: host/v1 must preserve its stable invalid-argument refusal after expiry commits", decision.name, err)
			}
			events := hostAuthorizationEvents(t, storage, id)
			last := events[len(events)-1]
			if last.Type != kernel.AuthorizationExpired || last.Source != kernel.SourceRuntime || last.Str("expired_at") != "2026-09-12T00:00:00Z" {
				t.Fatalf("due host %s persisted %+v: restart needs a durable runtime expiry judgment", decision.name, last)
			}
			state, _ := kernel.Fold(kernel.State{}, events, kernel.Config{Members: []kernel.MemberConfig{{Name: "worker"}}})
			if a := state.Authorization("authorization-1"); a == nil || a.Decision != "expired" || a.GrantEventID != "" || a.ConsumingWorkID != "" {
				t.Fatalf("due host %s replay = %+v: failed mutation must neither grant nor consume", decision.name, a)
			}
			repeated := decision.call(h, id)
			if !IsCode(repeated, CodeInvalidArgument) || !errors.Is(repeated, errAuthorizationExpired) {
				t.Fatalf("recorded host expiry retry = %v: host/v1 must return the same stable refusal", repeated)
			}
			if got := len(hostAuthorizationEvents(t, storage, id)); got != len(events) {
				t.Fatalf("recorded host expiry grew log from %d to %d: retry must not append a duplicate judgment", len(events), got)
			}
		})
	}
}

func TestHostRacingDueExpiryAndDecisionCommitOneTerminalJudgment(t *testing.T) {
	storage, id := exactHostStorage(t)
	var mu sync.Mutex
	observed := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	h := New(Options{Storage: storage, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return observed }})
	defer h.Close()
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, due := range []bool{false, true} {
		go func(due bool) {
			<-start
			mu.Lock()
			if due {
				observed = time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
			} else {
				observed = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			}
			mu.Unlock()
			_, err := h.Approve(context.Background(), ApproveRequest{Principal: Principal{ID: "operator:alice"}, JobID: id, ItemID: "approval-1"})
			errs <- err
		}(due)
	}
	close(start)
	for range 2 {
		<-errs
	}
	var grants, expiries int
	for _, event := range hostAuthorizationEvents(t, storage, id) {
		switch event.Type {
		case kernel.AuthorizationGranted:
			grants++
		case kernel.AuthorizationExpired:
			expiries++
		}
	}
	if grants+expiries != 1 {
		t.Fatalf("host race committed %d grants and %d expiries: serialized writer/CAS must choose one terminal judgment", grants, expiries)
	}
}

func TestHostExactApprovalCarriesPrincipalAndCommitsOneBatch(t *testing.T) {
	storage, id := exactHostStorage(t)
	h := New(Options{Storage: storage, Now: func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }})
	defer h.Close()
	if _, err := h.Approve(context.Background(), ApproveRequest{Principal: Principal{ID: "operator:alice"}, JobID: id, ItemID: "approval-1"}); err != nil {
		t.Fatalf("host rejected a valid exact approval: suspended work cannot resume; preserve the request principal and append one decision batch: %v", err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	got := storage.jobs[id].records[len(storage.jobs[id].records)-2:]
	var grant, reply kernel.Event
	if err := json.Unmarshal(got[0].Data, &grant); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got[1].Data, &reply); err != nil {
		t.Fatal(err)
	}
	if grant.Type != kernel.AuthorizationGranted || reply.Type != kernel.InboxReplied || grant.Str("approver_principal") != "operator:alice" || reply.Str("principal") != "operator:alice" {
		t.Fatalf("decision records = %+v / %+v: audit and exact authority diverged; append grant then reply with the authenticated request principal", grant, reply)
	}
}

func TestHostExactApprovalRefusesEmptySelfAndLegacyPrincipals(t *testing.T) {
	for _, principal := range []string{"", "agent:worker"} {
		storage, id := exactHostStorage(t)
		h := New(Options{Storage: storage})
		_, err := h.Approve(context.Background(), ApproveRequest{Principal: Principal{ID: principal}, JobID: id, ItemID: "approval-1"})
		_ = h.Close()
		if !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("principal %q returned %v: unauthenticated or self authority could approve mutation; reject it as invalid", principal, err)
		}
	}
}

func TestHostQuestionAnswerKeepsLegacySingleReplySemantics(t *testing.T) {
	storage, id := exactHostStorage(t)
	h := New(Options{Storage: storage})
	defer h.Close()
	if _, err := h.Answer(context.Background(), AnswerRequest{Principal: Principal{ID: "operator:alice"}, JobID: id, ItemID: "question-1", Text: "staging"}); err != nil {
		t.Fatalf("question answer failed: exact authorization must not break generic questions; append their legacy reply: %v", err)
	}
}

func TestHostRacingApproversCommitOneDecision(t *testing.T) {
	storage, id := exactHostStorage(t)
	h := New(Options{Storage: storage, Now: func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }})
	defer h.Close()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, principal := range []string{"operator:alice", "operator:bob"} {
		go func(principal string) {
			ready.Done()
			<-start
			_, err := h.Approve(context.Background(), ApproveRequest{Principal: Principal{ID: principal}, JobID: id, ItemID: "approval-1"})
			errs <- err
		}(principal)
	}
	ready.Wait()
	close(start)
	success := 0
	for range 2 {
		if err := <-errs; err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("racing approvers produced %d successful decisions: one action could gain duplicate authority; serialize under writer CAS so exactly one wins", success)
	}
}
