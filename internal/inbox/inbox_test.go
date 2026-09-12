package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
)

// blocked writes a real run directory whose log leaves backend waiting on an
// approval, and returns its path.
//
// The log is written THROUGH logstore rather than by hand, which is what makes
// these tests worth having: a hand-written file would let this package's reader
// and the real writer drift apart, and the drift would look like "this run has
// no questions" -- indistinguishable from a run that has none.
func blocked(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r1")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), []byte(
		"name: team\n"+
			"members:\n"+
			"  - name: backend\n"+
			"    tools: [read, write, bash]\n"+
			"stages:\n"+
			"  - name: execute\n"+
			"    advance_when: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		// run_id is in the payload because the real `run start` puts it there and
		// the reducer reads it from there. An earlier version of this fixture
		// omitted it, and TestTheListNamesTheRun caught the empty RunID -- the
		// fixture was wrong, not the code, and a fixture that is not faithful to
		// the real writer is worse than no fixture.
		{ID: "e1", Type: kernel.RunStarted, Payload: map[string]any{
			"actor": "team", "run_id": "r1", "budget_usd": 1.0}},
		{ID: "e2", Type: kernel.StageEntered, Payload: map[string]any{"stage": "execute", "index": 0}},
		{ID: "e3", Type: kernel.AgentActivated, Actor: "backend", Payload: map[string]any{"agent": "backend"}},
		{ID: "e4", Type: kernel.ToolCallDenied, Actor: "backend",
			Payload: map[string]any{"tool": "bash", "policy": "ask"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAQuestionSurvivesTheProcessThatAskedIt(t *testing.T) {
	dir := blocked(t)

	// Nothing of the asking process remains: this is a fresh read of a
	// directory, which is the whole claim the package makes.
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := r.List(true)
	if len(items) != 1 {
		t.Fatalf("read %d pending questions from the log, want 1: %+v", len(items), items)
	}
	got := items[0]
	if got.ID != "inbox-1" {
		t.Errorf("id = %q, want inbox-1 (the reducer mints it, so a different id means "+
			"the answer will match nothing)", got.ID)
	}
	if got.Kind != "tool_approval" {
		t.Errorf("kind = %q, want tool_approval", got.Kind)
	}
	if !strings.Contains(got.Question, "bash") {
		t.Errorf("question = %q, want it to name the tool being approved", got.Question)
	}
	if got.Agent != "backend" {
		t.Errorf("agent = %q, want backend", got.Agent)
	}
	if got.OnTimeout != "deny" {
		t.Errorf("on_timeout = %q, want deny: a human deciding whether to deal with "+
			"this now needs to know what ignoring it costs", got.OnTimeout)
	}
	if got.Replied {
		t.Error("the question reads as already answered, and nobody answered it")
	}
}

func TestTheListNamesTheRunBecauseIdsRepeatAcrossRuns(t *testing.T) {
	dir := blocked(t)
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := r.List(true)
	if len(items) == 0 {
		t.Fatal("no questions")
	}
	if items[0].RunID == "" {
		t.Error("the item carries no run id. inbox-1 is minted per run, so two blocked " +
			"runs both have one, and a list showing only the id invites approving the wrong")
	}
}

func TestAnsweringUnblocksTheMemberThatWasWaiting(t *testing.T) {
	dir := blocked(t)

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if m := st.Member("backend"); m == nil || m.State != kernel.MemberWaiting {
		t.Fatalf("backend is not waiting before the answer: %+v", m)
	}

	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove}); err != nil {
		t.Fatal(err)
	}

	// Re-read from disk. Folding the same directory again is how another
	// process would see the answer, which is the only view that matters.
	r2, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	st2 := r2.State()
	m := st2.Member("backend")
	if m == nil {
		t.Fatal("backend vanished from the run")
	}
	if m.State == kernel.MemberWaiting {
		t.Error("backend is still waiting after the approval was appended: the answer " +
			"did not reach the reducer, so the run waits forever while looking healthy")
	}
	if m.BlockedOn != nil {
		t.Errorf("blocked_on survived the answer: %v", m.BlockedOn)
	}
	if pending := r2.List(true); len(pending) != 0 {
		t.Errorf("%d questions still pending after answering the only one: %+v",
			len(pending), pending)
	}
}

func TestTheLogSaysWhetherAHumanApprovedOrRejected(t *testing.T) {
	// The reducer unblocks on inbox_id alone and reads neither the text nor the
	// decision, so nothing downstream forces this to be recorded. That is
	// exactly why it is tested: for a command whose purpose is authorising a
	// tool, "who said yes" is the one fact the history must not lose.
	for _, tc := range []struct {
		name  string
		reply Reply
	}{
		{"approve", Reply{Decision: DecisionApprove}},
		{"reject", Reply{Decision: DecisionReject, Text: "it hits staging"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := blocked(t)
			ev, err := Answer(dir, "inbox-1", tc.reply)
			if err != nil {
				t.Fatal(err)
			}
			if got := ev.Str("decision"); got != tc.reply.Decision {
				t.Errorf("logged decision = %q, want %q: the reducer cannot tell these "+
					"apart, so if the event does not say it, nothing does",
					got, tc.reply.Decision)
			}
			if got := ev.Str("text"); got != tc.reply.Text {
				t.Errorf("logged text = %q, want %q", got, tc.reply.Text)
			}
			if ev.Source != kernel.SourceHuman {
				t.Errorf("source = %q, want %q: it is the only thing distinguishing a "+
					"human approving a tool from the runtime timing the question out",
					ev.Source, kernel.SourceHuman)
			}
			if ev.Seq == 0 {
				t.Error("the appended event came back with no seq, so it was never written")
			}
		})
	}
}

func question(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		{ID: "e1", Type: kernel.RunStarted, Payload: map[string]any{"run_id": "r1"}},
		{ID: "e2", Type: kernel.InboxCreated, Payload: map[string]any{
			"inbox_id": "inbox-1", "kind": "question", "question": "which target?",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exactApproval(t *testing.T, mutate func(*kernel.Event)) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r1")
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
	if _, err := store.Append([]kernel.Event{
		{ID: "start-exact", Type: kernel.RunStarted, Payload: map[string]any{"run_id": "r1"}},
		{ID: "activate-exact", Type: kernel.AgentActivated, Actor: "worker", Payload: map[string]any{"agent": "worker"}},
	}); err != nil {
		t.Fatal(err)
	}
	request := kernel.Event{ID: "authorization-request", Type: kernel.AuthorizationRequested, Actor: "worker", Payload: map[string]any{
		"schema": "arxi.authorization/v1", "authorization_id": "authorization-1", "inbox_id": "approval-1",
		"requester_principal": "agent:worker", "suspension_id": "suspension-1", "parent_work_id": "parent-1",
		"provider_call_id": "call-1", "tool": "bash", "argument_digest": strings.Repeat("a", 64),
		"action_digest": strings.Repeat("b", 64), "tool_schema_version": "bash/v1", "policy_version": "policy-1",
		"workspace_profile_id": "workspace-1", "expires_at": "2026-09-12T00:00:00Z", "after_ms": int64(60000),
	}}
	if mutate != nil {
		mutate(&request)
	}
	item := kernel.Event{ID: "approval-item", Type: kernel.InboxCreated, Payload: map[string]any{
		"inbox_id": "approval-1", "kind": "tool_approval", "question": "allow bash?",
		"authorization_id": "authorization-1", "action_digest": strings.Repeat("b", 64),
	}}
	if _, err := store.Append([]kernel.Event{request, item}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readExactApprovalState(t *testing.T, dir string) ([]kernel.Event, kernel.State) {
	t.Helper()
	read, err := logstore.ReadConfirmed(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := decodeEvents(dir, read.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := kernel.Fold(kernel.State{}, events, kernel.Config{Members: []kernel.MemberConfig{{Name: "worker"}}})
	return events, state
}

func TestDueExactDecisionsPersistOneExpiryAndReplayTheRefusal(t *testing.T) {
	for _, decision := range []Reply{
		{Decision: DecisionApprove, Principal: "operator:alice"},
		{Decision: DecisionReject, Text: "unsafe", Principal: "operator:alice"},
	} {
		t.Run(decision.Decision, func(t *testing.T) {
			dir := exactApproval(t, nil)
			observed := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
			result, err := DecideExact(dir, "approval-1", decision, observed)
			if !errors.Is(err, ErrAuthorizationExpired) {
				t.Fatalf("due %s error = %v, want ErrAuthorizationExpired: elapsed authority must fail closed after recording expiry", decision.Decision, err)
			}
			if result.Reply.Type != "" || result.Authorization == nil || result.Authorization.Type != kernel.AuthorizationExpired {
				t.Fatalf("due %s result = %#v: failure must persist only authorization.expired, never a reply or terminal human decision", decision.Decision, result)
			}
			events, state := readExactApprovalState(t, dir)
			last := events[len(events)-1]
			a := state.Authorization("authorization-1")
			if last.Type != kernel.AuthorizationExpired || last.Source != kernel.SourceRuntime || last.Str("expired_at") != "2026-09-12T00:00:00Z" {
				t.Fatalf("persisted expiry = %+v: replay needs the immutable deadline judgment in a runtime event", last)
			}
			if a == nil || a.Decision != "expired" || !state.InboxItem("approval-1").Replied {
				t.Fatalf("replayed authorization/inbox = %+v/%+v: restart could expose elapsed authority again; fold the durable expiry as terminal", a, state.InboxItem("approval-1"))
			}
		})
	}
}

func TestAlreadyRecordedExpiryIsStableAndIdempotent(t *testing.T) {
	dir := exactApproval(t, nil)
	at := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if _, err := DecideExact(dir, "approval-1", Reply{Decision: DecisionApprove, Principal: "operator:alice"}, at); !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("first due approval error = %v, want ErrAuthorizationExpired", err)
	}
	eventsBefore, _ := readExactApprovalState(t, dir)
	_, err := DecideExact(dir, "approval-1", Reply{Decision: DecisionReject, Text: "still unsafe", Principal: "operator:bob"}, at.Add(time.Hour))
	if !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("decision after recorded expiry = %v, want ErrAlreadyAnswered: the durable terminal fact must win without another expiry append", err)
	}
	eventsAfter, _ := readExactApprovalState(t, dir)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("recorded expiry grew log from %d to %d events: retries must not duplicate terminal judgments", len(eventsBefore), len(eventsAfter))
	}
}

func TestExactApprovalRecordsPrincipalAndDecisionInOneBatch(t *testing.T) {
	dir := exactApproval(t, nil)
	oldNow := now
	now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	defer func() { now = oldNow }()
	if _, err := AnswerExact(dir, "approval-1", Reply{Decision: DecisionApprove, Principal: "operator:alice"}); err != nil {
		t.Fatalf("a valid exact approval failed: suspended work cannot resume; commit its grant and reply together: %v", err)
	}
	read, err := logstore.ReadConfirmed(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := decodeEvents(dir, read.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-2:]
	if last[0].Type != kernel.AuthorizationGranted || last[1].Type != kernel.InboxReplied {
		t.Fatalf("exact approval appended %q then %q: the grant must fold before the reply and share its batch; append authorization.granted followed by inbox.replied", last[0].Type, last[1].Type)
	}
	if last[0].Str("approver_principal") != "operator:alice" || last[1].Str("principal") != "operator:alice" {
		t.Fatalf("decision principals = %q/%q: audit cannot identify who authorized the mutation; record the authenticated principal on both records", last[0].Str("approver_principal"), last[1].Str("principal"))
	}
}

func TestExactApprovalFailsClosedOnPrincipalAndBindingErrors(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		mutate    func(*kernel.Event)
		want      error
	}{
		{"empty principal", "", nil, ErrInvalidPrincipal},
		{"self approval", "agent:worker", nil, ErrInvalidPrincipal},
		{"mismatched digest", "operator:alice", func(e *kernel.Event) { e.Payload["inbox_id"] = "request-only" }, ErrAuthorizationBinding},
		{"missing binding", "operator:alice", func(e *kernel.Event) { e.Payload["inbox_id"] = "request-only" }, ErrAuthorizationBinding},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := exactApproval(t, tt.mutate)
			_, err := AnswerExact(dir, "approval-1", Reply{Decision: DecisionApprove, Principal: tt.principal})
			if !errors.Is(err, tt.want) {
				t.Fatalf("exact approval error = %v, want %v: ambiguous authority could execute unintended work; refuse before appending either decision record", err, tt.want)
			}
		})
	}
}

func TestDecisionVerbsRequireTheirExactItemKind(t *testing.T) {
	dir := blocked(t)
	if _, err := AnswerExact(dir, "inbox-1", Reply{Decision: DecisionAnswer, Text: "staging"}); !errors.Is(err, ErrWrongDecisionKind) {
		t.Fatalf("answering approval returned %v, want ErrWrongDecisionKind", err)
	}

	for _, reply := range []Reply{{Decision: DecisionApprove}, {Decision: DecisionReject, Text: "no"}} {
		dir = question(t)
		if _, err := AnswerExact(dir, "inbox-1", reply); !errors.Is(err, ErrWrongDecisionKind) {
			t.Fatalf("%s on question returned %v, want ErrWrongDecisionKind", reply.Decision, err)
		}
	}

	dir = question(t)
	if _, err := AnswerExact(dir, "inbox-1", Reply{Decision: DecisionAnswer, Text: "staging"}); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerRequiresNonWhitespaceText(t *testing.T) {
	dir := question(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionAnswer, Text: " \n\t"}); err == nil {
		t.Fatal("whitespace answer was accepted")
	}
}

func TestAnsweringTwiceIsRefusedBecauseTheSecondReplyBuysATurn(t *testing.T) {
	dir := blocked(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove}); err != nil {
		t.Fatal(err)
	}
	_, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove})
	if !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second answer returned %v, want ErrAlreadyAnswered: applyInboxReplied "+
			"spawns a turn for the member it unblocks, so a duplicate costs money", err)
	}
}

