// Command arxi is the binary.
//
// Today it implements schema, surface, why, blueprint validate, run start
// (live, calling real models, or --sim), serve, the trigger group, the eval
// group (--sim only), provider add and the
// model group; for everything else it answers "declared but not implemented"
// with the exact name of the capability. That is on purpose: the surface is
// frozen and verified by tests BEFORE the executor exists, so adding a new
// command is implementing something that was already promised, not inventing a
// new promise.
//
// The measured figure, rather than this list, is the one to trust: 16 of 47
// declared capabilities are wired. See the README, where a probe that walks the
// registry against the built binary produces it.
//
// This paragraph has already been wrong twice. It claimed three commands after
// six existed, and it said `run start` was --sim only after the live executor
// had landed. A doc comment that overstates what is missing is the kind of stale
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
	case "blueprint":
		if len(args) > 1 && args[1] == "validate" {
			cmdBlueprintValidate(args[2:])
			return
		}
	case "run":
		if len(args) > 1 && args[1] == "start" {
			cmdRunStart(args[2:])
			return
		}
	case "trigger":
		cmdTrigger(args[1:])
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
	}

	// Everything else: if it is declared, say so precisely. An "unknown command"
	// when the command DOES exist in the surface is the worst possible answer:
	// it sends the user hunting for a typo they never made.
	notImplemented(args)
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
			fmt.Fprintf(os.Stderr,
				"arxi %s is declared in the surface but not implemented yet.\n\n"+
					"  description: %s\n  tool:        %s\n  protocol:    %s\n  since:       surface v%d\n\n"+
					"See the whole surface: arxi surface\n",
				c.CLI(), c.Desc, c.Name(), c.ProtocolType(), c.Since)
			os.Exit(2)
		}
	}

	fmt.Fprintf(os.Stderr, "arxi: %q does not exist in the surface.\nTry: arxi surface\n",
		strings.Join(args, " "))
	os.Exit(2)
}

func usage() {
	fmt.Print(`arxi - agent systems you can actually debug

USAGE
  arxi <command> [args]

IMPLEMENTED TODAY
  schema                     emit the surface manifest (JSON)
  surface                    see the whole surface, human readable
  why <file>                 explain why a run is not advancing
  blueprint validate <file>  check a blueprint and print the resolved config
  run start <bp> <prompt>    run a blueprint to completion (--sim only today)
  serve [--listen ADDR]      speak the NDJSON protocol; stdio without --listen
  version                    version of the binary and of the surface

SHORT FLAGS
  One letter means the same thing on every command that has that parameter:
  -b is --budget, -p is --prompt, -r is --run, -f is --path, -J is --json.
  A letter is an error on a command without that parameter, rather than being
  quietly ignored, because ignoring it discards the value and reports success.

  arxi surface --flags       the whole assignment, and what each letter reaches

The rest of the surface is declared and verified by tests, but has no executor
yet. 'arxi surface' lists everything that is going to exist.

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
		fmt.Fprintln(os.Stderr, "usage: arxi why <file.json> [--json]\n\n"+
			"The file can be {\"state\":..., \"config\":...} or a bare State.\n"+
			"short: -J json\n"+
			"Try: testdata/scenarios/blocked-on-approval.json")
		os.Exit(2)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}

	var wrap struct {
		State  *kernel.State  `json:"state"`
		Config *kernel.Config `json:"config"`
	}
	var st kernel.State
	var cfg kernel.Config
	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.State != nil {
		st = *wrap.State
		if wrap.Config != nil {
			cfg = *wrap.Config
		}
	} else if err := json.Unmarshal(raw, &st); err != nil {
		fatal(err)
	}

	w := kernel.Explain(st, cfg)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(w); err != nil {
			fatal(err)
		}
		return
	}

	for _, l := range w.Lines {
		if l.Depth == 0 {
			fmt.Println(l.Text)
			continue
		}
		fmt.Printf("%s└─ %s\n", strings.Repeat("   ", l.Depth-1), l.Text)
	}
	if len(w.Fix) > 0 {
		fmt.Println("\npossible remedies:")
		for _, f := range w.Fix {
			fmt.Printf("  $ %s\n", f)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "arxi:", err)
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
