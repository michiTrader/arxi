// Package surface declares ALL of iash's capability exactly once.
//
// The central idea: the CLI, the tools the agents see and the NDJSON protocol
// messages are not three things that have to be kept in sync. They are three
// projections of this list.
//
//	Cmd{Path: []string{"run","start"}}  ->  CLI:      iash run start
//	                                    ->  tool:     iash_run_start
//	                                    ->  protocol: run.start
//
// The cost of adding a capability is one entry here. The cost of forgetting to
// expose it somewhere is zero, because there is no "somewhere": there is a
// mechanical derivation and tests that verify there are no exceptions.
//
// This is what makes it possible for an iash agent to use iash. An agent that
// can read `iash schema` can discover what the system knows how to do without
// anybody writing it a prompt with the list.
package surface

import (
	"sort"
	"strings"
)

// SurfaceVersion versions the ENTIRE surface, not each tool.
//
// The alternative that was discarded was `@v1` per tool. It sounds more granular
// and it is worse: it forces every client to negotiate a version per tool, and
// in practice capabilities evolve together. With one number plus the
// Since/DeprecatedIn fields per command, a client knows what to expect from a
// single comparison.
const SurfaceVersion = 1

// Kind says where a capability is exposed. These are flags because almost all of
// them are exposed in more than one place.
type Kind uint8

const (
	// CLIOnly is for what a human does in their terminal and an agent should not
	// be able to do on its own (install a provider, open the designer).
	CLIOnly Kind = 1 << iota
	// AgentTool is exposed as a tool to the agents.
	AgentTool
	// Protocol is exposed as an NDJSON protocol message.
	Protocol
)

// Policy is the default policy of a tool exposed to agents.
type Policy string

const (
	PolicyAllow Policy = "allow"
	PolicyAsk   Policy = "ask"
	PolicyDeny  Policy = "deny"
)

// Param is a parameter of a command.
type Param struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Desc       string   `json:"desc"`
	Positional bool     `json:"positional,omitempty"`
	Required   bool     `json:"required,omitempty"`
	Default    string   `json:"default,omitempty"`
	Enum       []string `json:"enum,omitempty"`
}

// Cmd is a capability of the system declared exactly once.
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

// Name is the name as a tool: iash_run_start.
func (c Cmd) Name() string { return "iash_" + strings.Join(c.Path, "_") }

// CLI is the invocation as CLI: run start.
func (c Cmd) CLI() string { return strings.Join(c.Path, " ") }

// ProtocolType is the protocol message type: run.start.
//
// That this is a one-line function and not a translation table is the whole
// point of the design. That is why `cancel` is called cancel and not abort: the
// name of the CLI verb IS the name of the protocol message, and if there were
// two vocabularies the mapping would have to be maintained by hand forever.
func (c Cmd) ProtocolType() string { return strings.Join(c.Path, ".") }

func p(name, typ, desc string) Param   { return Param{Name: name, Type: typ, Desc: desc} }
func req(pp Param) Param               { pp.Required = true; return pp }
func pos(pp Param) Param               { pp.Positional = true; pp.Required = true; return pp }
func def(pp Param, d string) Param     { pp.Default = d; return pp }
func enum(pp Param, v ...string) Param { pp.Enum = v; return pp }

