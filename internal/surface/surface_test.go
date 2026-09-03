package surface

import (
	"strings"
	"testing"
)

// TestOneSingleSurface verifies that the three projections come out of the same
// declaration. If somebody ever adds a hand-written translation table, this test
// is the one that will tell them.
func TestOneSingleSurface(t *testing.T) {
	c := Cmd{Path: []string{"run", "start"}}
	if got := c.CLI(); got != "run start" {
		t.Errorf("CLI = %q", got)
	}
	if got := c.Name(); got != "arxi_run_start" {
		t.Errorf("tool = %q", got)
	}
	if got := c.ProtocolType(); got != "run.start" {
		t.Errorf("protocol = %q", got)
	}
}

// TestAbortDoesNotExist: the vocabulary has to be one.
//
// The CLI says `cancel`, so the protocol message is `run.cancel`. If an `abort`
// existed somewhere, there would be two words for the same thing and somebody
// would have to maintain the mapping between them forever.
func TestAbortDoesNotExist(t *testing.T) {
	for _, c := range Registry {
		for _, seg := range c.Path {
			if seg == "abort" || seg == "kill" || seg == "stop" {
				t.Errorf("%s uses %q; the canonical verb is cancel", c.CLI(), seg)
			}
		}
	}
	if Lookup("run", "cancel") == nil {
		t.Error("run cancel is missing")
	}
}

// TestScheduleDoesNotExist: cron is ONE trigger source, not the category.
// Calling the family `schedule` would make webhook: and file: look like
// second-class features taped on.
func TestScheduleDoesNotExist(t *testing.T) {
	for _, c := range Registry {
		if c.Path[0] == "schedule" || c.Path[0] == "cron" {
			t.Errorf("%s: the family is called trigger", c.CLI())
		}
	}
	tc := Lookup("trigger", "create")
	if tc == nil {
		t.Fatal("trigger create is missing")
	}
	var on *Param
	for i := range tc.Params {
		if tc.Params[i].Name == "on" {
			on = &tc.Params[i]
		}
	}
	if on == nil {
		t.Fatal("trigger create does not declare --on")
	}
	for _, want := range []string{"cron:", "webhook:", "file:", "event:"} {
		if !strings.Contains(on.Desc, want) {
			t.Errorf("--on does not document the source %q: %s", want, on.Desc)
		}
	}
}

// TestSingleNamespace: there is no `run team` nor `run agent`.
//
// A team is an agent with another kind. Two verbs would force the user to know
// what type a thing is before being able to execute it, and force us to
// duplicate every run feature for both.
func TestSingleNamespace(t *testing.T) {
	for _, c := range Registry {
		if len(c.Path) >= 2 && c.Path[0] == "run" {
			if c.Path[1] == "team" || c.Path[1] == "agent" {
				t.Errorf("%s: a single namespace, `run <actor>`", c.CLI())
			}
		}
	}
	rs := Lookup("run", "start")
	if rs == nil {
		t.Fatal("run start is missing")
	}
	if rs.Params[0].Name != "actor" {
		t.Errorf("run start receives %q, it should receive 'actor'", rs.Params[0].Name)
	}
}

// TestBudgetIsMandatory: nothing that spends money starts without a ceiling.
//
// An invisible default is a surprise bill. Making the user type the number is
// the only way for them to know the ceiling exists.
func TestBudgetIsMandatory(t *testing.T) {
	mustRequire := func(cmd *Cmd, param string) {
		t.Helper()
		if cmd == nil {
			t.Fatalf("command missing (looking for --%s)", param)
		}
		for _, p := range cmd.Params {
			if p.Name == param {
				if !p.Required {
					t.Errorf("%s: --%s has to be mandatory", cmd.CLI(), param)
				}
				if p.Default != "" {
					t.Errorf("%s: --%s cannot have a default (%q): an invisible ceiling is a surprise bill",
						cmd.CLI(), param, p.Default)
				}
				return
			}
		}
		t.Errorf("%s does not declare --%s", cmd.CLI(), param)
	}

	mustRequire(Lookup("run", "start"), "budget")
	mustRequire(Lookup("trigger", "create"), "budget")
	// The period is as important as the amount: "10 dollars" without a period is
	// an open subscription.
	mustRequire(Lookup("trigger", "create"), "budget-period")
	mustRequire(Lookup("eval", "run"), "budget")
}

