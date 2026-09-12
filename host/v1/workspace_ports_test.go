package v1

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
)

type recordingProvisioner struct {
	mu       sync.Mutex
	calls    map[string]int
	handles  map[string]Workspace
	released []Workspace
	err      error
}

func (p *recordingProvisioner) Provision(_ context.Context, req WorkspaceRequest) (Workspace, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return "", p.err
	}
	if p.calls == nil {
		p.calls, p.handles = map[string]int{}, map[string]Workspace{}
	}
	p.calls[req.Actor]++
	if p.handles[req.Actor] == "" {
		p.handles[req.Actor] = Workspace("opaque:" + string(req.JobID) + ":" + req.Actor)
	}
	return p.handles[req.Actor], nil
}
func (p *recordingProvisioner) ProvisionSession(ctx context.Context, req WorkspaceRequest) (WorkspaceSessionV1, error) {
	handle, err := p.Provision(ctx, req)
	if err != nil {
		return WorkspaceSessionV1{}, err
	}
	return WorkspaceSessionV1{ID: "session:" + string(req.JobID) + ":" + req.Actor, Workspace: handle}, nil
}
func (p *recordingProvisioner) RecoverSession(_ context.Context, req WorkspaceRecoveryRequestV1) (WorkspaceSessionV1, error) {
	return WorkspaceSessionV1{ID: req.SessionID, Workspace: req.Workspace}, nil
}
func (p *recordingProvisioner) Release(_ context.Context, handle Workspace) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released = append(p.released, handle)
	return nil
}

type recordingTools struct {
	mu    sync.Mutex
	calls []ToolInvocation
}

func (t *recordingTools) Execute(_ context.Context, call ToolInvocation) (ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, call)
	return ToolResult{Content: json.RawMessage(`"done"`)}, nil
}

func TestHostToolExecutionDeliversExactOpaqueHandleOncePerActor(t *testing.T) {
	spaces := &recordingProvisioner{}
	tools := &recordingTools{}
	x := &textExecutor{tools: tools, workspaces: spaces, jobID: "job-1"}
	for i := 0; i < 2; i++ {
		if _, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "writer", Tool: "read", Args: map[string]any{"path": "a"}}); err != nil {
			t.Fatalf("CallTool %d: %v", i, err)
		}
	}
	if spaces.calls["writer"] != 1 {
		t.Fatalf("provision calls = %d, want 1: repeated tool calls must retain the same actor session", spaces.calls["writer"])
	}
	if len(tools.calls) != 2 || tools.calls[0].Workspace != spaces.handles["writer"] || tools.calls[1].Workspace != spaces.handles["writer"] {
		t.Fatalf("tool calls = %#v, handle = %q: the exact opaque provisioner value must reach every invocation unchanged", tools.calls, spaces.handles["writer"])
	}
	if tools.calls[0].JobID != "job-1" || tools.calls[0].Actor != "writer" {
		t.Fatalf("tool identity = %#v: job and actor ownership must accompany the handle", tools.calls[0])
	}
}

func TestPreparedOpaqueHandleIsReusedWithoutProvisioning(t *testing.T) {
	spaces := &recordingProvisioner{}
	tools := &recordingTools{}
	handle := Workspace("opaque:prepared")
	x := &textExecutor{tools: tools, workspaces: spaces, jobID: "job-prepared",
		sessions: map[string]WorkspaceSessionV1{"writer": {ID: "session:prepared", Workspace: handle}}}
	if _, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "writer", Tool: "read"}); err != nil {
		t.Fatal(err)
	}
	if spaces.calls["writer"] != 0 || len(tools.calls) != 1 || tools.calls[0].Workspace != handle {
		t.Fatalf("provision calls/tools = %d/%#v: acceptance-prepared handle must reach execution unchanged without a second provision", spaces.calls["writer"], tools.calls)
	}
}

func TestWorkspaceProvisionFailurePreventsToolDispatch(t *testing.T) {
	spaces := &recordingProvisioner{err: errors.New("workspace mode none has no root")}
	tools := &recordingTools{}
	x := &textExecutor{tools: tools, workspaces: spaces, jobID: "job-none"}
	if _, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "text", Tool: "read"}); err == nil {
		t.Fatal("none workspace executed a file tool: absence of a root must fail before tool dispatch")
	}
	if len(tools.calls) != 0 {
		t.Fatalf("tool calls = %#v: provisioning refusal must prevent all external tool dispatch", tools.calls)
	}
}

func TestTextExecutorReleasesPreparedSessionsIdempotently(t *testing.T) {
	spaces := &recordingProvisioner{}
	session := WorkspaceSessionV1{ID: "session:prepared", Workspace: "opaque:prepared"}
	x := &textExecutor{workspaces: spaces, sessions: map[string]WorkspaceSessionV1{"writer": session}}
	if err := x.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := x.release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spaces.released) != 1 || spaces.released[0] != session.Workspace {
		t.Fatalf("released handles = %v: a durable successful outcome must release each exact accepted host session once, while restart-safe retries remain idempotent", spaces.released)
	}
}

func TestRecoveredWorkspaceSessionMustMatchIdentityAndExactHandleBeforeDispatch(t *testing.T) {
	frozen := map[string]WorkspaceSessionV1{"writer": {ID: "stable-session", Workspace: "opaque:exact"}}
	for _, test := range []struct {
		name    string
		changed WorkspaceSessionV1
	}{
		{name: "session identity", changed: WorkspaceSessionV1{ID: "other-session", Workspace: "opaque:exact"}},
		{name: "opaque handle", changed: WorkspaceSessionV1{ID: "stable-session", Workspace: "opaque:other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spaces := &changingRecoveryProvisioner{recovered: test.changed}
			got, err := recoverWorkspaceSessions(context.Background(), "job-recovered", spaces, frozen)
			if err == nil || got != nil {
				t.Fatalf("changed %s recovered as %#v, %v: recovery must verify durable session identity and the exact opaque handle before any dispatch", test.name, got, err)
			}
		})
	}
}

type changingRecoveryProvisioner struct {
	recovered WorkspaceSessionV1
}

func (p *changingRecoveryProvisioner) Provision(context.Context, WorkspaceRequest) (Workspace, error) {
	return p.recovered.Workspace, nil
}
func (p *changingRecoveryProvisioner) ProvisionSession(context.Context, WorkspaceRequest) (WorkspaceSessionV1, error) {
	return p.recovered, nil
}
func (p *changingRecoveryProvisioner) RecoverSession(context.Context, WorkspaceRecoveryRequestV1) (WorkspaceSessionV1, error) {
	return p.recovered, nil
}
func (*changingRecoveryProvisioner) Release(context.Context, Workspace) error { return nil }
