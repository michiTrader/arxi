//go:build windows

package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
	"github.com/michiTrader/arxi/internal/workspacefs"
)

func TestWindowsDoesNotAdvertiseDirectFiles(t *testing.T) {
	capabilities := workspace.CurrentCapabilities("windows")
	for _, profile := range capabilities.Profiles {
		if profile.ID == workspace.DirectFilesProfileID {
			t.Fatalf("Windows advertised %q without handle-relative support: preflight must refuse the run instead of promising weaker file access", profile.ID)
		}
	}
}

func TestWindowsRefusesDirectFilesBeforeAnyToolExecutes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	session, err := workspacefs.OpenLocalSession(root, "windows-direct-file-refusal")
	if err != nil {
		t.Fatalf("creating the refusal fixture failed: %v\n  the test cannot prove no-execution without a real candidate root", err)
	}
	provisioner := fixedWindowsSession{session: session}
	runner := &Runner{
		Sessions: provisioner,
		Requests: map[string]workspacefs.Request{
			"backend": {
				JobID: "job-windows-refusal", Member: "backend", Mode: workspace.ModeCopy,
				ProfileID: workspace.DirectFilesProfileID, ProfileIdentity: "unsupported-on-windows", ProvisionerVersion: "test",
			},
		},
	}
	marker := filepath.Join(root, "must-not-exist.txt")
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{name: "write", args: map[string]any{"path": "must-not-exist.txt", "content": "executed"}},
		{name: "read", args: map[string]any{"path": "must-not-exist.txt"}},
		{name: "grep", args: map[string]any{"pattern": "executed"}},
		{name: "edit", args: map[string]any{"path": "must-not-exist.txt", "old": "x", "new": "y"}},
	} {
		_, err := runner.RunTool(context.Background(), "backend", call.name, call.args)
		if err == nil || !strings.Contains(err.Error(), "strong handle-relative workspace roots are unavailable on windows") {
			t.Errorf("Windows %s refusal = %v: an unsupported direct-file profile must fail explicitly before tool execution", call.name, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after Windows direct-file refusals = %v: refusing an unsupported profile must execute no write", err)
	}
}

type fixedWindowsSession struct {
	session workspacefs.Session
}

func (p fixedWindowsSession) Provision(context.Context, workspacefs.Request) (workspacefs.Session, error) {
	return p.session, nil
}

func (p fixedWindowsSession) Release(context.Context, workspacefs.Request, workspacefs.Session) error {
	return nil
}
