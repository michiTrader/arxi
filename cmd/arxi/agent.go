package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/rolestore"
	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
	"github.com/michiTrader/arxi/internal/toolstore"
)

// policyDir is where per-agent tool policy overrides live. A var so tests can
// point it somewhere disposable, following providerDir and evalDir.
var policyDir = toolstore.DefaultDir

// agentDir is where the agents themselves live, for the same reason.
//
// Two directories and not one: an agent is a blueprint somebody wrote and
// commits, while a policy override is an operator's standing answer to "stop
// asking me about this" that run.go deliberately keeps OUTSIDE the frozen
// snapshot. Merging them would have to freeze both or neither.
var agentDir = agentstore.DefaultDir

func openPolicies() *toolstore.Store {
	s, err := toolstore.Open(policyDir)
	if err != nil {
		fatal(err)
	}
	return s
}

// openAgents is the writer's store: it creates agents/ if it is not there.
func openAgents() *agentstore.Store {
	s, err := agentstore.Open(agentDir)
	if err != nil {
		fatal(err)
	}
	return s
}

// readAgents is the reader's store, and it creates nothing.
//
// `agent list`, `agent show` and `run start <name>` report on the directory, and
// reporting on it must not bring it into existence -- the same rule overridesFor
// applies to policies/ below. agentstore.At explains what a reader needs
// prepared, which is nothing: a missing directory is no agents, and a missing
// file is ErrNotExist.
func readAgents() *agentstore.Store {
	s, err := agentstore.At(agentDir)
	if err != nil {
		fatal(err)
	}
	return s
}

const agentUsage = "usage: arxi agent create <name> [--model M] [--role R] [--tools a,b] [--advisory]\n" +
	"       arxi agent list [--json]\n" +
	"       arxi agent show <name> [--json]\n" +
	"       arxi agent tool policy --agent <name> [--allow <tool>] [--ask <tool>]\n" +
	"                              [--deny <tool>] [--reset <tool>]\n" +
	"  an agent is a blueprint with one member, stored in " + agentstore.DefaultDir + "/<name>.yaml.\n" +
	"  edit it by hand to grow it into a team: arxi blueprint validate checks it.\n" +
	"  --role names a role from `arxi role define`, whose defaults fill the flags you\n" +
	"  leave out; they are copied in as the agent is written, not looked up later.\n"

// cmdAgent routes the agent subcommands.
//
// All four declared paths are wired. The notImplemented fall-through at the
// bottom is not dead code: it is what answers `arxi agent bogus` with the list
// of words that do work, and it is where a fifth agent verb lands the day the
// surface declares one and before anybody writes it. Reaching for "unknown
// command" instead would blame the user for a typo they did not make.
func cmdAgent(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, agentUsage)
		os.Exit(2)
	}

	switch args[0] {
	case "create":
		cmdAgentCreate(args[1:])
		return
	case "list":
		cmdAgentList(args[1:])
		return
	case "show":
		cmdAgentShow(args[1:])
		return
	}

	if args[0] == "tool" {
		// `agent tool` is not itself a command: the registry declares only
		// `agent tool policy`. Answering a bare `agent tool` with the agent
		// subcommand list would print "tool is not an agent command" above a
		// list containing tool, which is why surface.SubcommandsUnder exists.
		if len(args) < 2 || args[1] != "policy" {
			fmt.Fprintf(os.Stderr, "arxi agent tool: expected \"policy\"\n"+
				"  usage: arxi agent tool policy --agent <name> [--allow <tool>]\n")
			os.Exit(2)
		}
		cmdAgentToolPolicy(args[2:])
		return
	}

	notImplemented(append([]string{"agent"}, args...))
}

// cmdAgentCreate writes a new agent: a blueprint with one member.
//
// Validation runs twice on purpose. Record.Validate is called here so a bad name
// or an unknown tool is a usage error (exit 2) about something the user typed,
// and agentstore.Create calls it again because every writer must pass through it
// -- including the agent-facing arxi_agent_create that serve will project from
// this same surface entry. The two can never disagree: this is a call to that
// one implementation, not a copy of its rules.
func cmdAgentCreate(args []string) {
	c := surface.Lookup("agent", "create")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent create: %v\n\n%s", err, agentUsage)
		os.Exit(2)
	}

	r := agentstore.Record{
		Name:     vals["name"],
		Model:    vals["model"],
		Role:     vals["role"],
		Tools:    splitTools(vals["tools"]),
		Advisory: vals["advisory"] == "true",
	}
	// The role is applied BEFORE Validate, so the record that is checked is the
	// record that will be written. The other order looks equivalent and is not: it
	// would let a role-supplied field reach the store without passing the one gate
	// this function claims covers everything the user is about to commit.
	rd := applyRole(&r, vals["role"])
	if err := r.Validate(); err != nil {
		// Checked before the store is touched, following `agent tool policy`: an
		// unknown tool is the invocation being wrong, not the filesystem. The tool
		// case is the one that earns the early check -- tool.ValidateGrants names
		// EVERY bad name at once, so `--tools reed,gerp` is one round trip and not
		// two, and no directory is created for a command that cannot succeed.
		fmt.Fprintf(os.Stderr, "arxi agent create: %v\n", err)
		os.Exit(2)
	}

	path, err := openAgents().Create(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent create: %v\n", err)
		if errors.Is(err, agentstore.ErrExists) {
			// Exit 2, and the existing file is the news. It may be an agent
			// somebody grew into a team, and what matters is that this command did
			// not touch it: a create that overwrote would destroy a member list,
			// a tool grant and a stage with no way back.
			fmt.Fprintf(os.Stderr, "  nothing was written. read the one that is there: "+
				"arxi agent show %s\n  or choose another name -- `agent create` never overwrites.\n", r.Name)
			os.Exit(2)
		}
		// Exit 1: the invocation was fine and the filesystem refused.
		os.Exit(1)
	}
	printAgentCreated(r, path, rd)
}

