// Package surface declara TODA la capacidad de iash una sola vez.
//
// La idea central: la CLI, las herramientas que ven los agentes y los mensajes
// del protocolo NDJSON no son tres cosas que hay que mantener sincronizadas.
// Son tres proyecciones de esta lista.
//
//	Cmd{Path: []string{"run","start"}}  ->  CLI:       iash run start
//	                                    ->  tool:      iash_run_start
//	                                    ->  protocolo: run.start
//
// El costo de agregar una capacidad es una entrada acá. El costo de olvidarse
// de exponerla en algún lado es cero, porque no hay "algún lado": hay una
// derivación mecánica y tests que verifican que no haya excepciones.
//
// Esto es lo que hace posible que un agente de iash use iash. Un agente que
// puede leer `iash schema` puede descubrir qué sabe hacer el sistema sin que
// nadie le escriba un prompt con la lista.
package surface

import (
	"sort"
	"strings"
)

// SurfaceVersion versiona la superficie ENTERA, no cada herramienta.
//
// La alternativa que se descartó era `@v1` por herramienta. Suena más
// granular y es peor: obliga a cada cliente a negociar versión por
// herramienta, y en la práctica las capacidades evolucionan juntas. Con un
// número y los campos Since/DeprecatedIn por comando, un cliente sabe qué
// esperar con una sola comparación.
const SurfaceVersion = 1

// Kind dice dónde se expone una capacidad. Son flags porque casi todas se
// exponen en más de un lugar.
type Kind uint8

const (
	// CLIOnly es para lo que un humano hace en su terminal y un agente no
	// debería poder hacer solo (instalar un proveedor, abrir el diseñador).
	CLIOnly Kind = 1 << iota
	// AgentTool se expone como herramienta a los agentes.
	AgentTool
	// Protocol se expone como mensaje del protocolo NDJSON.
	Protocol
)

// Policy es la política por defecto de una herramienta expuesta a agentes.
type Policy string

const (
	PolicyAllow Policy = "allow"
	PolicyAsk   Policy = "ask"
	PolicyDeny  Policy = "deny"
)

// Param es un parámetro de un comando.
type Param struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Desc       string   `json:"desc"`
	Positional bool     `json:"positional,omitempty"`
	Required   bool     `json:"required,omitempty"`
	Default    string   `json:"default,omitempty"`
	Enum       []string `json:"enum,omitempty"`
}

// Cmd es una capacidad del sistema declarada una sola vez.
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

// Name es el nombre como herramienta: iash_run_start.
func (c Cmd) Name() string { return "iash_" + strings.Join(c.Path, "_") }

// CLI es la invocación como CLI: run start.
func (c Cmd) CLI() string { return strings.Join(c.Path, " ") }

// ProtocolType es el tipo de mensaje del protocolo: run.start.
//
// Que esto sea una función de una línea y no una tabla de traducción es el
// punto entero del diseño. Por eso `cancel` se llama cancel y no abort: el
// nombre del verbo de la CLI ES el nombre del mensaje del protocolo, y si
// hubiera dos vocabularios habría que mantener el mapeo a mano para siempre.
func (c Cmd) ProtocolType() string { return strings.Join(c.Path, ".") }

func p(name, typ, desc string) Param   { return Param{Name: name, Type: typ, Desc: desc} }
func req(pp Param) Param               { pp.Required = true; return pp }
func pos(pp Param) Param               { pp.Positional = true; pp.Required = true; return pp }
func def(pp Param, d string) Param     { pp.Default = d; return pp }
func enum(pp Param, v ...string) Param { pp.Enum = v; return pp }

