// Package internal_test verifies the architectural boundaries with the compiler
// as a witness, not with the good will of whoever writes the next commit.
//
// This is the concrete answer to the concession made in ADR-0007: Rust
// guarantees more things in the type system, but Go lets us inspect the import
// graph with `go list`, and that is a guarantee Rust does NOT give by default.
// If the kernel imports net/http, this test fails and explains why it is wrong.
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
			t.Skip("go is not in the PATH")
		}
		// If go does exist and still fails, it is a real error: skipping here
		// would turn this test into decoration.
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var p pkgInfo
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ownClosure returns only the PROJECT packages in the dependency closure.
//
// This function exists because of a real false positive: the first version used
// the full `go list -deps`, and the closure of `fmt` includes os, time, syscall
// and half the runtime. The test failed claiming the kernel imported `os` when
// all it did was import `fmt`. What matters is what the package imports
// DIRECTLY, and which of our own packages it drags along.
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

// forbidden maps every import banned in the kernel to its reason. The reason
// goes into the error message because a test that says "do not import time"
// without explaining why ends up silenced with a //nolint.
var forbidden = map[string]string{
	"time":         "time.Now() inside the reducer breaks replay and the virtual clock of --sim",
	"net":          "the reducer talks to nobody: it returns effects and somebody else runs them",
	"net/http":     "the reducer talks to nobody: it returns effects and somebody else runs them",
	"os":           "reading env or files makes the same log yield different states depending on the machine",
	"os/exec":      "running processes is an effect, and effects are returned, not performed",
	"math/rand":    "randomness without an explicit seed makes the fold irreproducible",
	"crypto/rand":  "randomness without an explicit seed makes the fold irreproducible",
	"database/sql": "persistence belongs to the executor; state is derived from the log",
	"io":           "I/O belongs to the executor by definition",
	"bufio":        "I/O belongs to the executor by definition",
}

func TestKernelIsPure(t *testing.T) {
	for _, p := range ownClosure(t, mod+"internal/kernel") {
		for _, imp := range p.Imports {
			if why, bad := forbidden[imp]; bad {
				t.Errorf("%s imports %q.\n  why this is wrong: %s\n"+
					"  what to do: return an Effect describing the operation and "+
					"let the executor perform it (see docs/design/10-execution.md §10.1)",
					p.ImportPath, imp, why)
			}
		}
	}
}

func TestKernelDoesNotImportOtherLayers(t *testing.T) {
	p := list(t, mod+"internal/kernel")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod) {
			t.Errorf("the kernel depends on %s.\n"+
				"The kernel is the bottom layer: it cannot know anybody else exists. "+
				"If it needs a piece of data, it is handed to it through Config or Event.", d)
		}
	}
}

func TestSurfaceDoesNotImportTheExecutor(t *testing.T) {
	p := list(t, mod+"internal/surface")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod+"internal/exec") {
			t.Errorf("the surface imports %s.\n"+
				"The surface is a declaration of capabilities: it describes what "+
				"can be done, not how. If it imports the executor, the manifest and "+
				"the docs can no longer be generated without dragging in the whole runtime.", d)
		}
	}
}