// roleDefaults records what a `--role` contributed, for the message printed after.
//
// Carried rather than recomputed, because "what did the role supply" cannot be
// read back off the finished record: an agent with `tools: [read]` looks identical
// whether the user typed `--tools read` or a role did, and the whole reason to say
// so is that the second one came out of a file the user is not looking at.
type roleDefaults struct {
	name        string   // the role that was named, defined or not
	found       bool     // it is defined
	hasDefaults bool     // it defines something -- an empty role is legal
	tools       bool     // the tool grant came from it
	advisory    bool     // it is what made this agent advisory
	path        string   // the file it came from
	defined     []string // what IS defined, when the named role is not
}

// applyRole fills the fields the user left blank from a defined role.
//
// Resolved here, at create time, and copied into the agent's YAML: the rendered
// file records the outcome and carries no reference back to roles/. rolestore's
// package doc argues that at length -- a `role:` meaning "read roles/reviewer.json
// when the run starts" would put part of a run's rules outside the snapshot
// `run start` freezes, so redefining a role would silently change agents somebody
// reviewed months earlier. The consequence here is the one worth printing: a role
// reaches the agents created after it and no others.
//
// An undefined role is a NOTE and not a refusal. `role:` is a free-form string
// that predates this store by the whole history of the tree: kernel.Decide picks
// the steer target by Role == "coordinator", builds each member's identity as
// "name (role)", and every blueprint in examples/ names roles nothing has ever
// defined. Refusing an unknown one would make `role define` a prerequisite for a
// field that never had one, and would break those files. Saying it is still worth
// the line -- it is the only spelling check `--role` can offer, which is the whole
// argument for letting an empty role be defined at all.
//
// A role that IS defined and does not load is exit 1, because the opposite choice
// loses something the user asked for: it would write an agent missing the defaults
// they named the role to get, and the only sign would be a note in output they
// have already scrolled past.
func applyRole(r *agentstore.Record, name string) roleDefaults {
	rd := roleDefaults{name: name}
	if strings.TrimSpace(name) == "" {
		return rd
	}

	st := readRoles()
	rec, err := st.Load(name)
	if errors.Is(err, rolestore.ErrNotExist) {
		// The error from Names is dropped, and the note degrades to "none defined".
		// Load has just answered ErrNotExist, which means the read got as far as a
		// missing file, so the directory is readable or absent -- and neither is
		// worth failing a create over when the agent is about to be written anyway.
		rd.defined, _ = st.Names()
		return rd
	}
	if err != nil {
		// The second line says why a bad role file stops a create rather than
		// degrading to one, and it deliberately does not repeat rolestore's own
		// "text a human can edit" -- that sentence is already on screen directly
		// above for a parse failure, and printing it twice reads as a machine
		// padding out an error it does not understand.
		fmt.Fprintf(os.Stderr, "arxi agent create: %v\n", err)
		fmt.Fprint(os.Stderr, "  no agent was written: --role names that file, so its defaults are part\n"+
			"  of this agent. fix it, or leave --role out and type the fields yourself.\n")
		os.Exit(1)
	}
	rd.found = true
	rd.path = st.Path(name)
	rd.hasDefaults = len(rec.Tools) > 0 || rec.Advisory

	// An explicit --tools wins, and the role only fills a blank: a flag typed just
	// now beats a file written last week. Note what cannot be spelled -- `--tools
	// ""` is indistinguishable from an absent flag after parseInvocation, and the
	// declaration has no negation, so an agent that must NOT have the role's grant
	// does not name the role.
	//
	// Nothing re-validates rec.Tools, and that is not an omission: rolestore.Load
	// runs the same tool.ValidateGrants on the way in, so a name that reaches here
	// has already passed the check r.Validate is about to repeat. It is also why
	// Load validates at all -- refused there, the message can name the role's file;
	// refused here, it would blame a `--tools` flag the user never typed.
	if len(r.Tools) == 0 && len(rec.Tools) > 0 {
		r.Tools = rec.Tools
		rd.tools = true
	}

	// Advisory is OR'd rather than assigned, and the asymmetry is real: --advisory
	// can only add the trait, so an advisory role cannot be declined for one agent.
	// `role define --advisory` prints that, and printAgentCreated says it again in
	// the caveat, because the consequence is an agent that never takes a turn.
	if rec.Advisory && !r.Advisory {
		r.Advisory = true
		rd.advisory = true
	}
	return rd
}

// printRoleInherited says what the role gave this agent, and where it came from.
//
// Printed for a defined role that supplied nothing, too. A role is defaults for
// flags the user did not type, so `--role auditor --tools read` legitimately
// inherits nothing at all, and silence there is indistinguishable from the role
// having been ignored -- which is the bug this line would be the only way to see.
func printRoleInherited(rd roleDefaults) {
	if rd.name == "" {
		return
	}

	if !rd.found {
		fmt.Printf("  role:   %s is not defined, so no defaults were applied. that is not\n"+
			"          an error: `role:` is a free-form label, and the blueprints in this\n"+
			"          tree name roles nobody defined. it is how a misspelling shows up.\n", rd.name)
		if len(rd.defined) == 0 {
			fmt.Printf("          no roles are defined yet: arxi role define %s --tools read\n", rd.name)
		} else {
			fmt.Printf("          defined: %s\n", strings.Join(rd.defined, ", "))
		}
		return
	}

	switch {
	case rd.tools && rd.advisory:
		fmt.Printf("  role:   %s supplied the tool grant and advisory (%s).\n", rd.name, rd.path)
	case rd.tools:
		fmt.Printf("  role:   %s supplied the tool grant (%s).\n", rd.name, rd.path)
	case rd.advisory:
		fmt.Printf("  role:   %s supplied advisory (%s).\n", rd.name, rd.path)
	case !rd.hasDefaults:
		fmt.Printf("  role:   %s is defined with no defaults, so nothing was applied --\n"+
			"          the name alone is what makes a misspelling of it visible.\n", rd.name)
		return
	default:
		fmt.Printf("  role:   %s is defined and supplied nothing: the command line already\n"+
			"          named every field it sets, and an explicit flag wins.\n", rd.name)
		return
	}
	fmt.Print("          copied as this agent was written, so redefining the role later\n" +
		"          will not change this one.\n")
}

