package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
)

// TestTheConfinementHoldsForACommandThatTriesToEscape is the test that matters
// most in this package, because it exercises the combination rather than the
// parts.
//
// Every other test here checks one mechanism. This one asks the question a user
// actually has: if a model writes a plausible, careless command, does anything
// land outside the workspace? The parts can each be correct while the
// composition is not -- Resolve guards the tool arguments, but `bash` receives a
// script that Resolve never sees.
func TestTheConfinementHoldsForACommandThatTriesToEscape(t *testing.T) {
	r := runner(t)
	ctx := context.Background()

	// The member's workspace is r.Root/backend, so "../outside.txt" from inside
	// it is r.Root/outside.txt -- the RUN directory, which also holds the log and
	// the frozen blueprint. Opening a workspace creates the run directory, so
	// this has to come after the first tool call below; the path is computed
	// here and the sentinel written once it exists.
	outside := filepath.Join(r.Root, "outside.txt")

	// These are the two honest cases, and the distinction is the point.
	//
	// The write TOOL is confined: Resolve refuses the path, nothing runs.
	if _, err := r.RunTool(ctx, "backend", "write",
		map[string]any{"path": "../outside.txt", "content": "clobbered"}); err == nil {
		t.Error("the write tool escaped the workspace")
	}
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The bash TOOL is not, and cannot be by this mechanism: the script is a
	// program, not a path, and `echo x > ../outside.txt` carries no argument for
	// Resolve to check. What confines it is the cwd and, in a real deployment,
	// the OS.
	//
	// This is asserted as it truly behaves rather than as one would prefer,
	// because a test claiming bash was confined would be the most dangerous line
	// in the package: somebody would rely on it. And the reach is worse than
	// "outside its own workspace" -- one level up is the RUN directory, holding
	// the append-only log and the frozen blueprint that make a run explainable.
	// That is recorded in the package doc, not hidden behind a passing test.
	out, err := r.RunTool(ctx, "backend", "bash",
		map[string]any{"command": "echo clobbered > ../outside.txt"})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "original" {
		t.Errorf("the sentinel outside the workspace survived a shell redirect, "+
			"so this test is no longer describing what bash can do (%q)\n"+
			"  if bash became confined, that is good news and this assertion has to "+
			"be rewritten deliberately rather than left to pass by accident", out)
	}

	// What IS guaranteed: the command starts inside the workspace, so every
	// relative path a script uses without thinking lands in the right place.
	// That is the protection bash actually gets, and it is worth having because
	// careless-relative is the common case, not deliberate-absolute.
	if _, err := r.RunTool(ctx, "backend", "bash",
		map[string]any{"command": "echo inside > made-by-bash.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunTool(ctx, "backend", "read",
		map[string]any{"path": "made-by-bash.txt"}); err != nil {
		t.Errorf("a file the command created with a relative path is not in the "+
			"member's workspace: %v", err)
	}
}

// TestOneMemberCannotReachAnotherMembersWorkspaceThroughATool checks the
// isolation on the path the executor uses, not on workspaceFor.
func TestOneMemberCannotReachAnotherMembersWorkspaceThroughATool(t *testing.T) {
	r := runner(t)
	ctx := context.Background()

	if _, err := r.RunTool(ctx, "backend", "write",
		map[string]any{"path": "secret.txt", "content": "backend's work"}); err != nil {
		t.Fatal(err)
	}

	// frontend guessing the sibling directory by name, which is the obvious
	// thing for a model that has seen the blueprint to try.
	if _, err := r.RunTool(ctx, "frontend", "read",
		map[string]any{"path": "../backend/secret.txt"}); err == nil {
		t.Error("frontend read backend's file\n" +
			"  the worktree default exists so two agents cannot overwrite each " +
			"other; a sibling reachable by name gives that up while looking isolated")
	}
}

// TestAWorkspaceSurvivesTheToolCallsThatUseIt guards the property a multi-turn
// agent depends on and nothing else asserts end to end.
func TestAWorkspaceSurvivesTheToolCallsThatUseIt(t *testing.T) {
	r := runner(t)
	ctx := context.Background()

	steps := []map[string]any{
		{"command": "echo step1 >> log.txt"},
		{"command": "echo step2 >> log.txt"},
		{"command": "cat log.txt"},
	}
	var last string
	for i, args := range steps {
		out, err := r.RunTool(ctx, "backend", "bash", args)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		last = out
	}
	if !strings.Contains(last, "step1") || !strings.Contains(last, "step2") {
		t.Errorf("the third command saw %q, want both earlier steps\n"+
			"  an agent that lost its files between tool calls would watch its own "+
			"work vanish, and no reader of the log could explain why", last)
	}
}

// TestTheControlTheDocumentationPointsAtIsRealAndStaysReal pins the one claim
// the package doc makes about a mechanism it does not own.
//
// The doc says confinement does not stop a bash script, and that what stands
// between a model and `bash` is the tool POLICY resolving to ask. If that
// default ever changed to allow, the documentation here would become false in
// the most damaging possible direction -- reassuring, and wrong -- while every
// test in this package kept passing, because none of them touch internal/tool.
//
// Deliberately checked through tool.Resolve rather than by copying its table. A
// duplicated expectation would agree with the doc and drift from the code.
func TestTheControlTheDocumentationPointsAtIsRealAndStaysReal(t *testing.T) {
	if got := tool.Resolve([]string{"read", "bash"}, nil, "bash"); got != surface.PolicyAsk {
		t.Errorf("granted bash resolves to %q, but the package doc tells the reader "+
			"a human sees the command before it runs\n"+
			"  confinement does NOT stop a script, so if this is no longer ask then "+
			"nothing does, and the doc is now reassuring and wrong", got)
	}
	if got := tool.Resolve([]string{"read"}, nil, "bash"); got != surface.PolicyDeny {
		t.Errorf("ungranted bash resolves to %q, want deny", got)
	}
}
