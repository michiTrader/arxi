// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
package surface

import (
	"sort"
	"strings"
)

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
const SurfaceVersion = 1

// Implementation note.
// Implementation note.
type Kind uint8

const (
	// Implementation note.
	// Implementation note.
	CLIOnly Kind = 1 << iota
	// Implementation note.
	AgentTool
	// Implementation note.
	Protocol
)

// Implementation note.
type Policy string

const (
	PolicyAllow Policy = "allow"
	PolicyAsk   Policy = "ask"
	PolicyDeny  Policy = "deny"
)

// Implementation note.
type Param struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Desc       string   `json:"desc"`
	Positional bool     `json:"positional,omitempty"`
	Required   bool     `json:"required,omitempty"`
	Default    string   `json:"default,omitempty"`
	Enum       []string `json:"enum,omitempty"`
}

// Implementation note.
type Cmd struct {
	Path         []string
	Desc         string
	Kind         Kind
	Params       []Param
	ToolPolicy   Policy
	Mutates      bool
	Idempotent   bool
	Since        int
	DeprecatedIn int
}

// Implementation note.
func (c Cmd) Name() string { return "iash_" + strings.Join(c.Path, "_") }

// Implementation note.
func (c Cmd) CLI() string { return strings.Join(c.Path, " ") }

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func (c Cmd) ProtocolType() string { return strings.Join(c.Path, ".") }

func p(name, typ, desc string) Param   { return Param{Name: name, Type: typ, Desc: desc} }
func req(pp Param) Param               { pp.Required = true; return pp }
func pos(pp Param) Param               { pp.Positional = true; pp.Required = true; return pp }
func def(pp Param, d string) Param     { pp.Default = d; return pp }
func enum(pp Param, v ...string) Param { pp.Enum = v; return pp }