// TestMutatingToolsAreNotAllowByDefault: if an agent can change the world
// without asking, that has to be a written decision, not an oversight.
func TestMutatingToolsAreNotAllowByDefault(t *testing.T) {
	// These four are allow on purpose: they are the coordination mechanism
	// between agents and they are scoped to the run. Without them the agents
	// cannot collaborate and the tool has no reason to exist.
	//
	// `arxi_state_unlock` is the one that deserves a second sentence, because its
	// `force` param lets an agent end a lease another agent still holds -- authority
	// nothing else on this list has. It is allow for the same reason `state_lock` is:
	// a release only a human can perform means every agent lock runs to its expiry,
	// so the crashed-holder stall of docs/design/20-use-cases.md §20.8 comes back for
	// every early finish. PolicyAsk was the alternative and was rejected because the
	// common case -- releasing a lock you hold yourself, the entire point of the verb
	// -- would then need a human for every turn.
	//
	// What `arxi event log <run> --type 'lock.*'` shows is WHOSE lease ended and that
	// a decision ended it rather than a clock: previous_holder and reason: "forced".
	// It does not show whose decision, and this comment claimed it did. Actor is left
	// empty by lockEvent so that naming a member does not disable that member's own
	// lock.* watcher, and an empty actor folds to holder "human" for every forced
	// release. So read this exception list as three coordination tools plus one
	// accepted gap, and do not let the missing half of the argument be re-derived from
	// here as though it were a property the code has.
	expected := map[string]bool{
		"arxi_state_set":    true,
		"arxi_event_emit":   true,
		"arxi_state_lock":   true,
		"arxi_state_unlock": true,
	}
	for _, td := range BuildManifest().Tools {
		if td.Mutates && td.Policy == PolicyAllow && !expected[td.Name] {
			t.Errorf("%s mutates the world and is allow by default.\n"+
				"If that is intentional, add it to the exception list of this "+
				"test with the reason; if not, set ToolPolicy: PolicyAsk.", td.Name)
		}
	}
}

// TestReadsAlwaysHaveJSON: a machine-readable output that exists in "most" of
// the commands is useless for automating anything.
func TestReadsAlwaysHaveJSON(t *testing.T) {
	for _, td := range BuildManifest().Tools {
		if td.Mutates {
			continue
		}
		props, _ := td.InputSchema["properties"].(map[string]any)
		if props["json"] == nil {
			t.Errorf("%s reads but does not accept --json", td.Name)
		}
	}
}

// TestSingleVersionedManifest: one number for the entire surface, not an @v1 per
// tool. Per-tool granularity sounds better and forces every client to negotiate
// a version 30 times.
func TestSingleVersionedManifest(t *testing.T) {
	m := BuildManifest()
	if m.SurfaceVersion != SurfaceVersion {
		t.Errorf("surface_version = %d", m.SurfaceVersion)
	}
	if len(m.Tools) == 0 {
		t.Fatal("empty manifest")
	}
	for _, td := range m.Tools {
		if strings.Contains(td.Name, "@") {
			t.Errorf("%s versions per tool; the version belongs to the surface", td.Name)
		}
		if td.Since == 0 {
			t.Errorf("%s does not declare since: a client cannot know whether it can use it", td.Name)
		}
	}
}

// TestUniformScope: everything exposed to the protocol has a derivable type.
func TestUniformScope(t *testing.T) {
	for _, c := range Registry {
		if c.Kind&Protocol == 0 {
			continue
		}
		if c.ProtocolType() == "" {
			t.Errorf("%s is exposed to the protocol without a type", c.CLI())
		}
		if strings.Contains(c.ProtocolType(), " ") {
			t.Errorf("protocol type with a space: %q", c.ProtocolType())
		}
	}
}

// TestRunWhyExists: it is the command that justifies the structured log. If it
// does not exist, the log is just a big file.
func TestRunWhyExists(t *testing.T) {
	c := Lookup("run", "why")
	if c == nil {
		t.Fatal("run why is missing")
	}
	if c.Kind&AgentTool == 0 {
		t.Error("run why has to be a tool: a coordinator needs to diagnose its subordinates")
	}
	if c.Mutates {
		t.Error("run why should not mutate anything")
	}
}

