package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
	"github.com/michiTrader/arxi/internal/toolstore"
)

// policyDir is where per-agent tool policy overrides live. A var so tests can
// point it somewhere disposable, following providerDir and evalDir.
var policyDir = toolstore.DefaultDir

func openPolicies() *toolstore.Store {
	s, err := toolstore.Open(policyDir)
	if err != nil {
		fatal(err)
	}
	return s
}

// cmdAgent routes the agent subcommands.
//
// Only `tool policy` is implemented. The rest fall through to notImplemented
// rather than "unknown command", because the surface declares `agent list`,
// `agent create` and `agent show` and blaming the user for a typo they did not
// make is the worst available answer.
func cmdAgent(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: arxi agent tool policy --agent <name> "+
			"[--allow <tool>] [--ask <tool>] [--deny <tool>] [--reset <tool>]\n")
		os.Exit(2)
	}

	if args[0] == "tool" {
		// `agent tool` is not itself a command: the registry declares only
		// `agent tool policy`. Answering a bare `agent tool` with the agent
		// subcommand list would print "tool is not an agent command" above a
		// list containing tool, which is why surface.SubcommandsUnder exists.
		if len(args) < 2 || args[1] != "policy" {
			fmt.Fprintf(os.Stderr, "arxi agent tool: expected \"policy\"\n"+
				"  usage: arxi agent tool policy --agent <name> [--allow <tool>]\n")
			os.Exit(2)
		}
		cmdAgentToolPolicy(args[2:])
		return
	}

	notImplemented(append([]string{"agent"}, args...))
}

// cmdAgentToolPolicy sets or clears one tool policy override for one agent.
//
// This is the second remedy `run why` prints, and §20.2 recommends it by name:
// when an approval will recur every turn, fix the policy instead of the symptom.
// Before this existed the recommendation was a dead end -- the loop it escapes
// had no exit, because approving a tool call respawns the turn while the policy
// still says ask, so the model calls the tool and is asked again.
//
// It is CLI-only, and that is a security boundary rather than an omission
// (§20.12): an agent that can widen its own tool policy does not have a policy.
// The registry says Kind: CLIOnly, so this is never exposed as a tool and never
// reachable over the protocol.
func cmdAgentToolPolicy(args []string) {
	args, err := expandShort(surface.Lookup("agent", "tool", "policy"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: %v\n", err)
		os.Exit(2)
	}

	var agent string
	// change is a SLICE and not a map, and that is a defect this command shipped
	// with for one commit. Keyed by tool name, `--allow bash --deny bash`
	// collapsed to a single entry, so the "one tool at a time" refusal below
	// counted one change and applied whichever flag was written last. A
	// contradiction the command claimed to refuse was silently resolved by map
	// assignment order.
	//
	// Found by running it, not by testing it: every unit assertion about the
	// guard passed, because they all used two DIFFERENT tools.
	var change []toolChange
	var reset []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func(name string) string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "arxi agent tool policy: %s needs a value\n", name)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		switch {
		case a == "--agent":
			agent = val("--agent")
		case strings.HasPrefix(a, "--agent="):
			agent = strings.TrimPrefix(a, "--agent=")
		case a == "--allow":
			change = append(change, toolChange{val("--allow"), surface.PolicyAllow})
		case strings.HasPrefix(a, "--allow="):
			change = append(change, toolChange{strings.TrimPrefix(a, "--allow="), surface.PolicyAllow})
		case a == "--ask":
			change = append(change, toolChange{val("--ask"), surface.PolicyAsk})
		case strings.HasPrefix(a, "--ask="):
			change = append(change, toolChange{strings.TrimPrefix(a, "--ask="), surface.PolicyAsk})
		case a == "--deny":
			change = append(change, toolChange{val("--deny"), surface.PolicyDeny})
		case strings.HasPrefix(a, "--deny="):
			change = append(change, toolChange{strings.TrimPrefix(a, "--deny="), surface.PolicyDeny})
		case a == "--reset":
			reset = append(reset, val("--reset"))
		case strings.HasPrefix(a, "--reset="):
			reset = append(reset, strings.TrimPrefix(a, "--reset="))
		default:
			fmt.Fprintf(os.Stderr, "arxi agent tool policy: unexpected %q\n"+
				"  usage: arxi agent tool policy --agent <name> "+
				"[--allow <tool>] [--ask <tool>] [--deny <tool>] [--reset <tool>]\n", a)
			os.Exit(2)
		}
	}

	if strings.TrimSpace(agent) == "" {
		// --agent is declared required in the surface, and it is required for a
		// blunt reason: a policy change with no agent has no meaning, and
		// guessing one from context would be guessing about authorization.
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: --agent is required\n"+
			"  a policy belongs to one agent, and there is no default\n")
		os.Exit(2)
	}

	if len(change)+len(reset) == 0 {
		// Showing the current policy would be a reasonable thing to do here,
		// but `agent show` is the declared way to read one and this command
		// declares itself as a change. Printing on no flags would make the
		// mutating command dual-purpose, and the surface says what it is.
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: nothing to change\n"+
			"  say what the policy should be: --allow, --ask, --deny or --reset <tool>\n")
		os.Exit(2)
	}

	if len(change)+len(reset) > 1 {
		// Refused rather than applied in some order. Two flags in one command
		// means the operator is thinking of two changes, and if they conflict
		// -- `--allow bash --deny bash` -- there is no reading of the command
		// line that says which one they meant.
		var named []string
		for _, c := range change {
			named = append(named, fmt.Sprintf("%s=%s", c.tool, c.policy))
		}
		for _, r := range reset {
			named = append(named, r+"=default")
		}
		sort.Strings(named)
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: one tool at a time (got %s)\n"+
			"  two policy flags in one command have no order that is written down;\n"+
			"  run it twice instead\n", strings.Join(named, ", "))
		os.Exit(2)
	}

	// Tool names are checked before the store is touched, so an unknown tool is
	// a usage error (exit 2) naming the flag the user typed, rather than the
	// store's own error routed through fatal (exit 1). The store keeps its check
	// regardless: that one protects a hand-edited file, and this one answers a
	// person at a keyboard.
	for _, c := range change {
		requireKnownTool(c.tool)
	}
	for _, r := range reset {
		requireKnownTool(r)
	}

	store := openPolicies()

	// --reset first, because it is the only branch that takes the other path
	// through the store.
	for _, name := range reset {
		had, err := store.Clear(agent, name)
		if err != nil {
			fatal(err)
		}
		if !had {
			// Not an error: the end state is what was asked for. But saying so
			// matters, because the operator may have typed the wrong agent, and
			// "done" would let them believe a change had landed.
			fmt.Printf("%s: %s had no override, so nothing changed.\n", agent, name)
			fmt.Printf("  it resolves to %s, which is the default for %s.\n",
				defaultPolicyOf(name), name)
			return
		}
		fmt.Printf("%s: %s is back to its default policy (%s).\n",
			agent, name, defaultPolicyOf(name))
		printNotRetroactive()
		return
	}

	for _, c := range change {
		name, pol := c.tool, c.policy
		prev, had, err := store.Set(agent, name, pol)
		if err != nil {
			fatal(err)
		}
		def := defaultPolicyOf(name)
		switch {
		case had && prev == pol:
			fmt.Printf("%s: %s was already %s. nothing changed.\n", agent, name, pol)
			return
		case !had && pol == def:
			// An override that restates the default is stored, and saying so
			// matters. The walk printed "read is now allow (the default is
			// allow)" for it, which reads as a change and is not one: the
			// resolver would answer identically with no override at all.
			//
			// It is not refused, because an operator pinning a default against a
			// future change to it is making a real decision. It is just not
			// described as an effect it does not have.
			fmt.Printf("%s: %s is %s, which is already the default. "+
				"the override is recorded but changes nothing.\n", agent, name, pol)
			fmt.Printf("  remove it with: arxi agent tool policy --agent %s --reset %s\n",
				agent, name)
			return
		case had:
			fmt.Printf("%s: %s is now %s (was %s).\n", agent, name, pol, prev)
		default:
			fmt.Printf("%s: %s is now %s (the default is %s).\n",
				agent, name, pol, def)
		}

		// Only said for allow, and only because it is the widening direction.
		// `--allow bash` on an agent without bash succeeds at what it was asked
		// to do and changes nothing observable, which is the most confusing
		// possible outcome -- so the message exists to pre-empt it.
		//
		// It used to print for every policy, including deny, where it read as
		// advice about granting to somebody who had just refused something. A
		// note that is true but irrelevant is how notes stop being read.
		if pol == surface.PolicyAllow {
			fmt.Printf("  this does not grant %s. an agent that was never given "+
				"%s is still denied it.\n", name, name)
		}
		printNotRetroactive()
		return
	}
}