func TestAnUnknownIdIsToldApartFromAnAnsweredOne(t *testing.T) {
	dir := blocked(t)
	_, err := Answer(dir, "inbox-99", Reply{Decision: DecisionApprove})
	if !errors.Is(err, ErrNoSuchItem) {
		t.Fatalf("unknown id returned %v, want ErrNoSuchItem", err)
	}
	if errors.Is(err, ErrAlreadyAnswered) {
		t.Error("an unknown id also matches ErrAlreadyAnswered, so the two cannot be " +
			"told apart: one is a typo, the other is two people at one run")
	}
}

func TestARejectionWithoutAReasonIsRefused(t *testing.T) {
	dir := blocked(t)
	_, err := Answer(dir, "inbox-1", Reply{Decision: DecisionReject})
	if err == nil {
		t.Fatal("a reason-less rejection was accepted. The reason reaches the agent as " +
			"context, and without one its best guess is to retry what was just refused")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error does not mention the reason: %v", err)
	}
	// And it must not have been written.
	r, err2 := OpenRun(dir)
	if err2 != nil {
		t.Fatal(err2)
	}
	if pending := r.List(true); len(pending) != 1 {
		t.Errorf("the refused rejection changed the log: %d pending, want 1", len(pending))
	}
}

func TestWhitespaceIsNotAReason(t *testing.T) {
	dir := blocked(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionReject, Text: "   \n\t"}); err == nil {
		t.Fatal("spaces were accepted as a reason, which reaches the agent as nothing")
	}
}