// TestNoDuplicates: two entries with the same path would be two tools with the
// same name, and the client would pick one at random.
func TestNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Registry {
		if seen[c.CLI()] {
			t.Errorf("duplicate path: %s", c.CLI())
		}
		seen[c.CLI()] = true
	}
}

// TestProtocolSetIsDerivedFromTheRegistry: the dispatchable set must BE the
// registry, filtered.
//
// The failure this forbids is a server holding its own list of accepted message
// types. Then `arxi schema` advertises run.why while the server answers "unknown
// type", and the client has been lied to by the one document it was told to
// trust. The check is mechanical: every Protocol-flagged entry has to come out
// of ProtocolCommands, and nothing else may.
func TestProtocolSetIsDerivedFromTheRegistry(t *testing.T) {
	got := map[string]bool{}
	for _, c := range ProtocolCommands() {
		got[c.ProtocolType()] = true
	}
	for _, c := range Registry {
		exposed := c.Kind&Protocol != 0
		if exposed && !got[c.ProtocolType()] {
			t.Errorf("%s is declared Kind|Protocol but ProtocolCommands omits it.\n"+
				"  consequence: arxi schema advertises %q and the server answers "+
				"unknown type. The client is following the manifest correctly and "+
				"still failing, with nothing in either place to explain why.",
				c.CLI(), c.ProtocolType())
		}
		if !exposed && got[c.ProtocolType()] {
			t.Errorf("%s is NOT declared Kind|Protocol and ProtocolCommands includes it.\n"+
				"  consequence: a capability deliberately kept off the wire is "+
				"reachable over a socket. §20.12 lists the 13 exclusions as a "+
				"security boundary, not an oversight.", c.CLI())
		}
	}
	if len(got) == 0 {
		t.Fatal("no protocol commands at all: the server would accept nothing and " +
			"this test would pass vacuously forever")
	}
}

// TestProtocolCommandsAreStablyOrdered: the advertised capability set is a
// document. One that reorders between builds makes a golden test impossible and
// a diff between two versions unreadable, so the order is defined, not
// whatever Registry happens to be in.
func TestProtocolCommandsAreStablyOrdered(t *testing.T) {
	cs := ProtocolCommands()
	for i := 1; i < len(cs); i++ {
		if cs[i-1].ProtocolType() >= cs[i].ProtocolType() {
			t.Fatalf("protocol commands are not sorted: %q before %q.\n"+
				"  consequence: two builds of the same registry advertise their "+
				"capabilities in different orders, so nothing downstream can diff "+
				"or pin the set", cs[i-1].ProtocolType(), cs[i].ProtocolType())
		}
	}
}

// TestLookupProtocolIsTheInverseOfProtocolType: the forward and reverse
// directions must use the same data.
//
// A map from message type to command, written by hand, is exactly the
// translation table TestOneSingleSurface exists to forbid — it just hides on the
// server side where no test was looking. Splitting on "." and deferring to
// Lookup keeps one source: c.Path.
func TestLookupProtocolIsTheInverseOfProtocolType(t *testing.T) {
	for _, c := range ProtocolCommands() {
		back := LookupProtocol(c.ProtocolType())
		if back == nil {
			t.Errorf("LookupProtocol(%q) found nothing, but ProtocolCommands "+
				"listed it.\n  consequence: the server advertises a message type "+
				"it then cannot resolve, so the capability is unreachable and the "+
				"manifest is wrong about itself", c.ProtocolType())
			continue
		}
		if back.CLI() != c.CLI() {
			t.Errorf("LookupProtocol(%q) resolved to %q.\n"+
				"  consequence: a request is dispatched to a different capability "+
				"than the one it named, which for a mutating command means the "+
				"wrong thing happens and the log records it as intended",
				c.ProtocolType(), back.CLI())
		}
	}
}

