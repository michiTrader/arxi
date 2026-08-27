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
	if got := c.Name(); got != "iash_run_start" {
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
	// These three are allow on purpose: they are the coordination mechanism
	// between agents and they are scoped to the run. Without them the agents
	// cannot collaborate and the tool has no reason to exist.
	expected := map[string]bool{
		"iash_state_set":  true,
		"iash_event_emit": true,
		"iash_state_lock": true,
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
// types. Then `iash schema` advertises run.why while the server answers "unknown
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
				"  consequence: iash schema advertises %q and the server answers "+
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
