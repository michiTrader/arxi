package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

type nativeLoopExecutor struct {
	log               *memLog
	requests          []turn.Request
	toolCalls         int
	toolCallIDs       []string
	toolResultText    string
	callsPerRound     int
	outcomes          map[string]TurnToolOutcome
	policies          map[string]string
	authorizedToolRun int
}

func (x *nativeLoopExecutor) SpawnTurn(context.Context, kernel.SpawnTurn) ([]kernel.Event, error) {
	panic("native turn must use the durable coordinator")
}
func (x *nativeLoopExecutor) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	panic("native tool must use the durable coordinator")
}
func (x *nativeLoopExecutor) AskHuman(context.Context, kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}
func (x *nativeLoopExecutor) PrepareTurn(context.Context, kernel.SpawnTurn) (turn.Request, error) {
	return turn.Request{
		Schema: turn.Schema, Provider: "fake", Protocol: "fake-turn/v1",
		BaseURL: "https://fake.invalid/v1", Model: "fake-model", MaxTokens: 100,
		Messages: []turn.Message{{Role: turn.RoleUser, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "inspect"}}}},
	}, nil
}
func (x *nativeLoopExecutor) CompleteTurn(_ context.Context, req turn.Request) (turn.Response, error) {
	if x.log != nil {
		events, _ := x.log.Read(1, 0)
		if len(events) == 0 || events[len(events)-1].Type != kernel.ExecWorkStarted {
			panic("model request was not prepared and started before dispatch")
		}
	}
	x.requests = append(x.requests, req)
	if len(req.Messages) == 1 {
		count := x.callsPerRound
		if count == 0 {
			count = 1
		}
		content := []turn.ContentBlock{{Type: turn.BlockText, Text: "checking"}}
		for i := 0; i < count; i++ {
			callID, path := "provider-call-7", "README.md"
			if count > 1 {
				callID, path = fmt.Sprintf("provider-call-%d", i+1), fmt.Sprintf("file-%d.txt", i+1)
			}
			call, err := turn.NewToolCall(callID, "read", []byte(fmt.Sprintf(`{"path":%q}`, path)))
			if err != nil {
				return turn.Response{}, err
			}
			content = append(content, turn.ContentBlock{Type: turn.BlockToolCall, ToolCall: &call})
		}
		return turn.Response{Schema: turn.Schema, ID: "response-1", Model: req.Model,
			Content: content, FinishReason: turn.FinishToolCalls,
			Usage: turn.Usage{InputTokens: 10, OutputTokens: 2}}, nil
	}
	return turn.Response{Schema: turn.Schema, ID: "response-2", Model: req.Model,
		Content:      []turn.ContentBlock{{Type: turn.BlockText, Text: "done"}},
		FinishReason: turn.FinishStop, Usage: turn.Usage{InputTokens: 14, OutputTokens: 1}}, nil
}
func (x *nativeLoopExecutor) ResolveTurnToolPolicy(_ kernel.SpawnTurn, call turn.ToolCall) string {
	if policy := x.policies[call.ID]; policy != "" {
		return policy
	}
	return "allow"
}
func (x *nativeLoopExecutor) ExecuteAuthorizedTurnTool(_ context.Context, _ kernel.SpawnTurn, call turn.ToolCall) (TurnToolOutcome, error) {
	x.authorizedToolRun++
	x.toolCalls++
	x.toolCallIDs = append(x.toolCallIDs, call.ID)
	return TurnToolOutcome{Policy: "allow", Continue: true, Result: turn.ToolResult{CallID: call.ID,
		Content: []turn.ContentBlock{{Type: turn.BlockText, Text: x.toolResultText}}}}, nil
}
func (x *nativeLoopExecutor) ExecuteTurnTool(_ context.Context, _ kernel.SpawnTurn, call turn.ToolCall) (TurnToolOutcome, error) {
	x.toolCalls++
	x.toolCallIDs = append(x.toolCallIDs, call.ID)
	if outcome, ok := x.outcomes[call.ID]; ok {
		return outcome, nil
	}
	return TurnToolOutcome{Policy: "allow", Continue: true, Result: turn.ToolResult{
		CallID: call.ID, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: x.toolResultText}},
	}}, nil
}
func (x *nativeLoopExecutor) FinishTurn(e kernel.SpawnTurn, trace []TurnEntry) ([]kernel.Event, error) {
	return []kernel.Event{
		{Type: kernel.AgentActivated, Actor: e.Agent},
		{Type: kernel.LLMResponse, Actor: e.Agent, Payload: map[string]any{"ok": true, "text": "done"}},
		{Type: kernel.AgentTurnDone, Actor: e.Agent},
	}, nil
}

