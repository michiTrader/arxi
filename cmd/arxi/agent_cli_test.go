package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
)

// policyOf reads the stored policy for one tool, or "" when there is none.
//
// It reads the file rather than calling the store, because the point of these
// tests is what the BINARY did. A helper that went through toolstore would test
// the same code twice and miss a CLI that never called it.
func policyOf(t *testing.T, dir, agent, toolName string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "policies", agent+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading the policy of %s: %v", agent, err)
	}
	var rec struct {
		Tools map[string]string `json:"tools"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("the policy file the binary wrote does not parse: %v\n%s", err, raw)
	}
	return rec.Tools[toolName]
}

// TestAllowingATheApprovalLoopIsEscapable is the reason this command exists.
//
// §20.2 and `run why` both recommend it by name: when an approval will recur
// every turn, fix the policy instead of the symptom. Before it existed that
// recommendation was a dead end — approving a tool call respawns the turn while
// the policy still says ask, so the model calls the tool and is asked again.
func TestAllowingATheApprovalLoopIsEscapable(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", r.code, r.out)
	}
	if got := policyOf(t, dir, "backend", "bash"); got != "allow" {
		t.Errorf("stored policy = %q, want allow\n"+
			"  consequence: the loop run why sends the user here to escape has "+
			"no exit\n%s", got, r.out)
	}
}

// TestTheStoredOverrideChangesWhatTheResolverAnswers.
//
// The file saying "allow" is not the property that matters; the resolver
// returning allow is. Asserted through tool.Resolve with the stored map so the
// two cannot drift.
func TestTheStoredOverrideChangesWhatTheResolverAnswers(t *testing.T) {
	dir := workdir(t)
	arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")

	stored := policyOf(t, dir, "backend", "bash")
	overrides := map[string]surface.Policy{"bash": surface.Policy(stored)}

	if got := tool.Resolve([]string{"read", "bash"}, overrides, "bash"); got != surface.PolicyAllow {
		t.Errorf("with the stored override the resolver says %q, want allow", got)
	}
	// And with no override it still asks, so the test above is measuring the
	// override rather than a default that was always allow.
	if got := tool.Resolve([]string{"read", "bash"}, nil, "bash"); got != surface.PolicyAsk {
		t.Errorf("without an override bash resolves to %q, want ask", got)
	}
}

// TestTwoFlagsForTheSameToolAreRefusedAndWriteNothing guards defect one, the
// serious one.
//
// `--allow bash --deny bash` was collected into a map keyed by tool name, so the
// two flags collapsed to one entry, the "one tool at a time" guard counted one
// change, and the last flag written won. A guard whose whole purpose is refusing
// an ambiguous authorization change could not see the ambiguous case.
//
// It is asserted with the SAME tool on purpose. Every plausible unit test of
// that guard uses two different tools, which is exactly why the defect survived
// twenty green tests and a clean build.
func TestTwoFlagsForTheSameToolAreRefusedAndWriteNothing(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend",
		"--allow", "bash", "--deny", "bash")
	if r.code == 0 {
		t.Fatalf("a contradiction was accepted (exit 0):\n%s\n"+
			"  consequence: an ambiguous authorization change is resolved by "+
			"map assignment order, silently", r.out)
	}
	if got := policyOf(t, dir, "backend", "bash"); got != "" {
		t.Errorf("a refused command wrote a policy (%q); the refusal has to be "+
			"total or it is just a warning", got)
	}
	// The message names both policies, not just the tool. "got bash, bash"
	// would have hidden the defect the same way the map did.
	if !strings.Contains(r.out, "bash=allow") || !strings.Contains(r.out, "bash=deny") {
		t.Errorf("the refusal does not show both policies:\n%s", r.out)
	}
}

// TestTwoFlagsForDifferentToolsAreAlsoRefused.
//
// Two changes in one command have no written-down order, and this command
// changes authorization. Refusing is cheaper to explain than an order.
func TestTwoFlagsForDifferentToolsAreAlsoRefused(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend",
		"--allow", "bash", "--deny", "write")
	if r.code == 0 {
		t.Fatalf("two changes in one command were accepted:\n%s", r.out)
	}
	if policyOf(t, dir, "backend", "bash") != "" || policyOf(t, dir, "backend", "write") != "" {
		t.Error("a refused command wrote a policy")
	}
}

// TestADenialIsNotGivenAdviceAboutGranting guards defect two.
//
// Every change used to print "this does not grant bash", including --deny. True,
// and irrelevant to somebody who has just refused something, which is how notes
// stop being read.
func TestADenialIsNotGivenAdviceAboutGranting(t *testing.T) {
	dir := workdir(t)

	deny := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--deny", "write")
	if strings.Contains(deny.out, "does not grant") {
		t.Errorf("a denial was told about granting:\n%s", deny.out)
	}

	// And the note is still there where it belongs: allow is the widening
	// direction it was written for.
	allow := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")
	if !strings.Contains(allow.out, "does not grant") {
		t.Errorf("--allow no longer says an override cannot grant:\n%s\n"+
			"  consequence: --allow bash on an agent without bash succeeds and "+
			"changes nothing observable, which is the most confusing outcome", allow.out)
	}
}

// TestAnOverrideThatRestatesTheDefaultSaysSo guards defect three.
//
// `--allow read` printed "read is now allow (the default is allow)", which reads
// as a change and is not one. Not refused — pinning a default against a future
// change to it is a real decision — but not described as an effect it does not
// have.
func TestAnOverrideThatRestatesTheDefaultSaysSo(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "read")
	if r.code != 0 {
		t.Fatalf("exit %d; pinning a default is a legitimate decision:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "already the default") {
		t.Errorf("output does not say the override changes nothing:\n%s", r.out)
	}
	if !strings.Contains(r.out, "--reset read") {
		t.Errorf("output does not say how to remove it:\n%s", r.out)
	}
	// It is recorded, because the operator asked for it.
	if got := policyOf(t, dir, "backend", "read"); got != "allow" {
		t.Errorf("stored = %q, want allow; the override was described as "+
			"recorded and was not", got)
	}
}

// TestAMistypedToolIsAUsageErrorAndWritesNothing guards defect four.
//
// `--allow bahs` reached the store, whose error went through fatal and exited 1.
// A mistyped flag is a usage error.
func TestAMistypedToolIsAUsageErrorAndWritesNothing(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bahs")
	if r.code != 2 {
		t.Errorf("exit %d, want 2 for a mistyped flag:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "bahs") {
		t.Errorf("the error does not quote what was typed:\n%s", r.out)
	}
	if !strings.Contains(r.out, "bash") {
		t.Errorf("the error does not list the tools that exist:\n%s", r.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "policies", "backend.json")); err == nil {
		t.Error("a policy file was written for a refused command")
	}
}

// TestSettingOneToolDoesNotForgetAnotherThroughTheCLI.
//
// The store merges, and this checks the CLI actually uses that path. Losing a
// DENY is the direction that matters: it widens an agent as a side effect of a
// command about a different tool.
func TestSettingOneToolDoesNotForgetAnotherThroughTheCLI(t *testing.T) {
	dir := workdir(t)
	arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--deny", "write")
	arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")

	if got := policyOf(t, dir, "backend", "write"); got != "deny" {
		t.Errorf("write = %q after a command about bash, want deny\n"+
			"  consequence: a deny lost by a command that never mentioned it", got)
	}
	if got := policyOf(t, dir, "backend", "bash"); got != "allow" {
		t.Errorf("bash = %q, want allow", got)
	}
}

// TestResettingRemovesTheOverrideAndSaysWhatTheDefaultIs.
func TestResettingRemovesTheOverrideAndSaysWhatTheDefaultIs(t *testing.T) {
	dir := workdir(t)
	arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")

	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--reset", "bash")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "ask") {
		t.Errorf("the output does not say what the policy is now:\n%s", r.out)
	}
	if got := policyOf(t, dir, "backend", "bash"); got != "" {
		t.Errorf("the override survived --reset: %q", got)
	}
}

// TestResettingSomethingNeverSetIsNotReportedAsAChange.
//
// The end state is what was asked for, so it is not an error. But the operator
// may have typed the wrong agent, and "done" would let them believe a change
// landed.
func TestResettingSomethingNeverSetIsNotReportedAsAChange(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--reset", "bash")
	if r.code != 0 {
		t.Fatalf("exit %d; the end state is the one asked for:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "nothing changed") {
		t.Errorf("output does not say nothing changed:\n%s\n"+
			"  consequence: a wrong agent name reads as a successful change", r.out)
	}
}

// TestSettingTheSamePolicyTwiceSaysNothingChanged.
func TestSettingTheSamePolicyTwiceSaysNothingChanged(t *testing.T) {
	dir := workdir(t)
	arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")

	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")
	if r.code != 0 {
		t.Fatalf("exit %d for a repeat:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "already allow") {
		t.Errorf("a repeat is described as a change:\n%s", r.out)
	}
}

// TestTheOutputSaysAWaitingRunIsNotUnblocked.
//
// The one surprising property of this command, in the place people read. The
// policy is copied into the executor at run start, so a run already blocked on
// an approval does not see the change — and `run why` sends people here while
// looking at exactly that run.
func TestTheOutputSaysAWaitingRunIsNotUnblocked(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--allow", "bash")

	if !strings.Contains(r.out, "already waiting") {
		t.Errorf("the output does not say a waiting run is unaffected:\n%s\n"+
			"  consequence: the operator run why sent here believes their "+
			"blocked run will now proceed, and it will not", r.out)
	}
	if !strings.Contains(r.out, "arxi inbox") {
		t.Errorf("the output does not say how to answer the run that is "+
			"actually waiting:\n%s", r.out)
	}
}

// TestAPolicyChangeWithNoAgentIsRefused.
//
// --agent is declared required, and a policy change with no agent has no
// meaning. Guessing one from context would be guessing about authorization.
func TestAPolicyChangeWithNoAgentIsRefused(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "tool", "policy", "--allow", "bash")
	if r.code != 2 {
		t.Errorf("exit %d, want 2:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "--agent") {
		t.Errorf("the error does not name the missing flag:\n%s", r.out)
	}
}

// TestNoPolicyFlagSaysWhichFlagsExist rather than printing the current policy.
//
// `agent show` is the declared way to read a policy. Printing on no flags would
// make a mutating command dual-purpose.
func TestNoPolicyFlagSaysWhichFlagsExist(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend")
	if r.code != 2 {
		t.Errorf("exit %d, want 2:\n%s", r.code, r.out)
	}
	for _, flag := range []string{"--allow", "--ask", "--deny", "--reset"} {
		if !strings.Contains(r.out, flag) {
			t.Errorf("the usage error does not mention %s:\n%s", flag, r.out)
		}
	}
}

// TestABareAgentToolDoesNotContradictItself.
//
// `agent tool` is not a command — the registry declares only `agent tool
// policy`. Answering it with the agent subcommand list would print "tool is not
// an agent command" above a list containing tool.
func TestABareAgentToolDoesNotContradictItself(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "tool")
	if r.code == 0 {
		t.Fatalf("a bare `agent tool` succeeded:\n%s", r.out)
	}
	if !strings.Contains(r.out, "policy") {
		t.Errorf("the error does not say what completes the command:\n%s", r.out)
	}
}