// printAgentCreated says what was written, where, and what to do next.
//
// The policy of each granted tool is on the first line because it is the one
// thing about the new agent that the flags do not already say: `--tools
// read,write` looks like two equal grants, and `write` is mutating, so it comes
// out `ask` and the run will stop for an approval the operator did not expect.
// §20.2 prints it here for that reason.
//
// roleDefaults is threaded in rather than re-derived, because a field the role
// supplied is indistinguishable from one the user typed by the time the record is
// written -- and after `--role auditor` the grant on that first line may be one
// nobody on this command line asked for.
func printAgentCreated(r agentstore.Record, path string, rd roleDefaults) {
	fmt.Printf("agent %s created (%s)\n", r.Name, grantSummary(r.Tools, overridesFor(r.Name)))
	fmt.Printf("  file:   %s\n", path)
	printRoleInherited(rd)

	// The next step is printed with --model already in it when the agent has no
	// model, rather than as advice next to a command that would fail. arxi
	// invents no default model anywhere -- that would be a spend decision taken
	// in the binary, where upgrading could change what an unchanged command costs
	// -- so checkEveryMemberHasAModel refuses a live run that names none, minutes
	// and one objective after this command is the wrong place to learn it.
	next := fmt.Sprintf("arxi run start %s \"<objective>\" --budget 5.00", r.Name)
	if r.Model == "" {
		next += " --model <id>"
		fmt.Printf("  note:   no model, so a live run needs one on the command line.\n" +
			"          add `model:` to the file to stop repeating it (see: arxi model list).\n")
	}

	// An advisory agent is not given `run it:`, because running it is the one
	// thing this file cannot do.
	//
	// applyRunStarted activates the members of the entered stage and an advisory
	// member starts MemberInactive, so a run of this file alone enters `work`,
	// activates nobody, and records run.quiescent -- "nobody is working and nobody
	// can start" -- after zero turns. Printing the command anyway would send the
	// operator to a failed run to discover a property of --advisory that was known
	// here, which is the same false promise as the missing stage: everything looks
	// like it worked until the log says nothing happened.
	//
	// The role gets a second mention inside the caveat, having already had one
	// above, for the case that earns it: an operator who typed no --advisory is
	// reading a caveat about a flag they did not use, and the answer to "then turn
	// it off" is that they cannot. There is no --no-advisory in the declaration, so
	// the only way out is a different role or none.
	if r.Advisory {
		fmt.Printf("  caveat: advisory -- it answers when something wakes it and takes no "+
			"turn on\n"+
			"          its own, so `arxi run start %s` alone enters the stage,\n"+
			"          activates nobody, and goes quiescent after zero turns.\n", r.Name)
		if rd.advisory {
			fmt.Printf("          the role is what made it advisory, and there is no " +
				"--no-advisory:\n" +
				"          an agent that must work names another role, or none.\n")
		}
		fmt.Print("  use it: add this member to a blueprint that has one who works, plus a\n" +
			"          watcher to wake it (see examples/feature-team.yaml).\n")
		return
	}
	fmt.Printf("  run it: %s\n", next)
}

// splitTools reads a `--tools a,b` value.
//
// Empty entries are dropped instead of being passed through. `--tools read,` is a
// trailing comma somebody typed, and grant validation would otherwise refuse it
// with `unknown tool ""` -- an error naming a tool the user never wrote, about a
// character they can barely see.
//
// Whitespace is trimmed for the same reason: `--tools "read, grep"` is one shell
// argument with a space in it, and refusing " grep" would be refusing a spelling
// the shell handed over, not one the user chose.
//
// Duplicates are kept. `--tools read,read` renders `tools: [read, read]`, which
// the blueprint schema accepts and tool.Resolve answers identically for, so
// there is nothing to protect against; ResolveAll de-duplicates for display.
func splitTools(csv string) []string {
	var out []string
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// grantSummary describes a tool grant as the resolved policy of each tool.
//
// Every tool carries its policy even when they all agree, rather than naming
// only the exceptions. The exception form -- `tools: read, write (bash: ask)` --
// is shorter and is what §20.2 first drafted, but it asks the reader to already
// know the default table in order to understand what the unmarked names mean,
// and the default table is exactly what this line exists to tell them. It is
// also the form that made the doc's own example wrong: it listed `write`
// unmarked beside `read`, and `write` is mutating, so it resolves to ask.
//
// Order is tool.ResolveAll's, which sorts, for the reason its comment gives: a
// resolved policy listing that reorders itself between runs cannot be diffed
// against what the same agent could do last week.
func grantSummary(granted []string, overrides map[string]surface.Policy) string {
	if len(granted) == 0 {
		return "no tools"
	}
	return "tools: " + grantList(granted, overrides)
}

// grantList is the same listing without the `tools: ` prefix, for a field block
// whose label already says the word.
//
// Two annotations are attached here rather than left for the reader to deduce:
//
// `override` marks a policy that came from policies/<member>.json instead of the
// default table. Without it the line is indistinguishable from the same tool
// resolving that way on its own, and the two are not the same fact: one is undone
// with `--reset`, the other cannot be undone at all. Presence in the map is the
// test, deliberately, not difference from the default -- an override that agrees
// with today's default is still a file on disk that will keep applying if the
// default changes, and it is still the thing `--reset` removes.
//
// `not a known tool` marks a name the runtime has no implementation for.
// `agent create` refuses those, but a hand-edited `tools: [reed]` reaches here,
// and every other layer stays silent about it: blueprint.Load takes any string
// list, and ResolveAll answers `allow` for an unknown grant rather than failing.
// So the operator's typo looks like a granted tool everywhere except at the
// moment an agent calls it. This screen is where that is cheap to learn.
func grantList(granted []string, overrides map[string]surface.Policy) string {
	if len(granted) == 0 {
		return "none"
	}
	rs := tool.ResolveAll(granted, overrides)
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		notes := ""
		if _, ok := overrides[r.Tool]; ok {
			notes += ", override"
		}
		if !tool.Known[r.Tool] {
			notes += ", not a known tool"
		}
		parts = append(parts, fmt.Sprintf("%s (%s%s)", r.Tool, r.Policy, notes))
	}
	return strings.Join(parts, ", ")
}