// TestLookupProtocolRefusesWhatIsNotOnTheWire: a real path that is not exposed
// must be rejected as firmly as a nonexistent one.
//
// `design` opens an interactive designer on the operator's terminal and `serve`
// is the server itself. Resolving them by path alone — which is what Lookup
// does — would let a socket client reach both. The Kind check is the boundary,
// so it is checked here rather than trusted to the caller: a server that has to
// remember to re-check has a hole the day somebody adds a second dispatch site.
func TestLookupProtocolRefusesWhatIsNotOnTheWire(t *testing.T) {
	for _, name := range []string{"design", "serve", "provider.add", "model.enable",
		"agent.tool.policy", "role.define", "blueprint.create", "blueprint.install"} {
		if c := LookupProtocol(name); c != nil {
			t.Errorf("LookupProtocol(%q) resolved to %s, which is not Kind|Protocol.\n"+
				"  consequence: a capability held back from the wire on purpose is "+
				"reachable over a socket. `agent tool policy` over the network means "+
				"an agent can widen its own tool policy, which is the same as having "+
				"no policy.", name, c.CLI())
		}
	}
	// A nonexistent type and an empty one must both be nil rather than panic:
	// both arrive from the network on the first malformed line anybody sends.
	if LookupProtocol("") != nil {
		t.Error("an empty message type resolved to a command")
	}
	if LookupProtocol("run.nonsense") != nil {
		t.Error("an unknown message type resolved to a command")
	}
	if LookupProtocol("...") != nil {
		t.Error("a type of nothing but separators resolved to a command")
	}
}

// TestServeIsNotItselfAProtocolMessage: the server cannot be one of the things
// it serves.
//
// `serve` accepted over the wire means a client can make the server spawn a
// second server, which either fails on a bound address or forks the process tree
// on every request. It is CLIOnly for that reason and the reason is written down
// here so a future "for symmetry, everything should be a protocol message" reads
// as the mistake it is.
func TestServeIsNotItselfAProtocolMessage(t *testing.T) {
	c := Lookup("serve")
	if c == nil {
		t.Fatal("serve is missing from the registry")
	}
	if c.Kind&Protocol != 0 {
		t.Error("serve is exposed to the protocol.\n" +
			"  consequence: a client can ask the server to start another server. " +
			"That either fails on an address already bound or forks a process per " +
			"request, and neither is a capability anybody asked for.")
	}
	if c.Kind&AgentTool != 0 {
		t.Error("serve is exposed as an agent tool.\n" +
			"  consequence: an agent inside a run could open a socket onto its own " +
			"orchestrator. §20.12 puts serve on the operator side of that line.")
	}
}

// TestWireParamsIsWhatTheSchemaPromises: the server must validate against the
// same names the manifest hands out.
//
// This drifted once already in the obvious way: jsonSchema normalized `if-seq`
// to `if_seq` and synthesized a `json` property, both invisible to anything
// reading c.Params. A server validating against c.Params would reject `if_seq`,
// a name the manifest itself told the client to send, and would have no idea
// `json` was legal. One function, read by both, is the only arrangement where
// that cannot happen.
func TestWireParamsIsWhatTheSchemaPromises(t *testing.T) {
	for _, c := range Registry {
		if c.Kind&AgentTool == 0 {
			continue
		}
		wire := map[string]bool{}
		for _, pp := range c.WireParams() {
			wire[pp.Name] = true
		}
		props, _ := jsonSchema(c)["properties"].(map[string]any)
		for name := range props {
			if !wire[name] {
				t.Errorf("%s: the schema advertises %q and WireParams does not know it.\n"+
					"  consequence: the server rejects a parameter its own manifest "+
					"told the client to send", c.CLI(), name)
			}
		}
		for name := range wire {
			if props[name] == nil {
				t.Errorf("%s: WireParams accepts %q and the schema never mentions it.\n"+
					"  consequence: a parameter that works is undiscoverable, so it "+
					"is used by whoever read the source and by nobody else",
					c.CLI(), name)
			}
		}
	}
	// The specific case that caused this: a dash must reach the wire as an
	// underscore, because JSON keys with dashes are awkward in every client.
	rp := Lookup("run", "prompt")
	if rp == nil {
		t.Fatal("run prompt is missing")
	}
	var found bool
	for _, pp := range rp.WireParams() {
		if pp.Name == "if_seq" {
			found = true
		}
		if pp.Name == "if-seq" {
			t.Error("run prompt exposes `if-seq` on the wire with a dash, but the " +
				"tool schema normalizes it to `if_seq`: the two disagree and the " +
				"client can only satisfy one of them")
		}
	}
	if !found {
		t.Error("run prompt does not expose if_seq on the wire, so the CAS on seq " +
			"that ADR-0006 requires is unreachable from a protocol client")
	}
}

