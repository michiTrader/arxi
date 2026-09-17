package workspacefs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestACopySnapshotSharesNoInodeWithTheSourceTree is the provisioner half of
// the hardlink guarantee.
//
// The confined write API cannot detect a hardlink: it confines paths, and a
// hardlink is a second name for one inode rather than a path pointing
// elsewhere (pinned in internal/toolrun/hardlink_test.go). The guarantee that
// a write cannot reach outside the workspace therefore does not come from the
// file API alone -- it comes from this: a copy snapshot contains no inode
// shared with anything outside it.
//
// That holds because copyTrackedTree materializes each tracked blob with a
// fresh os.WriteFile of `cat-file` output. It never links, and it cannot
// reproduce a hardlink even when the source repository contains one, because
// Git records a hardlinked file as an ordinary 100644 blob -- the link is not
// in the object model.
//
// The adversarial case is exactly that: a source repository whose tracked file
// is hardlinked to a file outside the repository. If the snapshot ever shared
// that inode, a writing member would mutate the operator's file through a name
// the path confinement has no reason to refuse.
//
// An optimization that hardlinked or reflinked snapshot entries to save I/O
// would break this while every path-confinement test kept passing. This test
// is where that has to be noticed.
func TestACopySnapshotSharesNoInodeWithTheSourceTree(t *testing.T) {
	requireGit(t)

	outside := t.TempDir()
	victim := filepath.Join(outside, "operator-file.txt")
	if err := os.WriteFile(victim, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.invalid")
	runGit(t, root, "config", "user.name", "Inode Test")
	if err := os.Link(victim, filepath.Join(root, "hardlinked.txt")); err != nil {
		t.Skipf("this filesystem does not support hardlinks (%v), so the case cannot be built here", err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "tracked file hardlinked outside the repository")

	probe, err := Probe(context.Background(), root)
	if err != nil {
		t.Skipf("probe requires a usable Git checkout: %v", err)
	}
	manager := &Manager{Root: t.TempDir()}
	req := request(probe, workspace.ModeCopy, "writer")
	session, err := manager.Provision(context.Background(), req)
	if err != nil {
		t.Skipf("copy provisioning unavailable here: %v", err)
	}
	wsRoot, ok := session.WorkspaceRoot()
	if !ok {
		t.Fatal("a provisioned copy session reports no workspace root")
	}

	snapshot := filepath.Join(wsRoot, "hardlinked.txt")
	snapshotInfo, err := os.Stat(snapshot)
	if err != nil {
		t.Fatalf("the snapshot is missing the tracked path: %v", err)
	}
	victimInfo, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(snapshotInfo, victimInfo) {
		t.Fatalf("the snapshot entry shares an inode with %s: a writing member would mutate a file "+
			"outside its workspace through a path nothing has reason to refuse, because the confined "+
			"write API cannot see a hardlink", victim)
	}

	// The end-to-end consequence, asserted rather than inferred from SameFile:
	// writing the snapshot leaves the outside file untouched.
	if err := os.WriteFile(snapshot, []byte("mutated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original\n" {
		t.Fatalf("writing the snapshot changed the file outside the workspace to %q: the copy layout's "+
			"whole claim is that it is a snapshot, not a view", body)
	}
}
