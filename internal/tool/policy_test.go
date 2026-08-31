package tool

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// TestATooldNotGrantedIsDenied is the default the whole design rests on.
func TestAToolNotGrantedIsDenied(t *testing.T) {
	got := Resolve([]string{"read"}, nil, "bash")
	if got != surface.PolicyDeny {
		t.Errorf("bash on an agent granted only read = %q, want deny\n"+
			"  why this matters: a permissive default turns every oversight into a "+
			"silent hole, and the person who pays for the hole is never the person "+
			"who forgot.", got)
	}
}

// TestAGrantedMutatingToolAsksRatherThanActs covers the second rule: being
// allowed to have a tool is not being allowed to use it unsupervised.
func TestAGrantedMutatingToolAsksRatherThanActs(t *testing.T) {
	for _, name := range []string{"write", "bash", "edit"} {
		got := Resolve([]string{"read", "write", "bash", "edit"}, nil, name)
		if got != surface.PolicyAsk {
			t.Errorf("%s granted = %q, want ask\n"+
				"  why this matters: if an agent can change the world without "+
				"asking, that has to be a written decision, not a default.", name, got)
		}
	}
}

// TestAGrantedReadingToolIsAllowed covers the third rule. Without it the design
// is merely safe, not usable: an agent that has to ask permission to read a file
// costs a human interruption per turn and will not be used.
func TestAGrantedReadingToolIsAllowed(t *testing.T) {
	for _, name := range []string{"read", "grep"} {
		got := Resolve([]string{"read", "grep"}, nil, name)
		if got != surface.PolicyAllow {
			t.Errorf("%s granted = %q, want allow", name, got)
		}
	}
}

// TestAnOverrideCanAllowAMutatingTool: the escape hatch has to work, or the
// remedy `run why` prints is a lie.
//
// `arxi agent tool policy --agent backend --allow bash` is a real command in the
// surface, and §20.3 prints it as one of two remedies for a blocked run. If an
// override could not beat the mutating default, the documented fix for "you are
// being asked about this tool every turn" would do nothing.
func TestAnOverrideCanAllowAMutatingTool(t *testing.T) {
	over := map[string]surface.Policy{"bash": surface.PolicyAllow}
	got := Resolve([]string{"read", "bash"}, over, "bash")
	if got != surface.PolicyAllow {
		t.Errorf("bash with an explicit --allow override = %q, want allow\n"+
			"  consequence: the remedy `arxi agent tool policy --agent X --allow "+
			"bash`, which `run why` prints, would not actually change anything.", got)
	}
}

// TestAnOverrideCannotGrantAToolTheAgentNeverHad keeps the grant list load
// bearing.
//
// Otherwise the two mechanisms disagree about which is authoritative, and the
// answer people would discover is "neither" -- an agent created without `bash`
// could be handed `bash` by a policy command whose stated job is to adjust how a
// granted tool is treated.
func TestAnOverrideCannotGrantAToolTheAgentNeverHad(t *testing.T) {
	over := map[string]surface.Policy{"bash": surface.PolicyAllow}
	got := Resolve([]string{"read"}, over, "bash")
	if got != surface.PolicyDeny {
		t.Errorf("bash allowed by override but never granted = %q, want deny\n"+
			"  consequence: the grant list becomes decorative, and `agent show` "+
			"would describe an agent that can do more than it says.", got)
	}
}

// TestAnOverrideCanTightenAsWellAsLoosen: policy is not a one-way ratchet.
func TestAnOverrideCanTightenAsWellAsLoosen(t *testing.T) {
	over := map[string]surface.Policy{"read": surface.PolicyAsk}
	if got := Resolve([]string{"read"}, over, "read"); got != surface.PolicyAsk {
		t.Errorf("read tightened to ask = %q, want ask", got)
	}
	over = map[string]surface.Policy{"bash": surface.PolicyDeny}
	if got := Resolve([]string{"bash"}, over, "bash"); got != surface.PolicyDeny {
		t.Errorf("bash tightened to deny = %q, want deny", got)
	}
}