// TestShortFlagsAreUnambiguous is the invariant that makes a global map safe.
//
// One letter, one meaning, surface-wide. If two parameters ever claim the same
// letter, then `-b` means a spend ceiling on one command and something else on
// another, and the user who learned it on the first command silently does the
// wrong thing on the second. That is the failure short flags exist to avoid, so
// it is checked rather than assumed.
func TestShortFlagsAreUnambiguous(t *testing.T) {
	owner := map[string]string{}
	for _, pp := range ShortFlags() {
		name, letter := pp.Name, pp.Desc
		if prev, taken := owner[letter]; taken {
			t.Fatalf("-%s is claimed by both %q and %q. A letter that means two "+
				"things is worse than no letter: the user who learned -%s on one "+
				"command silently does something else on the other. Give one of "+
				"them a different letter, or no short form at all, in shortFlags",
				letter, prev, name, letter)
		}
		owner[letter] = name
	}
	if len(owner) == 0 {
		t.Fatal("no short flags at all, so this test proves nothing. Either " +
			"shortFlags was emptied or ShortFlags() stopped reading it")
	}
}

// TestShortFlagsAreSingleLetters keeps the shorthand short.
//
// A two-character short flag is not a shorthand, it is a second spelling of the
// long name, and it collides with the one-letter parser: `-ab` has to be either
// a flag named "ab" or two flags "a" and "b", and it cannot be both.
func TestShortFlagsAreSingleLetters(t *testing.T) {
	for _, pp := range ShortFlags() {
		if len([]rune(pp.Desc)) != 1 {
			t.Fatalf("%q maps to %q, which is not one character. A multi-letter "+
				"short flag is ambiguous against grouped flags and buys nothing "+
				"over the long name; use the long form instead", pp.Name, pp.Desc)
		}
	}
}

// TestEveryShortFlagAbbreviatesARealParameter stops the map from outliving the
// surface.
//
// A letter for a parameter no command has is a promise the CLI cannot keep: the
// help text offers it, the parser accepts it, and every command rejects it. This
// is the drift a per-command alias table would have hidden, caught here instead.
func TestEveryShortFlagAbbreviatesARealParameter(t *testing.T) {
	real := map[string]bool{}
	for _, c := range Registry {
		for _, pp := range c.WireParams() {
			real[strings.ReplaceAll(pp.Name, "_", "-")] = true
		}
	}
	for _, pp := range ShortFlags() {
		if !real[pp.Name] {
			t.Fatalf("-%s abbreviates %q, which no command in the surface "+
				"declares. Either a parameter was renamed and shortFlags was not, "+
				"or the letter was invented for a parameter that never existed. "+
				"Remove it from shortFlags or fix the name", pp.Desc, pp.Name)
		}
	}
}

// TestLongForResolvesOnlyWithinTheCommand is why LongFor takes a receiver.
//
// `-r` is `run` on the thirteen commands that have a run parameter, and it is
// nothing at all on the ones that do not. Resolving letters globally and letting
// each command ignore what it does not recognise is how `blueprint validate -r x`
// would validate nothing and report success.
func TestLongForResolvesOnlyWithinTheCommand(t *testing.T) {
	why := Lookup("run", "why")
	if why == nil {
		t.Fatal("run why is missing from the registry; this test cannot check " +
			"short-flag resolution without a command that takes a run id")
	}
	if got := why.LongFor("r"); got != "run" {
		t.Fatalf("run why resolves -r to %q, want \"run\". The command declares a "+
			"run parameter, so its documented short form has to reach it or the "+
			"shorthand is decoration", got)
	}

	validate := Lookup("blueprint", "validate")
	if validate == nil {
		t.Fatal("blueprint validate is missing from the registry; this test " +
			"needs a command WITHOUT a run parameter to prove -r is scoped")
	}
	if got := validate.LongFor("r"); got != "" {
		t.Fatalf("blueprint validate resolves -r to %q, want no resolution. A "+
			"letter the command has no parameter for must fail, not bind to "+
			"something else: silently accepting -r here means the flag is "+
			"discarded and the command reports success on input it ignored", got)
	}
}

