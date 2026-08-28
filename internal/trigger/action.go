package trigger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/michiTrader/iash/internal/surface"
)

// Action is a parsed `--then` value: what the trigger does when it fires.
//
// THE DECISION HERE IS THAT THERE IS NO ACTION VOCABULARY.
//
// `--then` was declared as "run:|emit:|notify:", three prefixes naming a set of
// things a trigger can do. That is a second surface, and it had already drifted
// before a line of trigger code existed: `notify` is not a command in this
// system, has no entry in the registry, and nothing anywhere says what it would
// send or to whom. Meanwhile §20.10's own example writes
//
//	--then "run start security-team 'audit dependencies for new CVEs'"
//
// with no prefix at all, because `run start` is simply what the user wants to
// happen. The document was right and the parameter description was the copy
// nobody updated.
//
// So `--then` is a command path from internal/surface, followed by its
// arguments. `run start`, `event emit`, `run cancel` — every capability the
// system has, with no per-action plumbing and nothing to keep in sync. A verb
// added to the registry is triggerable the same day; a verb renamed cannot leave
// a stale prefix behind, because there are no prefixes.
type Action struct {
	Raw  string   // what the user typed, echoed by `trigger show`
	Path []string // the registry path: {"run","start"}
	Args []string // everything after it, already split
}

// CLI renders the action the way the user would type it.
func (a Action) CLI() string {
	return strings.TrimSpace(strings.Join(a.Path, " ") + " " + strings.Join(a.Args, " "))
}

// ParseAction parses a `--then` value against the declared surface.
//
// Two things are checked, and the second is the one that matters.
//
// First, the command must exist. A trigger naming a verb nothing implements is
// stored, listed as active, and fails at 3am with nobody reading stderr.
//
// Second, the command must be something a NON-HUMAN may invoke, which is exactly
// Kind&AgentTool. A trigger fires unattended by definition, so a trigger is not
// a human, and the thirteen commands withheld from agents (§20.12) are withheld
// for reasons that apply here identically or more strongly:
//
//   - `inbox approve` is the human's side of the conversation. A trigger that
//     approves inbox items turns every `ToolPolicy: ask` in the system into
//     `allow` — on a schedule, while nobody is watching.
//   - `provider add`, `model enable`, `agent tool policy` are operator
//     decisions about credentials, spend and permission.
//   - `run attach` blocks waiting for output nobody is reading.
//   - `serve` and `design` are an operator's socket and an interactive editor.
//
// `trigger create` is itself `ToolPolicy: ask`, so an agent can propose one with
// a human approval. Without this check that approval would be worth much less
// than it looks: approving "create a trigger" would be approving whatever the
// trigger's --then contains, and a human reading a diff is not going to
// reconstruct §20.12's table from memory to notice that one line hands an agent
// the inbox.
//
// The set is DERIVED from the Kind flags, never listed here. A hand-written list
// of forbidden verbs is how the description this function replaces went stale.
func ParseAction(then string) (Action, error) {
	then = strings.TrimSpace(then)
	if then == "" {
		return Action{}, fmt.Errorf("--then is empty: the trigger would fire on " +
			"schedule and do nothing, which is indistinguishable from a trigger " +
			"that is broken.\n" +
			"  what to write: an iash command, e.g.\n" +
			"    --then \"run start security-team 'audit dependencies for new CVEs'\"")
	}

	// A prefix is refused explicitly rather than treated as an unknown command.
	// "run:start ..." is what somebody types after reading the old description,
	// and "there is no command \"run:start\"" would send them looking for a
	// missing feature instead of telling them to drop the colon.
	if head := strings.Fields(then)[0]; strings.Contains(head, ":") {
		return Action{}, fmt.Errorf("--then %q starts with %q, and --then takes no "+
			"scheme prefix.\n"+
			"  what to write: the iash command itself, e.g. --then \"run start "+
			"team 'objective'\"\n"+
			"  why there is no vocabulary here: the action IS a command from the "+
			"same surface as everything else, so every verb iash gains is "+
			"triggerable with no prefix to add and none to go stale",
			then, head)
	}

	fields := strings.Fields(then)

	// Longest path first: `agent tool policy` is three words and `agent` is a
	// prefix of it. Trying shortest-first would match nothing here, but the day
	// a one-word command shares a prefix with a two-word one it would match the
	// wrong entry and the arguments would silently become the wrong arguments.
	for n := min(len(fields), 3); n >= 1; n-- {
		path := fields[:n]
		c := surface.Lookup(path...)
		if c == nil {
			continue
		}
		if c.Kind&surface.AgentTool == 0 {
			return Action{}, fmt.Errorf("--then %q names %q, which is deliberately "+
				"not available to anything but a human at a terminal.\n"+
				"  why: a trigger fires unattended, so it is not a human. The "+
				"commands withheld from non-humans are the ones where an "+
				"unattended invocation defeats a control: `inbox approve` is the "+
				"human's side of the conversation and a trigger that approves "+
				"inbox items turns every ask-policy in the system into allow, on "+
				"a schedule, at 3am.\n"+
				"  what to do: trigger the work itself (`run start ...`) and let "+
				"the operator decision stay an operator decision",
				then, c.CLI())
		}
		return Action{Raw: then, Path: append([]string{}, path...), Args: append([]string{}, fields[n:]...)}, nil
	}

	return Action{}, fmt.Errorf("--then %q does not name an iash command.\n"+
		"  triggerable commands: %s\n"+
		"  this is refused at create time rather than stored, because a trigger "+
		"whose action nothing implements sits in `trigger list` marked active and "+
		"fails at 3am into a stderr nobody is reading",
		then, strings.Join(TriggerableCommands(), ", "))
}

// TriggerableCommands lists what `--then` accepts, derived from the registry.
//
// Derived and not written down, for the reason this whole file exists: the
// hand-listed "run:|emit:|notify:" it replaces named an action that does not
// exist, and nothing failed. This cannot go stale — if it is wrong, it is wrong
// because the Kind flags are wrong, and those are what the security boundary is
// actually made of.
func TriggerableCommands() []string {
	var out []string
	for _, c := range surface.Registry {
		if c.Kind&surface.AgentTool != 0 {
			out = append(out, c.CLI())
		}
	}
	sort.Strings(out)
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