// Registry is the complete surface.
var Registry = []Cmd{
	// ---- core: providers and models ----------------------------------------
	{Path: []string{"provider", "add"}, Desc: "register a model provider",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "provider name")),
			p("base-url", "string", "OpenAI-compatible endpoint"),
			p("api-key-env", "string", "environment variable holding the key")}},
	{Path: []string{"model", "list"}, Desc: "list available models",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"model", "enable"}, Desc: "enable a model",
		Kind: CLIOnly, Mutates: true, Since: 1, Params: []Param{pos(p("model", "string", "model id"))}},
	{Path: []string{"model", "disable"}, Desc: "disable a model",
		Kind: CLIOnly, Mutates: true, Since: 1, Params: []Param{pos(p("model", "string", "model id"))}},

	// ---- run: the central verb ---------------------------------------------
	// A single namespace: `run <actor>`. There is no `run team` nor `run agent`,
	// because a team IS an agent with a different kind. Two verbs would force the
	// user to know what type something is before being able to use it.
	{Path: []string{"run", "start"}, Desc: "start a run over an actor (agent or team)",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{
			pos(p("actor", "string", "agent or team to execute")),
			pos(p("prompt", "string", "objective of the run")),
			// --budget is MANDATORY and has no default. An invisible default is
			// a surprise bill; making the user type the number is the only way
			// for them to know it exists.
			req(p("budget", "number", "spend ceiling in USD for the whole tree")),
			def(p("max-turns", "number", "turn ceiling"), "0"),
			enum(def(p("workspace", "string", "filesystem isolation"), "auto"),
				"shared", "worktree", "copy", "none"),
			p("sim", "bool", "run with a fake executor, without spending money"),
			p("attach", "bool", "follow the output live")}},
	{Path: []string{"run", "list"}, Desc: "list runs",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{p("status", "string", "filter by status")}},
	{Path: []string{"run", "show"}, Desc: "view the state of a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id"))}},
	// `run why` is the reason the structured log exists: it answers "why is
	// nothing happening?" by walking the wait graph.
	{Path: []string{"run", "why"}, Desc: "explain why a run is not advancing, and how to unblock it",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id"))}},
	{Path: []string{"run", "tree"}, Desc: "view the tree of runs and its spend",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "root run id"))}},
	{Path: []string{"run", "result"}, Desc: "get the result of a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id"))}},
	{Path: []string{"run", "attach"}, Desc: "follow a run live",
		Kind: CLIOnly | Protocol, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id"))}},
	{Path: []string{"run", "pause"}, Desc: "pause a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id"))}},
	{Path: []string{"run", "unpause"}, Desc: "resume a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			p("budget", "number", "new spend ceiling")}},
	// `cancel`, not `abort`. See ProtocolType(): the CLI verb is the protocol
	// message type, so having two words for the same thing would mean
	// maintaining a translation table forever.
	{Path: []string{"run", "cancel"}, Desc: "cancel a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			p("reason", "string", "reason, stays in the log")}},
	{Path: []string{"run", "fork"}, Desc: "fork a run from a seq",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			p("at-seq", "number", "seq to fork from")}},
	{Path: []string{"run", "replay"}, Desc: "replay a run from its log, without executing effects",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			p("until-seq", "number", "replay up to this seq")}},
	{Path: []string{"run", "prompt"}, Desc: "inject a new cause into a live run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			pos(p("text", "string", "the message")),
			p("to", "string", "recipient; if absent uses steer_target from the blueprint"),
			// CAS on seq, not on turn (ADR-0006). The turn does not identify a
			// version of the state; the seq does.
			p("if-seq", "number", "only apply if the run is at this seq"),
			enum(def(p("on-busy", "string", "what to do if the recipient is busy"), "queue"),
				"reject", "queue", "steer")}},
	{Path: []string{"run", "steer"}, Desc: "correct the course of a live run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			pos(p("text", "string", "the correction")),
			p("to", "string", "recipient"),
			p("if-seq", "number", "only apply if the run is at this seq"),
			enum(def(p("on-busy", "string", "what to do if it is busy"), "steer"),
				"reject", "queue", "steer")}},

	// ---- agents and roles --------------------------------------------------
	{Path: []string{"agent", "list"}, Desc: "list agents",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"agent", "create"}, Desc: "create an agent",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name")),
			p("model", "string", "model to use"),
			p("role", "string", "role"),
			p("tools", "string", "tools, comma-separated"),
			// advisory is a trait of the role, not a flag of a feature: it means
			// "gives an opinion but does not decide" and each blueprint
			// interprets the consequence (does not count toward the quorum, does
			// not block the advance...).
			p("advisory", "bool", "gives an opinion but does not count toward advance rules")}},
	{Path: []string{"agent", "show"}, Desc: "view an agent",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},
	{Path: []string{"agent", "tool", "policy"}, Desc: "change the tool policy of an agent",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{req(p("agent", "string", "agent")),
			p("allow", "string", "tool to allow"),
			p("ask", "string", "tool to ask about"),
			p("deny", "string", "tool to deny")}},
	{Path: []string{"role", "define"}, Desc: "define a reusable role",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "role name")),
			p("advisory", "bool", "the role gives an opinion but does not decide"),
			p("tools", "string", "default tools")}},

	// ---- blueprints --------------------------------------------------------
	{Path: []string{"blueprint", "validate"}, Desc: "validate a blueprint without executing it",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("path", "string", "blueprint file"))}},
	{Path: []string{"blueprint", "create"}, Desc: "create a blueprint",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},
	{Path: []string{"blueprint", "install"}, Desc: "install a published blueprint",
		Kind: CLIOnly, Mutates: true, Since: 1,
		Params: []Param{pos(p("ref", "string", "blueprint reference"))}},

	// ---- shared state ------------------------------------------------------
	{Path: []string{"state", "get"}, Desc: "read a key from the run state",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("key", "string", "key"))}},
	{Path: []string{"state", "set"}, Desc: "write a key of the run state",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("key", "string", "key")), pos(p("value", "string", "value")),
			p("if-seq", "number", "compare-and-swap on the run seq")}},
	{Path: []string{"state", "lock"}, Desc: "take a cooperative lock",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("key", "string", "key")),
			p("ttl", "string", "lock expiry")}},

	// ---- events ------------------------------------------------------------
	{Path: []string{"event", "emit"}, Desc: "emit a custom.* event",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Mutates: true, Since: 1,
		Params: []Param{pos(p("type", "string", "type; agents can only use custom.*")),
			p("payload", "string", "JSON payload")}},
	{Path: []string{"event", "log"}, Desc: "view the event log of a run",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("run", "string", "run id")),
			p("type", "string", "filter by type"),
			p("since-seq", "number", "from this seq")}},
	{Path: []string{"event", "trace"}, Desc: "follow the causal chain of an event",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("event", "string", "event id"))}},

	// ---- triggers ----------------------------------------------------------
	// `trigger`, not `schedule`: cron is ONE of the sources. Calling it schedule
	// would make webhook and file-watch look like second-class features.
	{Path: []string{"trigger", "create"}, Desc: "create a trigger",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{
			pos(p("name", "string", "trigger name")),
			req(p("on", "string", "cron:|every:|at:|webhook:|file:|event:")),
			req(p("then", "string", "run:|emit:|notify:")),
			// A trigger without a spend ceiling per period is an open
			// subscription to the provider's bill.
			req(p("budget", "number", "spend ceiling per period")),
			req(p("budget-period", "string", "period of the ceiling: day|week|month")),
			enum(def(p("on-missed", "string", "what to do if an execution was missed"), "skip"),
				"skip", "run-once", "run-all"),
			enum(def(p("overlap", "string", "what to do if the previous one is still running"), "skip"),
				"skip", "queue", "parallel", "cancel-previous")}},
	{Path: []string{"trigger", "list"}, Desc: "list triggers",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"trigger", "show"}, Desc: "view a trigger",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},
	{Path: []string{"trigger", "pause"}, Desc: "pause a trigger",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("name", "string", "name"))}},

	// ---- inbox: the human in the loop --------------------------------------
	{Path: []string{"inbox"}, Desc: "view pending questions",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"inbox", "approve"}, Desc: "approve a request",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "item id"))}},
	{Path: []string{"inbox", "reject"}, Desc: "reject a request",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "item id")),
			p("reason", "string", "reason")}},
	{Path: []string{"inbox", "reply"}, Desc: "reply to a question",
		Kind: CLIOnly | Protocol, Mutates: true, Since: 1,
		Params: []Param{pos(p("id", "string", "item id")),
			pos(p("text", "string", "the answer"))}},

	// ---- evaluation --------------------------------------------------------
	// eval comes for free from the pure reducer: it is the same fold over fixed
	// cases.
	{Path: []string{"eval", "run"}, Desc: "run an evaluation suite",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAsk, Mutates: true, Since: 1,
		Params: []Param{pos(p("suite", "string", "suite file")),
			req(p("budget", "number", "spend ceiling of the suite"))}},
	{Path: []string{"eval", "compare"}, Desc: "compare two evaluation runs",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("a", "string", "run A")), pos(p("b", "string", "run B"))}},

	// ---- meta --------------------------------------------------------------
	{Path: []string{"design"}, Desc: "open the interactive blueprint designer",
		Kind: CLIOnly, Since: 1},
	// `schema` is what makes the surface reflexive: an agent reads this and
	// discovers what the system knows how to do, without anybody writing it the
	// list.
	{Path: []string{"schema"}, Desc: "emit the surface manifest",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"serve"}, Desc: "serve the NDJSON protocol over stdio or socket",
		Kind: CLIOnly, Since: 1,
		Params: []Param{def(p("listen", "string", "address; empty = stdio"), "")}},
}

// ToolDecl is a tool as an agent or an MCP client sees it.
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

// Manifest is the surface projected into a single versioned document.
type Manifest struct {
	SurfaceVersion int        `json:"surface_version"`
	Tools          []ToolDecl `json:"tools"`
}

// BuildManifest projects the Registry into the tool manifest.
func BuildManifest() Manifest {
	m := Manifest{SurfaceVersion: SurfaceVersion}
	for _, c := range Registry {
		if c.Kind&AgentTool == 0 {
			continue
		}
		pol := c.ToolPolicy
		if pol == "" {
			// With no declared policy, deny. A permissive default turns every
			// oversight into a silent security hole.
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

// jsonSchema derives the input schema from the declared Params.
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
	// Everything that reads accepts --json. It is added here and not by hand in
	// each command so that forgetting is impossible: a machine-readable output
	// that exists in "most" commands is useless for automating anything.
	if !c.Mutates {
		props["json"] = map[string]any{"type": "boolean", "description": "JSON output"}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Lookup finds a command by its path.
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
