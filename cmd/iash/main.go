// Command iash is the binary.
//
// Today it implements schema, surface, why, blueprint validate, run start
// (--sim only) and serve; for everything else it answers "declared but not
// implemented" with the exact name of the capability. That is on purpose: the
// surface is frozen and verified by tests BEFORE the executor exists, so adding
// a new command is implementing something that was already promised, not
// inventing a new promise.
//
// This paragraph has already been wrong once — it claimed three commands after
// six existed. A doc comment that overstates what is missing is the kind of stale
// documentation that costs a reader nothing and a contributor everything: they
// reimplement what is already in the tree.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/michiTrader/iash/internal/kernel"
	"github.com/michiTrader/iash/internal/surface"
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
		fmt.Printf("iash %s (surface v%d)\n", version, surface.SurfaceVersion)
		return
	case "schema":
		cmdSchema()
		return
	case "surface":
		cmdSurface()
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
	}

	// Everything else: if it is declared, say so precisely. An "unknown command"
	// when the command DOES exist in the surface is the worst possible answer:
	// it sends the user hunting for a typo they never made.
	for n := len(args); n >= 1; n-- {
		if c := surface.Lookup(args[:n]...); c != nil {
			fmt.Fprintf(os.Stderr,
				"iash %s is declared in the surface but not implemented yet.\n\n"+
					"  description: %s\n  tool:        %s\n  protocol:    %s\n  since:       surface v%d\n\n"+
					"See the whole surface: iash surface\n",
				c.CLI(), c.Desc, c.Name(), c.ProtocolType(), c.Since)
			os.Exit(2)
		}
	}

	fmt.Fprintf(os.Stderr, "iash: %q does not exist in the surface.\nTry: iash surface\n",
		strings.Join(args, " "))
	os.Exit(2)
}

func usage() {
	fmt.Print(`iash - agent systems you can actually debug

USAGE
  iash <command> [args]

IMPLEMENTED TODAY
  schema                     emit the surface manifest (JSON)
  surface                    see the whole surface, human readable
  why <file>                 explain why a run is not advancing
  blueprint validate <file>  check a blueprint and print the resolved config
  run start <bp> <prompt>    run a blueprint to completion (--sim only today)
  serve [--listen ADDR]      speak the NDJSON protocol; stdio without --listen
  version                    version of the binary and of the surface

The rest of the surface is declared and verified by tests, but has no executor
yet. 'iash surface' lists everything that is going to exist.

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
func cmdSurface() {
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
		fmt.Fprintln(os.Stderr, "usage: iash why <file.json> [--json]\n\n"+
			"The file can be {\"state\":..., \"config\":...} or a bare State.\n"+
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
	fmt.Fprintln(os.Stderr, "iash:", err)
	os.Exit(1)
}
