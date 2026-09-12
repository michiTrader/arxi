package v1_test

import (
	"context"
	"encoding/json"
	"testing"

	host "github.com/michiTrader/arxi/host/v1"
)

type textProvider struct{}

func (textProvider) CompleteText(_ context.Context, req host.TextRequest) (host.TextResponse, error) {
	return host.TextResponse{Text: "reply to " + req.Prompt}, nil
}

type tools struct{}

func (tools) Execute(_ context.Context, call host.ToolInvocation) (host.ToolResult, error) {
	return host.ToolResult{Content: append(json.RawMessage(nil), call.Arguments...)}, nil
}

type workspaces struct{}

func (workspaces) Provision(_ context.Context, req host.WorkspaceRequest) (host.Workspace, error) {
	return host.Workspace("workspace:" + req.Actor), nil
}

func (workspaces) Release(context.Context, host.Workspace) error { return nil }

type authorizer struct{}

func (authorizer) Authorize(_ context.Context, req host.AuthorizationRequest) (host.AuthorizationDecision, error) {
	return host.AuthorizationDecision{Allowed: req.Principal.ID != ""}, nil
}

func TestExternalPackageCanImplementExtensionPorts(t *testing.T) {
	var _ host.TextProvider = textProvider{}
	var _ host.ToolExecutor = tools{}
	var _ host.WorkspaceProvisioner = workspaces{}
	var _ host.Authorizer = authorizer{}

	h := host.New(host.Options{
		Provider:   textProvider{},
		Storage:    newMemoryStorage(),
		Tools:      tools{},
		Workspaces: workspaces{},
		Authorizer: authorizer{},
	})
	result, err := h.Submit(context.Background(), host.SubmitRequest{
		Principal: host.Principal{ID: "external-host"},
		Blueprint: "name: example\nmembers:\n  - {name: worker}\nstages:\n  - {name: work, advance_when: all}\n",
		Prompt:    "do the work",
		BudgetUSD: 1,
		Simulated: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.JobID == "" || result.AcceptedSeq != 1 {
		t.Fatalf("Submit result = %#v", result)
	}
	sub, err := h.Subscribe(context.Background(), host.SubscribeRequest{
		Principal: host.Principal{ID: "external-host"}, JobID: result.JobID,
		Filter: host.EventFilter{TypePrefixes: []string{"run."}},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	batch, err := sub.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(batch.Events) != 1 || batch.Events[0].Type != "run.started" || batch.AfterSeq < result.AcceptedSeq {
		t.Fatalf("subscription batch = %#v", batch)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close subscription: %v", err)
	}
	caps, err := h.Capabilities(context.Background(), host.CapabilitiesRequest{
		Principal: host.Principal{ID: "external-host"},
	})
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	for _, capability := range []host.Capability{
		host.CapabilitySubmit, host.CapabilityInspect, host.CapabilityCancel,
		host.CapabilityApprove, host.CapabilityReject, host.CapabilityAnswer,
		host.CapabilityWait, host.CapabilitySubscribe,
	} {
		if !caps.Has(capability) {
			t.Fatalf("installed capabilities = %#v; missing %q", caps, capability)
		}
	}
	job, err := h.Wait(context.Background(), host.WaitRequest{
		Principal: host.Principal{ID: "external-host"}, JobID: result.JobID,
	})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !job.Terminal || job.Status != host.JobSucceeded {
		t.Fatalf("Wait job = %#v", job)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPhaseOneStatusAndCapabilities(t *testing.T) {
	if !host.JobSucceeded.Terminal() || host.JobBlocked.Terminal() {
		t.Fatal("terminal status classification is wrong")
	}
	set := host.CapabilitySet{Capabilities: []host.Capability{host.CapabilitySubmit, host.CapabilitySubscribe}}
	if !set.Has(host.CapabilitySubscribe) || set.Has(host.CapabilityAnswer) {
		t.Fatal("capability membership is wrong")
	}
}

func TestExternalWorkspacePortsWithoutDeclarationFailClosed(t *testing.T) {
	h := host.New(host.Options{Provider: textProvider{}, Storage: newMemoryStorage(), Tools: tools{}, Workspaces: workspaces{}})
	defer h.Close()
	_, err := h.Submit(context.Background(), host.SubmitRequest{
		Blueprint: "name: example\nworkspace: shared\nmembers:\n  - {name: reader, tools: [read]}\n",
		Prompt:    "inspect", BudgetUSD: 1, Simulated: true,
	})
	if err == nil {
		t.Fatal("unspecified external workspace ports accepted direct files: non-nil ports are implementations, not declarations of handle-relative safety")
	}
}

func TestExternalLegacyOptionsRemainSourceCompatibleForTextOnlyJobs(t *testing.T) {
	options := host.Options{Provider: textProvider{}, Storage: newMemoryStorage(), Tools: tools{}, Workspaces: workspaces{}}
	h := host.New(options)
	defer h.Close()
	result, err := h.Submit(context.Background(), host.SubmitRequest{
		Blueprint: "name: example\nmembers:\n  - {name: text}\n", Prompt: "write text", BudgetUSD: 1, Simulated: true,
	})
	if err != nil || result.JobID == "" {
		t.Fatalf("text-only submission with legacy options = %#v, %v: additive workspace declarations must not break existing source or text behavior", result, err)
	}
}