// TestAnUnknownAgentSubcommandListsWhatAgentAccepts.
//
// This test used to assert the opposite of its name: that `agent list`, `agent
// create` and `agent show` answered "declared in the surface but not implemented
// yet". All three are wired now. The promise it was really about -- that a
// declared path is never called unknown -- is measured across all fifty of them
// in TestEveryDeclaredPathAnswersAboutItselfOneWayOrTheOther, so a third copy
// aimed at whichever verbs happen to be unbuilt this week would need retargeting
// every time one gets built and would pin nothing the surface walk does not.
//
// What is pinned only here is the notImplemented fall-through at the end of
// cmdAgent's switch. With every declared agent verb wired it looks like dead
// code, and deleting it would send a mistyped `arxi agent lst` out of the group's
// dispatcher to be reported as an unknown top-level command -- telling the user
// to check the spelling of `agent`, the one word they got right.
func TestAnUnknownAgentSubcommandListsWhatAgentAccepts(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "agent", "lst")

	if r.code == 0 {
		t.Fatalf("a mistyped agent subcommand succeeded:\n%s", r.out)
	}
	if strings.Contains(r.out, "does not exist in the surface") {
		t.Errorf("`agent lst` is blamed on the whole path:\n%s\n"+
			"  consequence: `agent` is a real group and the user is told it is "+
			"not, so they check the group's spelling instead of the verb's.\n"+
			"  fix: keep the notImplemented fall-through at the end of cmdAgent.", r.out)
	}
	for _, want := range []string{"create", "list", "show", "tool"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("`agent lst` does not offer %q:\n%s\n"+
				"  consequence: the answer names the mistake without naming the "+
				"four words that would have worked, which is the whole reason "+
				"notImplemented looks up the longest declared prefix.", want, r.out)
		}
	}
}

// TestEveryGrantableToolCanBeGovernedFromTheCLI.
//
// Read as a check that two lists agree. tool.Known is the closed set a blueprint
// may grant, and a tool that can be granted and not governed is a hole with a
// policy-shaped door next to it.
func TestEveryGrantableToolCanBeGovernedFromTheCLI(t *testing.T) {
	dir := workdir(t)
	for name := range tool.Known {
		r := arxi(t, dir, "agent", "tool", "policy", "--agent", "backend", "--deny", name)
		if r.code != 0 {
			t.Errorf("--deny %s exited %d:\n%s\n  consequence: a tool a "+
				"blueprint can grant that an operator cannot govern", name, r.code, r.out)
		}
	}
}

// TestTheUsageScreenMentionsTheCommand.
//
// Belt and braces beside TestTheUsageScreenListsWhatIsActuallyImplemented,
// which caught exactly this drift for inbox. Named here so the failure says
// which command went missing.
func TestTheUsageScreenMentionsTheCommand(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "--help")
	if !strings.Contains(r.out, "agent tool policy") {
		t.Errorf("the usage screen does not mention agent tool policy:\n%s\n"+
			"  consequence: the escape from the approval loop is invisible to "+
			"the person most likely to be stuck in it", r.out)
	}
}
