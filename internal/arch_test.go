// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
package internal_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/michiTrader/iash/"

type pkgInfo struct {
	ImportPath string
	Imports    []string
	Deps       []string
}

func list(t *testing.T, pkg string) pkgInfo {
	t.Helper()
	out, err := exec.Command("go", "list", "-json", pkg).Output()
	if err != nil {
		if _, err2 := exec.LookPath("go"); err2 != nil {
			t.Skip("go not is en the PATH")
		}
		// Implementation note.
		// Implementation note.
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var p pkgInfo
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func ownClosure(t *testing.T, pkg string) []pkgInfo {
	t.Helper()
	root := list(t, pkg)
	seen := map[string]bool{root.ImportPath: true}
	out := []pkgInfo{root}
	for _, d := range root.Deps {
		if strings.HasPrefix(d, mod) && !seen[d] {
			seen[d] = true
			out = append(out, list(t, d))
		}
	}
	return out
}

// Implementation note.
// Implementation note.
// Implementation note.
var forbidden = map[string]string{
	"time":         "time.Now() inside of the reducer breaks replay and the clock virtual of --sim",
	"net":          "the reducer not habla with nadie: returns effects and another the executes",
	"net/http":     "the reducer not habla with nadie: returns effects and another the executes",
	"os":           "read env or files makes that the same log dé states distintos according to the máquina",
	"os/exec":      "ejecutar procesos is a effect, and the effects the returns, not the makes",
	"math/rand":    "aleatoriedad without semilla explícita makes the fold irreproducible",
	"crypto/rand":  "aleatoriedad without semilla explícita makes the fold irreproducible",
	"database/sql": "the persistencia is of the executor; the state is derives of the log",
	"io":           "I/O is of the executor for definición",
	"bufio":        "I/O is of the executor for definición",
}

func TestKernelEsPuro(t *testing.T) {
	for _, p := range ownClosure(t, mod+"internal/kernel") {
		for _, imp := range p.Imports {
			if why, bad := forbidden[imp]; bad {
				t.Errorf("%s importa %q.\n  for what is badly: %s\n"+
					"  what make: devolvé a Effect that describa the operación and "+
					"that the executor the haga (see docs/design/10-execution.md §10.1)",
					p.ImportPath, imp, why)
			}
		}
	}
}

func TestKernelNoImportaOtrasCapas(t *testing.T) {
	p := list(t, mod+"internal/kernel")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod) {
			t.Errorf("the kernel depends of %s.\n"+
				"The kernel is the capa of abajo: not can know that exists nadie more. "+
				"Si needs a dato, is lo pasan for Config or for Event.", d)
		}
	}
}

func TestSurfaceNoImportaElEjecutor(t *testing.T) {
	p := list(t, mod+"internal/surface")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod+"internal/exec") {
			t.Errorf("the surface importa %s.\n"+
				"The surface is a declaración of capabilities: describe what is "+
				"can make, not how. Si importa to the executor, dejan of poder "+
				"generarse the manifest and the docs without arrastrar everything the runtime.", d)
		}
	}
}
