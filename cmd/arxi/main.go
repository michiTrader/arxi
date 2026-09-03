// Command arxi is the binary.
//
// Today it implements schema, surface, why, blueprint validate, blueprint create,
// blueprint install, run start (live, calling real models, or --sim), run list,
// run show, run why, run tree, run prompt, run steer, run result, run pause,
// run unpause, run cancel, run fork, run replay, run attach, event emit,
// event log, event trace, state set, state get, state lock, state unlock, serve,
// the trigger group, the inbox group, the agent group, role define, the eval
// group (--sim only), provider add and the model group; for everything else it
// answers "declared but not implemented" with the exact name of the capability.
// That is on purpose: the surface is frozen and verified by tests BEFORE the
// executor exists, so adding a new command is implementing something that was
// already promised, not inventing a new promise.
//
// The measured figure, rather than this list, is the one to trust. See the
// README, where a probe that walks the registry against the built binary
// produces it -- and where a test fails if the number written down disagrees
// with the binary. No count is repeated here on purpose: this comment has no
// such test, so a figure in it can only rot.
//
// This paragraph has already been wrong six times. It claimed three commands
// after six existed, it said `run start` was --sim only after the live executor
// had landed, it carried "16 of 47" through six wirings and two registry
// corrections, it omitted `run prompt` for that verb's entire life -- found
// only when `run tree` was added to the same sentence -- it was silently
// missing `event emit`, the whole inbox group and `agent tool policy` until
// `run replay` landed and the coverage guard's own list of wired paths was read
// against it, and it went stale again one increment later: `state set` shipped
// wired, tested and counted in the README while this list did not mention it,
// found when `state get` was added to the same sentence. That is the second
// omission caught by the act of editing the line for the NEXT verb, which is
// the only mechanism that has ever caught one here.
//
// A doc comment that overstates what is missing is the kind of stale
// documentation that costs a reader nothing and a contributor everything: they
// reimplement what is already in the tree.
//
// Note that the measured figure did NOT move when the executor landed, because
// `run start` already counted as wired when only --sim worked. The number and
// this list answer different questions, which is why both are here: the count
// says how much of the surface is reachable, and this says what reaching it
// actually does.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

