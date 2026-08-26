// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
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
	case "why":
		cmdWhy(args[1:])
		return
	}

	// Implementation note.
	// Implementation note.
	// Implementation note.
	for n := len(args); n >= 1; n-- {
		if c := surface.Lookup(args[:n]...); c != nil {
			fmt.Fprintf(os.Stderr,
				"iash %s is declared in the surface but not yet implemented.\n\n"+
					"  description: %s\n  tool:        %s\n  protocol:    %s\n  since:       surface v%d\n\n"+
					"See the complete surface: iash surface\n",
				c.CLI(), c.Desc, c.Name(), c.ProtocolType(), c.Since)
			os.Exit(2)
		}
	}

	fmt.Fprintf(os.Stderr, "iash: %q does not exist in the surface.\nTry: iash surface\n",
		strings.Join(args, " "))
	os.Exit(2)
}

func usage() {
	fmt.Print(`iash - systems of agentes that is can depurar

USO
  iash <command> [args]

IMPLEMENTADO HOY
  schema            emitir the manifest of the surface (JSON)
  surface           see the surface complete, readable
  why <file>     explicar for what a run not advances
  version           version of the binario and of the surface

The rest of the surface is declared and verificada for tests, pero still
without executor. 'iash surface' list everything lo that va a existir.

DISEÑO
  docs/design/10-execution.md   the model of execution
  docs/adr/                     for what each decision
  spec/events.md                the catálogo of events
`)
}

func cmdSchema() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(surface.BuildManifest()); err != nil {
		fatal(err)
	}
}

// Implementation note.
// Implementation note.
func cmdSurface() {
	m := surface.BuildManifest()
	fmt.Printf("surface v%d · %d capabilities expuestas a agentes\n", m.SurfaceVersion, len(m.Tools))

	grupo := ""
	for _, c := range surface.Registry {
		if g := c.Path[0]; g != grupo {
			grupo = g
			fmt.Printf("\n%s\n", strings.ToUpper(grupo))
		}
		var marcas []string
		if c.Kind&surface.AgentTool != 0 {
			marcas = append(marcas, "tool")
		}
		if c.Kind&surface.Protocol != 0 {
			marcas = append(marcas, "proto")
		}
		if c.Mutates {
			marcas = append(marcas, "muta")
		}
		if c.ToolPolicy != "" {
			marcas = append(marcas, string(c.ToolPolicy))
		}
		fmt.Printf("  %-28s %-46s %s\n", c.CLI(), c.Desc, strings.Join(marcas, ","))
	}
	fmt.Printf("\nTotal declared (incluye CLI-only): %d\n", len(surface.Registry))
}

// Implementation note.
// Implementation note.
// Implementation note.
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
		fmt.Fprintln(os.Stderr, "uso: iash why <file.json> [--json]\n\n"+
			"The file can ser {\"state\":..., \"config\":...} or a State suelto.\n"+
			"Try with: testdata/scenarios/blocked-on-approval.json")
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
		fmt.Println("\nposibles remedies:")
		for _, f := range w.Fix {
			fmt.Printf("  $ %s\n", f)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "iash:", err)
	os.Exit(1)
}
