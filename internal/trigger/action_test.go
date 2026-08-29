package trigger

import (
	"strings"
	"testing"

	"github.com/michiTrader/iash/internal/surface"
)

func mustAction(t *testing.T, then string) Action {
	t.Helper()
	a, err := ParseAction(then)
	if err != nil {
		t.Fatalf("ParseAction(%q) failed: %v", then, err)
	}
	return a
}

// TestTheDocumentedActionParses is the same argument as its cron twin: §20.10
// prints one --then value, and that is the line a reader copies.
func TestTheDocumentedActionParses(t *testing.T) {
	const then = "run start security-team 'audit dependencies for new CVEs'"
	a := mustAction(t, then)
	if got := strings.Join(a.Path, " "); got != "run start" {
		t.Fatalf("--then %q resolved to the command %q, want \"run start\".\n"+
			"This exact line appears in docs/design/20-use-cases.md §20.10 and is "+
			"what users will paste", then, got)
	}
	if len(a.Args) == 0 {
		t.Fatal("the arguments after `run start` were dropped: the trigger would " +
			"fire the right command with no actor and no objective")
	}
}

// TestTheLongestCommandPathWins.
//
// `agent tool policy` is three words and `agent` is a prefix of nothing here,
// but `agent list`, `agent create` and `agent show` all start with the same
// word. Matching shortest-first would resolve the wrong entry and quietly
// reinterpret the remaining words as arguments: `agent create researcher` would
// become the command `agent` with the argument `create`, which is a different
// command with a different policy.
func TestTheLongestCommandPathWins(t *testing.T) {
	a := mustAction(t, "run start team 'x'")
	if strings.Join(a.Path, " ") != "run start" {
		t.Fatalf("`run start team 'x'` resolved to %q; a shortest-first match "+
			"reinterprets the verb as an argument and lands on a different "+
			"command with a different policy", strings.Join(a.Path, " "))
	}

	// A one-word command must still resolve, and its arguments must not be
	// swallowed into the path.
	b := mustAction(t, "inbox")
	if strings.Join(b.Path, " ") != "inbox" || len(b.Args) != 0 {
		t.Fatalf("`inbox` resolved to path %q args %q, want the bare command with "+
			"no arguments", b.Path, b.Args)
	}
}

// TestACommandWithheldFromNonHumansIsRefused is the security decision.
//
// A trigger fires unattended, so it is not a human. Every command here is
// withheld from agents in §20.12, and the reason applies to a trigger at least
// as strongly: `inbox approve` on a schedule turns every ask-policy in the
// system into allow, at 3am, with nobody reading the output.
//
// It matters that this is enforced HERE and not left to the human approving
// `trigger create`. That command is ToolPolicy: ask, so an agent can propose a
// trigger — and approving "create a trigger" would be approving whatever its
// --then contains. Nobody reconstructs §20.12's table from memory while reading
// a one-line diff.
func TestACommandWithheldFromNonHumansIsRefused(t *testing.T) {
	// Derived from the registry rather than listed, so a command that changes
	// Kind is covered without editing this test.
	var withheld []string
	for _, c := range surface.Registry {
		if c.Kind&surface.AgentTool == 0 {
			withheld = append(withheld, c.CLI())
		}
	}
	if len(withheld) == 0 {
		t.Fatal("no command in the registry is withheld from agents, so this test " +
			"proves nothing. Either the Kind flags were lost or AgentTool was renamed")
	}

	for _, cli := range withheld {
		t.Run(cli, func(t *testing.T) {
			_, err := ParseAction(cli + " something")
			if err == nil {
				t.Fatalf("--then %q was accepted. This command is not exposed to "+
					"agents, and a trigger is less supervised than an agent: it "+
					"runs with nobody present. `trigger create` is ask-policy, so "+
					"accepting this would mean a human approving \"create a "+
					"trigger\" had unknowingly approved this command too", cli)
			}
			if !strings.Contains(err.Error(), "unattended") {
				t.Fatalf("the refusal must say WHY an unattended invocation is the "+
					"problem, or the next reader assumes the command is simply "+
					"missing and adds it. Got: %v", err)
			}
		})
	}
}

// TestEveryTriggerableCommandActuallyParses is the other direction, and it is
// the one that catches a parser that is merely strict.
//
// TriggerableCommands is what the error message advertises. If a command on that
// list is refused by the parser, the tool tells the user to write something it
// will then reject — and the failure is silent in the worst way, because the
// user believes the documentation.
func TestEveryTriggerableCommandActuallyParses(t *testing.T) {
	list := TriggerableCommands()
	if len(list) == 0 {
		t.Fatal("TriggerableCommands is empty, so --then would reject everything " +
			"while the error message advertises nothing")
	}
	for _, cli := range list {
		a, err := ParseAction(cli + " arg")
		if err != nil {
			t.Fatalf("--then %q is advertised by TriggerableCommands and refused by "+
				"ParseAction: %v\nThe two must agree, or the error message sends "+
				"the user to a value the parser rejects", cli, err)
		}
		if strings.Join(a.Path, " ") != cli {
			t.Fatalf("--then %q resolved to %q: the path was mis-split, so the "+
				"trigger would fire a different command from the one named",
				cli, strings.Join(a.Path, " "))
		}
	}
}

