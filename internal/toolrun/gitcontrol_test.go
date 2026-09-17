//go:build linux

package toolrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheGitControlPathIsRefusedForReadAndWrite pins both halves of the
// worktree `.git` constraint, which are different promises and regress
// differently.
//
// Write: a worktree root holds a `.git` FILE containing
// `gitdir: <common>/worktrees/<name>`. Rewriting it repoints every Git
// operation performed from that root at a repository the member chose.
//
// Read: that same pointer names the operator's repository by absolute path,
// and following it reaches the common config, where a remote URL can carry an
// embedded credential. Read is the half most likely to be dropped by someone
// relaxing this rule, because the write half is the one that sounds dangerous
// -- so it is asserted explicitly rather than folded into a loop that only
// exercises writes.
func TestTheGitControlPathIsRefusedForReadAndWrite(t *testing.T) {
	for _, path := range []string{
		".git",
		".GIT",
		filepath.Join(".git", "config"),
		filepath.Join(".git", "worktrees", "w", "gitdir"),
	} {
		if _, err := ws(t).Resolve(path); err == nil {
			t.Errorf("Resolve(%q) exposed the repository control path: a member's promise is the tracked tree, never the control plane that produced it", path)
		}
		if err := ws(t).WriteFile(path, []byte("gitdir: /tmp/attacker\n")); err == nil {
			t.Errorf("WriteFile(%q) succeeded: in a worktree layout this redirects every Git operation run from this root at a repository the member chose", path)
		}
		if _, err := ws(t).ReadFile(path); err == nil {
			t.Errorf("ReadFile(%q) succeeded: the pointer discloses the operator's absolute repository path, and through it a config that may carry credentials", path)
		}
	}
}

// TestTheGitRefusalIsAboutTheRootEntryNotTheSubstring keeps the rule narrow.
//
// A refusal implemented with strings.Contains(".git") would also refuse
// `src/.gitignore`, `gitconfig` and `.gitattributes` -- ordinary tracked files
// a member legitimately edits. Over-refusing looks safe and is not: it makes
// the tool unusable on real repositories, and the pressure then is to delete
// the rule rather than narrow it.
func TestTheGitRefusalIsAboutTheRootEntryNotTheSubstring(t *testing.T) {
	for _, path := range []string{
		".gitignore",
		".gitattributes",
		filepath.Join("src", ".gitignore"),
		filepath.Join("src", ".git-hooks", "pre-commit"),
		"gitconfig",
	} {
		w := ws(t)
		// The write path refuses to create parent directories by design, so
		// the fixture makes them. Without this the test would read its own
		// setup failure as a refusal and prove nothing about the git rule.
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(filepath.Join(w.Root, dir), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.WriteFile(path, []byte("tracked\n")); err != nil {
			t.Errorf("WriteFile(%q) was refused: %v: these are ordinary tracked files, and refusing them would make the rule unusable rather than safe", path, err)
		}
	}
}

// TestANestedGitDirectoryIsStillReachable records a deliberate limit.
//
// The rule refuses the FIRST path component only. A vendored repository below
// the root would have its own `.git` at `vendor/dep/.git`, which this does not
// refuse -- and the source policy already refuses submodules at provisioning
// time, so the case should not arise in a provisioned workspace.
//
// Asserted rather than left implicit so the boundary is a decision on record:
// if a future layout can place a live control directory below the root, this
// test is where that discovery must land.
func TestANestedGitDirectoryIsStillReachable(t *testing.T) {
	w := ws(t)
	nested := filepath.Join("vendor", "dep")
	if err := os.MkdirAll(filepath.Join(w.Root, nested), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: elsewhere\n")); err != nil {
		t.Skipf("nested .git write refused (%v): if the rule was deliberately widened to every component, update this test to state the new boundary", err)
	}
}

// TestTheGitRefusalNamesWhatItProtects keeps the error actionable.
//
// A bare "reserved path" message sends the reader looking for a typo. The
// refusal has to say that the control plane is not part of the promise, or the
// next person treats it as an arbitrary restriction and routes around it.
func TestTheGitRefusalNamesWhatItProtects(t *testing.T) {
	err := ws(t).WriteFile(".git", []byte("x"))
	if err == nil {
		t.Fatal("writing .git was accepted")
	}
	for _, want := range []string{".git", "reserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q: a member (or a human reading the log) must be able to tell this apart from a path typo", err, want)
		}
	}
}
