// Package internal_test verifica los límites arquitectónicos con el compilador
// como testigo, no con la buena voluntad de quien escribe el próximo commit.
//
// Esto es la respuesta concreta a la concesión de ADR-0007: Rust garantiza más
// cosas en el tipo, pero Go permite verificar el grafo de imports con `go list`
// y eso es una garantía que Rust NO da por defecto. Si el kernel importa
// net/http, este test falla y explica por qué está mal.
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
			t.Skip("go no está en el PATH")
		}
		// Si go existe y falla, es un error de verdad: saltear acá sería
		// convertir el test en decorativo.
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var p pkgInfo
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ownClosure devuelve solo los paquetes DEL PROYECTO en la clausura de deps.
//
// Esta función existe por un falso positivo real: la primera versión usaba
// `go list -deps` completo, y la clausura de `fmt` incluye os, time, syscall y
// medio runtime. El test fallaba diciendo que el kernel importaba `os` cuando
// lo único que hacía era importar `fmt`. Lo que importa es qué importa el
// paquete DIRECTAMENTE, y qué paquetes nuestros arrastra.
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

// forbidden mapea cada import prohibido en el kernel a la razón. La razón va en
// el mensaje de error porque un test que dice "no importes time" sin explicar
// por qué se termina silenciando con un //nolint.
var forbidden = map[string]string{
	"time":         "time.Now() dentro del reducer rompe replay y el reloj virtual de --sim",
	"net":          "el reducer no habla con nadie: devuelve efectos y otro los ejecuta",
	"net/http":     "el reducer no habla con nadie: devuelve efectos y otro los ejecuta",
	"os":           "leer env o archivos hace que el mismo log dé estados distintos según la máquina",
	"os/exec":      "ejecutar procesos es un efecto, y los efectos los devuelve, no los hace",
	"math/rand":    "aleatoriedad sin semilla explícita hace el fold irreproducible",
	"crypto/rand":  "aleatoriedad sin semilla explícita hace el fold irreproducible",
	"database/sql": "la persistencia es del ejecutor; el estado se deriva del log",
	"io":           "I/O es del ejecutor por definición",
	"bufio":        "I/O es del ejecutor por definición",
}

func TestKernelEsPuro(t *testing.T) {
	for _, p := range ownClosure(t, mod+"internal/kernel") {
		for _, imp := range p.Imports {
			if why, bad := forbidden[imp]; bad {
				t.Errorf("%s importa %q.\n  por qué está mal: %s\n"+
					"  qué hacer: devolvé un Effect que describa la operación y "+
					"que el ejecutor la haga (ver docs/design/10-ejecucion.md §10.1)",
					p.ImportPath, imp, why)
			}
		}
	}
}

func TestKernelNoImportaOtrasCapas(t *testing.T) {
	p := list(t, mod+"internal/kernel")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod) {
			t.Errorf("el kernel depende de %s.\n"+
				"El kernel es la capa de abajo: no puede saber que existe nadie más. "+
				"Si necesita un dato, se lo pasan por Config o por Event.", d)
		}
	}
}

func TestSurfaceNoImportaElEjecutor(t *testing.T) {
	p := list(t, mod+"internal/surface")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod+"internal/exec") {
			t.Errorf("la superficie importa %s.\n"+
				"La superficie es una declaración de capacidades: describe qué se "+
				"puede hacer, no cómo. Si importa al ejecutor, dejan de poder "+
				"generarse el manifiesto y los docs sin arrastrar todo el runtime.", d)
		}
	}
}
