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
			// "auto" is in the enum because it is a real value, not the absence
			// of one: it means "use whatever the blueprint resolved to", which
			// is why the binary prints `workspace auto→worktree` instead of just
			// the answer. It was the declared Default and was missing from the
			// legal set, so the surface called its own common path illegal —
			// the protocol validator enforces Enum, so {"workspace": "auto"}
			// would have been rejected, and the tool schema offered an agent
			// four values where the CLI accepts five.
			//
			// A default outside its own enum is the dangerous direction of that
			// mismatch. A value nobody types is rejected only when somebody
			// finally types it; a DEFAULT is rejected on the invocation that
			// mentions nothing, which is the one every reader assumes is safe.
			enum(def(p("workspace", "string", "filesystem isolation"), "auto"),
				"auto", "shared", "worktree", "copy", "none"),
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
			// An iash command, with no scheme prefix. This was declared as
			// "run:|emit:|notify:" and that was a second surface: a
			// hand-written vocabulary of things a trigger can do, parallel to
			// the registry it is sitting in. It had already drifted before any
			// trigger code existed — `notify` is not a command here, has no
			// entry, and nothing says what it would send or to whom — while
			// §20.10's own example writes `--then "run start security-team
			// '...'"` with no prefix at all, because the action a user wants
			// IS a command.
			//
			// Naming the surface instead of copying part of it means every verb
			// iash gains is triggerable the day it lands, and no rename can
			// leave a stale prefix behind. Which commands are legal is derived
			// from Kind&AgentTool: a trigger fires unattended, so it is not a
			// human, and the commands withheld from non-humans (§20.12) are
			// withheld for reasons that apply here at least as strongly. See
			// trigger.ParseAction.
			req(p("then", "string", "iash command to run, e.g. run start team 'objective'")),
			// A trigger without a spend ceiling per period is an open
			// subscription to the provider's bill.
			req(p("budget", "number", "spend ceiling per period")),
			// Declared as an enum, not documented as one in prose. The three
			// values were written into the description ("day|week|month") and
			// nowhere the machine could see them, which made the closed set a
			// suggestion: jsonSchema omits `enum`, so an agent reads a free-form
			// string, and the protocol validator enforces enums, so it accepted
			// {"budget_period": "fortnight"} and left the meaning of the ceiling
			// to whoever implemented it later.
			//
			// A ceiling whose period is not understood is not a ceiling. This is
			// the one parameter on this command where a wrong value is silently
			// plausible rather than obviously broken.
			req(enum(p("budget-period", "string", "period of the ceiling"),
				"day", "week", "month")),
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
			req(p("budget", "number", "spend ceiling of the suite")),
			// --sim, for the same reason `run start` declares it, and it was
			// MISSING here until the CLI was RUN instead of read. cmd/iash
			// gates `eval run` on --sim because there is no LLM-backed
			// Executor, and it reads that flag out of this declaration — so an
			// undeclared --sim did not make the gate lenient, it made the
			// command unusable. parseInvocation refused the only flag that
			// gets past the gate, so every possible invocation exited 2.
			//
			// That is the failure mode of deriving the parser from the
			// registry, and it is the right one to have. A hand-written parser
			// would have accepted --sim and left the surface lying about what
			// the command takes: `iash schema` would advertise two parameters
			// where the CLI has three, and an agent would be told to call a
			// command it cannot make run. Here the surface and the parser
			// cannot disagree; they can only both be incomplete, loudly.
			p("sim", "bool", "run with a fake executor, without spending money"),
			// --json on a MUTATING command, the one exception in this registry,
			// so here is the reason.
			//
			// WireParams synthesises --json for every non-mutating command.
			// That rule is a FLOOR — "no reading command may lack it" — not a
			// ceiling, and the line it draws is receipts from data. `run start`
			// prints a receipt: an id and a log path, which you follow with
			// `run show --json` when you want the data.
			//
			// `eval run` has no such follow-up, because its output IS the data.
			// Without this flag the only machine access to a pass rate is a
			// regex over prose — and a regex over prose is precisely how
			// "0.65 → 0.80" gets read as an improvement, stripped of the
			// warning that travelled with it.
			//
			// That argument once ended "until eval runs are persisted", and
			// they now are, so the clause is gone rather than left standing as
			// a stale caveat. Persistence does NOT make this flag redundant.
			// The stored file holds the results; the notes — the prefix-bias
			// warning, the truncation warning — are derived at print time and
			// are the part a reader most needs and most easily loses. A caller
			// forced to re-derive them from a stored document is a caller who
			// will not.
			p("json", "bool", "emit the run as one JSON document")}},
	// The two runs are `baseline` and `candidate`, not `a` and `b`. Two reasons,
	// and the second one is why this was renamed before eval was implemented.
	//
	// A comparison has a direction, and `a`/`b` hides it. "Did it get better or
	// worse" is answered by which argument came first, which nobody remembers, so
	// half the readings of any regression report are inverted. Naming the roles
	// makes the direction part of the invocation.
	//
	// And single-letter names are a CLI convenience that must not reach the wire:
	// this command is Protocol-flagged and an AgentTool, so `a` and `b` became
	// `{"a":"r1","b":"r2"}` in requests and in the log, and properties named `a`
	// and `b` in the schema an agent reads to decide what to call. Caught by
	// TestTheWireHasNoShortFlags. Renaming is free today and a breaking change
	// the day the first eval client exists.
	// `eval list` is not scope creep beside compare, it is what makes compare
	// reachable. Run ids are UTC timestamps — e20260828T141503 — because there
	// is no counter to mint e1 and e2 from, and nobody retypes one of those
	// from memory. Without a listing, `compare` takes two arguments a user has
	// no way to discover, and the honest workflow becomes `ls evals/`, which is
	// a user reading the storage layout because the tool declined to.
	//
	// Idempotent and PolicyAllow: it reads files and spends nothing.
	{Path: []string{"eval", "list"}, Desc: "list evaluation runs",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1},
	{Path: []string{"eval", "compare"}, Desc: "compare two evaluation runs",
		Kind: CLIOnly | AgentTool | Protocol, ToolPolicy: PolicyAllow, Idempotent: true, Since: 1,
		Params: []Param{pos(p("baseline", "string", "run to compare against")),
			pos(p("candidate", "string", "run being judged"))}},

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

