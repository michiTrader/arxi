package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/workspace"
)

func TestNoneWorkspaceBuildsWithoutGitOrProvisioner(t *testing.T) {
	cfg := kernel.Config{Blueprint: "text", Workspace: "none", Members: []kernel.MemberConfig{{Name: "text"}}}.ResolveDefaults()
	snapshot := []byte("name: text\n")
	sum := sha256.Sum256(snapshot)
	artifact := runconfig.New("none-run", "live", hex.EncodeToString(sum[:]), "work", "", cfg, nil, nil)
	caps := workspace.CurrentCapabilities("windows")
	requirements, err := workspace.Resolve(workspace.ResolutionInput{TopLevel: workspace.ModeNone,
		Members: []workspace.Member{{Name: "text"}}})
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := workspace.Preflight(requirements, caps)
	if err != nil {
		t.Fatal(err)
	}
	artifact.WorkspaceContract = &runconfig.WorkspaceContract{Schema: workspace.SchemaV1,
		Source: workspace.SourceIdentity{Schema: workspace.SchemaV1, Kind: "none", DirtyPolicy: "excluded",
			UntrackedPolicy: "excluded", IgnoredPolicy: "excluded", SubmodulePolicy: "refused",
			SymlinkPolicy: "internal-relative-only", SpecialFilePolicy: "refused"},
		Requirements: requirements, Decisions: decisions}
	artifact.WorkspaceProfileID = decisions[0].ProfileIdentity
	if _, err := runtimeExecutor(t.TempDir(), artifact); err != nil {
		t.Fatalf("none workspace executor required Git, source probing, or a filesystem provisioner: %v", err)
	}
}
