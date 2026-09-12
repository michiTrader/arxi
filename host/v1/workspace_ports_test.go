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
		handles: map[string]Workspace{"writer": handle}}
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