// WireParams returns the parameters as a MACHINE client sees them: names
// normalized to underscores, plus the synthetic `json` flag that every
// non-mutating command accepts.
//
// This exists so the tool schema and the protocol server read the parameter list
// from the same function. They had drifted apart in the obvious way: jsonSchema
// normalized `if-seq` to `if_seq` and bolted on a `json` property, so a server
// that validated incoming requests against c.Params would have rejected
// `if_seq` — a name the manifest itself told the client to use — and accepted
// `json` nowhere. A client cannot be asked to obey a schema the server does not
// read.
//
// Positional is deliberately carried through untouched even though the wire has
// no positions. It stays because Required is what the wire enforces and pos()
// sets both; dropping Positional here would mean two Param shapes to keep in
// step for no gain.
func (c Cmd) WireParams() []Param {
	out := make([]Param, 0, len(c.Params)+1)
	for _, pp := range c.Params {
		pp.Name = strings.ReplaceAll(pp.Name, "-", "_")
		out = append(out, pp)
	}
	// Everything that reads accepts --json. It is added here and not by hand in
	// each command so that forgetting is impossible: a machine-readable output
	// that exists in "most" commands is useless for automating anything.
	if !c.Mutates {
		out = append(out, Param{Name: "json", Type: "bool", Desc: "JSON output"})
	}
	return out
}

