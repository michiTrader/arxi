package workspacefs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// The tests in this file record what actually happens to a member's writes.
//
// They exist because the audit trail built so far (ADR-0018, the copy control
// plane audit, the hardlink/inode boundary) answered only the containment
// question: can a writing member reach something it was not granted? The
// answer is no. But "the write is contained" and "the write is useful" are two
// different facts, and an ADR that advertises write on the strength of the
// first while assuming the second would advertise a guarantee nobody checked.
//
// ADR-0012 says a workspace name describes provisioned, platform-verified
// guarantees. A verified guarantee has to include what the member gets, not
// only what it is denied. So before proposing that Linux advertise `copy` with
// the write-capable profile, the lifecycle is measured here rather than
// assumed.
//
// The measured result is that writes are contained AND discarded: they live in
// the snapshot, and the snapshot is removed on release. That is a coherent
// design -- it is what makes `copy` safe -- but it is a property a writer
// platform decision has to state out loud, because "the agent can write" reads
// to most people as "the agent can change my files", and here it does not.

// TestWritesToACopySnapshotNeverReachTheOperatorRepository pins the half of
// the lifecycle that is a safety guarantee.
//
// This is the property that makes write-on-copy proposable at all. It is
// distinct from the inode pin in inode_isolation_test.go: that one proves no
// snapshot path SHARES storage with the source, this one exercises the
// ordinary case -- a member overwrites a tracked file and creates a new one --
// and proves neither is visible in the operator's checkout afterwards.
//
// Note the fixture deliberately leaves the operator's tracked.txt at "dirty\n"
// while the snapshot is built from the frozen commit ("frozen\n"). So the
// assertion below also distinguishes "the snapshot is a separate tree" from
// "the snapshot happens to hold identical bytes".
func TestWritesToACopySnapshotNeverReachTheOperatorRepository(t *testing.T) {
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
	source := probe.Source.CanonicalRoot

	// The snapshot starts at the frozen commit, not at the operator's dirty
	// working tree. If this is not true the rest of the test is measuring the
	// wrong tree.
	snapshot := filepath.Join(root, "tracked.txt")
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("read the snapshot's tracked file: %v", err)
	}
	if string(before) != "frozen\n" {
		t.Fatalf("the snapshot holds %q, not the frozen commit's %q", before, "frozen\n")
	}

	// A member overwrites a tracked path and creates a new one. These are the
	// only two mutating shapes the tool surface has: WriteFile and Edit, and
	// Edit is implemented as a read followed by WriteFile. There is no
	// delete, rename or mkdir tool, so this is the whole mutation surface.
	if err := os.WriteFile(snapshot, []byte("written-by-the-member\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(root, "created-by-the-member.txt")
	if err := os.WriteFile(created, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The operator's working tree is untouched on both counts.
	operator, err := os.ReadFile(filepath.Join(source, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(operator) != "dirty\n" {
		t.Fatalf("the member's write changed the operator's working tree: tracked.txt is now %q, "+
			"expected the fixture's untouched %q", operator, "dirty\n")
	}
	if _, err := os.Lstat(filepath.Join(source, "created-by-the-member.txt")); !os.IsNotExist(err) {
		t.Fatalf("a file the member created appeared in the operator's repository (lstat err = %v)", err)
	}
}

// TestReleasingACopyWorkspaceDiscardsEveryMemberWrite pins the half of the
// lifecycle that is a LIMIT rather than a guarantee, which is why it is
// asserted rather than left as folklore.
//
// Release removes the snapshot root outright (session.go: `os.RemoveAll(root)`
// for every non-worktree mode). Nothing copies, commits, diffs or otherwise
// publishes the tree first -- there is no write-back path anywhere in the
// repository, and the supervisor calls ReleaseWorkspaces exactly on the
// successful terminal outcome. So the better the run went, the more certainly
// the work is deleted.
//
// Stated plainly: a writing member on `copy` gets a scratch tree. Writes are
// real while the run is live -- a later tool call reads back what an earlier
// one wrote, which is what makes multi-step work possible -- and they are gone
// when the run succeeds.
//
// This is pinned, not filed as a bug, because it is the correct behaviour for
// the containment the other tests prove: a snapshot that published itself into
// the operator's repository would undo the isolation. The reason it needs a
// test is that it is the sentence a writer ADR is most likely to omit, and a
// future change that added silent write-back would otherwise break no test
// while changing what `copy` promises.
func TestReleasingACopyWorkspaceDiscardsEveryMemberWrite(t *testing.T) {
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

	product := filepath.Join(root, "the-members-work.txt")
	if err := os.WriteFile(product, []byte("the result of the run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Within the session the write is real and readable. If this ever stopped
	// holding, `copy` would not support multi-step work at all, and the
	// discard asserted below would be trivially true for the wrong reason.
	if body, err := os.ReadFile(product); err != nil || string(body) != "the result of the run\n" {
		t.Fatalf("a member's write is not readable within its own session (%q, err = %v)", body, err)
	}

	if err := manager.Release(context.Background(), req, session); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := os.Lstat(product); !os.IsNotExist(err) {
		t.Fatalf("the member's work survived release (lstat err = %v).\n"+
			"  If this now persists, the workspace specification's account of `copy` has changed and "+
			"the change is unstated: releasing used to remove the snapshot root outright, so a writing "+
			"member's output was scratch. Persisting it is a different promise -- and an egress path "+
			"out of the snapshot -- which needs its own platform decision, not a silent one", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("the snapshot root survived release (lstat err = %v)", err)
	}

	// The discard is not a write-back either: the operator's tree still does
	// not carry the member's file. This distinguishes "removed" from
	// "harvested then removed".
	source := probe.Source.CanonicalRoot
	if _, err := os.Lstat(filepath.Join(source, "the-members-work.txt")); !os.IsNotExist(err) {
		t.Fatalf("release published the member's work into the operator's repository (lstat err = %v)", err)
	}
}

// TestAMemberWriteSurvivesReprovisioningWithinTheSameRun records the fact that
// keeps the discard above from being the whole story.
//
// Provision is recovery-shaped: it adopts an existing root after verifying
// ownership and the snapshot, rather than rebuilding it. verifySnapshot checks
// that every tracked path is PRESENT and of the right file TYPE; it does not
// compare contents against the frozen blob. So a resumed run sees the writes
// its earlier passes made, which is what makes crash-resume coherent for a
// writing member.
//
// That is a deliberate consequence rather than a gap, but it is worth an
// explicit test because it is the exact place a plausible "verify the snapshot
// really matches the commit" hardening would silently destroy a resumed
// member's work.
//
// The SECOND manager is load-bearing and the test is worth nothing without it.
// Provision short-circuits on its in-memory `live` map before it reaches any
// verification, so re-provisioning through the same manager only proves the
// cache returns what it cached. A crash-resume is a new process: a new manager
// over the same root, which is the path that actually runs verifyMarker and
// verifySnapshot against what is on disk. (This was caught by mutation: a
// stricter verifySnapshot comparing content to the frozen blob left the
// same-manager version of this test passing.)
func TestAMemberWriteSurvivesRecoveryByAFreshManager(t *testing.T) {
	probe := testRepo(t)
	shared := t.TempDir()
	req := request(probe, workspace.ModeCopy, "writer")

	first := &Manager{Root: shared}
	session, err := first.Provision(context.Background(), req)
	if err != nil {
		t.Skipf("copy provisioning unavailable here: %v", err)
	}
	root, _ := session.WorkspaceRoot()
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("progress\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The process restarts. Nothing is carried over but the directory.
	resumed, err := (&Manager{Root: shared}).Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("recovery refused an in-progress snapshot: %v\n"+
			"  a writing member's run could then never resume: every recovery would either fail or "+
			"silently restart from the frozen tree", err)
	}
	resumedRoot, _ := resumed.WorkspaceRoot()
	body, err := os.ReadFile(filepath.Join(resumedRoot, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "progress\n" {
		t.Fatalf("recovery reverted a member's write to %q: work completed before a resume would be "+
			"lost without any error saying so", body)
	}
}