// overridesFor reads one agent's policy overrides without creating the store.
//
// A read must not have the side effect of making a directory. toolstore.Open
// MkdirAll's, so `agent show` in a repository that has never set a policy would
// leave an empty policies/ behind, and the next `git status` would ask the user
// about a directory they did not create. os.Stat answers absence instead, and
// absence is a complete answer: there can be no override inside a directory that
// is not there.
//
// Errors resolve to "no overrides", and that is safe only because this is the
// echo of a policy and never the enforcement of one. A live run reads the same
// store through openPolicies().LoadAll() and fatals when it cannot, so an
// unreadable policies/ stops the run rather than being quietly ignored. Failing
// `agent show` over it would withhold the model, the tools and the path --
// everything the command was asked for -- to report on a file it was not.
func overridesFor(agent string) map[string]surface.Policy {
	if _, err := os.Stat(policyDir); err != nil {
		return nil
	}
	rec, err := openPolicies().Load(agent)
	if err != nil {
		return nil
	}
	return rec.Tools
}

// cmdAgentList prints one row per stored agent.
//
// It deliberately does NOT read the policy store, and §20.2 says why: the table
// is a directory listing, and a resolved policy per tool would not fit a column
// anyway. The second reason is the one the code cares about -- reading policies
// here would create policies/ as a side effect of listing agents.
func cmdAgentList(args []string) {
	c := surface.Lookup("agent", "list")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent list: %v\n\n%s", err, agentUsage)
		os.Exit(2)
	}

	entries, err := readAgents().List()
	if err != nil {
		// Exit 1: the invocation was fine, the directory is what could not be
		// read. A missing directory is not this error -- Names answers that with
		// no names and no error, so a fresh repository prints the empty case.
		fmt.Fprintf(os.Stderr, "arxi agent list: %v\n", err)
		os.Exit(1)
	}

	if vals["json"] == "true" {
		out := make([]agentJSON, 0, len(entries))
		for _, e := range entries {
			out = append(out, agentPayload(e, false))
		}
		emitJSON(out)
		return
	}

	// An empty list says so, following `trigger list` and `model list`. A bare
	// header row is the output that makes a user wonder whether the command
	// worked, and "there are none yet" is precisely what it fails to say.
	if len(entries) == 0 {
		fmt.Printf("no agents in %s/\n", agentDir)
		fmt.Println("  create one: arxi agent create reviewer " +
			"--model claude-sonnet-4-6 --tools read,grep")
		return
	}

	printAgentTable(entries)
}

// printAgentTable writes the NAME/MODEL/TOOLS/ADVISORY table.
//
// Widths are measured rather than fixed, for the reason `trigger list` gives: a
// hardcoded %-16s turns into a ragged table the first time somebody uses a
// longer name, and a model id like `anthropic/claude-sonnet-4-6` is longer than
// any width guessed here.
//
// A file that does not load still gets a row. Skipping it would answer "which
// agents do I have" by leaving one out, and the name is the thing the operator
// needs in order to fix it; the cause does not fit a cell, so it goes in the
// footer where a multi-line validation error can be read.
func printAgentTable(entries []agentstore.Entry) {
	rows := [][4]string{{"NAME", "MODEL", "TOOLS", "ADVISORY"}}
	broken := 0
	for _, e := range entries {
		if e.Err != nil {
			broken++
			rows = append(rows, [4]string{e.Name, "-", "unreadable -- see below", "-"})
			continue
		}
		rows = append(rows, agentRow(e))
	}

	var w [4]int
	for _, r := range rows {
		for i, cell := range r {
			if len(cell) > w[i] {
				w[i] = len(cell)
			}
		}
	}

	var b strings.Builder
	for _, r := range rows {
		for i, cell := range r {
			if i == len(r)-1 {
				b.WriteString(cell) // no trailing padding on the last column
				continue
			}
			fmt.Fprintf(&b, "%-*s  ", w[i], cell)
		}
		b.WriteString("\n")
	}
	fmt.Print(b.String())

	if broken == 0 {
		return
	}
	fmt.Printf("\n%d of %d could not be read:\n", broken, len(entries))
	for _, e := range entries {
		if e.Err == nil {
			continue
		}
		fmt.Printf("  %s: %v\n", e.Path, e.Err)
	}
	fmt.Println("  each is an ordinary blueprint: arxi blueprint validate <path> " +
		"reports the same thing, and `run start` on that name would fail with it.")
}

// agentRow reduces one loaded agent to a table row.
//
// A one-member file -- what `agent create` writes -- reports that member's model,
// grant and advisory flag. A file somebody grew into a team cannot: three members
// can name three models and disagree about advisory, and printing members[0]
// under the agent's name would present one member as the whole file. So a grown
// file says how many members it has and leaves the detail to `agent show`, which
// prints every one of them.
//
// No members at all is a real row and not an impossible one: blueprint.Load
// accepts an empty member list on purpose (its comment says a run with no members
// is caught when the run starts), so a hand-edited file can reach here loading
// cleanly and still be unrunnable. Saying `no members` is how the operator learns
// that before `run start` tells them.
func agentRow(e agentstore.Entry) [4]string {
	ms := e.Blueprint.Config.Members
	switch len(ms) {
	case 0:
		return [4]string{e.Name, "no members", "-", "-"}
	case 1:
		return [4]string{e.Name, dash(ms[0].Model), dash(toolsCell(ms[0].Tools)), yesNo(ms[0].Advisory)}
	}

	// The union of the grants, because "what this file may do" is still one
	// answer. Advisory is not a union: `mixed` says the members disagree rather
	// than picking whichever value came first.
	var union []string
	advisory, mixed := ms[0].Advisory, false
	for _, m := range ms {
		union = append(union, m.Tools...)
		if m.Advisory != advisory {
			mixed = true
		}
	}
	flag := yesNo(advisory)
	if mixed {
		flag = "mixed"
	}
	return [4]string{e.Name, fmt.Sprintf("team of %d", len(ms)), dash(toolsCell(union)), flag}
}

// toolsCell lists a grant in ResolveAll's order, without the policies.
//
// The order is shared with `agent show` by calling the same function, so the two
// screens list one agent's tools identically -- and de-duplicated, because a
// hand-written `tools: [read, read]` grants nothing twice.
func toolsCell(granted []string) string {
	rs := tool.ResolveAll(granted, nil)
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Tool)
	}
	return strings.Join(names, ", ")
}

