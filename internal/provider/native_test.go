package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/turn"
)

func TestOpenAINativeTurnRunsToolAndReinjectsProviderCallID(t *testing.T) {
	var requests []chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			body := map[string]any{
				"id": "resp-1", "model": req.Model,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant", "content": nil,
						"tool_calls": []any{map[string]any{
							"id": "provider-call-7", "type": "function",
							"function": map[string]any{"name": "read", "arguments": `{"path":"README.md"}`},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		_ = json.NewEncoder(w).Encode(okBody(14, 1, "done"))
	}))
	t.Cleanup(srv.Close)

	runner := &fakeRunner{result: "line one\nline two\n"}
	x := executorFor(t, srv, kernel.MemberConfig{Name: "backend", Tools: []string{"read"}})
	x.Tools = runner
	req, err := x.PrepareTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := x.CompleteTurn(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	call := first.Content[0].ToolCall
	if call == nil || call.ID != "provider-call-7" {
		t.Fatalf("canonical call = %#v", call)
	}
	outcome, err := x.ExecuteTurnTool(context.Background(), kernel.SpawnTurn{Agent: "backend"}, *call)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages,
		turn.Message{Role: turn.RoleAssistant, Content: first.Content},
		turn.Message{Role: turn.RoleTool, Content: []turn.ContentBlock{{Type: turn.BlockToolResult, ToolResult: &outcome.Result}}},
	)
	second, err := x.CompleteTurn(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || runner.tool != "read" || runner.args["path"] != "README.md" {
		t.Fatalf("runner calls=%d tool=%q args=%v", runner.calls, runner.tool, runner.args)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "provider-call-7" || last.Content != runner.result {
		t.Fatalf("reinjected wire result = %#v", last)
	}
	trace := []exec.TurnEntry{{Response: &first}, {Tool: &exec.TurnToolEntry{Call: *call, Outcome: outcome}}, {Response: &second}}
	events, err := x.FinishTurn(kernel.SpawnTurn{Agent: "backend"}, trace)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agent.activated", "tool.call", "tool.call_completed", "llm.response", "agent.turn_done"}
	if got := types(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if events[1].Payload["call_id"] != "provider-call-7" || events[2].Payload["call_id"] != "provider-call-7" {
		t.Fatalf("call IDs were lost: %#v %#v", events[1].Payload, events[2].Payload)
	}
	if events[3].Payload["tokens_in"] != 24 || events[3].Payload["tokens_out"] != 3 {
		t.Fatalf("usage = %#v", events[3].Payload)
	}
}

func TestAnthropicNativeTurnRunsThroughDurableRunner(t *testing.T) {
	var requests []anthropicRequest
	var paths, keys, versions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
		paths = append(paths, r.URL.Path)
		keys = append(keys, r.Header.Get("x-api-key"))
		versions = append(versions, r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg-1", "type": "message", "role": "assistant", "model": req.Model,
				"content":     []any{map[string]any{"type": "tool_use", "id": "toolu_provider_7", "name": "read", "input": map[string]any{"path": "README.md"}}},
				"stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg-2", "type": "message", "role": "assistant", "model": req.Model,
			"content":     []any{map[string]any{"type": "text", "text": "done"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 14, "output_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)

	runner := &fakeRunner{result: "line one\nline two\n"}
	x := &Executor{
		Resolver: fixedResolver{res: model.Resolution{
			Provider: "anthropic", Protocol: model.ProtocolAnthropicMessages,
			Model: "claude-sonnet-4-6", BaseURL: srv.URL, APIKeyEnv: "ANTHROPIC_TEST_KEY",
		}},
		DefaultModel: "claude-sonnet-4-6", Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"read"}}},
		Prompt: "fix the failing test", Tools: runner,
		NewClient: func(res model.Resolution) *Client {
			return &Client{BaseURL: res.BaseURL, APIKeyEnv: res.APIKeyEnv, Getenv: func(string) string { return "test-secret" }}
		},
	}
	log := newNativeTestLog()
	r := &exec.Runner{Log: log, Clock: exec.NewVirtualClock(), Executor: x, RunID: "run-anthropic"}
	source := kernel.Event{Seq: 1, ID: "source-anthropic", Type: kernel.RunPrompt, Source: kernel.SourceHuman}
	res, err := r.RunStep(context.Background(), source, []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || len(requests) != 2 {
		t.Fatalf("runner calls=%d provider requests=%d, want 1/2", runner.calls, len(requests))
	}
	for i := range requests {
		if paths[i] != "/messages" || keys[i] != "test-secret" || versions[i] != anthropicVersion {
			t.Fatalf("request %d path=%q key=%q version=%q", i, paths[i], keys[i], versions[i])
		}
	}
	if requests[0].System != "" || len(requests[0].Messages) != 1 || requests[0].Messages[0].Role != "user" ||
		len(requests[0].Messages[0].Content) != 1 || requests[0].Messages[0].Content[0].Text != "fix the failing test" ||
		len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "read" {
		t.Fatalf("first request = %#v", requests[0])
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" ||
		last.Content[0].ToolUseID != "toolu_provider_7" || last.Content[0].Content != runner.result {
		t.Fatalf("reinjected wire result = %#v", last)
	}
	want := []string{"agent.activated", "tool.call", "tool.call_completed", "llm.response", "agent.turn_done"}
	if got := types(res.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if res.Events[1].Payload["call_id"] != "toolu_provider_7" || res.Events[2].Payload["call_id"] != "toolu_provider_7" {
		t.Fatalf("call IDs were lost: %#v %#v", res.Events[1].Payload, res.Events[2].Payload)
	}
	if res.Events[3].Payload["tokens_in"] != 24 || res.Events[3].Payload["tokens_out"] != 3 {
		t.Fatalf("usage = %#v", res.Events[3].Payload)
	}
}

func TestOpenAINativeTurnRunsThroughDurableRunner(t *testing.T) {
	var requests []chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp-1", "model": req.Model,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant", "content": nil,
						"tool_calls": []any{map[string]any{
							"id": "provider-call-7", "type": "function",
							"function": map[string]any{"name": "read", "arguments": `{"path":"README.md"}`},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(okBody(14, 1, "done"))
	}))
	t.Cleanup(srv.Close)

	runner := &fakeRunner{result: "line one\nline two\n"}
	x := executorFor(t, srv, kernel.MemberConfig{Name: "backend", Tools: []string{"read"}})
	x.Tools = runner
	log := newNativeTestLog()
	r := &exec.Runner{Log: log, Clock: exec.NewVirtualClock(), Executor: x, RunID: "run-1"}
	source := kernel.Event{Seq: 1, ID: "source-1", Type: kernel.RunPrompt, Source: kernel.SourceHuman}

	res, err := r.RunStep(context.Background(), source, []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || len(requests) != 2 {
		t.Fatalf("runner calls=%d provider requests=%d, want 1/2", runner.calls, len(requests))
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "provider-call-7" || last.Content != runner.result {
		t.Fatalf("reinjected wire result = %#v", last)
	}
	want := []string{"agent.activated", "tool.call", "tool.call_completed", "llm.response", "agent.turn_done"}
	if got := types(res.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	all, err := log.Read(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var prepared, started, finished, toolCheckpoint int
	childKinds := map[string]string{}
	for i, event := range all {
		if event.Str("work_scope") != "turn_child" {
			continue
		}
		switch event.Type {
		case kernel.ExecWorkPrepared:
			prepared++
			childKinds[event.Str("work_id")] = event.Str("child_kind")
		case kernel.ExecWorkStarted:
			started++
		case kernel.ExecWorkFinished:
			finished++
			if childKinds[event.Str("work_id")] == "tool" {
				toolCheckpoint = i + 1
			}
		}
	}
	if prepared != 3 || started != 3 || finished != 3 {
		t.Fatalf("child prepared/started/finished = %d/%d/%d, want 3/3/3", prepared, started, finished)
	}
	if toolCheckpoint == 0 {
		t.Fatal("did not find committed tool checkpoint")
	}

	// Reconstruct exactly the durable prefix that existed after the tool result
	// was committed but before the next model request was prepared. The parent
	// turn is started but deliberately has no terminal record at this boundary.
	resumeLog := newNativeTestLog()
	resumeLog.events = append(resumeLog.events, all[:toolCheckpoint]...)
	resumeLog.head = resumeLog.events[len(resumeLog.events)-1].Seq
	resumeRunner := &fakeRunner{result: "MUST NOT RUN AGAIN"}
	x.Tools = resumeRunner
	resumed := &exec.Runner{Log: resumeLog, Clock: exec.NewVirtualClock(), Executor: x, RunID: "run-1"}
	resumeResult, err := resumed.RunStep(context.Background(), source, []kernel.Effect{kernel.SpawnTurn{Agent: "backend"}})
	if err != nil {
		t.Fatal(err)
	}
	if resumeRunner.calls != 0 {
		t.Fatalf("resume repeated committed tool %d times", resumeRunner.calls)
	}
	if len(requests) != 3 {
		t.Fatalf("provider requests after resume = %d, want 3", len(requests))
	}
	resumedWireResult := requests[2].Messages[len(requests[2].Messages)-1]
	if resumedWireResult.ToolCallID != "provider-call-7" || resumedWireResult.Content != runner.result {
		t.Fatalf("resumed wire result = %#v", resumedWireResult)
	}
	if got := types(resumeResult.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("resumed events = %v, want %v", got, want)
	}
}

func TestAnthropicCanonicalResponseMapsTextRefusalAndCacheUsage(t *testing.T) {
	resp, err := canonicalAnthropicResponse(&anthropicResponse{
		ID: "msg-text", Model: "claude-sonnet-4-6", StopReason: "end_turn",
		Content: []anthropicContentBlock{{Type: "text", Text: "hello"}},
		Usage:   anthropicUsage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 7, CacheCreationInputTokens: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != turn.FinishStop || len(resp.Content) != 1 || resp.Content[0].Text != "hello" ||
		resp.Usage != (turn.Usage{InputTokens: 11, OutputTokens: 3, CacheReadTokens: 7, CacheWriteTokens: 5}) {
		t.Fatalf("canonical text response = %#v", resp)
	}

	refusal, err := canonicalAnthropicResponse(&anthropicResponse{
		ID: "msg-refused", Model: "claude-sonnet-4-6", StopReason: "refusal",
		StopDetails: &anthropicStopDetails{Category: "policy", Explanation: "cannot comply"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if refusal.FinishReason != turn.FinishRefusal || refusal.Refusal == nil ||
		refusal.Refusal.Code != "policy" || refusal.Refusal.Message != "cannot comply" {
		t.Fatalf("canonical refusal = %#v", refusal)
	}
}

func TestOpenAIAndAnthropicWireResponsesProduceEquivalentCanonicalEvents(t *testing.T) {
	openAIFirst, err := canonicalOpenAIResponse(&chatResponse{
		ID: "first", Model: "claude-sonnet-4-6", Choices: []chatChoice{{FinishReason: "tool_calls", Message: chatResponseMessage{
			ToolCalls: []chatToolCall{{ID: "provider-call-equivalent", Type: "function", Function: chatToolFunction{Name: "read", Arguments: `{"path":"README.md"}`}}},
		}}}, Usage: usage{PromptTokens: 10, CompletionTokens: 2, PromptTokensDetails: promptTokenDetails{CachedTokens: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	anthropicFirst, err := canonicalAnthropicResponse(&anthropicResponse{
		ID: "first", Model: "claude-sonnet-4-6", StopReason: "tool_use",
		Content: []anthropicContentBlock{{Type: "tool_use", ID: "provider-call-equivalent", Name: "read", Input: json.RawMessage(`{"path":"README.md"}`)}},
		Usage:   anthropicUsage{InputTokens: 10, OutputTokens: 2, CacheReadInputTokens: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	openAILast, err := canonicalOpenAIResponse(&chatResponse{
		ID: "last", Model: "claude-sonnet-4-6", Choices: []chatChoice{{FinishReason: "stop", Message: chatResponseMessage{Content: stringPointer("done")}}},
		Usage: usage{PromptTokens: 14, CompletionTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	anthropicLast, err := canonicalAnthropicResponse(&anthropicResponse{
		ID: "last", Model: "claude-sonnet-4-6", StopReason: "end_turn", Content: []anthropicContentBlock{{Type: "text", Text: "done"}},
		Usage: anthropicUsage{InputTokens: 14, OutputTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(openAIFirst, anthropicFirst) || !reflect.DeepEqual(openAILast, anthropicLast) {
		t.Fatalf("wire adapters produced different canonical turns:\nOpenAI: %#v / %#v\nAnthropic: %#v / %#v", openAIFirst, openAILast, anthropicFirst, anthropicLast)
	}
	call := *openAIFirst.Content[0].ToolCall
	outcome := exec.TurnToolOutcome{Policy: "allow", Continue: true, Result: turn.ToolResult{CallID: call.ID, Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "result"}}}}
	trace := []exec.TurnEntry{{Response: &openAIFirst}, {Tool: &exec.TurnToolEntry{Call: call, Outcome: outcome}}, {Response: &openAILast}}
	newExecutor := func(protocol string) *Executor {
		return &Executor{DefaultModel: "claude-sonnet-4-6", Members: []kernel.MemberConfig{{Name: "backend"}}, Resolver: fixedResolver{res: model.Resolution{Provider: "test", Protocol: protocol, Model: "claude-sonnet-4-6"}}}
	}
	openAIEvents, err := newExecutor(model.ProtocolOpenAIChatCompletions).FinishTurn(kernel.SpawnTurn{Agent: "backend"}, trace)
	if err != nil {
		t.Fatal(err)
	}
	trace[0].Response, trace[2].Response = &anthropicFirst, &anthropicLast
	anthropicEvents, err := newExecutor(model.ProtocolAnthropicMessages).FinishTurn(kernel.SpawnTurn{Agent: "backend"}, trace)
	if err != nil {
		t.Fatal(err)
	}
	for i := range openAIEvents {
		openAIEvents[i].ID, anthropicEvents[i].ID = "", ""
	}
	if !reflect.DeepEqual(openAIEvents, anthropicEvents) {
		t.Fatalf("canonical projections differ:\nOpenAI: %#v\nAnthropic: %#v", openAIEvents, anthropicEvents)
	}
}

func stringPointer(value string) *string { return &value }

func TestNativeAdaptersRejectStreamingBeforeDispatch(t *testing.T) {
	req := turn.Request{Schema: turn.Schema, Model: "test", MaxTokens: 10, Stream: true}
	if _, err := openAIRequest(req); err == nil {
		t.Fatal("OpenAI streaming reached the ordinary JSON transport; an SSE response would be decoded as one JSON document")
	}
	if _, err := anthropicTurnRequest(req); err == nil {
		t.Fatal("Anthropic streaming reached the ordinary JSON transport; an SSE response would be decoded as one JSON document")
	}
}

func TestOpenAIRefusalAndUnknownFinishProjectAsFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		code   string
	}{
		{name: "content filter", reason: "content_filter", code: "content_filter"},
		{name: "unknown reason", reason: "future_reason", code: "unsupported_finish_reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := canonicalOpenAIResponse(&chatResponse{ID: "response", Model: "claude-sonnet-4-6", Choices: []chatChoice{{FinishReason: tc.reason, Message: chatResponseMessage{}}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Refusal == nil || resp.Refusal.Code != tc.code {
				t.Fatalf("canonical failure = %#v, want refusal code %q", resp, tc.code)
			}
			x := &Executor{DefaultModel: "claude-sonnet-4-6", Members: []kernel.MemberConfig{{Name: "backend"}}, Resolver: fixedResolver{res: model.Resolution{Model: "claude-sonnet-4-6"}}}
			events, err := x.FinishTurn(kernel.SpawnTurn{Agent: "backend"}, []exec.TurnEntry{{Response: &resp}})
			if err != nil {
				t.Fatal(err)
			}
			if ok, _ := events[1].Payload["ok"].(bool); ok || events[1].Str("error") == "" {
				t.Fatalf("failure projected as success: %#v", events[1].Payload)
			}
		})
	}
}

func TestNativeTurnPolicyStopsBeforeRunner(t *testing.T) {
	for _, policy := range []string{"deny", "ask"} {
		t.Run(policy, func(t *testing.T) {
			call, err := turn.NewToolCall("call-1", "bash", []byte(`{"command":"rm harmless.tmp"}`))
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{result: "MUST NOT RUN"}
			x := &Executor{Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"bash"}}}, Tools: runner}
			if policy == "deny" {
				x.Members[0].Tools = nil
			}
			outcome, err := x.ExecuteTurnTool(context.Background(), kernel.SpawnTurn{Agent: "backend"}, call)
			if err != nil {
				t.Fatal(err)
			}
			if runner.calls != 0 {
				t.Fatalf("%s reached runner %d times", policy, runner.calls)
			}
			if outcome.Continue || outcome.Policy != policy || !outcome.Result.IsError || outcome.Result.CallID != call.ID {
				t.Fatalf("outcome = %#v", outcome)
			}
		})
	}
}

func TestNativeAskSurvivesTurnDoneInReducer(t *testing.T) {
	call, err := turn.NewToolCall("provider-call-ask", "bash", []byte(`{"command":"go test ./..."}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &Executor{
		Members:      []kernel.MemberConfig{{Name: "backend", Tools: []string{"bash"}}},
		DefaultModel: "claude-sonnet-4-6",
		Resolver: fixedResolver{res: model.Resolution{
			Provider: "test", Protocol: model.ProtocolOpenAIChatCompletions, Model: "claude-sonnet-4-6",
		}},
	}
	outcome, err := x.ExecuteTurnTool(context.Background(), kernel.SpawnTurn{Agent: "backend"}, call)
	if err != nil {
		t.Fatal(err)
	}
	response := turn.Response{Schema: turn.Schema, ID: "resp-ask", Model: "claude-sonnet-4-6", FinishReason: turn.FinishToolCalls}
	events, err := x.FinishTurn(kernel.SpawnTurn{Agent: "backend"}, []exec.TurnEntry{
		{Response: &response},
		{Tool: &exec.TurnToolEntry{Call: call, Outcome: outcome}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		events[i].Seq = int64(i + 1)
	}
	state, _ := kernel.Fold(kernel.State{Members: []kernel.Member{{Name: "backend", State: kernel.MemberIdle}}}, events, kernel.Config{})
	member := state.Member("backend")
	if member == nil || member.State != kernel.MemberWaiting || member.TurnOpen {
		t.Fatalf("member after ask turn = %#v", member)
	}
	if len(state.Inbox) != 1 || member.BlockedOn == nil || member.BlockedOn["inbox_id"] != state.Inbox[0].ID {
		t.Fatalf("inbox=%#v blocked_on=%#v", state.Inbox, member.BlockedOn)
	}
}

type nativeTestLog struct {
	mu     sync.Mutex
	events []kernel.Event
	head   int64
}

func newNativeTestLog() *nativeTestLog { return &nativeTestLog{} }

func (l *nativeTestLog) Append(events []kernel.Event) ([]kernel.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(events)
}

func (l *nativeTestLog) AppendIfSeq(expectedSeq int64, events []kernel.Event) ([]kernel.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.head != expectedSeq {
		return nil, fmt.Errorf("log head changed from %d to %d", expectedSeq, l.head)
	}
	return l.appendLocked(events)
}

func (l *nativeTestLog) appendLocked(events []kernel.Event) ([]kernel.Event, error) {
	out := make([]kernel.Event, len(events))
	for i, event := range events {
		l.head++
		event.Seq = l.head
		l.events = append(l.events, event)
		out[i] = event
	}
	return out, nil
}

func (l *nativeTestLog) Read(fromSeq, toSeq int64) ([]kernel.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fromSeq < 1 {
		fromSeq = 1
	}
	if toSeq == 0 || toSeq > l.head {
		toSeq = l.head
	}
	var out []kernel.Event
	for _, event := range l.events {
		if event.Seq >= fromSeq && event.Seq <= toSeq {
			out = append(out, event)
		}
	}
	return out, nil
}

func (l *nativeTestLog) Head() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.head
}

func (l *nativeTestLog) Fold(config kernel.Config, untilSeq int64) (kernel.State, error) {
	events, _ := l.Read(1, untilSeq)
	state, _ := kernel.Fold(kernel.State{}, events, config)
	return state, nil
}

func (l *nativeTestLog) WriteSnapshot(kernel.State, int64) error { return nil }