func TestAnUnrecognisedDecisionIsRefusedRatherThanDefaulted(t *testing.T) {
	dir := blocked(t)
	for _, d := range []string{"", "yes", "APPROVE", "ok"} {
		_, err := Answer(dir, "inbox-1", Reply{Decision: d, Text: "x"})
		if err == nil {
			t.Errorf("decision %q was accepted. Every available default is a lie about "+
				"what a human said: approve authorises what nobody authorised, reject "+
				"discards work over a typo", d)
		}
	}
}

// endRun appends a terminal event to a run written by blocked, leaving its
// question pending and unanswerable.
//
// run.cancelled rather than run.succeeded, because that is the state a human can
// reach deliberately in one command (`arxi run cancel`) while a question is
// outstanding -- which is how this defect was found.
func endRun(t *testing.T, dir string) {
	t.Helper()
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append([]kernel.Event{{
		ID: "cancel-1", Type: kernel.RunCancelled, Source: kernel.SourceHuman,
		Payload: map[string]any{"reason": "the requirement moved"},
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestAnsweringAQuestionInARunThatEndedIsRefused.
//
// The append would succeed and change nothing: the reducer folds every event
// arriving at a terminal run into nothing (internal/kernel/decide.go:32). Before
// this check the CLI printed "approved. backend unblocked (r1 seq 7)" for it -- a
// sentence in which only the seq was true, and one that leaves whoever is waiting
// on that member waiting forever.
func TestAnsweringAQuestionInARunThatEndedIsRefused(t *testing.T) {
	dir := blocked(t)
	endRun(t, dir)

	_, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove})
	if !errors.Is(err, ErrRunOver) {
		t.Fatalf("answering in a cancelled run returned %v, want ErrRunOver", err)
	}
	if errors.Is(err, ErrAlreadyAnswered) {
		t.Error("a terminal run also matches ErrAlreadyAnswered, and the remedies have " +
			"nothing in common: one says look at the log, the other says the work needs " +
			"a new run")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("the error does not say HOW the run ended: %v", err)
	}

	// And nothing was written. An inert inbox.replied in the log is a human
	// decision recorded where nothing reads it.
	r, err2 := OpenRun(dir)
	if err2 != nil {
		t.Fatal(err2)
	}
	if pending := r.List(true); len(pending) != 1 {
		t.Errorf("the refused answer changed the log: %d pending, want 1", len(pending))
	}
}

// TestATypoIsStillATypoInARunThatEnded pins the ORDER of the two refusals.
//
// The terminal check is deliberately after the item lookup: "no such question" is
// the more precise answer when it applies, and a user who mistyped an id in a
// cancelled run needs to hear about the typo rather than be told the run is over
// and go on believing the id was right.
func TestATypoIsStillATypoInARunThatEnded(t *testing.T) {
	dir := blocked(t)
	endRun(t, dir)

	_, err := Answer(dir, "inbox-99", Reply{Decision: DecisionApprove})
	if !errors.Is(err, ErrNoSuchItem) {
		t.Fatalf("an unknown id in a terminal run returned %v, want ErrNoSuchItem", err)
	}
}

func TestAnsweredQuestionsAreStillReadable(t *testing.T) {
	dir := blocked(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove}); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	all := r.List(false)
	if len(all) != 1 {
		t.Fatalf("history lost the answered question: %+v", all)
	}
	if !all[0].Replied {
		t.Error(`the answered item does not read as replied, so "did I already approve ` +
			`that?" has no honest answer`)
	}
}

func TestTheReaderWorksWhileTheRunHoldsTheWriterLock(t *testing.T) {
	dir := blocked(t)

	// A run is in progress: something owns the directory for writing. This is
	// the normal state of affairs when a human types `arxi inbox`, because the
	// reason they are typing it is that a run stopped and is waiting.
	held, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatalf("reading the inbox failed while the run held the lock: %v\n"+
			"  this is the exact situation the command exists for", err)
	}
	if len(r.List(true)) != 1 {
		t.Error("the question was not readable while the log was locked for writing")
	}
}