// Implementation note.
var Registry = []Cmd{
	// Implementation note.
	{Path: []string{"provider", "add"}, Desc: "registrar a proveedor of modelos",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name of the proveedor")),
			p("base-url", "string", "endpoint compatible OpenAI"),
			p("api-key-env", "string", "variable of entorno with the key")}},
	{Path: []string{"model", "list"}, Desc: "list modelos disponibles",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"model", "enable"}, Desc: "habilitar a model",
		Kind: CLIOnly, Mutates: true, Since: 1, Params: []Param{pos(p("model", "string", "id of the model"))}},
	{Path: []string{"model", "disable"}, Desc: "deshabilitar a model",
		Kind: CLIOnly, Mutates: true, Since: 1, Params: []Param{pos(p("model", "string", "id of the model"))}},

	// Implementation note.
	// Implementation note.
	// Implementation note.
	// Implementation note.
	{Path: []string{"run", "start"}, Desc: "start a run sobre a actor (agente or equipo)",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{
			pos(p("actor", "string", "agente or equipo a ejecutar")),
			pos(p("prompt", "string", "objetivo of the run")),
			// Implementation note.
			// Implementation note.
			// Implementation note.
			req(p("budget", "number", "techo of spending en USD for the tree complete")),
			def(p("max-turns", "number", "techo of turns"), "0"),
			enum(def(p("workspace", "string", "aislamiento of filesystem"), "auto"),
				"shared", "worktree", "copy", "none"),
			p("sim", "bool", "ejecutar with executor fake, without gastar money"),
			p("attach", "bool", "continue the output en live")}},
	{Path: []string{"run", "list"}, Desc: "list runs",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{p("status", "string", "filtrar for state")}},
	{Path: []string{"run", "show"}, Desc: "see the state of a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run"))}},
	// Implementation note.
	// Implementation note.
	{Path: []string{"run", "why"}, Desc: "explicar for what a run not advances, and how desbloquearlo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run"))}},
	{Path: []string{"run", "tree"}, Desc: "see the tree of runs and its spending",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run raíz"))}},
	{Path: []string{"run", "result"}, Desc: "obtener the resultado of a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run"))}},
	{Path: []string{"run", "attach"}, Desc: "continue a run en live",
		Kind: CLIOnly | Protocol, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run"))}},
	{Path: []string{"run", "pause"}, Desc: "pausar a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run"))}},
	{Path: []string{"run", "unpause"}, Desc: "resume a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			p("budget", "number", "new techo of spending")}},
	// Implementation note.
	// Implementation note.
	// Implementation note.
	{Path: []string{"run", "cancel"}, Desc: "cancel a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			p("reason", "string", "motivo, remains en the log")}},
	{Path: []string{"run", "fork"}, Desc: "bifurcar a run from a seq",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			p("at-seq", "number", "seq from where bifurcar")}},
	{Path: []string{"run", "replay"}, Desc: "replay a run from its log, without ejecutar effects",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			p("until-seq", "number", "replay until this seq")}},
	{Path: []string{"run", "prompt"}, Desc: "inyectar a cause nueva en a run live",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			pos(p("text", "string", "the mensaje")),
			p("to", "string", "destinatario; if missing uses steer_target of the blueprint"),
			// Implementation note.
			// Implementation note.
			p("if-seq", "number", "only apply if the run is en this seq"),
			enum(def(p("on-busy", "string", "what make if the destinatario is busy"), "queue"),
				"reject", "queue", "steer")}},
	{Path: []string{"run", "steer"}, Desc: "fix the rumbo of a run live",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			pos(p("text", "string", "the fix")),
			p("to", "string", "destinatario"),
			p("if-seq", "number", "only apply if the run is en this seq"),
			enum(def(p("on-busy", "string", "what make if is busy"), "steer"),
				"reject", "queue", "steer")}},

	// Implementation note.
	{Path: []string{"agent", "list"}, Desc: "list agentes",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"agent", "create"}, Desc: "create a agente",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name")),
			p("model", "string", "model a use"),
			p("role", "string", "rol"),
			p("tools", "string", "tools, separadas for coma"),
			// Implementation note.
			// Implementation note.
			// Implementation note.
			p("advisory", "bool", "opina pero not cuenta for rules of avance")}},
	{Path: []string{"agent", "show"}, Desc: "see a agente",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},
	{Path: []string{"agent", "tool", "policy"}, Desc: "change the política of tools of a agente",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{req(p("agent", "string", "agente")),
			p("allow", "string", "tool a permitir"),
			p("ask", "string", "tool a ask"),
			p("deny", "string", "tool a denegar")}},
	{Path: []string{"role", "define"}, Desc: "definir a rol reutilizable",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name of the rol")),
			p("advisory", "bool", "the rol opina pero not decides"),
			p("tools", "string", "tools for defecto")}},

	// Implementation note.
	{Path: []string{"blueprint", "validate"}, Desc: "validar a blueprint without ejecutarlo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("path", "string", "file of the blueprint"))}},
	{Path: []string{"blueprint", "create"}, Desc: "create a blueprint",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},
	{Path: []string{"blueprint", "install"}, Desc: "instalar a blueprint publicado",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("ref", "string", "reference of the blueprint"))}},

	// Implementation note.
	{Path: []string{"state", "get"}, Desc: "read a key of the state of the run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("key", "string", "key"))}},
	{Path: []string{"state", "set"}, Desc: "write a key of the state of the run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("key", "string", "key")), pos(p("value", "string", "valor")),
			p("if-seq", "number", "compare-and-swap sobre the seq of the run")}},
	{Path: []string{"state", "lock"}, Desc: "tomar a lock cooperativo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("key", "string", "key")),
			p("ttl", "string", "vencimiento of the lock")}},

	// Implementation note.
	{Path: []string{"event", "emit"}, Desc: "emitir a event custom.*",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("type", "string", "tipo; the agentes only can custom.*")),
			p("payload", "string", "payload JSON")}},
	{Path: []string{"event", "log"}, Desc: "see the log of events of a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id of the run")),
			p("type", "string", "filtrar for tipo"),
			p("since-seq", "number", "from this seq")}},
	{Path: []string{"event", "trace"}, Desc: "continue the cadena causal of a event",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("event", "string", "id of the event"))}},

	// Implementation note.
	// Implementation note.
	// Implementation note.
	{Path: []string{"trigger", "create"}, Desc: "create a disparador",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{
			pos(p("name", "string", "name of the trigger")),
			req(p("on", "string", "cron:|every:|at:|webhook:|file:|event:")),
			req(p("then", "string", "run:|emit:|notify:")),
			// Implementation note.
			// Implementation note.
			req(p("budget", "number", "techo of spending for período")),
			req(p("budget-period", "string", "período of the techo: day|week|month")),
			enum(def(p("on-missed", "string", "what make if is perdió a execution"), "skip"),
				"skip", "run-once", "run-all"),
			enum(def(p("overlap", "string", "what make if the anterior continues corriendo"), "skip"),
				"skip", "queue", "parallel", "cancel-previous")}},
	{Path: []string{"trigger", "list"}, Desc: "list disparadores",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"trigger", "show"}, Desc: "see a disparador",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},
	{Path: []string{"trigger", "pause"}, Desc: "pausar a disparador",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},

	// Implementation note.
	{Path: []string{"inbox"}, Desc: "see questions pendientes",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"inbox", "approve"}, Desc: "aprobar a solicitud",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "id of the item"))}},
	{Path: []string{"inbox", "reject"}, Desc: "rechazar a solicitud",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "id of the item")),
			p("reason", "string", "motivo")}},
	{Path: []string{"inbox", "reply"}, Desc: "responder a question",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "id of the item")),
			pos(p("text", "string", "the answer"))}},

	// Implementation note.
	// Implementation note.
	{Path: []string{"eval", "run"}, Desc: "correr a suite of evaluación",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("suite", "string", "file of the suite")),
			req(p("budget", "number", "techo of spending of the suite"))}},
	{Path: []string{"eval", "compare"}, Desc: "comparar two corridas of evaluación",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("a", "string", "corrida A")), pos(p("b", "string", "corrida B"))}},

	// Implementation note.
	{Path: []string{"design"}, Desc: "open the diseñador interactivo of blueprints",
		Kind: CLIOnly, Since: 1},
	// Implementation note.
	// Implementation note.
	{Path: []string{"schema"}, Desc: "emitir the manifest of the surface",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"serve"}, Desc: "servir the protocolo NDJSON for stdio or socket",
		Kind: CLIOnly, Since: 1,
		Params: []Param{def(p("listen", "string", "dirección; vacío = stdio"), "")}},
}