func exactTestRunner(log *memLog, x *nativeLoopExecutor) *Runner {
	config := kernel.Config{Members: []kernel.MemberConfig{{Name: "backend"}}}
	_, _ = log.Append([]kernel.Event{{ID: "run-started", Type: kernel.RunStarted, Source: kernel.SourceHuman,
		Payload: map[string]any{"run_id": "run-exact", "actor": "backend"}}})
	return &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-exact", JobID: "job-exact",
		Config: config,
		Now:    func() string { return "2026-09-11T12:00:00Z" }, Authorization: AuthorizationConfig{
			ToolSchemaVersion: "arxi.tools/v1", PolicyVersion: "arxi.policy/v1",
			WorkspaceProfileID: "workspace-profile-1", TTLMS: 60_000,
		}}
}

func exactGrant(t *testing.T, r *Runner, log *memLog) kernel.ResumeAuthorization {
	t.Helper()
	var request kernel.Event
	for _, event := range log.events {
		if event.Type == kernel.AuthorizationRequested {
			request = event
		}
	}
	if request.Type == "" {
		t.Fatal("policy=ask did not persist authorization.requested: the exact call cannot be approved")
	}
	request.Seq = log.Head() + 1
	grant := kernel.Event{Seq: request.Seq, ID: "grant-exact", Type: kernel.AuthorizationGranted, Source: kernel.SourceHuman,
		Payload: map[string]any{"schema": "arxi.authorization/v1", "authorization_id": request.Str("authorization_id"),
			"action_digest": request.Str("action_digest"), "approver_principal": "operator:alice",
			"grant_event_id": "grant-exact", "expires_at": request.Str("expires_at")}}
	if _, err := log.Append([]kernel.Event{grant}); err != nil {
		t.Fatal(err)
	}
	resume := kernel.ResumeAuthorization{AuthorizationID: request.Str("authorization_id"),
		SuspensionID: request.Str("suspension_id"), ActionDigest: request.Str("action_digest")}
	state := kernel.State{RunID: "run-exact", Status: kernel.StatusRunning,
		Members: []kernel.Member{{Name: "backend", State: kernel.MemberWaiting}},
		Inbox:   []kernel.InboxItem{{ID: request.Str("inbox_id"), Kind: "tool_approval", AuthorizationID: request.Str("authorization_id"), ActionDigest: request.Str("action_digest")}},
		Authorizations: []kernel.Authorization{{Schema: "arxi.authorization/v1", ID: request.Str("authorization_id"), InboxID: request.Str("inbox_id"),
			RequesterPrincipal: request.Str("requester_principal"), SuspensionID: request.Str("suspension_id"), ParentWorkID: request.Str("parent_work_id"),
			ProviderCallID: request.Str("provider_call_id"), Tool: request.Str("tool"), ArgumentDigest: request.Str("argument_digest"),
			ActionDigest: request.Str("action_digest"), ToolSchemaVersion: request.Str("tool_schema_version"), PolicyVersion: request.Str("policy_version"),
			WorkspaceProfileID: request.Str("workspace_profile_id"), ExpiresAt: request.Str("expires_at")}},
	}
	state, _ = kernel.Decide(state, log.events[len(log.events)-1], kernel.Config{})
	if a := state.Authorization(resume.AuthorizationID); a == nil || a.Decision != "granted" {
		t.Fatalf("authorization.granted did not fold into a current grant: exact resume would be refused; state=%#v", state.Authorizations)
	}
	return resume
}