// dash renders an empty cell as `-` so a column never looks truncated.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// yesNo spells a bool the way §20.2's ADVISORY column does.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// agentJSON is one stored agent as a machine reader sees it.
//
// Members is a list of the config's own members rather than a flattened
// model/tools/advisory triple, and that is what keeps the document honest for a
// file that has grown: three members can name three models, and something
// flattened would have to pick one and call it the agent's. The member fields are
// kernel.MemberConfig's own tags, so an agent's members are spelled here exactly
// as `run show --json` and `serve` spell a running one's.
type agentJSON struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	SHA     string            `json:"sha,omitempty"`
	Members []agentMemberJSON `json:"members"`
	Stages  []string          `json:"stages,omitempty"`

	// Error is the file's own failure, carried instead of aborting the document.
	// `agent list --json` over a directory with one broken file must still list
	// the others, for the reason agentstore.Entry gives, and a reader that got a
	// shorter array with no explanation would not know one was missing.
	Error string `json:"error,omitempty"`
}

// agentMemberJSON is a member plus the policy each of its tools resolves to.
//
// Resolved is filled by `agent show` and absent from `agent list`, which does not
// read the policy store at all. Emitting it there would report every tool at its
// default and look like an answer about overrides rather than the absence of one.
type agentMemberJSON struct {
	kernel.MemberConfig
	Resolved []tool.Resolved `json:"resolved,omitempty"`
}

// agentPayload builds the JSON document for one entry.
//
// withPolicy is a parameter and not two functions because the shape must not
// diverge: a caller that unmarshals `agent list --json` and `agent show --json`
// into the same type is doing the obvious thing, and it works.
func agentPayload(e agentstore.Entry, withPolicy bool) agentJSON {
	out := agentJSON{Name: e.Name, Path: e.Path}
	if e.Err != nil {
		out.Error = e.Err.Error()
		return out
	}

	out.SHA = e.Blueprint.SHA
	out.Members = make([]agentMemberJSON, 0, len(e.Blueprint.Config.Members))
	for _, m := range e.Blueprint.Config.Members {
		am := agentMemberJSON{MemberConfig: m}
		if withPolicy {
			// Keyed by the MEMBER's name, not the agent's, because that is the key
			// the executor resolves with: provider.Executor does
			// tool.Resolve(m.Tools, x.ToolPolicy[agent], name) with the member name
			// as agent. For a one-member file the two are the same word; for a
			// grown one, using the file's name here would print a policy the run
			// would not apply.
			am.Resolved = tool.ResolveAll(m.Tools, overridesFor(m.Name))
		}
		out.Members = append(out.Members, am)
	}
	for _, s := range e.Blueprint.Config.Stages {
		out.Stages = append(out.Stages, s.Name)
	}
	return out
}

// cmdAgentShow prints one agent as it will actually run.
//
// It renders the loaded blueprint rather than the five fields `agent create` was
// given, which is agentstore.Load's whole reason for returning a *Blueprint: the
// file is authoritative. Somebody may have added a member, a stage or a watcher,
// and an `agent show` that described the create-time flags would keep answering
// about an agent that no longer exists.
func cmdAgentShow(args []string) {
	c := surface.Lookup("agent", "show")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent show: %v\n\n%s", err, agentUsage)
		os.Exit(2)
	}

	name := vals["name"]
	st := readAgents()
	bp, err := st.Load(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent show: %v\n", err)
		if errors.Is(err, agentstore.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "  what is stored: arxi agent list\n"+
				"  or write it:    arxi agent create %s --model <id> --tools read\n", name)
		} else {
			// Not reported as a missing agent, which is what the store's own
			// comment asks of this handler: a permission error and an unparseable
			// file are neither "no such agent" nor a wrong invocation, and telling
			// somebody their agent does not exist when the file is right there
			// sends them to `agent create` to make one they already have.
			fmt.Fprintf(os.Stderr, "  it did not load. an agent is an ordinary blueprint, so\n"+
				"  arxi blueprint validate %s reports the same thing.\n", st.Path(name))
		}
		// Exit 1 both ways: the invocation was correct and the store is what is
		// wrong. Exit 2 would claim the user mistyped the command, and `agent show
		// ghost` is a well-formed question with a negative answer.
		os.Exit(1)
	}

	e := agentstore.Entry{Name: name, Path: st.Path(name), Blueprint: bp}
	if vals["json"] == "true" {
		emitJSON(agentPayload(e, true))
		return
	}
	printAgent(e)
}

// printAgent writes the human form of `agent show`, in §20.2's shape.
func printAgent(e agentstore.Entry) {
	cfg := e.Blueprint.Config
	fmt.Printf("agent %s\n", e.Name)
	agentField("  ", "file", e.Path)
	// Twelve characters, as `run list` abbreviates a digest. The full one is in
	// --json, where a reader comparing it against a run's frozen snapshot lives.
	agentField("  ", "sha", shortSHA(e.Blueprint.SHA))

	// The filename is what `run start <name>` resolves; the blueprint's own `name:`
	// is what the run records and `run show` prints back. `agent create` writes
	// them equal, and a hand edit can part them, so a difference is stated here
	// rather than surfacing later as a run named after something else.
	if e.Blueprint.Name != "" && e.Blueprint.Name != e.Name {
		agentField("  ", "name", e.Blueprint.Name+"  (the file's own name:, which is what a run records)")
	}

	switch len(cfg.Members) {
	case 0:
		// The file loads: blueprint.Load accepts an empty member list on purpose
		// and leaves the refusal to `run start`. Saying so here is the difference
		// between learning it now and learning it with an objective and a budget
		// already typed.
		agentField("  ", "members", "none -- this file has nobody in it, and `run start` refuses that")
		fmt.Printf("            add one: `members:` then `  - {name: %s, tools: [read]}`\n", e.Name)
	case 1:
		// The member's name is what everything else addresses: `run steer <run>
		// <member>`, `agent tool policy --agent <member>`, and a watcher's `agent:`
		// key. The file name is what `run start` resolves. `agent create` writes one
		// word into both, so a difference here is a hand edit, and it is worth a
		// sentence because the failure it causes is a command that reports no such
		// member for an agent the operator can see in `agent list`.
		if m := cfg.Members[0]; m.Name != e.Name {
			agentField("  ", "member", m.Name+"  (steer and policy address this, not "+e.Name+")")
		}
		printAgentMember(cfg.Members[0], "  ")
	default:
		agentField("  ", "members", fmt.Sprintf("%d -- this file has grown past one agent", len(cfg.Members)))
		for _, m := range cfg.Members {
			fmt.Printf("  - %s\n", m.Name)
			printAgentMember(m, "      ")
		}
	}

	if len(cfg.Stages) > 0 {
		names := make([]string, 0, len(cfg.Stages))
		for _, s := range cfg.Stages {
			names = append(names, s.Name)
		}
		agentField("  ", "stages", strings.Join(names, " -> "))
	}
	printAgentOverrideNote(cfg.Members)
}