// Registry es la superficie completa.
var Registry = []Cmd{
	// ---- núcleo: proveedores y modelos -------------------------------------
	{Path: []string{"provider", "add"}, Desc: "registrar un proveedor de modelos",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre del proveedor")),
			p("base-url", "string", "endpoint compatible OpenAI"),
			p("api-key-env", "string", "variable de entorno con la clave")}},
	{Path: []string{"model", "list"}, Desc: "listar modelos disponibles",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"model", "enable"}, Desc: "habilitar un modelo",
		Kind: CLIOnly, Mutates: true, Since: 1, Params: []Param{pos(p("model", "string", "id del modelo"))}},
	{Path: []string{"model", "disable"}, Desc: "deshabilitar un modelo",
		Kind: CLIOnly, Mutates: true, Since: 1, Params: []Param{pos(p("model", "string", "id del modelo"))}},

	// ---- run: el verbo central ---------------------------------------------
	// Un solo namespace: `run <actor>`. No hay `run team` ni `run agent`,
	// porque un equipo ES un agente con kind distinto. Dos verbos obligarían al
	// usuario a saber de qué tipo es algo antes de poder usarlo.
	{Path: []string{"run", "start"}, Desc: "arrancar un run sobre un actor (agente o equipo)",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{
			pos(p("actor", "string", "agente o equipo a ejecutar")),
			pos(p("prompt", "string", "objetivo del run")),
			// --budget es OBLIGATORIO y no tiene default. Un default invisible
			// es una factura sorpresa; que el usuario tenga que escribir el
			// número es la única forma de que sepa que existe.
			req(p("budget", "number", "techo de gasto en USD para el árbol completo")),
			def(p("max-turns", "number", "techo de turnos"), "0"),
			enum(def(p("workspace", "string", "aislamiento de filesystem"), "auto"),
				"shared", "worktree", "copy", "none"),
			p("sim", "bool", "ejecutar con ejecutor falso, sin gastar dinero"),
			p("attach", "bool", "seguir la salida en vivo")}},
	{Path: []string{"run", "list"}, Desc: "listar runs",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{p("status", "string", "filtrar por estado")}},
	{Path: []string{"run", "show"}, Desc: "ver el estado de un run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run"))}},
	// `run why` es la razón de existir del log estructurado: responde
	// "¿por qué no pasa nada?" caminando el grafo de espera.
	{Path: []string{"run", "why"}, Desc: "explicar por qué un run no avanza, y cómo desbloquearlo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run"))}},
	{Path: []string{"run", "tree"}, Desc: "ver el árbol de runs y su gasto",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run raíz"))}},
	{Path: []string{"run", "result"}, Desc: "obtener el resultado de un run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run"))}},
	{Path: []string{"run", "attach"}, Desc: "seguir un run en vivo",
		Kind: CLIOnly | Protocol, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run"))}},
	{Path: []string{"run", "pause"}, Desc: "pausar un run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run"))}},
	{Path: []string{"run", "unpause"}, Desc: "reanudar un run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			p("budget", "number", "nuevo techo de gasto")}},
	// `cancel`, no `abort`. Ver ProtocolType(): el verbo de la CLI es el tipo
	// del mensaje del protocolo, así que tener dos palabras para lo mismo
	// significaría mantener una tabla de traducción para siempre.
	{Path: []string{"run", "cancel"}, Desc: "cancelar un run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			p("reason", "string", "motivo, queda en el log")}},
	{Path: []string{"run", "fork"}, Desc: "bifurcar un run desde un seq",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			p("at-seq", "number", "seq desde donde bifurcar")}},
	{Path: []string{"run", "replay"}, Desc: "reproducir un run desde su log, sin ejecutar efectos",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			p("until-seq", "number", "reproducir hasta este seq")}},
	{Path: []string{"run", "prompt"}, Desc: "inyectar una causa nueva en un run vivo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			pos(p("text", "string", "el mensaje")),
			p("to", "string", "destinatario; si falta usa steer_target del blueprint"),
			// CAS sobre seq, no sobre turn (ADR-0006). El turno no identifica
			// una versión del estado; el seq sí.
			p("if-seq", "number", "solo aplicar si el run está en este seq"),
			enum(def(p("on-busy", "string", "qué hacer si el destinatario está ocupado"), "queue"),
				"reject", "queue", "steer")}},
	{Path: []string{"run", "steer"}, Desc: "corregir el rumbo de un run vivo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			pos(p("text", "string", "la corrección")),
			p("to", "string", "destinatario"),
			p("if-seq", "number", "solo aplicar si el run está en este seq"),
			enum(def(p("on-busy", "string", "qué hacer si está ocupado"), "steer"),
				"reject", "queue", "steer")}},

	// ---- agentes y roles ---------------------------------------------------
	{Path: []string{"agent", "list"}, Desc: "listar agentes",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"agent", "create"}, Desc: "crear un agente",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre")),
			p("model", "string", "modelo a usar"),
			p("role", "string", "rol"),
			p("tools", "string", "herramientas, separadas por coma"),
			// advisory es un rasgo del rol, no un flag de una feature: significa
			// "opina pero no decide" y cada blueprint interpreta la
			// consecuencia (no cuenta para el quórum, no bloquea el avance...).
			p("advisory", "bool", "opina pero no cuenta para reglas de avance")}},
	{Path: []string{"agent", "show"}, Desc: "ver un agente",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre"))}},
	{Path: []string{"agent", "tool", "policy"}, Desc: "cambiar la política de herramientas de un agente",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{req(p("agent", "string", "agente")),
			p("allow", "string", "herramienta a permitir"),
			p("ask", "string", "herramienta a preguntar"),
			p("deny", "string", "herramienta a denegar")}},
	{Path: []string{"role", "define"}, Desc: "definir un rol reutilizable",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre del rol")),
			p("advisory", "bool", "el rol opina pero no decide"),
			p("tools", "string", "herramientas por defecto")}},

	// ---- blueprints --------------------------------------------------------
	{Path: []string{"blueprint", "validate"}, Desc: "validar un blueprint sin ejecutarlo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("path", "string", "archivo del blueprint"))}},
	{Path: []string{"blueprint", "create"}, Desc: "crear un blueprint",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre"))}},
	{Path: []string{"blueprint", "install"}, Desc: "instalar un blueprint publicado",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("ref", "string", "referencia del blueprint"))}},

	// ---- estado compartido -------------------------------------------------
	{Path: []string{"state", "get"}, Desc: "leer una clave del estado del run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("key", "string", "clave"))}},
	{Path: []string{"state", "set"}, Desc: "escribir una clave del estado del run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("key", "string", "clave")), pos(p("value", "string", "valor")),
			p("if-seq", "number", "compare-and-swap sobre el seq del run")}},
	{Path: []string{"state", "lock"}, Desc: "tomar un lock cooperativo",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("key", "string", "clave")),
			p("ttl", "string", "vencimiento del lock")}},

	// ---- eventos -----------------------------------------------------------
	{Path: []string{"event", "emit"}, Desc: "emitir un evento custom.*",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("type", "string", "tipo; los agentes solo pueden custom.*")),
			p("payload", "string", "payload JSON")}},
	{Path: []string{"event", "log"}, Desc: "ver el log de eventos de un run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "id del run")),
			p("type", "string", "filtrar por tipo"),
			p("since-seq", "number", "desde este seq")}},
	{Path: []string{"event", "trace"}, Desc: "seguir la cadena causal de un evento",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("event", "string", "id del evento"))}},

	// ---- triggers ----------------------------------------------------------
	// `trigger`, no `schedule`: cron es UNA de las fuentes. Llamarlo schedule
	// haría que webhook y file-watch parezcan features de segunda.
	{Path: []string{"trigger", "create"}, Desc: "crear un disparador",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{
			pos(p("name", "string", "nombre del trigger")),
			req(p("on", "string", "cron:|every:|at:|webhook:|file:|event:")),
			req(p("then", "string", "run:|emit:|notify:")),
			// Un trigger sin techo de gasto por período es una suscripción
			// abierta a la factura del proveedor.
			req(p("budget", "number", "techo de gasto por período")),
			req(p("budget-period", "string", "período del techo: day|week|month")),
			enum(def(p("on-missed", "string", "qué hacer si se perdió una ejecución"), "skip"),
				"skip", "run-once", "run-all"),
			enum(def(p("overlap", "string", "qué hacer si la anterior sigue corriendo"), "skip"),
				"skip", "queue", "parallel", "cancel-previous")}},
	{Path: []string{"trigger", "list"}, Desc: "listar disparadores",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"trigger", "show"}, Desc: "ver un disparador",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre"))}},
	{Path: []string{"trigger", "pause"}, Desc: "pausar un disparador",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "nombre"))}},

	// ---- inbox: el humano en el bucle --------------------------------------
	{Path: []string{"inbox"}, Desc: "ver preguntas pendientes",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"inbox", "approve"}, Desc: "aprobar una solicitud",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "id del item"))}},
	{Path: []string{"inbox", "reject"}, Desc: "rechazar una solicitud",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "id del item")),
			p("reason", "string", "motivo")}},
	{Path: []string{"inbox", "reply"}, Desc: "responder una pregunta",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "id del item")),
			pos(p("text", "string", "la respuesta"))}},

	// ---- evaluación --------------------------------------------------------
	// eval sale gratis del reducer puro: es el mismo fold sobre casos fijos.
	{Path: []string{"eval", "run"}, Desc: "correr una suite de evaluación",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("suite", "string", "archivo de la suite")),
			req(p("budget", "number", "techo de gasto de la suite"))}},
	{Path: []string{"eval", "compare"}, Desc: "comparar dos corridas de evaluación",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("a", "string", "corrida A")), pos(p("b", "string", "corrida B"))}},

	// ---- meta --------------------------------------------------------------
	{Path: []string{"design"}, Desc: "abrir el diseñador interactivo de blueprints",
		Kind: CLIOnly, Since: 1},
	// `schema` es lo que hace la superficie reflexiva: un agente lee esto y
	// descubre qué sabe hacer el sistema, sin que nadie le escriba la lista.
	{Path: []string{"schema"}, Desc: "emitir el manifiesto de la superficie",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"serve"}, Desc: "servir el protocolo NDJSON por stdio o socket",
		Kind: CLIOnly, Since: 1,
		Params: []Param{def(p("listen", "string", "dirección; vacío = stdio"), "")}},
}

// ToolDecl es una herramienta tal como la ve un agente o un cliente MCP.
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

// Manifest es la superficie proyectada a un documento único y versionado.
type Manifest struct {
	SurfaceVersion int        `json:"surface_version"`
	Tools          []ToolDecl `json:"tools"`
}

// BuildManifest proyecta el Registry al manifiesto de herramientas.
func BuildManifest() Manifest {
	m := Manifest{SurfaceVersion: SurfaceVersion}
	for _, c := range Registry {
		if c.Kind&AgentTool == 0 {
			continue
		}
		pol := c.ToolPolicy
		if pol == "" {
			// Sin política declarada, deny. Un default permisivo convierte
			// cada olvido en un agujero de seguridad silencioso.
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

// jsonSchema deriva el schema de entrada de los Params declarados.
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
	// Todo lo que lee acepta --json. Se agrega acá y no a mano en cada comando
	// para que sea imposible olvidarse: una salida legible por máquina que
	// existe "en la mayoría" de los comandos no sirve para automatizar nada.
	if !c.Mutates {
		props["json"] = map[string]any{"type": "boolean", "description": "salida JSON"}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Lookup busca un comando por su path.
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