func TestATruncatedLastLineIsStaleRatherThanCorrupt(t *testing.T) {
	dir := blocked(t)

	// A batch in flight: the writer has put bytes down and not yet finished the
	// line. Refusing here would make `arxi inbox` fail while a run is active.
	path := filepath.Join(dir, eventsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"e5","type":"inbox.replied","pay`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatalf("an unterminated final line was treated as corruption: %v", err)
	}
	if pending := r.List(true); len(pending) != 1 {
		t.Errorf("%d pending, want 1: the half-written line should be ignored, not "+
			"folded", len(pending))
	}
}

func TestACompleteProvisionalReplyStaysInvisible(t *testing.T) {
	dir := blocked(t)
	path := filepath.Join(dir, eventsFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reply := kernel.Event{
		Seq: 5, ID: "e5", Type: kernel.InboxReplied,
		Payload: map[string]any{"inbox_id": "inbox-1", "text": "approved"},
	}
	line, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(before, append(line, '\n')...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending.commit"),
		[]byte(fmt.Sprintf("%d\n", len(before))), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pending := r.List(true); len(pending) != 1 {
		t.Fatalf("%d pending, want 1: a complete provisional reply was folded before commit", len(pending))
	}
}

func TestAGarbledCompleteLineIsRefusedRatherThanSkipped(t *testing.T) {
	dir := blocked(t)
	path := filepath.Join(dir, eventsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json at all}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := OpenRun(dir); err == nil {
		t.Fatal("a complete but unparseable line was skipped. The log is the run's only " +
			"history, so silently dropping part of it produces a state with no events " +
			"to justify it")
	}
}

func TestADirectoryWithNoLogSaysSoAndPointsSomewhere(t *testing.T) {
	_, err := OpenRun(t.TempDir())
	if err == nil {
		t.Fatal("an empty directory was accepted as a run")
	}
	if !strings.Contains(err.Error(), "runs/") {
		t.Errorf("the error does not say where runs live, so the user has nowhere to "+
			"look: %v", err)
	}
}

func TestTheEventsFileNameAgreesWithLogstore(t *testing.T) {
	// This package names the log file itself rather than importing the constant,
	// which logstore keeps private. The risk of a copy is that it drifts and
	// every run then reads as having no questions -- indistinguishable from a
	// run that has none. So the agreement is checked by behaviour: logstore
	// wrote the file in blocked(), and this asserts the name it chose.
	dir := blocked(t)
	if _, err := os.Stat(filepath.Join(dir, eventsFileName)); err != nil {
		t.Fatalf("logstore did not write %q, so this package is reading the wrong "+
			"file and would report every run as having no questions: %v",
			eventsFileName, err)
	}
}

func TestQuestionsAreOrderedNumericallyNotLexicographically(t *testing.T) {
	// inbox-10 sorting before inbox-2 is cosmetic right up until somebody
	// approves the wrong line of a list they picked from by eye.
	ids := []string{"inbox-10", "inbox-2", "inbox-1"}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			a, b := ids[i], ids[j]
			if a == "inbox-10" && b == "inbox-2" && itemLess(a, b) {
				t.Error("inbox-10 sorts before inbox-2")
			}
		}
	}
	if !itemLess("inbox-2", "inbox-10") {
		t.Error("inbox-2 does not sort before inbox-10")
	}
	if !itemLess("inbox-1", "inbox-2") {
		t.Error("inbox-1 does not sort before inbox-2")
	}
	// A non-conforming id must not panic or reorder silently.
	if itemLess("inbox-x", "inbox-x") {
		t.Error("an id is less than itself")
	}
}

func TestARunWithNoFrozenBlueprintIsStillReadable(t *testing.T) {
	dir := blocked(t)
	if err := os.Remove(filepath.Join(dir, "blueprint.snapshot.yaml")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatalf("a run with no frozen blueprint could not be read: %v\n"+
			"  the questions are in the events, and every testdata scenario has no "+
			"blueprint", err)
	}
	if len(r.List(true)) != 1 {
		t.Error("the question was not readable without a blueprint")
	}
}

func TestItemFindsOneQuestionAndNamesTheRunWhenItCannot(t *testing.T) {
	dir := blocked(t)
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Item("inbox-1"); err != nil {
		t.Fatalf("the question that List returned was not findable by id: %v", err)
	}
	_, err = r.Item("inbox-7")
	if !errors.Is(err, ErrNoSuchItem) {
		t.Fatalf("missing id returned %v, want ErrNoSuchItem", err)
	}
}
