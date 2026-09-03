package kernel

import (
	"sort"
	"testing"
)

// TestKnownToolsIsSortedBecauseCallersPrintIt.
//
// Two callers show this list to a person: the blueprint loader puts it in a
// refusal ("granted tools are ...") and `agent show` lists it. Neither sorts it,
// deliberately -- that is the reason it is a slice rather than a map. If the
// declaration stops being sorted, the fix is to sort the declaration, not to make
// every caller sort a copy, because the caller that forgets is the one printing a
// refusal the user is trying to read.
func TestKnownToolsIsSortedBecauseCallersPrintIt(t *testing.T) {
	got := append([]string(nil), KnownTools...)
	if !sort.StringsAreSorted(got) {
		t.Errorf("KnownTools is %v, which is not sorted\n"+
			"  consequence: the refusal from `blueprint validate` and the listing "+
			"from `agent show` come out in declaration order, so the two disagree "+
			"and neither is scannable.\n"+
			"  remedy: sort the declaration in tools.go.", got)
	}
}

// TestKnownToolsHasNoDuplicates. A repeated entry makes ToolIsKnown no less
// correct and every listing built from it wrong, which is the kind of defect that
// survives because the code that would break is not the code that reads oddly.
func TestKnownToolsHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range KnownTools {
		if seen[name] {
			t.Errorf("KnownTools lists %q twice\n"+
				"  consequence: `agent show` prints the tool twice and the loader's "+
				"refusal offers it twice, while nothing behaves differently -- so the "+
				"only symptom is output nobody trusts.", name)
		}
		seen[name] = true
	}
}

// TestToolIsKnownAnswersForTheWholeListAndNothingElse pins the accessor to the
// declaration rather than to a second copy of it.
//
// ToolIsKnown is a loop over the slice today. If it is ever replaced by a map
// built at init -- the obvious "optimisation" over five strings -- this is what
// catches a map that was populated from a literal instead of from KnownTools.
func TestToolIsKnownAnswersForTheWholeListAndNothingElse(t *testing.T) {
	for _, name := range KnownTools {
		if !ToolIsKnown(name) {
			t.Errorf("ToolIsKnown(%q) = false for a name in KnownTools\n"+
				"  consequence: the loader refuses a grant the runner implements, so "+
				"a correct blueprint does not load and the message asks the user to "+
				"fix something that is already right.", name)
		}
	}
	for _, name := range []string{"", "bahs", "reed", "READ", "read ", "arxi_state_unlock"} {
		if ToolIsKnown(name) {
			t.Errorf("ToolIsKnown(%q) = true for a name that is not a tool\n"+
				"  consequence: the grant reaches policy resolution, where a name "+
				"outside the mutating set resolves to allow, and the run pays for the "+
				"turn that calls it before the runner refuses it.", name)
		}
	}
}

// TestEveryToolThatForcesAWorktreeIsAToolThatCanBeGranted ties this list to the
// one decision in the kernel that already read member grants.
//
// ResolveDefaults turns Workspace into "worktree" when some member holds a
// mutating tool, and it names those tools inline. Those two spellings of the tool
// vocabulary have to agree in this direction at least: a name that triggers
// worktree isolation but cannot be granted is unreachable code standing in for a
// safety default, and the reader who finds it has no way to tell whether the
// isolation is being applied or the name is dead.
//
// It is checked through behaviour because the inline set is not exported: grant
// exactly one tool and ask what workspace the run gets.
func TestEveryToolThatForcesAWorktreeIsAToolThatCanBeGranted(t *testing.T) {
	for _, name := range []string{"write", "bash", "edit"} {
		if !ToolIsKnown(name) {
			t.Errorf("%q forces worktree isolation in ResolveDefaults but is not in "+
				"KnownTools\n"+
				"  consequence: no blueprint can grant it, so the branch that "+
				"protects two agents from overwriting each other cannot be reached by "+
				"the name that is supposed to reach it.\n"+
				"  remedy: add it to KnownTools, or stop naming it in ResolveDefaults.",
				name)
		}
		cfg := Config{Members: []MemberConfig{{Name: "m", Tools: []string{name}}}}
		if got := cfg.ResolveDefaults().Workspace; got != "worktree" {
			t.Errorf("a member granted only %q got workspace %q, want worktree\n"+
				"  consequence: members that can write share one directory and "+
				"overwrite each other; the KV lock does not prevent it.", name, got)
		}
	}
}
