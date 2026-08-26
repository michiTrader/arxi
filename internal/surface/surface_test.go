package surface

import (
	"strings"
	"testing"
)

// Implementation note.
// Implementation note.
// Implementation note.
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

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func TestNoExisteAbort(t *testing.T) {
	for _, c := range Registry {
		for _, seg := range c.Path {
			if seg == "abort" || seg == "kill" || seg == "stop" {
				t.Errorf("%s uses %q; the verbo canónico is cancel", c.CLI(), seg)
			}
		}
	}
	if Lookup("run", "cancel") == nil {
		t.Error("missing run cancel")
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestNoExisteSchedule(t *testing.T) {
	for _, c := range Registry {
		if c.Path[0] == "schedule" || c.Path[0] == "cron" {
			t.Errorf("%s: the familia is llama trigger", c.CLI())
		}
	}
	tc := Lookup("trigger", "create")
	if tc == nil {
		t.Fatal("missing trigger create")
	}
	var on *Param
	for i := range tc.Params {
		if tc.Params[i].Name == "on" {
			on = &tc.Params[i]
		}
	}
	if on == nil {
		t.Fatal("trigger create not declara --on")
	}
	for _, want := range []string{"cron:", "webhook:", "file:", "event:"} {
		if !strings.Contains(on.Desc, want) {
			t.Errorf("--on not documenta the source %q: %s", want, on.Desc)
		}
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func TestNamespaceUnico(t *testing.T) {
	for _, c := range Registry {
		if len(c.Path) >= 2 && c.Path[0] == "run" {
			if c.Path[1] == "team" || c.Path[1] == "agent" {
				t.Errorf("%s: a only namespace, `run <actor>`", c.CLI())
			}
		}
	}
	rs := Lookup("run", "start")
	if rs == nil {
		t.Fatal("missing run start")
	}
	if rs.Params[0].Name != "actor" {
		t.Errorf("run start recibe %q, should recibir 'actor'", rs.Params[0].Name)
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func TestPresupuestoObligatorio(t *testing.T) {
	mustRequire := func(cmd *Cmd, param string) {
		t.Helper()
		if cmd == nil {
			t.Fatalf("command ausente (buscando --%s)", param)
		}
		for _, p := range cmd.Params {
			if p.Name == param {
				if !p.Required {
					t.Errorf("%s: --%s has that ser required", cmd.CLI(), param)
				}
				if p.Default != "" {
					t.Errorf("%s: --%s not can tener default (%q): a techo invisible is a factura sorpresa",
						cmd.CLI(), param, p.Default)
				}
				return
			}
		}
		t.Errorf("%s not declara --%s", cmd.CLI(), param)
	}

	mustRequire(Lookup("run", "start"), "budget")
	mustRequire(Lookup("trigger", "create"), "budget")
	// Implementation note.
	// Implementation note.
	mustRequire(Lookup("trigger", "create"), "budget-period")
	mustRequire(Lookup("eval", "run"), "budget")
}

// Implementation note.
// Implementation note.
func TestToolsMutantesNoSonAllowPorDefault(t *testing.T) {
	// Implementation note.
	// Implementation note.
	// Implementation note.
	esperadas := map[string]bool{
		"iash_state_set":  true,
		"iash_event_emit": true,
		"iash_state_lock": true,
	}
	for _, td := range BuildManifest().Tools {
		if td.Mutates && td.Policy == PolicyAllow && !esperadas[td.Name] {
			t.Errorf("%s muta the world and is allow for defecto.\n"+
				"Si is intencional, agregala a the list of excepciones of this "+
				"test with the motivo; if not, ponele ToolPolicy: PolicyAsk.", td.Name)
		}
	}
}

// Implementation note.
// Implementation note.
func TestLecturaSiempreTieneJSON(t *testing.T) {
	for _, td := range BuildManifest().Tools {
		if td.Mutates {
			continue
		}
		props, _ := td.InputSchema["properties"].(map[string]any)
		if props["json"] == nil {
			t.Errorf("%s lee pero not acepta --json", td.Name)
		}
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestManifiestoVersionadoUnico(t *testing.T) {
	m := BuildManifest()
	if m.SurfaceVersion != SurfaceVersion {
		t.Errorf("surface_version = %d", m.SurfaceVersion)
	}
	if len(m.Tools) == 0 {
		t.Fatal("manifest vacío")
	}
	for _, td := range m.Tools {
		if strings.Contains(td.Name, "@") {
			t.Errorf("%s versiona for tool; the version is of the surface", td.Name)
		}
		if td.Since == 0 {
			t.Errorf("%s not declara since: a cliente not can know if can usarla", td.Name)
		}
	}
}

// Implementation note.
func TestScopeUniforme(t *testing.T) {
	for _, c := range Registry {
		if c.Kind&Protocol == 0 {
			continue
		}
		if c.ProtocolType() == "" {
			t.Errorf("%s is expone to the protocolo without tipo", c.CLI())
		}
		if strings.Contains(c.ProtocolType(), " ") {
			t.Errorf("tipo of protocolo with espacio: %q", c.ProtocolType())
		}
	}
}

// Implementation note.
// Implementation note.
func TestRunWhyExiste(t *testing.T) {
	c := Lookup("run", "why")
	if c == nil {
		t.Fatal("missing run why")
	}
	if c.Kind&AgentTool == 0 {
		t.Error("run why has that ser a tool: a coordinador needs diagnosticar a their subordinados")
	}
	if c.Mutates {
		t.Error("run why not should mutar nothing")
	}
}

// Implementation note.
// Implementation note.
func TestSinDuplicados(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Registry {
		if seen[c.CLI()] {
			t.Errorf("path duplicado: %s", c.CLI())
		}
		seen[c.CLI()] = true
	}
}