// printAgentMember prints one member's fields, indented under its heading.
//
// The indent is a parameter rather than a constant because the same block is
// written twice: flush under a one-member file, where the member IS the agent and
// a nesting level would be ceremony, and indented under a name in a grown one,
// where three members' fields with the same left edge would read as one member
// with three models.
func printAgentMember(m kernel.MemberConfig, indent string) {
	// An absent model is stated, not left blank, and it names the flag that
	// supplies one. arxi invents no default model: checkEveryMemberHasAModel
	// refuses a live run whose members name none and whose command line has no
	// --model, so a blank cell here would hide the one thing standing between this
	// file and a run.
	model := m.Model
	if model == "" {
		model = "none -- a live run needs --model on the command line"
	}
	agentField(indent, "model", model)

	// Role is printed only when set. A `role:` line reading `-` invites the reader
	// to wonder what an empty role does, and the answer is nothing: the field is
	// optional and its absence is not a defect.
	if m.Role != "" {
		agentField(indent, "role", m.Role)
	}
	agentField(indent, "tools", grantList(m.Tools, overridesFor(m.Name)))
	agentField(indent, "advisory", yesNo(m.Advisory))

	// Activation is printed even though `agent create` never writes it, because
	// blueprint.Load resolves it: a file that says nothing about activation still
	// runs with one, and this screen reports how the agent will run rather than
	// which lines its file contains. The guard is for a Config that stops resolving
	// it, not for a file that omits it.
	if m.Activation != "" {
		agentField(indent, "activation", m.Activation)
	}
	if len(m.Stages) > 0 {
		agentField(indent, "stages", strings.Join(m.Stages, " -> "))
	}
}

// agentField prints one `label: value` line in §20.2's column.
//
// The value starts at column 10 of the indent, which is what §20.2's `model:
// claude-sonnet-4-6` and `advisory: no` lines both do -- `advisory:` is nine
// characters and lands exactly one space short of it.
//
// The padding is `%-9s ` and not `%-10s` because of `activation:`, which is
// eleven: %-10s pads to a width the label already exceeds and prints the value
// flush against the colon, `activation:always`. Padding to nine with a separate
// space is byte-identical for every label that fits and still leaves one space
// for the one that does not -- where widening the column to the longest label
// would instead move every line off the shape the doc prints.
func agentField(indent, label, value string) {
	fmt.Printf("%s%-9s %s\n", indent, label+":", value)
}

// printAgentOverrideNote closes `agent show` when a policy file is in play.
//
// The per-tool `override` marks say which tools; this says where they are and how
// to undo them, once, instead of repeating a path on every marked line.
//
// It also states the thing that separates a policy from a blueprint: overrides are
// read at `run start` from policies/ and are NOT copied into the frozen snapshot,
// so this screen can change tomorrow while the file it describes does not. A
// reader who assumes the snapshot froze the whole picture would expect a replay to
// reproduce a denial that a `--reset` has since removed.
func printAgentOverrideNote(ms []kernel.MemberConfig) {
	type ref struct{ agent, tool string }
	var applied, dead []ref
	for _, m := range ms {
		ov := overridesFor(m.Name)
		names := make([]string, 0, len(ov))
		for t := range ov {
			names = append(names, t)
		}
		// Map order is randomised per run, and the example command below names one
		// of these. Sorting keeps `agent show` reproducible between two invocations
		// that read the same file, which is what makes its output diffable.
		sort.Strings(names)
		for _, t := range names {
			if grantsTool(m.Tools, t) {
				applied = append(applied, ref{m.Name, t})
			} else {
				dead = append(dead, ref{m.Name, t})
			}
		}
	}
	all := append(append([]ref{}, applied...), dead...)
	if len(all) == 0 {
		return
	}

	agentField("  ", "policy", fmt.Sprintf("%d override(s) in %s",
		len(all), filepath.Join(policyDir, all[0].agent+".json")))
	fmt.Println("            read at run start and not frozen into the snapshot, so this")
	fmt.Println("            can change without the blueprint changing.")
	if len(dead) > 0 {
		// An override cannot widen a grant: Resolve returns deny for a tool the
		// member does not list, before it ever looks at the overrides map. A
		// `--allow bash` against an agent with no bash therefore sits in the file
		// doing nothing, and the reason it looks like it should work is that the
		// command accepted it.
		fmt.Printf("            %d name a tool the member does not grant, so they do nothing:\n"+
			"            an override changes a granted tool's policy, it cannot add one.\n"+
			"            first: %s %s -- add it to `tools:` or reset it.\n",
			len(dead), dead[0].agent, dead[0].tool)
	}
	fmt.Printf("            undo one: arxi agent tool policy --agent %s --reset %s\n",
		all[0].agent, all[0].tool)
}

// grantsTool reports whether a member's grant lists a tool.
//
// tool.Resolve answers the same question internally with an unexported helper.
// Duplicating four lines is better than exporting it: the export would invite a
// caller to make a policy decision out of the answer, which is Resolve's job and
// not a caller's, and here the answer is only ever used to explain a screen.
func grantsTool(granted []string, name string) bool {
	for _, g := range granted {
		if g == name {
			return true
		}
	}
	return false
}

