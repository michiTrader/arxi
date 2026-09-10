package v1

import (
	"context"
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/runconfig"
)

// textExecutor adapts the public, provider-neutral text port to the internal
// durable effect contract. It deliberately implements only Phase 1 text turns.
type textExecutor struct {
	provider  TextProvider
	effective runconfig.Artifact
}

func (x *textExecutor) SpawnTurn(ctx context.Context, effect kernel.SpawnTurn) ([]kernel.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, exec.NotDispatched(fmt.Errorf("spawn turn for %s: %w", effect.Agent, err))
	}
	model := x.effective.DefaultModel
	for _, member := range x.effective.Config.Members {
		if member.Name == effect.Agent && member.Model != "" {
			model = member.Model
			break
		}
	}
	response, err := x.provider.CompleteText(ctx, TextRequest{
		Model: model, System: textSystem(effect.Context), Prompt: x.effective.Prompt,
		MaxTokens: effect.Context.MaxTokens,
	})
	if err != nil {
		// The public port promises that Go errors are ambiguous. Do not fabricate a
		// durable provider response or claim that dispatch did not occur.
		return nil, fmt.Errorf("spawn turn for %s: %w", effect.Agent, err)
	}
	payload := map[string]any{
		"agent": effect.Agent, "cost_usd": 0.0,
		"coalesced": effect.Coalesced, "model": model,
		"ok": true, "text": response.Text,
	}
	events := []kernel.Event{
		{Type: kernel.AgentActivated, Source: kernel.SourceRuntime, Actor: effect.Agent,
			Payload: map[string]any{"agent": effect.Agent}},
		{Type: kernel.LLMResponse, Source: kernel.SourceAgent, Actor: effect.Agent, Payload: payload},
		{Type: kernel.StageSubmitted, Source: kernel.SourceAgent,
			Actor: effect.Agent, Payload: map[string]any{"agent": effect.Agent, "result": response.Text}},
		{Type: kernel.AgentTurnDone, Source: kernel.SourceAgent,
			Actor: effect.Agent, Payload: map[string]any{"agent": effect.Agent}},
	}
	return events, nil
}

func textSystem(spec kernel.ContextSpec) string {
	parts := make([]string, 0, 4+len(spec.Situation)+len(spec.Shared)+len(spec.Cause))
	if spec.Identity != "" {
		parts = append(parts, "Identity: "+spec.Identity)
	}
	parts = append(parts, spec.Situation...)
	if spec.Memory != "" {
		parts = append(parts, "Memory: "+spec.Memory)
	}
	parts = append(parts, spec.Shared...)
	if len(spec.Cause) > 0 {
		parts = append(parts, "Causes: "+strings.Join(spec.Cause, ", "))
	}
	return strings.Join(parts, "\n")
}

func (x *textExecutor) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	return nil, exec.NotDispatched(fmt.Errorf("public text provider does not support tool calls"))
}

func (x *textExecutor) AskHuman(_ context.Context, effect kernel.AskHuman) ([]kernel.Event, error) {
	return []kernel.Event{{
		Type: kernel.InboxCreated, Source: kernel.SourceRuntime, Actor: effect.Agent,
		Payload: map[string]any{"inbox_id": effect.ID, "kind": effect.Kind,
			"question": effect.Question, "agent": effect.Agent,
			"on_timeout": effect.OnTimeout, "timeout_ms": effect.TimeoutMs},
	}}, nil
}
