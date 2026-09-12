package workspacefs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable: worktree capability must be absent when the executable cannot be found")
	}
}

func testRepo(t *testing.T) ProbeResult {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.invalid")
	runGit(t, root, "config", "user.name", "Workspace Test")
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("frozen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "mode.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "fixture")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := Probe(context.Background(), root)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return probe
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, body)
	}
	return strings.TrimSpace(string(body))
}

func request(probe ProbeResult, mode workspace.Mode, member string) Request {
	profile := workspace.Profile{Schema: workspace.ProfileSchemaV1, ID: workspace.DirectFilesProfileID,
		FileAccess: workspace.FileAccessWrite, HandleRelative: true, FinalLinkRaceFree: true,
		Process: workspace.ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}}
	identity, err := profile.Identity()
	if err != nil {
		panic(err)
	}
	return Request{JobID: "job-1", Member: member, Mode: mode, ProfileID: workspace.DirectFilesProfileID,
		ProfileIdentity: identity, ProvisionerVersion: probe.Capabilities.Provisioners[mode], Source: probe.Source}
}

func TestSourceLayoutsExposeFrozenTrackedTreeWithPromisedVisibility(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	for _, mode := range []workspace.Mode{workspace.ModeShared, workspace.ModeCopy, workspace.ModeWorktree} {
		t.Run(string(mode), func(t *testing.T) {
			a, err := manager.Provision(context.Background(), request(probe, mode, "writer-a"))
			if err != nil {
				t.Fatalf("Provision first member: %v", err)
			}
			b, err := manager.Provision(context.Background(), request(probe, mode, "writer-b"))
			if err != nil {
				t.Fatalf("Provision second member: %v", err)
			}
			rootA, _ := a.WorkspaceRoot()
			rootB, _ := b.WorkspaceRoot()
			body, err := os.ReadFile(filepath.Join(rootA, "tracked.txt"))
			if err != nil {
				t.Fatalf("read tracked source: %v", err)
			}
			want := "frozen\n"
			if strings.ReplaceAll(string(body), "\r\n", "\n") != want {
				t.Fatalf("%s tracked source = %q, want %q: layouts must expose exactly their declared source view", mode, body, want)
			}
			if mode != workspace.ModeShared {
				for _, excluded := range []string{"untracked.txt"} {
					if _, err := os.Stat(filepath.Join(rootA, excluded)); !os.IsNotExist(err) {
						t.Fatalf("%s included %s: separated layouts must contain only the frozen tracked tree", mode, excluded)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(rootA, "member.txt"), []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, siblingErr := os.Stat(filepath.Join(rootB, "member.txt"))
			if mode == workspace.ModeShared && siblingErr != nil {
				t.Fatalf("shared write was invisible to sibling: shared promises one source view, not isolation: %v", siblingErr)
			}
			if mode != workspace.ModeShared && !os.IsNotExist(siblingErr) {
				t.Fatalf("%s sibling saw another member's write: separated sessions must not alias", mode)
			}
			if mode == workspace.ModeWorktree {
				if got := runGit(t, rootA, "rev-parse", "HEAD"); got != probe.Source.Commit {
					t.Fatalf("worktree HEAD = %s, want frozen commit %s: real Git metadata must bind the source", got, probe.Source.Commit)
				}
			}
		})
	}
}

func TestNoneHasNoRootAndConcurrentProvisionAdoptsOnce(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	none := request(probe, workspace.ModeNone, "text")
	got, err := manager.Provision(context.Background(), none)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.WorkspaceRoot(); ok {
		t.Fatal("none returned a root: text-only mode must not silently acquire filesystem authority")
	}
	req := request(probe, workspace.ModeCopy, "writer")
	var wg sync.WaitGroup
	handles := make(chan Session, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, provisionErr := manager.Provision(context.Background(), req)
			if provisionErr != nil {
				t.Errorf("concurrent Provision: %v", provisionErr)
				return
			}
			handles <- s
		}()
	}
	wg.Wait()
	close(handles)
	var identity string
	for handle := range handles {
		if identity == "" {
			identity = handle.Identity()
		} else if handle.Identity() != identity {
			t.Fatalf("concurrent provision returned different identities: one member could receive competing snapshots")
		}
	}
}

func TestRestartOwnershipVerificationAndIdempotentRelease(t *testing.T) {
	probe := testRepo(t)
	root := t.TempDir()
	req := request(probe, workspace.ModeCopy, "writer")
	first := &Manager{Root: root}
	handle, err := first.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second := &Manager{Root: root}
	adopted, err := second.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("restart failed to adopt verified session: %v", err)
	}
	if adopted.Identity() != handle.Identity() {
		t.Fatalf("restart changed stable handle identity: durable member work would become unreachable")
	}
	foreign := req
	foreign.JobID = "another-job"
	if err := second.Release(context.Background(), foreign, adopted); err == nil {
		t.Fatal("foreign job released another workspace: release must verify exact ownership before removal")
	}
	path, _ := adopted.WorkspaceRoot()
	if err := os.WriteFile(filepath.Join(path, markerName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{Root: root}).Provision(context.Background(), req); err == nil {
		t.Fatal("corrupt ownership marker was adopted: path existence is not ownership evidence")
	}
}

func TestReleaseIsOwnershipCheckedAndIdempotent(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	for _, mode := range []workspace.Mode{workspace.ModeCopy, workspace.ModeWorktree} {
		req := request(probe, mode, "writer-"+string(mode))
		handle, err := manager.Provision(context.Background(), req)
		if err != nil {
			t.Fatalf("%s Provision: %v", mode, err)
		}
		if err := manager.Release(context.Background(), req, handle); err != nil {
			t.Fatalf("%s Release: %v", mode, err)
		}
		if err := manager.Release(context.Background(), req, handle); err != nil {
			t.Fatalf("%s second Release: %v: confirmed cleanup must be safely retryable", mode, err)
		}
	}
}

func TestCopyRefusesUnsafeSymlinksAndCaseCollisions(t *testing.T) {
	probe := testRepo(t)
	root := probe.Source.CanonicalRoot
	if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation unavailable: capability test cannot construct the refused Git object")
		}
		t.Fatal(err)
	}
	runGit(t, root, "add", "escape")
	runGit(t, root, "commit", "-m", "unsafe link")
	unsafeProbe, err := Probe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{Root: t.TempDir()}).Provision(context.Background(), request(unsafeProbe, workspace.ModeCopy, "writer")); err == nil {
		t.Fatal("escaping tracked symlink entered copy snapshot: only safe internal relative links may be reproduced")
	}

	if runtime.GOOS == "windows" {
		return
	}
	runGit(t, root, "rm", "escape")
	if err := os.WriteFile(filepath.Join(root, "Case"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "case"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "Case", "case")
	runGit(t, root, "commit", "-m", "collision")
	collisionProbe, err := Probe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{Root: t.TempDir()}).Provision(context.Background(), request(collisionProbe, workspace.ModeCopy, "writer")); err == nil {
		t.Fatal("case-colliding tracked paths entered snapshot: cross-platform restart would alias distinct files")
	}
}
