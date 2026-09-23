package workspacefs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestWorktreeGitFileIsWritableAndRedirectsTheCommonDir pins the SECOND layer
// of the worktree `.git` defense: what still holds when the first layer is
// bypassed.
//
// `git worktree add` places a `.git` FILE at the root of the worktree whose
// only content is `gitdir: <common>/worktrees/<name>`. That file sits inside
// the tool-visible tree. The first layer is toolrun, which since ADR-0018
// refuses `.git` as a reserved source-control path for read and write alike
// (internal/toolrun/gitcontrol_test.go); a member using the built-in file
// tools cannot reach it at all.
//
// This test deliberately writes the pointer DIRECTLY, bypassing toolrun, and
// asserts that ownership verification still refuses the release. That is not
// redundant with the toolrun rule -- it answers "what if something other than
// the built-in tools mutates the tree", and the honest answer today is: the
// redirect is detected, the release fails closed, and the worktree stays
// registered in the operator's repository for a human to remove.
//
// Both halves are asserted because they are different promises. Deleting the
// second would leave `git worktree remove --force` running against whatever
// repository the pointer happens to name.
func TestWorktreeGitFileIsWritableAndRedirectsTheCommonDir(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	req := request(probe, workspace.ModeWorktree, "writer")

	session, err := manager.Provision(context.Background(), req)
	if err != nil {
		t.Skipf("worktree provisioning unavailable here: %v", err)
	}
	root, ok := session.WorkspaceRoot()
	if !ok {
		t.Fatal("a provisioned worktree session reports no workspace root")
	}

	// The control file is in the tool-visible tree, not beside it.
	gitFile := filepath.Join(root, ".git")
	original, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatalf("a provisioned worktree has no .git file at its root: %v: "+
			"if Git stopped writing one, the redirect this test describes cannot happen "+
			"and the pin must be re-derived rather than deleted", err)
	}
	if !strings.HasPrefix(string(original), "gitdir:") {
		t.Fatalf(".git = %q, want a gitdir pointer", strings.TrimSpace(string(original)))
	}

	// Nothing in the write path treats it as reserved. This is the fact a
	// writer advertisement has to reckon with; assert it rather than assume it.
	decoy := t.TempDir()
	if out, err := gitCommand(context.Background(), decoy, "init").CombinedOutput(); err != nil {
		t.Skipf("cannot build a decoy repository here: %v: %s", err, out)
	}
	decoyGitDir, err := canonical(filepath.Join(decoy, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitFile, []byte("gitdir: "+decoyGitDir+"\n"), 0o600); err != nil {
		t.Fatalf("rewriting the worktree .git pointer: %v", err)
	}

	observed, err := gitOutput(context.Background(), root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatalf("resolving the redirected worktree: %v", err)
	}
	redirected, err := canonical(strings.TrimSpace(observed))
	if err != nil {
		t.Fatal(err)
	}
	if redirected != decoyGitDir {
		t.Fatalf("common dir after rewrite = %q, want the decoy %q", redirected, decoyGitDir)
	}
	if redirected == req.Source.CommonGitDir {
		t.Fatal("the rewrite did not take effect, so this test is not exercising the redirect it claims to")
	}

	// Half two: ownership verification refuses, and the refusal is the
	// protection. Release returns an error instead of removing the worktree.
	err = manager.Release(context.Background(), req, session)
	if err == nil {
		t.Fatal("Release accepted a worktree whose common directory was redirected: " +
			"ownership verification is the only thing standing between a rewritten .git " +
			"and `git worktree remove --force` running against a repository the member chose")
	}
	if !strings.Contains(err.Error(), "common directory") {
		t.Fatalf("Release refused with %v, want a refusal naming the common-directory mismatch: "+
			"a different refusal would mean the redirect was caught by accident rather than by the check "+
			"that exists for it", err)
	}

	// And the consequence of refusing: the worktree is still registered in the
	// operator's repository. Recorded so a writer decision states it instead of
	// discovering it.
	listing, listErr := gitOutput(context.Background(), req.Source.CanonicalRoot, "worktree", "list")
	if listErr != nil {
		t.Fatalf("listing worktrees: %v", listErr)
	}
	// `git worktree list` prints paths with forward slashes on every platform,
	// while root carries the OS separator (backslash on Windows). Comparing them
	// raw would fail on Windows for a formatting reason alone, hiding whether the
	// worktree is actually still registered -- normalize both to slashes so the
	// assertion tests registration, not the separator convention.
	if !strings.Contains(filepath.ToSlash(listing), filepath.ToSlash(root)) {
		t.Fatalf("worktree list = %q, want it to still contain %q: this assertion records that a "+
			"refused release leaves operator-visible state behind; if cleanup now happens, the "+
			"writer decision can promise something stronger and this pin should say so", listing, root)
	}
}
