package surface

import (
	"strings"
	"testing"
)

// TestUnaSolaSuperficie verifica que las tres proyecciones salgan de la misma
// declaración. Si alguien alguna vez agrega una tabla de traducción a mano,
// este test es el que se lo va a decir.
func TestUnaSolaSuperficie(t *testing.T) {
	c := Cmd{Path: []string{"run", "start"}}
	if got := c.CLI(); got != "run start" {
		t.Errorf("CLI = %q", got)
	}
	if got := c.Name(); got != "iash_run_start" {
		t.Errorf("tool = %q", got)
	}
	if got := c.ProtocolType(); got != "run.start" {
		t.Errorf("protocolo = %q", got)
	}
}

// TestNoExisteAbort: el vocabulario tiene que ser uno.
//
// La CLI dice `cancel`, así que el mensaje del protocolo es `run.cancel`. Si
// existiera un `abort` en algún lado, habría dos palabras para lo mismo y
// alguien tendría que mantener el mapeo entre ambas para siempre.
func TestNoExisteAbort(t *testing.T) {
	for _, c := range Registry {
		for _, seg := range c.Path {
			if seg == "abort" || seg == "kill" || seg == "stop" {
				t.Errorf("%s usa %q; el verbo canónico es cancel", c.CLI(), seg)
			}
		}
	}
	if Lookup("run", "cancel") == nil {
		t.Error("falta run cancel")
	}
}

// TestNoExisteSchedule: cron es UNA fuente de disparo, no la categoría.
// Llamar a la familia `schedule` haría que webhook: y file: parezcan features
// de segunda clase pegadas con cinta.
func TestNoExisteSchedule(t *testing.T) {
	for _, c := range Registry {
		if c.Path[0] == "schedule" || c.Path[0] == "cron" {
			t.Errorf("%s: la familia se llama trigger", c.CLI())
		}
	}
	tc := Lookup("trigger", "create")
	if tc == nil {
		t.Fatal("falta trigger create")
	}
	var on *Param
	for i := range tc.Params {
		if tc.Params[i].Name == "on" {
			on = &tc.Params[i]
		}
	}
	if on == nil {
		t.Fatal("trigger create no declara --on")
	}
	for _, want := range []string{"cron:", "webhook:", "file:", "event:"} {
		if !strings.Contains(on.Desc, want) {
			t.Errorf("--on no documenta la fuente %q: %s", want, on.Desc)
		}
	}
}

// TestNamespaceUnico: no hay `run team` ni `run agent`.
//
// Un equipo es un agente con otro kind. Dos verbos obligarían al usuario a
// saber de qué tipo es una cosa antes de poder ejecutarla, y a nosotros a
// duplicar cada feature de run para ambos.
func TestNamespaceUnico(t *testing.T) {
	for _, c := range Registry {
		if len(c.Path) >= 2 && c.Path[0] == "run" {
			if c.Path[1] == "team" || c.Path[1] == "agent" {
				t.Errorf("%s: un solo namespace, `run <actor>`", c.CLI())
			}
		}
	}
	rs := Lookup("run", "start")
	if rs == nil {
		t.Fatal("falta run start")
	}
	if rs.Params[0].Name != "actor" {
		t.Errorf("run start recibe %q, debería recibir 'actor'", rs.Params[0].Name)
	}
}

// TestPresupuestoObligatorio: nada que gaste dinero arranca sin techo.
//
// Un default invisible es una factura sorpresa. Que el usuario tenga que
// escribir el número es la única forma de que sepa que el techo existe.
func TestPresupuestoObligatorio(t *testing.T) {
	mustRequire := func(cmd *Cmd, param string) {
		t.Helper()
		if cmd == nil {
			t.Fatalf("comando ausente (buscando --%s)", param)
		}
		for _, p := range cmd.Params {
			if p.Name == param {
				if !p.Required {
					t.Errorf("%s: --%s tiene que ser obligatorio", cmd.CLI(), param)
				}
				if p.Default != "" {
					t.Errorf("%s: --%s no puede tener default (%q): un techo invisible es una factura sorpresa",
						cmd.CLI(), param, p.Default)
				}
				return
			}
		}
		t.Errorf("%s no declara --%s", cmd.CLI(), param)
	}

	mustRequire(Lookup("run", "start"), "budget")
	mustRequire(Lookup("trigger", "create"), "budget")
	// El período es tan importante como el monto: "10 dólares" sin período es
	// una suscripción abierta.
	mustRequire(Lookup("trigger", "create"), "budget-period")
	mustRequire(Lookup("eval", "run"), "budget")
}