// shortSHA abbreviates a digest to twelve characters, as `run list` does.
//
// The full digest is in --json. Twelve is long enough to compare against a run's
// blueprint.snapshot.sha by eye and short enough not to wrap the line; a reader
// checking that a run really started from this file wants the comparison, not the
// other fifty-two characters.
func shortSHA(sha string) string {
	if sha == "" {
		return "-"
	}
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// errUnresolvedActor reports an actor argument that names neither a file on disk
// nor a stored agent.
//
// A sentinel rather than a plain error because `run start` prints two different
// things. A blueprint that failed to load gets `blueprint is not valid` and the
// validation report; a name nothing answers to is not invalid -- there is nothing
// to validate -- and telling somebody who mistyped an agent name that their
// blueprint is invalid sends them looking for a file that does not exist.
var errUnresolvedActor = errors.New("no such blueprint file and no such stored agent")

// resolveActor turns `run start`'s first argument into a blueprint.
//
// A path is tried first and wins. The precedence is only ever visible in one
// situation -- a file named `reviewer` in the working directory beside an
// `agents/reviewer.yaml` -- and the file wins for two reasons. It is the more
// specific reading, because the user typed something that is literally there; and
// the opposite order lets a stored agent silently shadow the file somebody is in
// the middle of editing, which is the failure that takes an hour to see. `run
// start ./reviewer.yaml` remains the way to be explicit either way.
func resolveActor(actor string) (*blueprint.Blueprint, error) {
	// A directory is not the file. `run start .`, or a directory that happens to
	// share a name with an agent, must not shadow agents/<name>.yaml with something
	// that cannot be a blueprint: LoadFile would answer "is a directory", which is
	// true and tells the user nothing they can act on.
	if fi, err := os.Stat(actor); err == nil && !fi.IsDir() {
		return blueprint.LoadFile(actor)
	}

	// Something that still looks like a filename goes to LoadFile anyway, even
	// though the stat just failed, so the error names the file that was typed.
	// Handing `blueprints/team.yaml` to the store instead would report "no agent
	// named blueprints/team.yaml" -- an answer about a lookup nobody asked for, and
	// one the store would refuse for its separators before it even looked.
	if looksLikePath(actor) {
		return blueprint.LoadFile(actor)
	}

	bp, err := readAgents().Load(actor)
	if errors.Is(err, agentstore.ErrNotExist) {
		// Wrapped, not replaced: the sentinel is what run start branches on, and
		// the quoted name is what makes the message readable when the argument is
		// an empty string or has a space in it.
		return nil, fmt.Errorf("%q: %w", actor, errUnresolvedActor)
	}
	// Every other error is passed through unchanged -- a permission problem, or a
	// file that parsed and failed validation. Those are load failures and belong
	// under `run start`'s existing report, which prints a ValidationError's whole
	// list of problems.
	return bp, err
}

// looksLikePath says whether an argument was meant as a filename.
//
// A separator settles it. Beyond that only `.yaml` and `.yml` count, and not any
// extension at all: filepath.Ext("v1.2") is ".2", so a stored agent named `v1.2`
// would be sent to the filesystem and reported missing while its file sits in
// agents/. The narrow test costs a user who wrote `team.txt` nothing, because that
// file existing is what the stat above already answered.
func looksLikePath(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	switch strings.ToLower(filepath.Ext(s)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// cmdAgentToolPolicy sets or clears one tool policy override for one agent.
//
// This is the second remedy `run why` prints, and §20.2 recommends it by name:
// when an approval will recur every turn, fix the policy instead of the symptom.
// Before this existed the recommendation was a dead end -- the loop it escapes
// had no exit, because approving a tool call respawns the turn while the policy
// still says ask, so the model calls the tool and is asked again.
//
// It is CLI-only, and that is a security boundary rather than an omission
// (§20.12): an agent that can widen its own tool policy does not have a policy.
// The registry says Kind: CLIOnly, so this is never exposed as a tool and never
// reachable over the protocol.
func cmdAgentToolPolicy(args []string) {
	args, err := expandShort(surface.Lookup("agent", "tool", "policy"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: %v\n", err)
		os.Exit(2)
	}

	var agent string
	// change is a SLICE and not a map, and that is a defect this command shipped
	// with for one commit. Keyed by tool name, `--allow bash --deny bash`
	// collapsed to a single entry, so the "one tool at a time" refusal below
	// counted one change and applied whichever flag was written last. A
	// contradiction the command claimed to refuse was silently resolved by map
	// assignment order.
	//
	// Found by running it, not by testing it: every unit assertion about the
	// guard passed, because they all used two DIFFERENT tools.
	var change []toolChange
	var reset []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func(name string) string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "arxi agent tool policy: %s needs a value\n", name)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		switch {
		case a == "--agent":
			agent = val("--agent")
		case strings.HasPrefix(a, "--agent="):
			agent = strings.TrimPrefix(a, "--agent=")
		case a == "--allow":
			change = append(change, toolChange{val("--allow"), surface.PolicyAllow})
		case strings.HasPrefix(a, "--allow="):
			change = append(change, toolChange{strings.TrimPrefix(a, "--allow="), surface.PolicyAllow})
		case a == "--ask":
			change = append(change, toolChange{val("--ask"), surface.PolicyAsk})
		case strings.HasPrefix(a, "--ask="):
			change = append(change, toolChange{strings.TrimPrefix(a, "--ask="), surface.PolicyAsk})
		case a == "--deny":
			change = append(change, toolChange{val("--deny"), surface.PolicyDeny})
		case strings.HasPrefix(a, "--deny="):
			change = append(change, toolChange{strings.TrimPrefix(a, "--deny="), surface.PolicyDeny})
		case a == "--reset":
			reset = append(reset, val("--reset"))
		case strings.HasPrefix(a, "--reset="):
			reset = append(reset, strings.TrimPrefix(a, "--reset="))
		default:
			fmt.Fprintf(os.Stderr, "arxi agent tool policy: unexpected %q\n"+
				"  usage: arxi agent tool policy --agent <name> "+
				"[--allow <tool>] [--ask <tool>] [--deny <tool>] [--reset <tool>]\n", a)
			os.Exit(2)
		}
	}

	if strings.TrimSpace(agent) == "" {
		// --agent is declared required in the surface, and it is required for a
		// blunt reason: a policy change with no agent has no meaning, and
		// guessing one from context would be guessing about authorization.
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: --agent is required\n"+
			"  a policy belongs to one agent, and there is no default\n")
		os.Exit(2)
	}

	if len(change)+len(reset) == 0 {
		// Showing the current policy would be a reasonable thing to do here,
		// but `agent show` is the declared way to read one and this command
		// declares itself as a change. Printing on no flags would make the
		// mutating command dual-purpose, and the surface says what it is.
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: nothing to change\n"+
			"  say what the policy should be: --allow, --ask, --deny or --reset <tool>\n")
		os.Exit(2)
	}

	if len(change)+len(reset) > 1 {
		// Refused rather than applied in some order. Two flags in one command
		// means the operator is thinking of two changes, and if they conflict
		// -- `--allow bash --deny bash` -- there is no reading of the command
		// line that says which one they meant.
		var named []string
		for _, c := range change {
			named = append(named, fmt.Sprintf("%s=%s", c.tool, c.policy))
		}
		for _, r := range reset {
			named = append(named, r+"=default")
		}
		sort.Strings(named)
		fmt.Fprintf(os.Stderr, "arxi agent tool policy: one tool at a time (got %s)\n"+
			"  two policy flags in one command have no order that is written down;\n"+
			"  run it twice instead\n", strings.Join(named, ", "))
		os.Exit(2)
	}

	// Tool names are checked before the store is touched, so an unknown tool is
	// a usage error (exit 2) naming the flag the user typed, rather than the
	// store's own error routed through fatal (exit 1). The store keeps its check
	// regardless: that one protects a hand-edited file, and this one answers a
	// person at a keyboard.
	for _, c := range change {
		requireKnownTool(c.tool)
	}
	for _, r := range reset {
		requireKnownTool(r)
	}

	store := openPolicies()

	// --reset first, because it is the only branch that takes the other path
	// through the store.
	for _, name := range reset {
		had, err := store.Clear(agent, name)
		if err != nil {
			fatal(err)
		}
		if !had {
			// Not an error: the end state is what was asked for. But saying so
			// matters, because the operator may have typed the wrong agent, and
			// "done" would let them believe a change had landed.
			fmt.Printf("%s: %s had no override, so nothing changed.\n", agent, name)
			fmt.Printf("  it resolves to %s, which is the default for %s.\n",
				defaultPolicyOf(name), name)
			return
		}
		fmt.Printf("%s: %s is back to its default policy (%s).\n",
			agent, name, defaultPolicyOf(name))
		printNotRetroactive()
		return
	}

	for _, c := range change {
		name, pol := c.tool, c.policy
		prev, had, err := store.Set(agent, name, pol)
		if err != nil {
			fatal(err)
		}
		def := defaultPolicyOf(name)
		switch {
		case had && prev == pol:
			fmt.Printf("%s: %s was already %s. nothing changed.\n", agent, name, pol)
			return
		case !had && pol == def:
			// An override that restates the default is stored, and saying so
			// matters. The walk printed "read is now allow (the default is
			// allow)" for it, which reads as a change and is not one: the
			// resolver would answer identically with no override at all.
			//
			// It is not refused, because an operator pinning a default against a
			// future change to it is making a real decision. It is just not
			// described as an effect it does not have.
			fmt.Printf("%s: %s is %s, which is already the default. "+
				"the override is recorded but changes nothing.\n", agent, name, pol)
			fmt.Printf("  remove it with: arxi agent tool policy --agent %s --reset %s\n",
				agent, name)
			return
		case had:
			fmt.Printf("%s: %s is now %s (was %s).\n", agent, name, pol, prev)
		default:
			fmt.Printf("%s: %s is now %s (the default is %s).\n",
				agent, name, pol, def)
		}

		// Only said for allow, and only because it is the widening direction.
		// `--allow bash` on an agent without bash succeeds at what it was asked
		// to do and changes nothing observable, which is the most confusing
		// possible outcome -- so the message exists to pre-empt it.
		//
		// It used to print for every policy, including deny, where it read as
		// advice about granting to somebody who had just refused something. A
		// note that is true but irrelevant is how notes stop being read.
		if pol == surface.PolicyAllow {
			fmt.Printf("  this does not grant %s. an agent that was never given "+
				"%s is still denied it.\n", name, name)
		}
		printNotRetroactive()
		return
	}
}

