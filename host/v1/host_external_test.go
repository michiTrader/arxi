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
func (workspaces) ProvisionSession(ctx context.Context, req host.WorkspaceRequest) (host.WorkspaceSessionV1, error) {
	handle, err := (workspaces{}).Provision(ctx, req)
	return host.WorkspaceSessionV1{ID: "session:" + string(req.JobID) + ":" + req.Actor, Workspace: handle}, err
}
func (workspaces) RecoverSession(_ context.Context, req host.WorkspaceRecoveryRequestV1) (host.WorkspaceSessionV1, error) {
	return host.WorkspaceSessionV1{ID: req.SessionID, Workspace: req.Workspace}, nil
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
	var _ host.RecoverableWorkspaceProvisionerV1 = workspaces{}
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

func declaredWorkspaceCapabilities() *host.WorkspaceCapabilitiesV1 {
	return &host.WorkspaceCapabilitiesV1{
		Schema: host.WorkspaceCapabilitiesSchemaV1, CapabilityVersion: "example.host-workspaces/v1", Platform: "external-test",
		Modes: []string{"none", "shared", "copy", "worktree"}, SourceKinds: []string{"host-opaque"},
		Profiles: []host.WorkspaceProfileV1{
			{Schema: host.WorkspaceProfileSchemaV1, ID: "arxi.workspace/no-tools-v1", FileAccess: "none",
				Process: host.WorkspaceProcessProfileV1{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			{Schema: host.WorkspaceProfileSchemaV1, ID: "arxi.workspace/direct-files-v1", FileAccess: "write",
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: host.WorkspaceProcessProfileV1{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
		},
		Provisioners: map[string]string{"none": "example.none/v1", "shared": "example.opaque/v1", "copy": "example.opaque/v1", "worktree": "example.opaque/v1"},
	}
}

func TestExternalDeclaredProfilesSupportMixedMembers(t *testing.T) {
	storage := &captureCreateStorage{memoryStorage: newMemoryStorage()}
	h := host.New(host.Options{Provider: textProvider{}, Storage: storage, Tools: tools{}, Workspaces: workspaces{},
		WorkspaceCapabilities: declaredWorkspaceCapabilities()})
	defer h.Close()
	_, err := h.Submit(context.Background(), host.SubmitRequest{
		Blueprint: "name: example\nworkspace: shared\nmembers:\n  - {name: reader, tools: [read]}\n  - {name: text}\n",
		Prompt:    "inspect", BudgetUSD: 1, Simulated: true,
	})
	if err != nil {
		t.Fatalf("declared mixed profiles were refused: explicit per-member guarantees must select exact profile identities: %v", err)
	}
	var metadata struct {
		Effective struct {
			WorkspaceProfileID string `json:"workspace_profile_id"`
			WorkspaceContract  struct {
				Decisions []struct {
					Member          string `json:"member"`
					ProfileID       string `json:"profile_id"`
					ProfileIdentity string `json:"profile_identity"`
				} `json:"platform_decisions"`
			} `json:"workspace_contract"`
		} `json:"effective"`
		WorkspaceSessions map[string]host.WorkspaceSessionV1 `json:"workspace_sessions"`
	}
	if err := json.Unmarshal(storage.created.Record.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	decisions := metadata.Effective.WorkspaceContract.Decisions
	if len(decisions) != 2 || decisions[0].Member != "reader" || decisions[1].Member != "text" ||
		decisions[0].ProfileIdentity == decisions[0].ProfileID || decisions[1].ProfileIdentity == decisions[1].ProfileID ||
		metadata.Effective.WorkspaceProfileID == decisions[0].ProfileID || metadata.Effective.WorkspaceProfileID == decisions[1].ProfileID {
		t.Fatalf("frozen mixed profile binding = %#v / %q: authorization must bind exact per-member decision identities, never one display label", decisions, metadata.Effective.WorkspaceProfileID)
	}
	if session := metadata.WorkspaceSessions["reader"]; session.ID == "" || session.Workspace == "" || metadata.WorkspaceSessions["text"].ID != "" {
		t.Fatalf("durable mixed workspace sessions = %#v: only workspace-backed members need stable host session identity", metadata.WorkspaceSessions)
	}
}

type captureCreateStorage struct {
	*memoryStorage
	created host.CreateJob
}

func (s *captureCreateStorage) Create(ctx context.Context, req host.CreateJob) (host.CreateResult, error) {
	s.created = req
	return s.memoryStorage.Create(ctx, req)
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
