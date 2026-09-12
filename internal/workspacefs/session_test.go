package workspacefs

import (
	"context"
	"encoding/json"
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

func TestVerifyAcceptsOperatorCheckoutAdvanceWhenFrozenObjectsRemain(t *testing.T) {
	probe := testRepo(t)
	root := probe.Source.CanonicalRoot
	if err := os.WriteFile(filepath.Join(root, "later.txt"), []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "later.txt")
	runGit(t, root, "commit", "-m", "advance operator checkout")
	if got := runGit(t, root, "rev-parse", "HEAD"); got == probe.Source.Commit {
		t.Fatal("fixture HEAD did not advance, so the test cannot distinguish frozen source from operator checkout state")
	}
	verified, err := Verify(context.Background(), probe.Source)
	if err != nil {
		t.Fatalf("Verify rejected an available frozen object after source advance: %v", err)
	}
	if verified.Source != probe.Source {
		t.Fatalf("verified source = %#v, want frozen %#v: resume must keep the accepted source identity", verified.Source, probe.Source)
	}
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
	if err := os.WriteFile(second.markerPath(canonicalRequest(req)), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{Root: root}).Provision(context.Background(), req); err == nil {
		t.Fatal("corrupt ownership marker was adopted: path existence is not ownership evidence")
	}
}

func TestSharedIdentityAdoptionAndReleaseAreCanonicalAcrossMembers(t *testing.T) {
	probe := testRepo(t)
	managed := t.TempDir()
	firstReq := request(probe, workspace.ModeShared, "writer-a")
	secondReq := request(probe, workspace.ModeShared, "writer-b")
	first, err := (&Manager{Root: managed}).Provision(context.Background(), firstReq)
	if err != nil {
		t.Fatalf("provision first shared member: %v", err)
	}
	secondManager := &Manager{Root: managed}
	second, err := secondManager.Provision(context.Background(), secondReq)
	if err != nil {
		t.Fatalf("restart could not adopt shared workspace through another member: %v", err)
	}
	firstRoot, _ := first.WorkspaceRoot()
	secondRoot, _ := second.WorkspaceRoot()
	if first.Identity() != second.Identity() || firstRoot != secondRoot {
		t.Fatal("shared members received different durable identities: provision and release would disagree about ownership")
	}
	if err := secondManager.Release(context.Background(), secondReq, second); err != nil {
		t.Fatalf("release shared workspace through canonical member identity: %v", err)
	}
	if _, err := os.Stat(firstRoot); !os.IsNotExist(err) {
		t.Fatalf("released shared workspace still exists: shared cleanup must be real and retryable: %v", err)
	}
	if err := secondManager.Release(context.Background(), firstReq, first); err != nil {
		t.Fatalf("second shared release: %v: canonical cleanup must be idempotent across members", err)
	}
}

func TestOwnershipMetadataIsOutsideTheToolVisibleWorkspace(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	req := request(probe, workspace.ModeCopy, "writer")
	handle, err := manager.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := handle.WorkspaceRoot()
	if _, err := os.Stat(filepath.Join(root, ".arxi-workspace.json")); !os.IsNotExist(err) {
		t.Fatalf("ownership metadata remains inside the agent-writable source root: direct tools could forge release authority: %v", err)
	}
	markerPath := manager.markerPath(canonicalRequest(req))
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("protected ownership metadata is missing: restart cannot prove adoption authority: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".arxi-workspace.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), req, handle); err != nil {
		t.Fatalf("agent-created legacy marker affected protected ownership: %v", err)
	}
}

