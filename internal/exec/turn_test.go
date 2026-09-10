package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

type nativeLoopExecutor struct {
	log            *memLog
	requests       []turn.Request
	toolCalls      int
	toolCallIDs    []string
	toolResultText string
	callsPerRound  int
	outcomes       map[string]TurnToolOutcome
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