func TestExactAuthorizationSuspendsBeforeToolStartAndResumesOriginalCall(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{log: log, toolResultText: "exact result", policies: map[string]string{"provider-call-7": "ask"}}
	r := exactTestRunner(log, x)
	source := testSource(21)
	if _, err := r.RunStep(context.Background(), source, []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}}); err != nil {
		t.Fatal(err)
	}
	if x.toolCalls != 0 {
		t.Fatalf("policy=ask reached the runner %d times: approval must precede every external dispatch", x.toolCalls)
	}
	var toolStarts int
	for _, event := range log.events {
		if event.Type == kernel.ExecWorkStarted && event.Str("work_scope") == "turn_child" && event.Str("parent_work_id") != "" {
			for _, prepared := range log.events {
				if prepared.Type == kernel.ExecWorkPrepared && prepared.Str("work_id") == event.Str("work_id") && prepared.Str("child_kind") == "tool" {
					toolStarts++
				}
			}
		}
	}
	if toolStarts != 0 {
		t.Fatalf("policy=ask wrote %d tool child starts before approval: crash recovery would claim unapproved work began", toolStarts)
	}
	resume := exactGrant(t, r, log)
	x.policies["provider-call-7"] = "deny"
	if _, err := r.resumeAuthorization(context.Background(), Work{Source: kernel.Event{ID: "grant-exact"}}, resume); err != nil {
		t.Fatal(err)
	}
	if x.authorizedToolRun != 1 || len(x.requests) != 2 {
		t.Fatalf("authorized tool/model calls = %d/%d, want 1/2: resume must execute once then continue the exact transcript", x.authorizedToolRun, len(x.requests))
	}
	result := x.requests[1].Messages[len(x.requests[1].Messages)-1].Content[0].ToolResult
	if result == nil || result.CallID != "provider-call-7" || result.Content[0].Text != "exact result" {
		t.Fatalf("reinjected authorized result = %#v: original provider call id and exact result were not preserved", result)
	}
}

func TestExactAuthorizationResumeRaceConsumesAndRunsOnce(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{policies: map[string]string{"provider-call-7": "ask"}, toolResultText: "once"}
	r := exactTestRunner(log, x)
	if _, err := r.RunStep(context.Background(), testSource(23), []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}}); err != nil {
		t.Fatal(err)
	}
	resume := exactGrant(t, r, log)
	x.policies["provider-call-7"] = "allow"
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.resumeAuthorization(context.Background(), Work{Source: kernel.Event{ID: "grant-exact"}}, resume)
		}(i)
	}
	wg.Wait()
	if x.authorizedToolRun != 1 {
		t.Fatalf("racing resumes invoked the runner %d times, want one: grant consumption must be the dispatch lock", x.authorizedToolRun)
	}
	consumed, started := 0, 0
	for _, event := range log.events {
		if event.Type == kernel.AuthorizationConsumed {
			consumed++
		}
		if event.Type == kernel.ExecWorkStarted && event.Str("work_scope") == "turn_child" {
			for _, prepared := range log.events {
				if prepared.Type == kernel.ExecWorkPrepared && prepared.Str("work_id") == event.Str("work_id") && prepared.Str("child_kind") == "tool" {
					started++
				}
			}
		}
	}
	if consumed != 1 || started != 1 {
		t.Fatalf("racing resumes wrote consume/start = %d/%d, want 1/1: the authoritative CAS batch was not single-use", consumed, started)
	}
}

