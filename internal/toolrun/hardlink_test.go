//go:build linux

package toolrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestAHardlinkedPathIsWrittenThroughToItsOtherNames records a real limit of
// the confined write API, found while auditing writable layouts for the
// pending writer platform decision.
//
// The confinement story is about PATHS: no absolute paths, no traversal, no
// following a symlink out, and every component opened handle-relative. A
// hardlink defeats all of that without violating any of it. It is not a path
// pointing elsewhere -- it is a second name for the same inode, and there is
// nothing about the name inside the workspace that distinguishes it. `openat`
// with `O_NOFOLLOW` does not help, because there is no link to refuse.
//
// So a write through a hardlinked name lands in whatever else shares that
// inode, including a file outside the workspace. Asserted here because it is
// the kind of fact that reads as a contradiction of the confinement promise
// when someone discovers it cold, and the honest framing is narrower: the API
// confines the path, and the provisioner is what guarantees no inode inside
// the workspace is shared with anything outside it.
//
// The companion assertion lives in internal/workspacefs
// (TestACopySnapshotSharesNoInodeWithTheSourceTree): a copy snapshot is
// materialized with fresh writes per tracked blob, so no snapshot path can be
// hardlinked to the operator's tree even when the source repository itself
// contains hardlinks. Those two together are the actual guarantee. Deleting
// either leaves the other proving less than it looks.
func TestAHardlinkedPathIsWrittenThroughToItsOtherNames(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "operator-file.txt")
	if err := os.WriteFile(victim, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Link(victim, filepath.Join(root, "hardlinked.txt")); err != nil {
		t.Skipf("this filesystem does not support hardlinks (%v), so the limit cannot be exercised here", err)
	}

	w, err := OpenWorkspace(root, "w", WithFileAccess(workspace.FileAccessWrite))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("hardlinked.txt", []byte("mutated\n")); err != nil {
		t.Fatalf("writing a plain relative path inside the workspace was refused: %v: "+
			"if hardlinks became detectable the refusal is an improvement, but then this test "+
			"must be rewritten to assert that instead of silently passing for a new reason", err)
	}

	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "mutated\n" {
		t.Fatalf("the file outside the workspace reads %q after the write: this test exists to record "+
			"that a hardlinked name IS written through. If that is no longer true the confinement got "+
			"stronger and the provisioner-side guarantee is no longer the only thing preventing it -- "+
			"update this test and the writer decision's claims together", body)
	}
}
