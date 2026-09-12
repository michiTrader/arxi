package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/runconfig"
)

// textExecutor adapts the public, provider-neutral text port to the internal
// durable effect contract. It deliberately implements only Phase 1 text turns.
type textExecutor struct {
	provider   TextProvider
	tools      ToolExecutor
	workspaces WorkspaceProvisioner
	jobID      JobID
	effective  runconfig.Artifact

	mu       sync.Mutex
	handles  map[string]Workspace
	requests map[string]WorkspaceRequest
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

func (x *textExecutor) CallTool(ctx context.Context, effect kernel.CallTool) ([]kernel.Event, error) {
	if x.tools == nil || x.workspaces == nil {
		return nil, exec.NotDispatched(fmt.Errorf("public text host has no tool executor and workspace provisioner configured"))
	}
	handle, err := x.workspace(ctx, effect.Agent)
	if err != nil {
		return nil, exec.NotDispatched(err)
	}
	arguments, err := json.Marshal(effect.Args)
	if err != nil {
		return nil, exec.NotDispatched(fmt.Errorf("encode tool arguments: %w", err))
	}
	result, err := x.tools.Execute(ctx, ToolInvocation{JobID: x.jobID, Actor: effect.Agent,
		Name: effect.Tool, Arguments: append(json.RawMessage(nil), arguments...), Workspace: handle})
	if err != nil {
		return nil, err
	}
	return []kernel.Event{{Type: kernel.ToolCallCompleted, Source: kernel.SourceAgent, Actor: effect.Agent,
		Payload: map[string]any{"tool": effect.Tool, "result": string(result.Content)}}}, nil
}

func (x *textExecutor) workspace(ctx context.Context, actor string) (Workspace, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if handle := x.handles[actor]; handle != "" {
		return handle, nil
	}
	req := WorkspaceRequest{JobID: x.jobID, Actor: actor}
	handle, err := x.workspaces.Provision(ctx, req)
	if err != nil {
		return "", fmt.Errorf("provision workspace for job %s actor %q: %w", x.jobID, actor, err)
	}
	if handle == "" {
		return "", fmt.Errorf("workspace provisioner returned an empty handle for job %s actor %q", x.jobID, actor)
	}
	if x.handles == nil {
		x.handles, x.requests = map[string]Workspace{}, map[string]WorkspaceRequest{}
	}
	x.handles[actor], x.requests[actor] = handle, req
	return handle, nil
}

func (x *textExecutor) release(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	var releaseErr error
	for actor, handle := range x.handles {
		if err := x.workspaces.Release(ctx, handle); err != nil {
			releaseErr = errors.Join(releaseErr, fmt.Errorf("release workspace for %s: %w", actor, err))
		}
	}
	if releaseErr == nil {
		x.handles = nil
	}
	return releaseErr
}

func (x *textExecutor) AskHuman(_ context.Context, effect kernel.AskHuman) ([]kernel.Event, error) {
	return []kernel.Event{{
		Type: kernel.InboxCreated, Source: kernel.SourceRuntime, Actor: effect.Agent,
		Payload: map[string]any{"inbox_id": effect.ID, "kind": effect.Kind,
			"question": effect.Question, "agent": effect.Agent,
			"on_timeout": effect.OnTimeout, "timeout_ms": effect.TimeoutMs},
	}}, nil
}