// toolChange is one --allow/--ask/--deny flag, kept as a pair so two flags
// naming the same tool stay two flags. See the comment on `change`.
type toolChange struct {
	tool   string
	policy surface.Policy
}

// requireKnownTool exits with a usage error for a tool name that is not
// grantable, naming the alternatives.
func requireKnownTool(name string) {
	if tool.Known[name] {
		return
	}
	fmt.Fprintf(os.Stderr, "arxi agent tool policy: unknown tool %q\n"+
		"  known tools: %s\n", name, strings.Join(knownToolNames(), ", "))
	os.Exit(2)
}

// printNotRetroactive says the one thing about this command that surprises
// people, in the place they will actually read it.
//
// The policy is read when a run starts and copied into its executor, so a run
// that is currently blocked on an approval does not see the change. The person
// running this command is very often looking at exactly that blocked run --
// `run why` sent them here -- so leaving this in a doc comment would mean the
// only people who learn it are the ones who did not need to.
func printNotRetroactive() {
	fmt.Printf("  this applies to runs started from now on. a run already " +
		"waiting is not unblocked by it:\n")
	fmt.Printf("  answer that one with: arxi inbox\n")
}

// defaultPolicyOf is the policy a tool has with no override, for messages.
//
// It asks tool.Resolve rather than restating the rule, with the tool granted so
// the grant check passes and the default is what is measured. A copy of the
// table here would be a second opinion about policy, and the day it disagreed
// the CLI would print one thing while the run did another.
func defaultPolicyOf(name string) surface.Policy {
	return tool.Resolve([]string{name}, nil, name)
}

// knownToolNames lists the grantable tools in a stable order.
func knownToolNames() []string {
	out := make([]string, 0, len(tool.Known))
	for k := range tool.Known {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
