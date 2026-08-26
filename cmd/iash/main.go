// Command iash es el binario.
//
// Hoy implementa tres comandos de verdad (schema, surface, why) y para todo lo
// demás responde "declarado pero no implementado" con el nombre exacto de la
// capacidad. Eso es a propósito: la superficie está congelada y verificada por
// tests ANTES de que exista el ejecutor, así que agregar un comando nuevo es
// implementar algo que ya estaba prometido, no inventar una promesa nueva.
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
		fmt.Printf("iash %s (superficie v%d)\n", version, surface.SurfaceVersion)
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

	// Todo lo demás: si está declarado, decirlo con precisión. Un "comando
	// desconocido" cuando el comando SÍ existe en la superficie es la peor
	// respuesta posible: manda al usuario a buscar un typo que no cometió.
	for n := len(args); n >= 1; n-- {
		if c := surface.Lookup(args[:n]...); c != nil {
			fmt.Fprintf(os.Stderr,
				"iash %s está declarado en la superficie pero todavía no implementado.\n\n"+
					"  descripción: %s\n  tool:        %s\n  protocolo:   %s\n  desde:       superficie v%d\n\n"+
					"Ver la superficie completa: iash surface\n",
				c.CLI(), c.Desc, c.Name(), c.ProtocolType(), c.Since)
			os.Exit(2)
		}
	}

	fmt.Fprintf(os.Stderr, "iash: %q no existe en la superficie.\nProbá: iash surface\n",
		strings.Join(args, " "))
	os.Exit(2)
}

func usage() {
	fmt.Print(`iash - sistemas de agentes que se pueden depurar

USO
  iash <comando> [args]

IMPLEMENTADO HOY
  schema            emitir el manifiesto de la superficie (JSON)
  surface           ver la superficie completa, legible
  why <archivo>     explicar por qué un run no avanza
  version           versión del binario y de la superficie

El resto de la superficie está declarada y verificada por tests, pero todavía
sin ejecutor. 'iash surface' lista todo lo que va a existir.

DISEÑO
  docs/design/10-ejecucion.md   el modelo de ejecución
  docs/adr/                     por qué cada decisión
  spec/events.md                el catálogo de eventos
`)
}

func cmdSchema() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(surface.BuildManifest()); err != nil {
		fatal(err)
	}
}

// cmdSurface renderiza el MISMO manifiesto que cmdSchema, en formato humano.
// Dos vistas, una fuente: si divergen es un bug, no una decisión de producto.
func cmdSurface() {
	m := surface.BuildManifest()
	fmt.Printf("superficie v%d · %d capacidades expuestas a agentes\n", m.SurfaceVersion, len(m.Tools))

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
	fmt.Printf("\nTotal declarado (incluye CLI-only): %d\n", len(surface.Registry))
}

// cmdWhy lee un estado desde JSON y lo explica. Que esto funcione sin ejecutor
// es la prueba de que el reducer es puro de verdad: `run why` no necesita nada
// del runtime, solo el estado que salió del fold.
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
		fmt.Fprintln(os.Stderr, "uso: iash why <archivo.json> [--json]\n\n"+
			"El archivo puede ser {\"state\":..., \"config\":...} o un State suelto.\n"+
			"Probá con: testdata/scenarios/blocked-on-approval.json")
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
		fmt.Println("\nposibles remedios:")
		for _, f := range w.Fix {
			fmt.Printf("  $ %s\n", f)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "iash:", err)
	os.Exit(1)
}
