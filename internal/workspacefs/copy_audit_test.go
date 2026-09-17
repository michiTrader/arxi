package workspacefs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestCopySnapshotCarriesNoRepositoryControlPlane records the result of
// auditing `copy` against the same adversarial standard that found the
// worktree `.git` pointer.
//
// The worktree finding raised the obvious follow-up: does the other writable
// layout have an equivalent control file in its root? It does not, and the
// reason is structural rather than incidental, which is what makes it worth
// pinning:
//
//   - `copy` materializes the snapshot with copyTrackedTree, which writes only
//     blobs named by the tracked-entry walk. It never runs `git worktree add`,
//     so nothing places a `gitdir:` pointer in the root.
//   - A repository cannot smuggle one in as content either -- see
//     TestGitRefusesToTrackAPathNamedGit below.
//
// The consequence for a writer platform decision is that the redirect
// constraint is specific to `worktree`; `copy` needs no equivalent caveat.
// Asserting it means a future change to copyTrackedTree that starts
// materializing a control plane fails here instead of quietly widening what a
// writable snapshot exposes.
func TestCopySnapshotCarriesNoRepositoryControlPlane(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	req := request(probe, workspace.ModeCopy, "writer")

	session, err := manager.Provision(context.Background(), req)
	if err != nil {
		t.Skipf("copy provisioning unavailable here: %v", err)
	}
	root, ok := session.WorkspaceRoot()
	if !ok {
		t.Fatal("a provisioned copy session reports no workspace root")
	}

	if _, err := os.Lstat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatalf("a copy snapshot contains a .git entry (lstat err = %v): the snapshot's promise is the "+
			"tracked tree, and a control plane in its root would give a writing member the same "+
			"redirect and disclosure surface the worktree layout has", err)
	}

	// The snapshot is not empty, so the assertion above is about the absence
	// of a control plane rather than the absence of a workspace.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the copy snapshot is empty, so the .git assertion above proves nothing")
	}
}

// TestGitRefusesToTrackAPathNamedGit pins the upstream invariant the audit
// above leans on.
//
// If this ever stopped holding -- a Git version that permits it, or a source
// kind that is not Git -- then `copy` could carry a `.git` entry as ordinary
// tracked content and would inherit the worktree redirect surface. Pinning the
// assumption rather than only the conclusion is what makes that discovery loud
// instead of silent.
func TestGitRefusesToTrackAPathNamedGit(t *testing.T) {
	probe := testRepo(t)
	root := probe.Source.CanonicalRoot

	body := filepath.Join(root, "control-plane-probe.txt")
	if err := os.WriteFile(body, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := gitOutput(context.Background(), root, "hash-object", "-w", body)
	if err != nil {
		t.Skipf("cannot create a blob to attempt the add: %v", err)
	}

	out, err := gitCommand(context.Background(), root, "update-index", "--add",
		"--cacheinfo", "100644,"+trimSpace(blob)+",.git").CombinedOutput()
	if err == nil {
		t.Fatalf("Git accepted a tracked path named .git (output %q): the copy audit assumes this is "+
			"impossible, so if it became possible a copy snapshot could carry a gitdir: pointer as "+
			"ordinary content and the worktree redirect constraint would apply to copy too", out)
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
