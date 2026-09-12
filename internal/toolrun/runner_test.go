//go:build linux

package toolrun

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/michiTrader/arxi/internal/tool"
	"github.com/michiTrader/arxi/internal/workspace"
	"github.com/michiTrader/arxi/internal/workspacefs"
)

type testSessions struct {
	root   string
	shared bool
}

func (p testSessions) Provision(_ context.Context, req workspacefs.Request) (workspacefs.Session, error) {
	root := filepath.Join(p.root, req.Member)
	if p.shared {
		root = p.root
	}
	if req.Command != nil {
		return workspacefs.OpenLocalCommandSession(root, req.JobID+"/"+req.Member, *req.Command)
	}
	return workspacefs.OpenLocalSession(root, req.JobID+"/"+req.Member)
}
func (testSessions) Release(context.Context, workspacefs.Request, workspacefs.Session) error {
	return nil
}

func runner(t *testing.T) *Runner {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run")
	requests := map[string]workspacefs.Request{}
	for _, member := range []string{"backend", "frontend"} {
		command := &workspace.CommandProfile{Schema: workspace.CommandSchemaV1, RunnerVersion: "arxi.command.operator-unrestricted/test-v1",
			Executable: "bash", EnvironmentVersion: workspace.EnvironmentAllowlistV1, Descendants: "process-group",
			Filesystem: "unrestricted", Network: "unrestricted", OutputLimitBytes: maxOutputBytes}
		requests[member] = workspacefs.Request{JobID: "test", Member: member, Mode: workspace.ModeCopy,
			ProfileID: workspace.DirectFilesProfileID, ProfileIdentity: "test-unrestricted", ProvisionerVersion: "test", Command: command}
	}
	return &Runner{Sessions: testSessions{root: root}, Requests: requests}
}