const version = "0.0.1-spec"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return
	case "--version", "version":
		fmt.Printf("arxi %s (surface v%d)\n", version, surface.SurfaceVersion)
		return
	case "schema":
		cmdSchema()
		return
	case "surface":
		cmdSurface(args[1:])
		return
	case "serve":
		cmdServe(args[1:])
		return
	case "why":
		cmdWhy(args[1:])
		return
	case "design":
		cmdDesign(args[1:])
		return
	case "blueprint":
		if len(args) > 1 && args[1] == "validate" {
			cmdBlueprintValidate(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "create" {
			cmdBlueprintCreate(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "install" {
			cmdBlueprintInstall(args[2:])
			return
		}
	case "run":
		if len(args) > 1 && args[1] == "start" {
			cmdRunStart(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "unpause" {
			cmdRunUnpause(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "pause" {
			cmdRunPause(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "list" {
			cmdRunList(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "show" {
			cmdRunShow(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "why" {
			cmdRunWhy(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "tree" {
			cmdRunTree(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "prompt" {
			cmdRunPrompt(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "steer" {
			cmdRunSteer(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "result" {
			cmdRunResult(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "cancel" {
			cmdRunCancel(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "fork" {
			cmdRunFork(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "replay" {
			cmdRunReplay(args[2:])
			return
		}
		if len(args) > 1 && args[1] == "attach" {
			cmdRunAttach(args[2:])
			return
		}
	case "trigger":
		cmdTrigger(args[1:])
		return
	case "event":
		cmdEvent(args[1:])
		return
	case "eval":
		cmdEval(args[1:])
		return
	case "provider":
		cmdProvider(args[1:])
		return
	case "model":
		cmdModel(args[1:])
		return
	case "inbox":
		cmdInbox(args[1:])
		return
	case "agent":
		cmdAgent(args[1:])
		return
	case "role":
		cmdRole(args[1:])
		return
	case "state":
		cmdState(args[1:])
		return
	}

	// Everything else: if it is declared, say so precisely. An "unknown command"
	// when the command DOES exist in the surface is the worst possible answer:
	// it sends the user hunting for a typo they never made.
	notImplemented(args)
}

// article picks "a" or "an" for a command name.
//
// Trivial, and it earns its place: hardcoding "an" produced "is not an model
// command" and "is not an run command" on five of the seven groups, which reads
// as carelessness in the one message whose whole job is to be trusted about what
// went wrong. Vowel-initial group names ("agent", "eval") are the minority, so
// the wrong constant was also the more visible one.
//
// The argument may be a multi-word prefix ("agent tool"), and only the first
// letter of the first word decides. That is what English does, and it was worth
// making explicit rather than leaving correct by accident: reading word[0] of
// the joined string gets "agent tool" right for the same reason it would get
// "an eval suite" right, which is not a reason at all.
func article(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "a"
	}
	switch name[0] {
	case 'a', 'e', 'i', 'o', 'u':
		return "an"
	}
	return "a"
}

// notImplemented answers for a command this binary does not run yet.
//
// Extracted from main's fallback when `trigger` acquired its own subcommand
// switch, which needed the identical answer for a declared-but-unbuilt trigger
// subcommand. Copying the block would have made the two drift, and the way they
// drift is the worse direction: a group that grew its own dispatcher starts
// reporting "unknown command" for capabilities `arxi surface` publishes.
func notImplemented(args []string) {
	for n := len(args); n >= 1; n-- {
		if c := surface.Lookup(args[:n]...); c != nil {
			fmt.Fprint(os.Stderr, notImplementedMessage(c))
			os.Exit(2)
		}
	}

	// Before calling the whole thing unknown, check whether the FIRST word is a
	// real group. `arxi model --help` used to answer "model --help does not
	// exist in the surface", naming the group -- which is false, since `model`
	// exists and three of its subcommands are built. The user was told the
	// opposite of the truth about the one word they got right.
	//
	// The two answers send the reader to different places: "no such command"
	// means check the spelling, this means the group is real and only the verb
	// is wrong. `trigger` and `eval` already answered this way from their own
	// dispatchers; doing it here fixes every group at once, and means the next
	// group to grow a dispatcher does not have to remember.
	// The LONGEST declared prefix is used, not the first word, because depth
	// varies. `agent tool` is declared only as part of `agent tool policy`, and
	// blaming word two there printed a message that contradicted itself in a
	// single breath: `"tool" is not an agent command` directly above `it
	// accepts: list, create, show, tool`. Naming the deepest prefix that really
	// exists puts the blame on the first word that is actually wrong.
	//
	// n runs from len(args) DOWN, and includes len(args) itself, which is the
	// case of a prefix that is real but incomplete: `arxi agent tool` names
	// nothing wrong at all, it just stops one word short. Telling that user
	// what `agent tool` accepts is the whole answer, and telling them "tool is
	// not an agent command" was worse than useless.
	for n := len(args); n >= 1; n-- {
		subs := surface.SubcommandsUnder(args[:n]...)
		if len(subs) == 0 {
			continue
		}
		prefix := strings.Join(args[:n], " ")

		// An incomplete path is a different sentence from a wrong one. There is
		// no bad word to quote, so quoting the empty string, or blaming a word
		// the user got right, would both be lies about what happened.
		if n == len(args) {
			fmt.Fprintf(os.Stderr,
				"arxi %s needs a subcommand.\n\n  it accepts: %s\n\n"+
					"See the whole surface: arxi surface\n",
				prefix, strings.Join(subs, ", "))
			os.Exit(2)
		}

		fmt.Fprintf(os.Stderr,
			"arxi %s: %q is not %s %s command.\n\n  it accepts: %s\n\n"+
				"See the whole surface: arxi surface\n",
			prefix, strings.Join(args[n:], " "), article(prefix),
			prefix, strings.Join(subs, ", "))
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "arxi: %q does not exist in the surface.\nTry: arxi surface\n",
		strings.Join(args, " "))
	os.Exit(2)
}

// notImplementedMessage is the wording of that answer, split out from the
// printing of it.
//
// The split is here for a test, and the test needed it because the surface is
// now fully built. Checking this wording used to mean running the binary
// against a path that really was unwired and reading what came back -- and
// `design` was the last one. At 50 of 50 there is no path left to point that
// check at, and a test that asserts a copy of the sentence instead is a test of
// its own copy: it passes forever while the sentence it is guarding drifts.
//
// So the check reads the string from the function that prints it, and stays
// exactly as strong as it was. What it can no longer prove on its own -- that
// main's fallback still arrives here at all -- is proved next to it, by a
// subcommand that is misspelled rather than unbuilt: only this function answers
// that, and it answers with a different sentence.
//
// The day surface v-next declares something before it is wired, both halves
// keep working, and neither has to be remembered.
func notImplementedMessage(c *surface.Cmd) string {
	return fmt.Sprintf(
		"arxi %s is declared in the surface but not implemented yet.\n\n"+
			"  description: %s\n  tool:        %s\n  protocol:    %s\n  since:       surface v%d\n\n"+
			"See the whole surface: arxi surface\n",
		c.CLI(), c.Desc, c.Name(), c.ProtocolType(), c.Since)
}

func usage() {
	fmt.Print(`arxi - agent systems you can actually debug

USAGE
  arxi <command> [args]

IMPLEMENTED TODAY
  schema                     emit the surface manifest (JSON)
  surface                    see the whole surface, human readable
  why <file>                 explain why a run is not advancing
  design                     compose a team on screen from the agents you have
  blueprint validate <file>  check a blueprint and print the resolved config
  blueprint create <name>    compose stored agents into a team: --members a,b
  blueprint install <ref>    install a blueprint from a path or https URL: --as
  provider add <name>        register a provider (--base-url, --api-key-env)
  model list                 see which models may be called, and their status
  run start <bp> <prompt>    run a blueprint file or a stored agent (or --sim)
  run result <run>           the recorded result, and an exit code to gate on
  run pause <run>            stop opening turns; a turn already open finishes
  run unpause <run>          pick a run back up (--budget raises the ceiling)
  run cancel <run>           end a run for good (--reason lands in the log)
  run fork <run>             branch a run into a new one at --at-seq
  run replay <run>           fold the log again, no executor, at --until-seq
  run attach <run>           follow a live run: the events from now on
  event log <run>            the log itself: what every other verb reads
  event trace <event>        the causal chain of one event, root first
  state set <run> <k> <v>    write a key the other members read
  state get <run> <key>      read it back; exit 3 if it is not set
  state lock <run> <key>     claim a key with a --ttl; exit 3 if it is held
  state unlock <run> <key>   hand it back; --force ends a live lease
  inbox                      questions an agent cannot continue without
  agent create <name>        store an agent: --model, --tools, --role
  agent list                 the agents in agents/, with model and tools
  agent show <name>          what 'run start <name>' will actually execute
  agent tool policy          stop being asked about a tool every turn
  role define <name>         defaults for 'agent create --role', copied once
  trigger list               schedules, and the loop that fires them
  eval run <suite>           suites, pass rates, and two runs side by side
  serve [--listen ADDR]      speak the NDJSON protocol; stdio without --listen
  version                    version of the binary and of the surface

SHORT FLAGS
  One letter means the same thing on every command that has that parameter:
  -b is --budget, -p is --prompt, -r is --run, -f is --path, -J is --json.
  A letter is an error on a command without that parameter, rather than being
  quietly ignored, because ignoring it discards the value and reports success.

  arxi surface --flags       the whole assignment, and what each letter reaches

Every capability 'arxi surface' declares is now wired to a command. This page is
the short list; 'arxi surface' is the whole one, and it names the protocol
message and the tool name for each. Something declared by a later surface version
and not built yet says so by name rather than calling itself unknown.

'run start' calls real models. It resolves each member's model against the
registered providers and charges --budget from the tokens they report, so a
first run wants 'provider add' before it. --sim drives the same reducer, log and
loop with no model calls, which is what makes a simulated log worth reading.
Tools and the inbox are still refused rather than faked, so a stage needing work
submitted goes quiet and names the member that owes it.

Its first argument is a blueprint file or the name of an agent from
'agent create'. A file that exists wins, so a stored agent can never shadow the
file you are editing; './name.yaml' says which one you meant either way.

DESIGN
  docs/design/10-execution.md   the execution model
  docs/adr/                     why each decision
  spec/events.md                the event catalogue
`)
}

func cmdSchema() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(surface.BuildManifest()); err != nil {
		fatal(err)
	}
}

// cmdSurface renders the SAME manifest as cmdSchema, in human format.
// Two views, one source: if they diverge it is a bug, not a product decision.
func cmdSurface(args []string) {
	// `--flags` prints the short-flag assignment from the same map the parser
	// reads. A shorthand nobody can list is not a shorthand: the user has to
	// find it in the source or guess, and guessing is how they discover that -b
	// is not what they assumed on this particular command.
	for _, a := range args {
		if a == "--flags" || a == "-flags" {
			printShortFlags()
			return
		}
	}

	m := surface.BuildManifest()
	fmt.Printf("surface v%d · %d capabilities exposed to agents\n", m.SurfaceVersion, len(m.Tools))

	group := ""
	for _, c := range surface.Registry {
		if g := c.Path[0]; g != group {
			group = g
			fmt.Printf("\n%s\n", strings.ToUpper(group))
		}
		var marks []string
		if c.Kind&surface.AgentTool != 0 {
			marks = append(marks, "tool")
		}
		if c.Kind&surface.Protocol != 0 {
			marks = append(marks, "proto")
		}
		if c.Mutates {
			marks = append(marks, "mutates")
		}
		if c.ToolPolicy != "" {
			marks = append(marks, string(c.ToolPolicy))
		}
		fmt.Printf("  %-28s %-46s %s\n", c.CLI(), c.Desc, strings.Join(marks, ","))
	}
	fmt.Printf("\nTotal declared (includes CLI-only): %d\n", len(surface.Registry))
}

// cmdWhy reads a state from JSON and explains it. That this works with no
// executor is the proof that the reducer is genuinely pure: `run why` needs
// nothing from the runtime, only the state that came out of the fold.
func cmdWhy(args []string) {
	// `why` is the CLI spelling of `run why`, so it inherits that command's
	// short flags: -J for --json. Looking it up rather than hardcoding "-J" is
	// what keeps this from becoming a place where the letter could differ.
	args, err := expandShort(surface.Lookup("run", "why"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi why: %v\n", err)
		os.Exit(2)
	}

	asJSON := false
	var path string
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else if !strings.HasPrefix(a, "-") {
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: arxi why <run|file.json> [--json]\n\n"+
			"A run id, or a file holding {\"state\":..., \"config\":...} or a bare State.\n"+
			"short: -J json\n"+
			"Try: testdata/scenarios/blocked-on-approval.json")
		os.Exit(2)
	}

	// A run id is accepted here too, and this is a fix rather than a feature.
	// Every message in this binary that mentions why prints a RUN ID after it,
	// so the argument a user arrives with is far more often an id than a path.
	// This spelling used to answer "open <id>: no such file or directory",
	// which describes somebody's stuck run as a missing file and sends them
	// looking for one. whySubject tries the run first and reports both
	// readings when neither works.
	st, cfg, err := whySubject(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi why: %v\n", err)
		os.Exit(1)
	}

	emitWhy(kernel.Explain(st, cfg), asJSON)
}

// onExit holds work that must happen before the process dies.
//
// It exists because os.Exit does not run deferred calls, and the thing most
// often deferred in this binary is releasing a run's writer lock. Measured, not
// theorised: `run unpause` on a run with an unparseable policy file appended
// run.unpaused, then fataled out of openPolicies with the lock held. writer.lock
// stayed on disk holding pid 5871, and the next command on that run refused with
//
//	run directory runs/... is already open for writing by pid 5871
//	... remove runs/.../writer.lock by hand after confirming no process is running
//
// for a run that had merely hit a bad config file. That advice is also dangerous
// to generalise: an operator who learns to delete writer.lock after a crash will
// eventually delete a live one.
//
// A registry here rather than a store.Close() before each os.Exit, because there
// were four exit paths under one lock in `run unpause` alone (two fatals, an
// append error, and the stopped-early branch) and the next command added will
// have its own. Fixing them one at a time is how the second one gets missed.
//
// Only a hard kill can still strand a lock, and nothing in a process can help
// with that -- which is what the manual remedy in the message is actually for.
var onExit []func()

// atExit registers cleanup to run before fatal or exitWith terminates.
func atExit(f func()) { onExit = append(onExit, f) }

// runExitHooks runs the registered cleanups, most recent first.
//
// Reverse order for the same reason defer uses it: later work was set up on top
// of earlier work, so unwinding the other way round can release something that
// is still in use.
func runExitHooks() {
	for i := len(onExit) - 1; i >= 0; i-- {
		onExit[i]()
	}
	onExit = nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "arxi:", err)
	runExitHooks()
	os.Exit(1)
}

// printShortFlags lists the short flags and the commands each one reaches.
//
// The commands are computed, not written down. A hand-kept list here would be
// the third copy of the same fact — after the registry and the parser — and the
// one nobody updates, so the help would confidently name a command whose
// parameter had been renamed.
func printShortFlags() {
	all := surface.ShortFlags()
	fmt.Printf("surface v%d · %d short flags, each meaning the same thing everywhere\n\n",
		surface.SurfaceVersion, len(all))

	for _, pp := range all {
		letter, name := pp.Desc, pp.Name
		var users []string
		for _, c := range surface.Registry {
			if c.LongFor(letter) != "" {
				users = append(users, c.CLI())
			}
		}
		where := fmt.Sprintf("%d commands", len(users))
		if len(users) <= 3 {
			where = strings.Join(users, ", ")
		}
		fmt.Printf("  -%-2s --%-14s %s\n", letter, name, where)
	}

	fmt.Print("\nA letter is only valid on a command that HAS that parameter: -r is\n" +
		"--run on the commands that take a run id and an error on the rest,\n" +
		"because binding it to something else would discard the value silently.\n" +
		"Booleans can be grouped (-SJ); flags that take a value cannot.\n" +
		"The NDJSON protocol has no short flags: a machine saves nothing by them.\n")
}