// jsonSchema derives the input schema from the declared Params.
func jsonSchema(c Cmd) map[string]any {
	props := map[string]any{}
	var required []string
	for _, pp := range c.WireParams() {
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
		props[pp.Name] = e
		if pp.Required {
			required = append(required, pp.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// ProtocolCommands returns every command a protocol client may send, sorted by
// message type.
//
// This is DERIVED from the Kind flag and never hand-listed. A server with its
// own list of accepted types is a second surface: the day somebody adds a
// registry entry with Kind|Protocol, `iash schema` advertises a message type the
// server answers "unknown type" to, and the client is being lied to by the only
// document it was told to trust. Deriving it means the cost of exposing a new
// capability over the wire stays one entry in Registry.
//
// Sorted because this list is the server's advertised capability set, and a
// capability set that reorders between builds makes a golden test impossible and
// a diff between two versions unreadable.
func ProtocolCommands() []Cmd {
	var out []Cmd
	for _, c := range Registry {
		if c.Kind&Protocol != 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProtocolType() < out[j].ProtocolType() })
	return out
}

// LookupProtocol resolves a protocol message type to its command.
//
// It splits on "." and defers to Lookup rather than comparing ProtocolType()
// strings, so the inverse uses the same path data as the forward direction. A
// separate map from type to command would be the translation table this design
// exists to avoid, and it would be the thing that goes stale.
//
// A command that is not exposed to the protocol returns nil even when the path
// is real: `design` exists, and admitting it here would let a socket client open
// an interactive designer on the operator's terminal.
func LookupProtocol(msgType string) *Cmd {
	if msgType == "" {
		return nil
	}
	c := Lookup(strings.Split(msgType, ".")...)
	if c == nil || c.Kind&Protocol == 0 {
		return nil
	}
	return c
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

// shortFlags maps a parameter name to its one-letter form, ONCE, for the whole
// surface. A letter means the same thing in every command that has that
// parameter, or it does not exist.
//
// The obvious implementations are both worse, and for the same reason.
//
// Per-command aliases (`run start` declaring `-p` for prompt, `run prompt`
// declaring `-p` for something else) would be a second surface. It is the same
// mistake a hand-kept list of protocol types would have been: the registry says
// one thing, a table somewhere else says another, and the two drift the first
// time somebody adds a parameter and updates only one of them. Worse here than
// there, because the drift is invisible — `-p` keeps working, it just stops
// meaning what the reader learned it meant.
//
// Deriving the letter from the first character of the name is worse still. The
// surface has `budget` and `base-url`, `to` and `tools` and `text` and `type`
// and `ttl`, `on` and `on-busy` and `on-missed` and `overlap`. Auto-assignment
// hands the letter to whichever entry the registry happens to list first, so
// `-b 5` means a spend ceiling today and a provider URL after somebody sorts
// the file alphabetically. A short flag whose meaning depends on the order of a
// slice is worse than no short flag at all: the script that used it does not
// fail, it silently does something else with the number.
//
// So: an explicit, global, collision-free assignment, verified by
// TestShortFlagsAreUnambiguous. Only names that are frequent or long enough to
// be worth typing get one. A parameter with no entry has no short form, which
// is the correct default — a letter nobody can guess is not a shorthand, it is
// a second thing to memorize.
var shortFlags = map[string]string{
	// The ones that appear over and over across the surface.
	"run":    "r", // 13 commands
	"name":   "n", // 8
	"budget": "b", // 4
	"key":    "k", // 3
	"text":   "t", // 3
	"model":  "m", // 3
	"prompt": "p", // the objective of a run
	"path":   "f", // a file; -p is taken by prompt and -f is what every tool uses

	// Frequent enough on their own commands to be worth it.
	"json":      "J",
	"sim":       "S",
	"workspace": "w",
	"status":    "s",
	"reason":    "R",
	"listen":    "l",
	"actor":     "a",
	"suite":     "u",
	"type":      "T",
	"event":     "e",
	"id":        "i",
	"value":     "v",
}

// A note on POSITIONAL parameters, because the first version of this map got it
// wrong and the mistake was instructive.
//
// -p (prompt), -f (path) and -r (run) abbreviate parameters declared with pos(),
// whose identity is arguably their position. The first attempt therefore refused
// short flags for positionals — and that deleted exactly the useful cases: -r
// reaches thirteen commands, and -p and -f are the two a person types most.
//
// So the rule is the other way round: a declared parameter is ALWAYS reachable
// by name, and the position is a convenience layered on top. `--actor` is not a
// spelling invented for the CLI's benefit; it is the name this registry already
// publishes to the tool schema and the protocol, so accepting it costs nothing
// and refusing it would be the surface contradicting itself.
//
// What that rule creates is an obligation on every parser: if the registry
// declares a parameter, --<name> must reach it. That is enforced by
// TestEveryShortFlagReachesItsParameter in cmd/iash rather than trusted, because
// the failure mode is silent — expandShort produces a long flag the parser drops,
// and the command runs with a value the user believes they supplied.

// Short returns the one-letter flag for a parameter, or "" if it has none.
//
// It takes the CLI spelling (`if-seq`, not `if_seq`) because that is the form
// the user types and the form Params carries. The wire has no short flags at
// all: a machine has no fingers to save, and `{"b": 5}` in a log is a puzzle
// where `{"budget": 5}` is a fact.
func Short(paramName string) string { return shortFlags[paramName] }

// ShortFlags returns the whole assignment, sorted by letter.
//
// Exported so `iash surface` and the help text can print it from the same place
// the parser reads, rather than from a list somebody keeps up to date by hand.
func ShortFlags() []Param {
	out := make([]Param, 0, len(shortFlags))
	for name := range shortFlags {
		out = append(out, Param{Name: name, Desc: shortFlags[name]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Desc != out[j].Desc {
			return out[i].Desc < out[j].Desc
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LongFor resolves a one-letter flag to the parameter name it abbreviates for a
// given command, or "" if that command has no such parameter.
//
// The command matters. `-r` is `run` everywhere `run` exists, but on a command
// with no `run` parameter it is not "some other parameter starting with r" — it
// is an error. Resolving it globally and letting the command ignore what it does
// not know is how `iash blueprint validate -r foo` would end up silently
// validating nothing.
func (c Cmd) LongFor(letter string) string {
	if letter == "" {
		return ""
	}
	for _, pp := range c.WireParams() {
		cliName := strings.ReplaceAll(pp.Name, "_", "-")
		if shortFlags[cliName] == letter {
			return cliName
		}
	}
	return ""
}