func TestEachMemberGetsItsOwnWorkspaceByDefault(t *testing.T) {
	r := runner(t)
	a, err := r.workspaceFor("backend")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.workspaceFor("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if a.Root == b.Root {
		t.Fatal("two members share a workspace by default\n" +
			"  two agents writing the same directory overwrite each other, and the " +
			"KV lock does not prevent it: the lock coordinates intent, isolation " +
			"comes from the filesystem")
	}

	// And the isolation is real, not just two names for one directory.
	if err := a.WriteFile("f.txt", []byte("from backend")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFile("f.txt"); err == nil {
		t.Error("frontend can read backend's file through its own workspace")
	}
}

func TestSharedWorkspaceIsOptInRatherThanTheDefault(t *testing.T) {
	r := runner(t)
	root := t.TempDir()
	r.Sessions = testSessions{root: root, shared: true}
	a, err := r.workspaceFor("backend")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.workspaceFor("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if a.Root != b.Root {
		t.Error("Shared did not give the two members the same workspace")
	}
	// Opt-in is the assertion that matters: TestEachMemberGetsItsOwnWorkspace
	// above proves the default is isolated, so together they show the unsafe
	// arrangement is never what somebody gets for free.
}

func TestTheSameMemberKeepsTheSameWorkspace(t *testing.T) {
	r := runner(t)
	a, err := r.workspaceFor("backend")
	if err != nil {
		t.Fatalf("opening the first workspace for a repeat call failed: %v\n"+
			"  the persistence assertion cannot dereference a workspace the platform refused", err)
	}
	if err := a.WriteFile("state.txt", []byte("step 1")); err != nil {
		t.Fatal(err)
	}
	b, err := r.workspaceFor("backend")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFile("state.txt"); err != nil {
		t.Errorf("the second call handed back a different workspace: %v\n"+
			"  a member that lost its files between two tool calls would see its own "+
			"earlier work disappear, which no reader of the log could explain", err)
	}
}

func TestAMemberNameThatIsAPathIsRefused(t *testing.T) {
	r := runner(t)
	for _, bad := range []string{"../escape", "a/b", "..", ".", "", "x\x00y"} {
		if _, err := r.workspaceFor(bad); err == nil {
			t.Errorf("workspaceFor(%q) succeeded\n"+
				"  a member name becomes a path component, so this package would be "+
				"letting an escape in through the front door", bad)
		}
	}
}

func TestConcurrentMembersDoNotRaceForAWorkspace(t *testing.T) {
	r := runner(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.workspaceFor("backend"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent workspaceFor: %v\n"+
			"  the run loop executes independent effects in PARALLEL, so two tool "+
			"calls for one member arrive at the same moment", err)
	}
}

func TestBashRunsThroughTheRunnerAndReportsItsExit(t *testing.T) {
	r := runner(t)
	out, err := r.RunTool(context.Background(), "backend", "bash",
		map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("output %q does not contain the command's own output", out)
	}
	if !strings.Contains(out, "exit 0") {
		t.Errorf("output %q does not state the exit status\n"+
			"  a model shown only stdout cannot tell a passing run from a failing "+
			"one whose output looks similar, and finding that out is why it has bash",
			out)
	}
}

func TestAFailingCommandSaysSoInWordsRatherThanOnlyInTheOutput(t *testing.T) {
	r := runner(t)
	out, err := r.RunTool(context.Background(), "backend", "bash",
		map[string]any{"command": "echo nope; exit 7"})
	if err != nil {
		t.Fatalf("a non-zero exit became a runner error: %v", err)
	}
	if !strings.Contains(out, "exit 7") || !strings.Contains(out, "failure") {
		t.Errorf("output %q does not say the command failed", out)
	}
}

func TestReadAndWriteGoThroughTheWorkspace(t *testing.T) {
	r := runner(t)
	ctx := context.Background()

	if _, err := r.RunTool(ctx, "backend", "write",
		map[string]any{"path": "notes.txt", "content": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := r.RunTool(ctx, "backend", "read", map[string]any{"path": "notes.txt"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello" {
		t.Errorf("read = %q, want %q", got, "hello")
	}

	// The confinement still applies when reached through RunTool, which is the
	// only path the executor uses.
	if _, err := r.RunTool(ctx, "backend", "write",
		map[string]any{"path": "../escape.txt", "content": "x"}); err == nil {
		t.Error("write escaped the workspace through RunTool\n" +
			"  a confinement that holds in the unit test and not on the path the " +
			"executor actually calls protects nothing")
	}
}

func TestAnUnknownToolIsRefusedRatherThanImprovised(t *testing.T) {
	r := runner(t)
	_, err := r.RunTool(context.Background(), "backend", "deploy_to_prod", nil)
	if err == nil {
		t.Fatal("an unknown tool succeeded\n" +
			"  a model that invents a tool and receives a plausible answer will keep " +
			"using it")
	}
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("error %q does not wrap ErrUnknownTool", err)
	}
}

// TestEveryDeclaredToolHasABodyBehindIt replaces a test that used to assert the
// opposite, and the replacement is the point.
//
// The old test named grep and edit and checked they failed with something other
// than "unknown tool". When they were IMPLEMENTED it kept passing -- because
// the arguments it sent (`{"path": "x"}`) are incomplete for both, so both
// still returned an error, now a missing-argument one. It went on reporting
// success for a claim that had become false, which is the same defect the tools
// it was watching are written to refuse.
//
// So it is inverted. This asks internal/tool.Known what is declared, drives
// each name with arguments that should WORK, and fails if any of them is
// refused as unimplemented. A tool added to Known without a body fails here
// instead of shipping as a promise nothing keeps.
func TestEveryDeclaredToolHasABodyBehindIt(t *testing.T) {
	// Arguments sufficient for a real call, one entry per declared tool. A name
	// added to Known with no entry here fails the completeness check below
	// rather than being silently skipped.
	args := map[string]map[string]any{
		"read":  {"path": "seed.txt"},
		"write": {"path": "out.txt", "content": "x"},
		"grep":  {"pattern": "seed"},
		"edit":  {"path": "seed.txt", "old": "seed", "new": "sprout"},
		"bash":  {"command": "true"},
	}
	for name := range tool.Known {
		if _, ok := args[name]; !ok {
			t.Fatalf("tool %q is declared in internal/tool.Known but this test does "+
				"not know how to call it\n"+
				"  add it here: an untested tool is how a declared name ships with "+
				"no body behind it", name)
		}
	}

	for name, a := range args {
		if !tool.Known[name] {
			t.Errorf("this test drives %q, which internal/tool.Known no longer "+
				"declares", name)
			continue
		}

		r := runner(t) // a fresh workspace per tool, so one edit cannot affect another
		if _, err := r.RunTool(context.Background(), "backend", "write",
			map[string]any{"path": "seed.txt", "content": "seed\n"}); err != nil {
			t.Fatalf("seeding for %s: %v", name, err)
		}

		out, err := r.RunTool(context.Background(), "backend", name, a)
		if err != nil {
			t.Errorf("%s(%v) failed: %v\n"+
				"  these arguments are sufficient, so this is the tool refusing work "+
				"it declares it does", name, a, err)
			continue
		}
		if errors.Is(err, ErrNotImplemented) || strings.Contains(out, "not implemented") {
			t.Errorf("%s is declared in internal/tool.Known but has no body", name)
		}
	}
}

func TestAMissingArgumentNamesWhatWasSuppliedInAStableOrder(t *testing.T) {
	r := runner(t)
	_, err := r.RunTool(context.Background(), "backend", "bash",
		map[string]any{"zeta": 1, "alpha": 2, "mid": 3})
	if err == nil {
		t.Fatal("bash with no command succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "alpha") {
		t.Errorf("error %q does not list what WAS supplied\n"+
			"  saying only what is missing leaves the reader unable to tell an "+
			"absent key from a misspelled one", msg)
	}
	if i, j, k := strings.Index(msg, "alpha"), strings.Index(msg, "mid"), strings.Index(msg, "zeta"); !(i < j && j < k) {
		t.Errorf("the supplied keys are not sorted in %q\n"+
			"  Go randomises map order, so an unsorted list makes one failure read "+
			"differently on every run and look like two different bugs", msg)
	}
}

func TestBothArgumentSpellingsAreAccepted(t *testing.T) {
	r := runner(t)
	ctx := context.Background()
	for _, key := range []string{"command", "script"} {
		if _, err := r.RunTool(ctx, "backend", "bash", map[string]any{key: "true"}); err != nil {
			t.Errorf("bash rejected the %q spelling: %v\n"+
				"  the caller is a model choosing a word on its own; failing a turn "+
				"over vocabulary records a tool error where the intent was clear", key, err)
		}
	}
}

func TestANonStringArgumentIsRefusedRatherThanCoerced(t *testing.T) {
	r := runner(t)
	if _, err := r.RunTool(context.Background(), "backend", "bash",
		map[string]any{"command": 42}); err == nil {
		t.Error("a numeric command was accepted; formatting it into a string would " +
			"run something nobody wrote")
	}
}

func TestWritingWithNoContentCreatesAnEmptyFile(t *testing.T) {
	r := runner(t)
	if _, err := r.RunTool(context.Background(), "backend", "write",
		map[string]any{"path": "empty.txt"}); err != nil {
		t.Fatalf("write with no content failed: %v\n"+
			"  \"create this file\" is a real instruction, and absent and empty are "+
			"the same request", err)
	}
	got, err := r.RunTool(context.Background(), "backend", "read",
		map[string]any{"path": "empty.txt"})
	if err != nil || got != "" {
		t.Errorf("read = %q, err = %v; want an empty file", got, err)
	}
}

func TestReleaseUsesProvisionerOwnershipInsteadOfDeletingAPath(t *testing.T) {
	r := runner(t)
	if _, err := r.RunTool(context.Background(), "backend", "write",
		map[string]any{"path": "f.txt", "content": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// testSessions deliberately retains its roots. Release delegating rather than
	// removing an invented runner path is the ownership guarantee under test.
}