// TestTheMutatingSetAgreesWithTheKernel is the reason Mutating is a named var
// and not two literals.
//
// The kernel decides that a blueprint whose members can write needs a worktree
// rather than a shared directory, and it decides that from its own idea of which
// tools mutate. If the two sets drift, the failure is silent and it is the worst
// possible shape: a tool that counts as mutating for workspace isolation but not
// for policy gets a private directory to scribble in and no approval gate on the
// scribbling, while one that counts for policy but not isolation gets an
// approval gate and a shared directory to corrupt.
//
// The kernel's set is not exported, so this reads it through the behaviour it
// drives: ResolveDefaults turns Workspace into "worktree" exactly when some
// member holds a mutating tool, and leaves it "none" otherwise.
func TestTheMutatingSetAgreesWithTheKernel(t *testing.T) {
	for name := range Known {
		cfg := kernel.Config{
			Members: []kernel.MemberConfig{{Name: "m", Tools: []string{name}}},
		}
		got := cfg.ResolveDefaults()

		kernelSaysMutating := got.Workspace == "worktree"
		if kernelSaysMutating != Mutating[name] {
			t.Errorf("%q: kernel treats it as mutating=%v, tool.Mutating says %v\n"+
				"  why this is dangerous: the two sets drive different protections. "+
				"A tool that mutates for isolation but not for policy gets a private "+
				"directory and no approval gate; the reverse gets an approval gate "+
				"and a shared directory to corrupt. Neither failure is visible.",
				name, kernelSaysMutating, Mutating[name])
		}
	}
}

// TestResolveAllIsSortedAndComplete: this is what `agent show` prints.
func TestResolveAllIsSortedAndComplete(t *testing.T) {
	got := ResolveAll([]string{"write", "read", "bash"}, nil)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}

	want := []Resolved{
		{Tool: "bash", Policy: surface.PolicyAsk},
		{Tool: "read", Policy: surface.PolicyAllow},
		{Tool: "write", Policy: surface.PolicyAsk},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v\n"+
				"  why sorted: this listing is diffed against last week's to see "+
				"what an agent gained, and a listing that reorders itself makes "+
				"every diff unreadable.", i, got[i], want[i])
		}
	}
}

// TestResolveAllDropsDuplicates: `--tools read,read` is a typo, not a request
// for two entries in the table.
func TestResolveAllDropsDuplicates(t *testing.T) {
	got := ResolveAll([]string{"read", "read"}, nil)
	if len(got) != 1 {
		t.Errorf("got %d entries for read,read, want 1: %+v", len(got), got)
	}
}

// TestAnUnknownToolIsRefusedWithTheKnownOnes: the message has to be actionable,
// because a typo in a tool name is caught here or paid for mid-run.
func TestAnUnknownToolIsRefusedWithTheKnownOnes(t *testing.T) {
	err := ValidateGrants([]string{"read", "reed"})
	if err == nil {
		t.Fatal("ValidateGrants accepted the unknown tool \"reed\"\n" +
			"  consequence: the grant is silently useless and fails at the moment " +
			"it is first needed, halfway through a paid run.")
	}
	if !strings.Contains(err.Error(), "reed") {
		t.Errorf("the error does not name the bad tool: %v", err)
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("the error does not list the known tools, so the reader cannot "+
			"see that they meant \"read\": %v", err)
	}
}

// TestEveryBadToolIsReportedAtOnce: one edit-run cycle per typo is a bad trade
// when both typos are visible in the same list.
func TestEveryBadToolIsReportedAtOnce(t *testing.T) {
	err := ValidateGrants([]string{"reed", "writ"})
	if err == nil {
		t.Fatal("ValidateGrants accepted two unknown tools")
	}
	for _, want := range []string{"reed", "writ"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is missing from the error, so the user learns about the "+
				"typos one run at a time: %v", want, err)
		}
	}
}

// TestEveryKnownToolResolvesToARealPolicy guards against a tool being added to
// Known without anyone deciding how it is treated.
//
// The dangerous outcome is not an error, it is the empty string: a policy that
// is neither allow, ask nor deny would compare unequal to all three, and every
// caller that switches on it would fall through to whatever its default branch
// happens to do.
func TestEveryKnownToolResolvesToARealPolicy(t *testing.T) {
	granted := make([]string, 0, len(Known))
	for name := range Known {
		granted = append(granted, name)
	}

	for _, name := range granted {
		switch p := Resolve(granted, nil, name); p {
		case surface.PolicyAllow, surface.PolicyAsk, surface.PolicyDeny:
		default:
			t.Errorf("%q resolves to %q, which is not one of the three policies\n"+
				"  consequence: it compares unequal to all of them, so every "+
				"caller that switches on the policy silently takes its default "+
				"branch -- and the default branch of a permission check is the "+
				"one place a fallthrough must never happen.", name, p)
		}
	}
}
