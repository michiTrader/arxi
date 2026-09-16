package workspacefs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestWorktreeGitFileIsWritableAndRedirectsTheCommonDir pins a gap that a
// writer platform decision must answer before `worktree` can be advertised.
//
// `git worktree add` places a `.git` FILE at the root of the worktree whose
// only content is `gitdir: <common>/worktrees/<name>`. That file is inside the
// tool-visible tree, and toolrun's reserved-path list covers
// `.arxi-workspace.json` and the metadata directory — not `.git`. So a member
// holding write access to its own worktree can rewrite the pointer and make
// every subsequent Git operation performed from that root resolve against a
// repository it chose.
//
// This is not an escape of the confined file API: the write lands inside the
// workspace, which is exactly where the profile says writes may land. It is a
// consequence of `worktree` being a layout whose root contains a control file,
// which `shared` (ADR-0017, read-only) and `copy` (a plain snapshot) do not
// have.
//
// The existing ownership verification does catch it — `verifyWorktree`
// compares the observed common directory against the frozen source and
// refuses. The test pins both halves, because they are different promises:
// detection is real, but the resulting behavior is a REFUSED RELEASE, which
// leaves the worktree registered in the operator's repository. A platform
// decision that advertises `worktree` has to say which of those it promises —
// "a writer cannot redirect it" (it can) or "a redirected worktree is detected
// and the run fails closed, leaving cleanup to the operator" (what happens).
//
// Deliberately not asserting a fix: the behavior below is the current contract,
// and choosing between hardening the reserved-path list and documenting the
// failure mode is the decision itself, not a detail of it.
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
	if !strings.Contains(listing, root) {
		t.Fatalf("worktree list = %q, want it to still contain %q: this assertion records that a "+
			"refused release leaves operator-visible state behind; if cleanup now happens, the "+
			"writer decision can promise something stronger and this pin should say so", listing, root)
	}
}