// Implementation note.
type ToolDecl struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	Policy       Policy         `json:"policy"`
	Mutates      bool           `json:"mutates,omitempty"`
	Idempotent   bool           `json:"idempotent,omitempty"`
	Since        int            `json:"since"`
	DeprecatedIn int            `json:"deprecated_in,omitempty"`
	ProtocolType string         `json:"protocol_type"`
}

// Implementation note.
type Manifest struct {
	SurfaceVersion int        `json:"surface_version"`
	Tools          []ToolDecl `json:"tools"`
}

// Implementation note.
func BuildManifest() Manifest {
	m := Manifest{SurfaceVersion: SurfaceVersion}
	for _, c := range Registry {
		if c.Kind&AgentTool == 0 {
			continue
		}
		pol := c.ToolPolicy
		if pol == "" {
			// Implementation note.
			// Implementation note.
			pol = PolicyDeny
		}
		m.Tools = append(m.Tools, ToolDecl{
			Name:         c.Name(),
			Description:  c.Desc,
			InputSchema:  jsonSchema(c),
			Policy:       pol,
			Mutates:      c.Mutates,
			Idempotent:   c.Idempotent,
			Since:        c.Since,
			DeprecatedIn: c.DeprecatedIn,
			ProtocolType: c.ProtocolType(),
		})
	}
	sort.Slice(m.Tools, func(i, j int) bool { return m.Tools[i].Name < m.Tools[j].Name })
	return m
}

// Implementation note.
func jsonSchema(c Cmd) map[string]any {
	props := map[string]any{}
	var required []string
	for _, pp := range c.Params {
		name := strings.ReplaceAll(pp.Name, "-", "_")
		typ := pp.Type
		switch typ {
		case "bool":
			typ = "boolean"
		case "number":
			typ = "number"
		default:
			typ = "string"
		}
		e := map[string]any{"type": typ, "description": pp.Desc}
		if len(pp.Enum) > 0 {
			e["enum"] = pp.Enum
		}
		if pp.Default != "" {
			e["default"] = pp.Default
		}
		props[name] = e
		if pp.Required {
			required = append(required, name)
		}
	}
	// Implementation note.
	// Implementation note.
	// Implementation note.
	if !c.Mutates {
		props["json"] = map[string]any{"type": "boolean", "description": "output JSON"}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Implementation note.
func Lookup(path ...string) *Cmd {
	for i := range Registry {
		if len(Registry[i].Path) != len(path) {
			continue
		}
		ok := true
		for j := range path {
			if Registry[i].Path[j] != path[j] {
				ok = false
				break
			}
		}
		if ok {
			return &Registry[i]
		}
	}
	return nil
}
