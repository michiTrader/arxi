package v1

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/runconfig"
)

type recordingTextProvider struct {
	request  TextRequest
	response TextResponse
	err      error
}

func (p *recordingTextProvider) CompleteText(_ context.Context, request TextRequest) (TextResponse, error) {
	p.request = request
	return p.response, p.err
}

func TestTextExecutorTurnsACompletionIntoThePhaseOneLifecycle(t *testing.T) {
	provider := &recordingTextProvider{response: TextResponse{Text: "finished"}}
	executor := &textExecutor{provider: provider, effective: runconfig.Artifact{
		Prompt: "do it", DefaultModel: "default",
		Config: kernel.Config{Members: []kernel.MemberConfig{{Name: "worker", Model: "member-model"}}},
	}}
	events, err := executor.SpawnTurn(context.Background(), kernel.SpawnTurn{
		Agent: "worker", Coalesced: 2,
		Context: kernel.ContextSpec{Identity: "builder", Situation: []string{"one"}, Memory: "remember",
			Shared: []string{"two"}, Cause: []string{"event-1"}, MaxTokens: 321},
	})
	if err != nil {
		t.Fatalf("SpawnTurn: %v", err)
	}
	wantRequest := TextRequest{Model: "member-model", System: "Identity: builder\none\nMemory: remember\ntwo\nCauses: event-1", Prompt: "do it", MaxTokens: 321}
	if !reflect.DeepEqual(provider.request, wantRequest) {
		t.Fatalf("request = %#v, want %#v", provider.request, wantRequest)
	}
	wantTypes := []kernel.EventType{kernel.AgentActivated, kernel.LLMResponse, kernel.StageSubmitted, kernel.AgentTurnDone}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %#v, want %d lifecycle events", events, len(wantTypes))
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
	response := events[1].Payload
	for _, absent := range []string{"tokens_in", "tokens_out", "retryable", "error"} {
		if _, ok := response[absent]; ok {
			t.Errorf("llm.response publishes Phase 2 field %q: %#v", absent, response)
		}
	}
	if response["ok"] != true || response["text"] != "finished" || response["cost_usd"] != float64(0) {
		t.Errorf("llm.response = %#v", response)
	}
	if events[2].Payload["result"] != "finished" {
		t.Errorf("stage.submitted = %#v", events[2].Payload)
	}
}

func TestTextExecutorDoesNotFabricateEventsForAnUntrustworthyError(t *testing.T) {
	providerErr := errors.New("completion outcome is unknown")
	executor := &textExecutor{provider: &recordingTextProvider{err: providerErr}, effective: runconfig.Artifact{
		Prompt: "do it", DefaultModel: "default",
	}}
	events, err := executor.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "worker"})
	if !errors.Is(err, providerErr) {
		t.Fatalf("SpawnTurn error = %v, want wrapped provider error", err)
	}
	if errors.Is(err, exec.ErrNotDispatched) {
		t.Fatalf("provider error was incorrectly marked not dispatched: %v", err)
	}
	if events != nil {
		t.Fatalf("events = %#v, want none for an untrustworthy outcome", events)
	}
}