// TestToolsMutantesNoSonAllowPorDefault: si un agente puede cambiar el mundo
// sin preguntar, tiene que ser una decisión escrita, no un descuido.
func TestToolsMutantesNoSonAllowPorDefault(t *testing.T) {
	// Estas tres son allow a propósito: son el mecanismo de coordinación entre
	// agentes y están acotadas al run. Sin ellas los agentes no pueden
	// colaborar y la herramienta no tiene razón de existir.
	esperadas := map[string]bool{
		"iash_state_set":  true,
		"iash_event_emit": true,
		"iash_state_lock": true,
	}
	for _, td := range BuildManifest().Tools {
		if td.Mutates && td.Policy == PolicyAllow && !esperadas[td.Name] {
			t.Errorf("%s muta el mundo y es allow por defecto.\n"+
				"Si es intencional, agregala a la lista de excepciones de este "+
				"test con el motivo; si no, ponele ToolPolicy: PolicyAsk.", td.Name)
		}
	}
}

// TestLecturaSiempreTieneJSON: una salida legible por máquina que existe "en la
// mayoría" de los comandos no sirve para automatizar nada.
func TestLecturaSiempreTieneJSON(t *testing.T) {
	for _, td := range BuildManifest().Tools {
		if td.Mutates {
			continue
		}
		props, _ := td.InputSchema["properties"].(map[string]any)
		if props["json"] == nil {
			t.Errorf("%s lee pero no acepta --json", td.Name)
		}
	}
}

// TestManifiestoVersionadoUnico: un número para la superficie entera, no un @v1
// por herramienta. La granularidad por herramienta suena mejor y obliga a cada
// cliente a negociar versión 30 veces.
func TestManifiestoVersionadoUnico(t *testing.T) {
	m := BuildManifest()
	if m.SurfaceVersion != SurfaceVersion {
		t.Errorf("surface_version = %d", m.SurfaceVersion)
	}
	if len(m.Tools) == 0 {
		t.Fatal("manifiesto vacío")
	}
	for _, td := range m.Tools {
		if strings.Contains(td.Name, "@") {
			t.Errorf("%s versiona por herramienta; la versión es de la superficie", td.Name)
		}
		if td.Since == 0 {
			t.Errorf("%s no declara since: un cliente no puede saber si puede usarla", td.Name)
		}
	}
}

// TestScopeUniforme: todo lo que se expone al protocolo tiene tipo derivable.
func TestScopeUniforme(t *testing.T) {
	for _, c := range Registry {
		if c.Kind&Protocol == 0 {
			continue
		}
		if c.ProtocolType() == "" {
			t.Errorf("%s se expone al protocolo sin tipo", c.CLI())
		}
		if strings.Contains(c.ProtocolType(), " ") {
			t.Errorf("tipo de protocolo con espacio: %q", c.ProtocolType())
		}
	}
}

// TestRunWhyExiste: es el comando que justifica el log estructurado. Si no
// existe, el log es solo un archivo grande.
func TestRunWhyExiste(t *testing.T) {
	c := Lookup("run", "why")
	if c == nil {
		t.Fatal("falta run why")
	}
	if c.Kind&AgentTool == 0 {
		t.Error("run why tiene que ser una tool: un coordinador necesita diagnosticar a sus subordinados")
	}
	if c.Mutates {
		t.Error("run why no debería mutar nada")
	}
}

// TestSinDuplicados: dos entradas con el mismo path serían dos herramientas con
// el mismo nombre, y el cliente elegiría una al azar.
func TestSinDuplicados(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Registry {
		if seen[c.CLI()] {
			t.Errorf("path duplicado: %s", c.CLI())
		}
		seen[c.CLI()] = true
	}
}