func TestExactAuthorizationStartedCrashBecomesUnknownWithoutRedispatch(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{policies: map[string]string{"provider-call-7": "ask"}}
	r := exactTestRunner(log, x)
	if _, err := r.RunStep(context.Background(), testSource(24), []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}}); err != nil {
		t.Fatal(err)
	}
	resume := exactGrant(t, r, log)
	var request kernel.Event
	var childID, parentID string
	for _, event := range log.events {
		if event.Type == kernel.AuthorizationRequested {
			request = event
		}
		if event.Type == kernel.ExecWorkPrepared && event.Str("child_kind") == "tool" {
			childID, parentID = event.Str("work_id"), event.Str("parent_work_id")
		}
	}
	_, err := log.AppendIfSeq(log.Head(), []kernel.Event{
		{Type: kernel.AuthorizationConsumed, Payload: map[string]any{"schema": "arxi.authorization/v1", "authorization_id": request.Str("authorization_id"), "action_digest": request.Str("action_digest"), "grant_event_id": "grant-exact", "work_id": childID}},
		{Type: kernel.ExecWorkStarted, Payload: map[string]any{"work_id": childID, "parent_work_id": parentID, "work_scope": "turn_child"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.resumeAuthorization(context.Background(), Work{}, resume)
	if !errors.Is(err, ErrUnknownWork) {
		t.Fatalf("resume after consume/start = %v, want ErrUnknownWork: non-idempotent work must not redispatch", err)
	}
	if x.authorizedToolRun != 0 {
		t.Fatalf("resume after consume/start invoked runner %d times: crash recovery duplicated a potentially mutating call", x.authorizedToolRun)
	}
}

func TestExactAuthorizationRejectsChangedSuspensionBytes(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{policies: map[string]string{"provider-call-7": "ask"}}
	r := exactTestRunner(log, x)
	if _, err := r.RunStep(context.Background(), testSource(22), []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}}); err != nil {
		t.Fatal(err)
	}
	resume := exactGrant(t, r, log)
	for i := range log.events {
		if log.events[i].Type == kernel.ExecWorkPrepared && log.events[i].Str("child_kind") == "authorization" {
			log.events[i].Payload["request_json"] = log.events[i].Str("request_json") + " "
		}
	}
	_, err := r.RunStep(context.Background(), kernel.Event{Seq: log.Head(), ID: "grant-exact", Type: kernel.AuthorizationGranted}, []kernel.Effect{resume})
	if err != nil {
		t.Fatal(err)
	}
	if x.authorizedToolRun != 0 {
		t.Fatalf("changed suspension bytes reached runner %d times: recovery must fail closed", x.authorizedToolRun)
	}
}

func TestDurableNativeTurnReinjectsExactResultUnderProviderCallID(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{log: log, toolResultText: "line one\nline two\n"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-1"}
	effect := kernel.SpawnTurn{Agent: "backend"}

	res, err := r.RunStep(context.Background(), testSource(1), []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if x.toolCalls != 1 || len(x.requests) != 2 {
		t.Fatalf("model requests=%d tool calls=%d, want 2/1", len(x.requests), x.toolCalls)
	}
	result := x.requests[1].Messages[len(x.requests[1].Messages)-1].Content[0].ToolResult
	if result == nil || result.CallID != "provider-call-7" || result.Content[0].Text != x.toolResultText {
		t.Fatalf("reinjected result = %#v", result)
	}
	if got := typesOf(res.Events); !reflect.DeepEqual(got, []kernel.EventType{kernel.AgentActivated, kernel.LLMResponse, kernel.AgentTurnDone}) {
		t.Fatalf("domain events = %v", got)
	}
	var childPrepared, childFinished int
	for _, event := range log.events {
		if event.Str("work_scope") == "turn_child" && event.Type == kernel.ExecWorkPrepared {
			childPrepared++
		}
		if event.Str("work_scope") == "turn_child" && event.Type == kernel.ExecWorkFinished {
			childFinished++
		}
	}
	if childPrepared != 3 || childFinished != 3 {
		t.Fatalf("child prepared/finished = %d/%d, want 3/3", childPrepared, childFinished)
	}
}

func TestDurableNativeTurnResumeDoesNotRepeatCommittedTool(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{toolResultText: "exact output"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-1"}
	source := testSource(2)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work, err := manifest(r.RunID, source, []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepareStep(source, work); err != nil {
		t.Fatal(err)
	}
	_, _ = log.Append([]kernel.Event{r.progressEvent(kernel.ExecWorkStarted, map[string]any{"work_id": work[0].ID}, source)})

	req, _ := x.PrepareTurn(context.Background(), effect)
	progress := newDurableTurnProgress()
	first, err := r.runModelChild(context.Background(), work[0], 0, req, x, &progress)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := responseToolCalls(first)
	outcome, err := r.runToolChild(context.Background(), work[0], effect, 0, calls[0], x, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if x.toolCalls != 1 {
		t.Fatalf("setup executed tool %d times", x.toolCalls)
	}

	res, err := r.RunStep(context.Background(), source, []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if x.toolCalls != 1 {
		t.Fatalf("resume executed committed tool %d times, want once", x.toolCalls)
	}
	if len(res.Events) != 3 {
		t.Fatalf("resume domain events = %v", typesOf(res.Events))
	}
	lastReq := x.requests[len(x.requests)-1]
	got := lastReq.Messages[len(lastReq.Messages)-1].Content[0].ToolResult
	if got == nil || !reflect.DeepEqual(*got, outcome.Result) {
		t.Fatalf("resume reinjected %#v, want committed %#v", got, outcome.Result)
	}
}

func TestDurableNativeTurnRejectsConflictingProviderCallIDBeforeTool(t *testing.T) {
	call, _ := turn.NewToolCall("same", "read", []byte(`{"path":"a"}`))
	other, _ := turn.NewToolCall("same", "read", []byte(`{"path":"b"}`))
	seen := map[string]turn.ToolCall{call.ID: call}
	if err := validateRoundCalls([]turn.ToolCall{other}, seen); err == nil {
		t.Fatal("conflicting provider call id was accepted")
	}
}

func TestDurableNativeTurnReinjectsEveryToolResultInProviderOrder(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{log: log, toolResultText: "exact output", callsPerRound: 2}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-many"}

	if _, err := r.RunStep(context.Background(), testSource(10), []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}}); err != nil {
		t.Fatal(err)
	}
	if x.toolCalls != 2 || len(x.requests) != 2 {
		t.Fatalf("model requests=%d tool calls=%d, want 2/2", len(x.requests), x.toolCalls)
	}
	blocks := x.requests[1].Messages[len(x.requests[1].Messages)-1].Content
	if len(blocks) != 2 {
		t.Fatalf("reinjected result blocks = %d, want 2", len(blocks))
	}
	for i, block := range blocks {
		wantID := fmt.Sprintf("provider-call-%d", i+1)
		if block.ToolResult == nil || block.ToolResult.CallID != wantID || block.ToolResult.Content[0].Text != x.toolResultText {
			t.Fatalf("result %d = %#v, want call id %q", i, block.ToolResult, wantID)
		}
	}
}

func TestPreparedNativeChildDispatchesOnRecovery(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{log: log, toolResultText: "exact output"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-prepared"}
	source, effect := testSource(11), kernel.SpawnTurn{Agent: "backend"}
	work, err := manifest(r.RunID, source, []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepareStep(source, work); err != nil {
		t.Fatal(err)
	}
	_, _ = log.Append([]kernel.Event{r.progressEvent(kernel.ExecWorkStarted, map[string]any{"work_id": work[0].ID}, source)})
	request, _ := x.PrepareTurn(context.Background(), effect)
	prepared, _ := json.Marshal(request)
	progress := newDurableTurnProgress()
	if _, err := r.ensureTurnChild(work[0], turnChildID(work[0].ID, "model/0", string(prepared)), "model", "model/0", string(prepared), &progress); err != nil {
		t.Fatal(err)
	}

	if _, err := r.RunStep(context.Background(), source, []kernel.Effect{effect}); err != nil {
		t.Fatal(err)
	}
	if len(x.requests) != 2 || x.toolCalls != 1 {
		t.Fatalf("recovery model requests=%d tool calls=%d, want 2/1", len(x.requests), x.toolCalls)
	}
}

func TestStartedNativeChildWithoutOutcomeIsUnknown(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{toolResultText: "MUST NOT RUN"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-unknown"}
	source, effect := testSource(12), kernel.SpawnTurn{Agent: "backend"}
	work, _ := manifest(r.RunID, source, []kernel.Effect{effect})
	_ = r.prepareStep(source, work)
	_, _ = log.Append([]kernel.Event{r.progressEvent(kernel.ExecWorkStarted, map[string]any{"work_id": work[0].ID}, source)})
	request, _ := x.PrepareTurn(context.Background(), effect)
	prepared, _ := json.Marshal(request)
	progress := newDurableTurnProgress()
	child, err := r.ensureTurnChild(work[0], turnChildID(work[0].ID, "model/0", string(prepared)), "model", "model/0", string(prepared), &progress)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.startTurnChild(work[0], child); err != nil {
		t.Fatal(err)
	}

	_, err = r.RunStep(context.Background(), source, []kernel.Effect{effect})
	if !errors.Is(err, ErrUnknownWork) {
		t.Fatalf("recovery error = %v, want ErrUnknownWork", err)
	}
	if len(x.requests) != 0 || x.toolCalls != 0 {
		t.Fatalf("ambiguous child redispatched: model=%d tool=%d", len(x.requests), x.toolCalls)
	}
}

func TestNativeChildOutcomeAppendFailureLeavesUnknownWithoutRedispatch(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{toolResultText: "exact output"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-append-failure"}
	source, effect := testSource(13), kernel.SpawnTurn{Agent: "backend"}
	work, _ := manifest(r.RunID, source, []kernel.Effect{effect})
	_ = r.prepareStep(source, work)
	_, _ = log.Append([]kernel.Event{r.progressEvent(kernel.ExecWorkStarted, map[string]any{"work_id": work[0].ID}, source)})
	log.failAppendAt = log.appendCalls + 3 // prepare child and start it, then reject its terminal outcome

	_, err := r.runDurableTurn(context.Background(), work[0], effect, x)
	if err == nil || len(x.requests) != 1 {
		t.Fatalf("first run error=%v model requests=%d, want append error and one dispatch", err, len(x.requests))
	}
	log.failAppendAt = 0
	_, err = r.RunStep(context.Background(), source, []kernel.Effect{effect})
	if !errors.Is(err, ErrUnknownWork) {
		t.Fatalf("recovery error = %v, want ErrUnknownWork", err)
	}
	if len(x.requests) != 1 {
		t.Fatalf("model was redispatched after lost terminal append: %d calls", len(x.requests))
	}
}

func TestStartedNativeParentWithoutChildResumesAtSafeBoundary(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{toolResultText: "exact output"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-parent-gap"}
	source, effect := testSource(14), kernel.SpawnTurn{Agent: "backend"}
	work, _ := manifest(r.RunID, source, []kernel.Effect{effect})
	if err := r.prepareStep(source, work); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]kernel.Event{r.progressEvent(kernel.ExecWorkStarted, map[string]any{"work_id": work[0].ID}, source)}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.RunStep(context.Background(), source, []kernel.Effect{effect}); err != nil {
		t.Fatal(err)
	}
	if len(x.requests) != 2 || x.toolCalls != 1 {
		t.Fatalf("safe parent recovery dispatched model/tool %d/%d times, want 2/1; treating the parent marker as external ambiguity would abandon work before any provider call", len(x.requests), x.toolCalls)
	}
}

func TestCommittedNativeToolOutcomeWithWrongCallIDIsRefused(t *testing.T) {
	log := newMemLog()
	x := &nativeLoopExecutor{toolResultText: "must not run"}
	r := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run-wrong-result-id"}
	source, effect := testSource(15), kernel.SpawnTurn{Agent: "backend"}
	work, _ := manifest(r.RunID, source, []kernel.Effect{effect})
	_ = r.prepareStep(source, work)
	_, _ = log.Append([]kernel.Event{r.progressEvent(kernel.ExecWorkStarted, map[string]any{"work_id": work[0].ID}, source)})
	req, _ := x.PrepareTurn(context.Background(), effect)
	progress := newDurableTurnProgress()
	first, err := r.runModelChild(context.Background(), work[0], 0, req, x, &progress)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := responseToolCalls(first)
	call := calls[0]
	prepared, _ := json.Marshal(call)
	slot := "tool/0/" + call.ID
	child, err := r.ensureTurnChild(work[0], turnChildID(work[0].ID, slot+"/"+call.Name+"/"+call.ArgumentDigest, string(prepared)), "tool", slot, string(prepared), &progress)
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := json.Marshal(TurnToolOutcome{Policy: "allow", Continue: true, Result: turn.ToolResult{CallID: "different", Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "altered"}}}})
	if err := r.finishTurnChild(work[0], child, "completed", bad, nil); err != nil {
		t.Fatal(err)
	}

	res, err := r.RunStep(context.Background(), source, []kernel.Effect{effect})
	if err != nil {
		t.Fatalf("known pre-dispatch refusal became an ambiguous run error: %v", err)
	}
	if len(res.Errs) != 1 || !errors.Is(res.Errs[0], ErrNotDispatched) {
		t.Fatalf("wrong committed call ID errors = %v, want one ErrNotDispatched; reinjecting it would attach one tool's result to another provider request", res.Errs)
	}
	if x.toolCalls != 0 {
		t.Fatalf("wrong committed outcome reached tool runner %d times, want zero", x.toolCalls)
	}
}

func TestNativePolicyStopPreventsLaterCallsInSameBatch(t *testing.T) {
	x := &nativeLoopExecutor{callsPerRound: 3, outcomes: map[string]TurnToolOutcome{
		"provider-call-1": {Policy: "ask", Continue: false, Result: turn.ToolResult{CallID: "provider-call-1", IsError: true, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "approval required"}}}},
	}}
	r := &Runner{Log: newMemLog(), Clock: NewVirtualClock(), Executor: x, RunID: "run-policy-order"}

	if _, err := r.RunStep(context.Background(), testSource(16), []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(x.toolCallIDs, []string{"provider-call-1"}) {
		t.Fatalf("policy stop executed calls %v, want only the first; later calls must not bypass a stopped approval decision", x.toolCallIDs)
	}
}

func TestNativeResponseFinishReasonMustMatchToolCalls(t *testing.T) {
	call, _ := turn.NewToolCall("call-1", "read", []byte(`{"path":"README.md"}`))
	resp := turn.Response{Schema: turn.Schema, FinishReason: turn.FinishStop, Content: []turn.ContentBlock{{Type: turn.BlockToolCall, ToolCall: &call}}}
	if err := validateTurnResponse(resp); err == nil {
		t.Fatal("response with a tool call and stop finish reason was accepted; the coordinator could execute work the provider did not declare")
	}
}

func TestCommittedNativeToolOutcomeContainsExactCanonicalResult(t *testing.T) {
	outcome := TurnToolOutcome{Continue: true, Result: turn.ToolResult{
		CallID: "call-9", IsError: true,
		Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "bytes stay ordered"}, {Type: turn.BlockText, Text: "second"}},
	}}
	body, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	var got TurnToolOutcome
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, outcome) {
		t.Fatalf("round trip = %#v, want %#v", got, outcome)
	}
}
