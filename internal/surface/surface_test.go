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