// toolChange is one --allow/--ask/--deny flag, kept as a pair so two flags
// naming the same tool stay two flags. See the comment on `change`.
type toolChange struct {
	tool   string
	policy surface.Policy
}

// requireKnownTool exits with a usage error for a tool name that is not
// grantable, naming the alternatives.
func requireKnownTool(name string) {
	if tool.Known[name] {
		return
	}
	fmt.Fprintf(os.Stderr, "arxi agent tool policy: unknown tool %q\n"+
		"  known tools: %s\n", name, strings.Join(knownToolNames(), ", "))
	os.Exit(2)
}

// printNotRetroactive says the one thing about this command that surprises
// people, in the place they will actually read it.
//
// The policy is read when a run starts and copied into its executor, so a run
// that is currently blocked on an approval does not see the change. The person
// running this command is very often looking at exactly that blocked run --
// `run why` sent them here -- so leaving this in a doc comment would mean the
// only people who learn it are the ones who did not need to.
func printNotRetroactive() {
	fmt.Printf("  this applies to runs started from now on. a run already " +
		"waiting is not unblocked by it:\n")
	fmt.Printf("  answer that one with: arxi inbox\n")
}

// defaultPolicyOf is the policy a tool has with no override, for messages.
//
// It asks tool.Resolve rather than restating the rule, with the tool granted so
// the grant check passes and the default is what is measured. A copy of the
// table here would be a second opinion about policy, and the day it disagreed
// the CLI would print one thing while the run did another.
func defaultPolicyOf(name string) surface.Policy {
	return tool.Resolve([]string{name}, nil, name)
}

// knownToolNames lists the grantable tools in a stable order.
func knownToolNames() []string {
	out := make([]string, 0, len(tool.Known))
	for k := range tool.Known {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