func TestRestartRefusesCorruptForeignAndOrphanedOwnershipMetadata(t *testing.T) {
	probe := testRepo(t)
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Manager, Request, string)
	}{
		{name: "corrupt", mutate: func(t *testing.T, manager *Manager, req Request, _ string) {
			if err := os.WriteFile(manager.markerPath(req), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "foreign", mutate: func(t *testing.T, manager *Manager, req Request, _ string) {
			foreign := req
			foreign.JobID = "another-job"
			body, _ := json.Marshal(expectedMarker(foreign))
			if err := os.WriteFile(manager.markerPath(req), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "orphaned", mutate: func(t *testing.T, _ *Manager, _ Request, root string) {
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			managed := t.TempDir()
			manager := &Manager{Root: managed}
			req := request(probe, workspace.ModeCopy, "writer")
			handle, err := manager.Provision(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := handle.WorkspaceRoot()
			tc.mutate(t, manager, req, root)
			if _, err := (&Manager{Root: managed}).Provision(context.Background(), req); err == nil {
				t.Fatal("restart adopted workspace without exact protected ownership: path existence or stale metadata must never become authority")
			}
		})
	}
}

func TestPartialRecoveryRequiresMatchingProtectedOwnership(t *testing.T) {
	probe := testRepo(t)
	manager := &Manager{Root: t.TempDir()}
	req := request(probe, workspace.ModeCopy, "writer")
	partial := manager.path(req) + ".partial"
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "attacker.txt"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), req); err != nil {
		t.Fatalf("unowned partial workspace was not safely replaced: %v", err)
	}
	root := manager.path(req)
	if _, err := os.Stat(filepath.Join(root, "attacker.txt")); !os.IsNotExist(err) {
		t.Fatalf("foreign partial content survived reconciliation: partial paths are not ownership evidence: %v", err)
	}

	foreignReq := req
	foreignReq.JobID = "foreign-job"
	foreign := &Manager{Root: t.TempDir()}
	if err := os.MkdirAll(foreign.path(foreignReq)+".partial", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(foreign.markerPath(foreignReq)), 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(expectedMarker(req))
	if err := os.WriteFile(foreign.markerPath(foreignReq), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Provision(context.Background(), foreignReq); err == nil {
		t.Fatal("foreign protected metadata authorized partial cleanup: cross-job ownership must fail closed")
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

func TestSpoofedGitEnvironmentCannotRedirectProbe(t *testing.T) {
	probe := testRepo(t)
	outside := t.TempDir()
	runGit(t, outside, "init")
	t.Setenv("GIT_DIR", filepath.Join(outside, ".git"))
	t.Setenv("GIT_WORK_TREE", outside)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(t.TempDir(), "hooks"))
	observed, err := Probe(context.Background(), probe.Source.CanonicalRoot)
	if err != nil {
		t.Fatalf("spoofed Git environment redirected source probing: %v", err)
	}
	if observed.Source != probe.Source {
		t.Fatal("spoofed Git environment changed frozen source identity: subprocesses must use an explicit neutral environment")
	}
}

func TestGitFiltersCannotExecuteOrMutateFrozenSource(t *testing.T) {
	probe := testRepo(t)
	repo := probe.Source.CanonicalRoot
	if runtime.GOOS == "windows" {
		t.Skip("executable filter regression requires a Unix shell; Git environment spoofing remains covered on Windows")
	}
	sentinel := filepath.Join(t.TempDir(), "filter-ran")
	filter := filepath.Join(t.TempDir(), "filter.sh")
	script := "#!/bin/sh\nprintf ran > " + shellQuote(sentinel) + "\nprintf altered\n"
	if err := os.WriteFile(filter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "config", "filter.hostile.smudge", filter)
	runGit(t, repo, "config", "filter.hostile.clean", filter)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt filter=hostile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitattributes")
	runGit(t, repo, "commit", "-m", "hostile filter fixture")
	filtered, err := Probe(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []workspace.Mode{workspace.ModeCopy, workspace.ModeWorktree} {
		handle, err := (&Manager{Root: t.TempDir()}).Provision(context.Background(), request(filtered, mode, "writer"))
		if err != nil {
			t.Fatalf("%s provision with hostile filter: %v", mode, err)
		}
		root, _ := handle.WorkspaceRoot()
		body, err := os.ReadFile(filepath.Join(root, "tracked.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ReplaceAll(string(body), "\r\n", "\n") != "frozen\n" {
			t.Fatalf("%s filter altered frozen output to %q: snapshots must read tracked objects without executing filters", mode, body)
		}
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("Git clean or smudge filter executed during provisioning: repository configuration is untrusted: %v", err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
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
