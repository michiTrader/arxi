//go:build windows

package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/workspace"
)

var workspaceCommandForWindowsTest = workspace.CommandProfile{
	Schema: workspace.CommandSchemaV1, RunnerVersion: "test", Executable: "cmd.exe",
	EnvironmentVersion: workspace.EnvironmentAllowlistV1, Descendants: "contained",
	Filesystem: "unrestricted", Network: "unrestricted", OutputLimitBytes: maxOutputBytes,
}

func TestWindowsRefusesCommandBeforeExecutionWithoutJobContainment(t *testing.T) {
	root := t.TempDir()
	w := &Workspace{Root: root, Member: "windows-refusal"}
	w.command = &workspaceCommandForWindowsTest
	marker := filepath.Join(w.Root, "must-not-run.txt")
	_, err := w.Bash(context.Background(), "echo escaped > must-not-run.txt", time.Second)
	if err == nil || !strings.Contains(err.Error(), "Job Object") {
		t.Fatalf("Windows bash refusal = %v: lacking pre-start Job Object assignment must be an explicit no-execution result", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after Windows refusal = %v: direct-process fallback must never start", err)
	}
}