// TestTheAdvertisedListIsDerivedFromTheRegistry.
//
// The point of this file: `--then` was declared as "run:|emit:|notify:" and
// `notify` is not a command in this system. A hand-maintained action vocabulary
// went stale before a line of trigger code was written. This asserts the list is
// the registry's AgentTool set and not a copy of it.
func TestTheAdvertisedListIsDerivedFromTheRegistry(t *testing.T) {
	got := map[string]bool{}
	for _, c := range TriggerableCommands() {
		got[c] = true
	}
	for _, c := range surface.Registry {
		isTool := c.Kind&surface.AgentTool != 0
		if isTool && !got[c.CLI()] {
			t.Fatalf("%q is an agent tool and is missing from TriggerableCommands. "+
				"That list must be derived from the Kind flags: the description "+
				"this replaced named `notify:`, a command that does not exist, and "+
				"nothing failed", c.CLI())
		}
		if !isTool && got[c.CLI()] {
			t.Fatalf("%q is withheld from agents and appears in "+
				"TriggerableCommands, so the error message invites exactly the "+
				"invocation the parser refuses", c.CLI())
		}
	}
}

// TestNotifyIsRefusedAndSaysSo pins the specific stale value, because it is what
// somebody types after reading the old description — including anyone reading an
// older revision of this repository.
func TestNotifyIsRefusedAndSaysSo(t *testing.T) {
	for _, then := range []string{"notify:ops", "notify ops"} {
		if _, err := ParseAction(then); err == nil {
			t.Fatalf("--then %q was accepted. `notify` has no entry in the "+
				"registry, so this trigger would be stored, listed as active, and "+
				"fail at 3am into a stderr nobody reads", then)
		}
	}
}

// TestASchemePrefixIsRefusedWithTheFix.
//
// "run:start ..." is what a reader of the old description writes. Treating it as
// an unknown command would be technically true and useless: it sends them
// looking for a missing feature when the answer is to delete one character.
func TestASchemePrefixIsRefusedWithTheFix(t *testing.T) {
	_, err := ParseAction("run:start team 'x'")
	if err == nil {
		t.Fatal("--then \"run:start ...\" was accepted, so there are now two " +
			"spellings for one action and `trigger show` has to pick one to echo")
	}
	if !strings.Contains(err.Error(), "no scheme prefix") {
		t.Fatalf("the refusal must name the prefix as the problem. \"unknown "+
			"command\" would be true and would send the reader hunting for a "+
			"feature that is not missing. Got: %v", err)
	}
}

// TestAnEmptyActionIsRefused. A trigger that fires and does nothing is
// indistinguishable, in every output this system has, from one that is broken.
func TestAnEmptyActionIsRefused(t *testing.T) {
	for _, then := range []string{"", "   "} {
		a, err := ParseAction(then)
		if err == nil {
			t.Fatalf("--then %q was accepted: the trigger fires on schedule and "+
				"does nothing, which looks exactly like a trigger that is broken", then)
		}
		if a.Path != nil {
			t.Fatal("a rejected action must come back zeroed, or a caller that " +
				"ignores the error stores half a trigger")
		}
	}
}

// TestAnUnknownCommandListsWhatIsAccepted. A refusal a user cannot act on is
// answered by trying variations, and one of the variations will be a command
// that exists but should not be triggerable.
func TestAnUnknownCommandListsWhatIsAccepted(t *testing.T) {
	_, err := ParseAction("deploy production")
	if err == nil {
		t.Fatal("--then \"deploy production\" was accepted; `deploy` is not a " +
			"command in this system")
	}
	if !strings.Contains(err.Error(), "run start") {
		t.Fatalf("the refusal must list the triggerable commands, so the user's "+
			"next attempt is informed rather than a guess. Got: %v", err)
	}
}

// TestTheRawActionSurvivesParsing, for the reason Spec.Raw survives: `trigger
// show` echoes what the user wrote, and a normalised echo is not the line they
// can find in their shell history.
func TestTheRawActionSurvivesParsing(t *testing.T) {
	const then = "run start security-team 'audit dependencies'"
	if got := mustAction(t, then).Raw; got != then {
		t.Fatalf("Raw came back as %q, want %q", got, then)
	}
}