// TestLongForFindsTheSyntheticJSONFlag: --json is added by WireParams, not
// declared in Params, so a LongFor that walked Params directly would offer -J in
// the help text and then reject it. That is the exact drift WireParams was
// introduced to end, so it is checked on this path too.
func TestLongForFindsTheSyntheticJSONFlag(t *testing.T) {
	c := Lookup("run", "why")
	if c == nil {
		t.Fatal("run why is missing; this test needs a non-mutating command " +
			"to check that the synthesized json flag is reachable")
	}
	if got := c.LongFor("J"); got != "json" {
		t.Fatalf("run why resolves -J to %q, want \"json\". WireParams synthesizes "+
			"the json flag for every reading command, so LongFor must read "+
			"WireParams and not Params, or the shorthand exists in the help and "+
			"not in the parser", got)
	}

	m := Lookup("run", "start")
	if m == nil || !m.Mutates {
		t.Fatal("run start is missing or no longer mutating; this test needs a " +
			"mutating command to check that -J is NOT offered where --json is not")
	}
	if got := m.LongFor("J"); got != "" {
		t.Fatalf("run start resolves -J to %q, want no resolution. A mutating "+
			"command has no json flag, and offering its shorthand promises an "+
			"output mode the command does not have", got)
	}
}

// TestTheWireHasNoShortFlags: short forms are a courtesy to fingers. A protocol
// client has none, and `{"b": 5}` in a log is a puzzle where `{"budget": 5}` is
// a fact. If a letter ever leaked into WireParams, `arxi schema` would advertise
// it and the server would reject it.
func TestTheWireHasNoShortFlags(t *testing.T) {
	for _, c := range Registry {
		for _, pp := range c.WireParams() {
			if len([]rune(pp.Name)) == 1 {
				t.Fatalf("%s exposes a one-character parameter %q on the wire. "+
					"Short flags belong to the CLI only: a machine client saves "+
					"nothing by them and the log becomes unreadable. Keep the "+
					"long name in Params", c.CLI(), pp.Name)
			}
		}
	}
}

// TestAClosedSetIsDeclaredNotDescribed catches a parameter whose legal values
// live in its description instead of its Enum.
//
// `budget-period` was declared with the description "period of the ceiling:
// day|week|month" and no Enum, which makes the closed set a suggestion. Two
// things read Enum and neither reads prose: jsonSchema, so an agent is handed a
// free-form string where three values are legal, and the protocol validator, so
// {"budget_period": "fortnight"} was accepted. A ceiling whose period nothing
// agrees on is not a ceiling.
//
// Prefix schemes are exempt. `on` takes "cron:0 3 * * *" and `then` takes "run
// start team 'x'": the pipes in those descriptions separate PREFIXES, not
// values, and the payload after the colon is open by design. An enum there would
// reject every real invocation, so they are listed by name rather than pattern —
// a rule that says "unless it looks like a prefix" would be a rule nobody can
// apply to a new parameter.
func TestAClosedSetIsDeclaredNotDescribed(t *testing.T) {
	// Parameters whose values are prefixes with a free-form payload.
	openPayload := map[string]bool{
		"on":   true, // cron:EXPR, webhook:PATH, event:PATTERN
		"then": true, // run:..., emit:..., notify:...
	}

	for _, c := range Registry {
		for _, pp := range c.Params {
			if len(pp.Enum) > 0 || openPayload[pp.Name] {
				continue
			}
			if strings.Contains(pp.Desc, "|") {
				t.Fatalf("%s declares %q with alternatives in its description "+
					"(%q) and no Enum.\nOnly Enum is machine-readable: the tool "+
					"schema omits the constraint, so an agent sees a free-form "+
					"string, and the protocol validator enforces Enum, so it "+
					"accepts values this surface calls illegal. Use "+
					"enum(p(...), \"a\", \"b\") or, if the payload after a prefix "+
					"is genuinely open, add %q to openPayload in this test with "+
					"the reason",
					c.CLI(), pp.Name, pp.Desc, pp.Name)
			}
		}
	}
}

// TestAnEnumeratedParameterHasADefaultOrIsRequired.
//
// An optional enum with no default is a hole: the command has to invent a value,
// the invented one is invisible, and the surface no longer describes what runs.
// Every enum in this surface is either required (the caller must choose) or
// defaulted (the choice is written down where a reader can see it).
func TestAnEnumeratedParameterHasADefaultOrIsRequired(t *testing.T) {
	checked := 0
	for _, c := range Registry {
		for _, pp := range c.Params {
			if len(pp.Enum) == 0 {
				continue
			}
			checked++
			if !pp.Required && pp.Default == "" {
				t.Fatalf("%s declares %q as an enum that is neither required nor "+
					"defaulted. The command will pick a value at runtime, and a "+
					"default you cannot see is indistinguishable from a bug when "+
					"it fires. Either req() it or give it def()",
					c.CLI(), pp.Name)
			}
			// A default outside the enum answers a different question than the
			// one asked, and does it silently.
			if pp.Default != "" {
				ok := false
				for _, v := range pp.Enum {
					if v == pp.Default {
						ok = true
						break
					}
				}
				if !ok {
					t.Fatalf("%s defaults %q to %q, which is not one of its own "+
						"legal values %v. The default is what runs when the user "+
						"says nothing, so a default the validator would reject "+
						"means the common path is the illegal one",
						c.CLI(), pp.Name, pp.Default, pp.Enum)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no enumerated parameters were found, so this test proves " +
			"nothing. Either the registry lost its enums or Param.Enum was renamed")
	}
}

// TestEveryCLIAliasResolvesToADeclaredCommand is the cheap half of keeping an
// alias from becoming a lie.
//
// An alias whose command is not reachable by its own path is not a shortcut, it
// is a dangling pointer with a friendly name.
func TestEveryCLIAliasResolvesToADeclaredCommand(t *testing.T) {
	checked := 0
	for alias, target := range Aliases() {
		checked++

		if alias == "" {
			t.Fatalf("an empty alias is declared on %q; an empty string would make "+
				"LookupCLI answer for the invocation `arxi` with no arguments at all", target)
		}
		if len(strings.Fields(alias)) != 1 {
			t.Errorf("alias %q of %q is not exactly one word\n"+
				"  an alias is a shorter spelling of an existing command, not a "+
				"second grammar; the dispatcher only ever tries a single word",
				alias, target)
		}
		if Lookup(strings.Fields(target)...) == nil {
			t.Errorf("alias %q points at %q, which is not declared\n"+
				"  consequence: `arxi %s` would resolve to a command the rest of "+
				"the surface has never heard of, so schema, the protocol and the "+
				"usage guard would all disagree with the dispatcher",
				alias, target, alias)
		}
		if got := LookupCLI(alias); got == nil || got.CLI() != target {
			t.Errorf("LookupCLI(%q) did not return %q\n"+
				"  the declaration and the resolver disagree, which means the "+
				"registry says one thing and the CLI does another",
				alias, target)
		}
	}
	if checked == 0 {
		t.Fatal("no CLI aliases were found, so this test proves nothing. Either " +
			"the registry lost its aliases or Cmd.CLIAlias was renamed")
	}
}

// TestNoCLIAliasCollidesWithATopLevelWord is the half that matters.
//
// If an alias equals a real first word, one invocation has two meanings and the
// winner is decided by lookup order — which is exactly the failure the short
// flag table refused to accept for letters, for the same reason: the script that
// used it does not fail, it silently does something else.
//
// Both directions are checked. An alias must not shadow a declared group
// (`arxi run`), and it must not shadow one of the dispatcher's own top-level
// words either, because those are real invocations a registry entry may not
// cover.
func TestNoCLIAliasCollidesWithATopLevelWord(t *testing.T) {
	// Words main() dispatches itself, before any registry lookup happens.
	dispatcherOwned := []string{"help", "version", "schema", "surface", "serve"}

	seen := map[string]string{}
	for alias, target := range Aliases() {
		if prev, dup := seen[alias]; dup {
			t.Errorf("alias %q is declared by both %q and %q\n"+
				"  one word cannot abbreviate two commands; the winner would be "+
				"whichever the registry happens to list first",
				alias, prev, target)
		}
		seen[alias] = target

		if len(SubcommandsOf(alias)) > 0 {
			t.Errorf("alias %q of %q is also a declared group\n"+
				"  `arxi %s` would mean two things, and lookup order would pick",
				alias, target, alias)
		}
		if Lookup(alias) != nil {
			t.Errorf("alias %q of %q is also a declared command on its own\n"+
				"  the alias is unreachable, so it is dead weight that reads as a promise",
				alias, target)
		}
		for _, w := range dispatcherOwned {
			if alias == w {
				t.Errorf("alias %q of %q collides with %q, which main() handles itself\n"+
					"  the dispatcher's switch runs first, so the alias would never "+
					"fire and the registry would document a spelling that does nothing",
					alias, target, w)
			}
		}
	}
}
