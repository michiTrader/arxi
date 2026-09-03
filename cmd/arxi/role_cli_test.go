package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The role store, exercised as a process.
//
// # What a role is, and why that is what these tests are about
//
// `role define` writes defaults for `agent create --role <name>`: a tool grant
// and the advisory flag, nothing else. Those defaults are COPIED into the
// rendered agent as it is written and never read again. Every assertion here
// exists because that sentence has two halves that fail in opposite directions.
//
// The first half is that the copy happens at all. A role that is silently
// ignored looks exactly like a role that supplied nothing -- both print an agent
// and exit 0 -- so the only way to tell them apart is to read the file that came
// out. TestADefinedRoleFillsTheFlagsTheCommandLineLeftOut reads the YAML.
//
// The second half is that the copy is FROZEN. `run start` snapshots the
// blueprint, so a `role:` resolved at run time would put part of a run's rules
// outside the snapshot and let a redefinition change an agent somebody already
// reviewed. TestRedefiningARoleDoesNotReachTheAgentsThatCopiedIt deletes roles/
// outright and runs the agent anyway; if that ever fails, the design decision
// has been reversed by accident.
//
// # Why the subprocess
//
// Every interesting path in role.go and applyRole ends in os.Exit: an undefined
// role continues, a role file that will not parse does not, and the difference
// between those two is an exit code no in-process test can observe. The three
// refusals also each promise that nothing was written, which is a claim about the
// filesystem after the process died.
//
// # What is deliberately not here
//
// No test asserts that `role define` appears on the usage screen.
// TestTheUsageScreenListsWhatIsActuallyImplemented already walks the registry
// against the binary in both directions, so a wired verb missing from the screen
// -- or a screen entry naming an unwired verb -- is already a red build. A second
// list here would be a third copy of the same fact and the one nobody updates.

// writeRoleFile puts bytes in roles/<name>.json without going through the store.
//
// The unloadable cases need it because the store cannot produce them: Create
// validates, so a role file granting `reed` or holding `{ nope` can only arrive
// from a hand edit -- which is exactly how it will arrive in practice, since
// rolestore writes 0644 and the file is meant to be read and edited.
func writeRoleFile(t *testing.T, dir, name, body string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, "roles"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "roles", name+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// defineRole runs the real verb and fails the test if it refuses.
//
// The store is not written to directly, even though it would be shorter: a walk
// that starts by bypassing `role define` cannot notice the day `role define`
// stops producing a file `agent create` can read, which is the only defect this
// pair of verbs can have that neither has alone.
func defineRole(t *testing.T, dir string, args ...string) result {
	t.Helper()

	r := arxi(t, dir, append([]string{"role", "define"}, args...)...)
	if r.code != 0 {
		t.Fatalf("role define %v: exit %d, want 0:\n%s", args, r.code, r.out)
	}
	return r
}

// agentYAML is the rendered agent, read from disk.
//
// The file and not `agent show`, wherever a test is about what was written. Both
// would pass if applyRole mutated the record and the renderer dropped the field,
// and one of them would still be reading the record rather than the bytes a run
// will freeze.
func agentYAML(t *testing.T, dir, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "agents", name+".yaml"))
	if err != nil {
		t.Fatalf("reading the agent that was supposedly created: %v", err)
	}
	return string(raw)
}

// TestADefinedRoleFillsTheFlagsTheCommandLineLeftOut is the whole point of the verb.
//
// `--role auditor` alone, with no --tools and no --advisory, has to produce an
// agent carrying both. The output is checked as well as the file, and for a
// reason the file cannot cover: the grant on the first line is a grant NOBODY ON
// THIS COMMAND LINE ASKED FOR, and `write` resolves to `ask`, so an operator who
// is not told where it came from learns about it from a run that stops for an
// approval they did not expect.
func TestADefinedRoleFillsTheFlagsTheCommandLineLeftOut(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "auditor", "--tools", "read,write", "--advisory")

	r := arxi(t, dir, "agent", "create", "skeptic", "--model", "gpt-x", "--role", "auditor")
	if r.code != 0 {
		t.Fatalf("agent create --role: exit %d, want 0:\n%s", r.code, r.out)
	}

	for _, want := range []string{
		"tools: read (allow), write (ask)",
		"auditor supplied the tool grant and advisory (" +
			filepath.Join("roles", "auditor.json") + ")",
		"copied as this agent was written",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("agent create --role auditor does not say %q:\n%s\n"+
				"  consequence: the agent has a grant and a trait the command line "+
				"never mentioned, and no line on screen says where they came from.",
				want, r.out)
		}
	}

	yaml := agentYAML(t, dir, "skeptic")
	for _, want := range []string{"role: auditor", "tools: [read, write]", "advisory: true"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("the rendered agent does not carry %q:\n%s\n"+
				"  consequence: the role was reported as applied and is not in the "+
				"file, so the run freezes a blueprint the create screen described wrongly.",
				want, yaml)
		}
	}
}

// TestAnExplicitToolFlagWinsOverTheRolesGrant pins which way the precedence runs.
//
// A role is a default, so a flag the user typed has to beat it. The other order
// is the dangerous one: an operator narrowing an agent to `--tools grep` would
// silently get the role's `write` as well, and the screen would show a grant
// they had just finished removing.
//
// The advisory half of the same role is still inherited in this walk, which is
// what makes the assertion specific rather than "the role did nothing": one
// field came from the command line and the other from the file, in one create.
func TestAnExplicitToolFlagWinsOverTheRolesGrant(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "auditor", "--tools", "read,write", "--advisory")

	r := arxi(t, dir, "agent", "create", "narrow",
		"--model", "gpt-x", "--role", "auditor", "--tools", "grep")
	if r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}

	if !strings.Contains(r.out, "tools: grep (allow)") {
		t.Errorf("agent create --tools grep --role auditor does not report grep alone:\n%s\n"+
			"  consequence: the role widened a grant the operator had just narrowed.", r.out)
	}
	if !strings.Contains(r.out, "auditor supplied advisory (") {
		t.Errorf("the role line does not say advisory was the only thing inherited:\n%s\n"+
			"  consequence: `supplied the tool grant` here would credit the role "+
			"with a grant the command line set, which is the opposite of what happened.", r.out)
	}

	yaml := agentYAML(t, dir, "narrow")
	if !strings.Contains(yaml, "tools: [grep]") {
		t.Errorf("the rendered agent does not grant grep alone:\n%s\n"+
			"  consequence: the file a run freezes disagrees with the screen, and "+
			"the file is the one the agent gets.", yaml)
	}
	if strings.Contains(yaml, "write") {
		t.Errorf("the role's write grant reached an agent created with --tools grep:\n%s\n"+
			"  consequence: a mutating tool was granted by a default, past an "+
			"explicit narrower flag.", yaml)
	}
	if !strings.Contains(yaml, "advisory: true") {
		t.Errorf("the role's advisory default did not reach the file:\n%s\n"+
			"  consequence: naming a tool would have turned off an unrelated "+
			"default, which is not what a default means.", yaml)
	}
}

// TestRedefiningARoleDoesNotReachTheAgentsThatCopiedIt is the design decision,
// asserted rather than documented.
//
// A role could have been a reference: `role: auditor` in the YAML, resolved
// against roles/auditor.json at run start. It is a copy instead, because
// `run start` freezes blueprint.snapshot.yaml and a reference would put part of a
// run's rules outside the snapshot -- so redefining a role could change what an
// already-approved agent is allowed to do, and a replay would fold different
// rules than the run it is replaying.
//
// Three measurements, each stronger than the last. The role file is rewritten by
// hand to grant `bash` and force advisory, and `agent show` is unchanged. Then
// roles/ is deleted entirely and `agent show` still answers. Then the agent RUNS
// with no roles/ on disk at all, and the frozen snapshot carries the original
// grant. The third is the one that would catch a future `run start` that resolved
// the role: it cannot pass if anything at run time reads that directory.
func TestRedefiningARoleDoesNotReachTheAgentsThatCopiedIt(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "auditor", "--tools", "read,grep")

	if r := arxi(t, dir, "agent", "create", "scout", "--model", "m", "--role", "auditor"); r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}

	writeRoleFile(t, dir, "auditor", `{"name":"auditor","advisory":true,"tools":["bash"]}`)

	show := arxi(t, dir, "agent", "show", "scout")
	if !strings.Contains(show.out, "tools:    grep (allow), read (allow)") {
		t.Errorf("agent show after the role was redefined:\n%s\n"+
			"  consequence: rewriting a role file changed what an existing agent "+
			"may do, so an agent that was reviewed once cannot stay reviewed.", show.out)
	}
	if !strings.Contains(show.out, "advisory: no") {
		t.Errorf("agent show reports advisory after the role turned it on:\n%s\n"+
			"  consequence: a redefinition reached into an agent created before it.", show.out)
	}

	if err := os.RemoveAll(filepath.Join(dir, "roles")); err != nil {
		t.Fatal(err)
	}
	if show := arxi(t, dir, "agent", "show", "scout"); show.code != 0 {
		t.Fatalf("agent show with no roles/ directory: exit %d, want 0:\n%s\n"+
			"  consequence: deleting a role would break every agent that ever "+
			"named it, which is not what `copied` means.", show.code, show.out)
	}

	run := arxi(t, dir, "run", "start", "scout", "look around",
		"--sim", "--budget", "1.00", "--run-id", "noroles")
	if run.code != 0 {
		t.Fatalf("run start with no roles/ directory: exit %d, want 0:\n%s\n"+
			"  consequence: the role would be part of a run's inputs, so a run "+
			"could not be reproduced from its own directory.", run.code, run.out)
	}

	snap, err := os.ReadFile(filepath.Join(dir, "runs", "noroles", "blueprint.snapshot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap), "tools: [read, grep]") {
		t.Errorf("the frozen snapshot does not carry the grant the role supplied at create time:\n%s\n"+
			"  consequence: ADR-0001 makes the snapshot the whole config a replay "+
			"folds; a grant that lives outside it makes the replay a different run.", snap)
	}
}

// TestAnUndefinedRoleIsANoteAndNotARefusal protects the choice that looks like a
// missing validation.
//
// `role:` in a blueprint predates this store and is a free-form label: kernel
// picks the steer target by `Role == "coordinator"` and builds each member's
// identity from it, and every blueprint in examples/ names roles nothing defines.
// Refusing an undefined --role would therefore refuse spellings the rest of the
// tool depends on, and would make `role define` mandatory for a field that has
// always been a string.
//
// So the note is the whole safety net, and it is a real one: it is the only
// spelling check --role can offer at all. This walk is the fresh-directory case,
// where the suggestion has to be how to define the role rather than a list of
// roles that do not exist.
//
// The last assertion is the reason readRoles exists. `agent create --role`
// reads roles/ on an invocation that will overwhelmingly find nothing, and going
// through the writer's store would leave an empty roles/ behind every time --
// or fail with "create roles: permission denied" in a checkout the user cannot
// write to, which is an error about a directory nobody asked for.
func TestAnUndefinedRoleIsANoteAndNotARefusal(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "agent", "create", "ghosted", "--model", "m", "--role", "nope")
	if r.code != 0 {
		t.Fatalf("agent create --role nope: exit %d, want 0:\n%s\n"+
			"  consequence: an undefined role is refused, so every blueprint in "+
			"examples/ names a role this command would reject.", r.code, r.out)
	}

	for _, want := range []string{
		"nope is not defined, so no defaults were applied",
		"no roles are defined yet: arxi role define nope --tools read",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("agent create --role nope does not say %q:\n%s\n"+
				"  consequence: a misspelled --role is accepted in silence, and "+
				"the note is the only check this flag can have.", want, r.out)
		}
	}

	if !strings.Contains(agentYAML(t, dir, "ghosted"), "role: nope") {
		t.Errorf("the undefined role did not reach the file:\n%s\n"+
			"  consequence: the label kernel reads for identity and steering was "+
			"dropped because no file happened to define it.", agentYAML(t, dir, "ghosted"))
	}

	if _, err := os.Stat(filepath.Join(dir, "roles")); !os.IsNotExist(err) {
		t.Errorf("reading roles/ created it (stat err: %v)\n"+
			"  consequence: a read leaves a directory behind, and in a checkout "+
			"the user cannot write to it fails with an error about creating one.", err)
	}
}

// TestAnUndefinedRoleNamesTheRolesThatAreDefined is the note's useful half.
//
// "reviewr is not defined" tells a user something they can already see. The list
// beside it is what turns the note into a fix, because the defect is almost
// always a typo of a name that IS defined, and the difference between `reviewr`
// and `reviewer` is one character in the middle of a word.
func TestAnUndefinedRoleNamesTheRolesThatAreDefined(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "reviewer", "--tools", "read")
	defineRole(t, dir, "auditor", "--tools", "read,grep")

	r := arxi(t, dir, "agent", "create", "typo", "--model", "m", "--role", "reviewr")
	if r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "defined: auditor, reviewer") {
		t.Errorf("the note does not list the roles that exist:\n%s\n"+
			"  consequence: the user is told the name is unknown and not that the "+
			"name they meant is one character away.", r.out)
	}
	if strings.Contains(r.out, "no roles are defined yet") {
		t.Errorf("the empty-store suggestion printed beside two defined roles:\n%s\n"+
			"  consequence: the note tells the user to define a role in a "+
			"directory that already holds the one they meant.", r.out)
	}
}

// TestARoleFileThatWillNotLoadStopsTheCreate is the one path where --role refuses.
//
// An UNDEFINED role is a note, so this line is easy to read as an inconsistency.
// It is the opposite: naming a role that exists and cannot be read means the user
// asked for defaults nobody can supply, and writing the agent anyway would
// produce one that is missing exactly the fields they named the role to get --
// silently, since the record with no grant renders as a perfectly valid agent.
//
// Both spellings are checked because they fail in different layers -- JSON that
// does not parse, and JSON that parses and grants a tool the runtime has no
// implementation for -- and both must name the role's own file. The user has to
// know which file to open, and there is no `role show` to find it with.
func TestARoleFileThatWillNotLoadStopsTheCreate(t *testing.T) {
	for _, tc := range []struct {
		name, role, body, want string
	}{
		{"unparseable", "broken", "{ nope", "invalid character"},
		{"unknown tool", "bad", `{"name":"bad","tools":["reed"]}`, "unknown tool(s): reed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := workdir(t)
			path := writeRoleFile(t, dir, tc.role, tc.body)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			r := arxi(t, dir, "agent", "create", "doomed", "--model", "m", "--role", tc.role)
			if r.code != 1 {
				t.Fatalf("agent create --role %s: exit %d, want 1:\n%s\n"+
					"  consequence: exit 2 would blame the command line for a bad "+
					"file, and exit 0 would write an agent missing the defaults it named.",
					tc.role, r.code, r.out)
			}
			if !strings.Contains(r.out, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s\n"+
					"  consequence: the user cannot tell what is wrong with the file.",
					tc.want, r.out)
			}
			if !strings.Contains(r.out, filepath.Join("roles", tc.role+".json")) {
				t.Errorf("the refusal does not name the role's file:\n%s\n"+
					"  consequence: there is no `role show`, so a path they are not "+
					"given is a path they have to guess.", r.out)
			}
			if !strings.Contains(r.out, "no agent was written") {
				t.Errorf("the refusal does not say the create did not happen:\n%s\n"+
					"  consequence: the user retries after fixing the file and gets "+
					"`agent already exists` about an agent that was never written.", r.out)
			}

			if _, err := os.Stat(filepath.Join(dir, "agents")); !os.IsNotExist(err) {
				t.Errorf("a refused create left agents/ behind (stat err: %v)\n"+
					"  consequence: `no agent was written` is false, which is worse "+
					"than not saying it.", err)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
				t.Errorf("the role file changed: %q -> %q (%v)\n"+
					"  consequence: a read repaired or truncated the file the user "+
					"was just told to go and fix by hand.", before, after, err)
			}
		})
	}
}

// TestAnEmptyRoleIsReportedRatherThanSilentlyIgnored covers `role define x` bare.
//
// An empty role is legal, and it is not useless: registering the NAME is what
// turns `--role reviewr` from an accepted string into a visible typo, since the
// note lists what is defined. Both verbs have to say so, because a role that
// applies nothing and a role that was ignored produce the same agent -- and the
// second is a bug.
func TestAnEmptyRoleIsReportedRatherThanSilentlyIgnored(t *testing.T) {
	dir := workdir(t)

	def := defineRole(t, dir, "plain")
	if !strings.Contains(def.out, "no defaults, so nothing is applied to the agents that name it") {
		t.Errorf("role define with no flags does not say it defined nothing:\n%s\n"+
			"  consequence: the command looks like it did nothing at all, when what "+
			"it did was register the name.", def.out)
	}
	if !strings.Contains(def.out, "role plain defined (no tools)") {
		t.Errorf("role define does not name what it wrote:\n%s\n"+
			"  consequence: `defined ()` or a bare grant list reads as a "+
			"half-finished line.", def.out)
	}

	r := arxi(t, dir, "agent", "create", "blank", "--model", "m", "--role", "plain")
	if r.code != 0 {
		t.Fatalf("agent create --role plain: exit %d, want 0:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "plain is defined with no defaults, so nothing was applied") {
		t.Errorf("agent create does not distinguish an empty role from a missing one:\n%s\n"+
			"  consequence: silence here is indistinguishable from the role having "+
			"been ignored, which is the one bug this line exists to expose.", r.out)
	}
	if strings.Contains(r.out, "is not defined") {
		t.Errorf("a defined but empty role was reported as undefined:\n%s\n"+
			"  consequence: the user is sent to define a role they already defined.", r.out)
	}
}

// TestADefinedRoleThatSuppliedNothingSaysSo is the fourth of the four sentences,
// and the one that is easiest to argue away.
//
// `--role grepper --tools read --model m` names every field the role sets, so
// nothing is inherited and the honest report is that the role changed nothing.
// Printing nothing instead would be the same silence as an ignored role, from a
// command that had a file open and read it.
func TestADefinedRoleThatSuppliedNothingSaysSo(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "grepper", "--tools", "grep")

	r := arxi(t, dir, "agent", "create", "dup",
		"--model", "m", "--role", "grepper", "--tools", "read")
	if r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "grepper is defined and supplied nothing") {
		t.Errorf("the role line does not report an inheritance of nothing:\n%s\n"+
			"  consequence: the only screen that says whether the role was read at "+
			"all goes quiet in exactly the case where the reader cannot tell.", r.out)
	}
	if strings.Contains(r.out, "copied as this agent was written") {
		t.Errorf("a role that supplied nothing printed the copy caveat:\n%s\n"+
			"  consequence: it warns about a redefinition changing something that "+
			"was never inherited.", r.out)
	}
}

// TestAnAdvisoryRoleSaysThereIsNoWayToTurnItOff is about a flag the user did not type.
//
// An advisory agent gets no `run it:` line, because running it alone is the one
// thing it cannot do: it starts inactive, so the stage activates nobody and the
// run goes quiescent after zero turns. That caveat already exists for
// `--advisory`. What the role adds is a reader who never typed the flag, and
// whose obvious next move -- turn it off -- is not available: the declaration has
// no --no-advisory, so the way out is a different role or none.
//
// The plain --advisory create is run in the same walk as the control. Without it
// this test would pass on a binary that printed the role explanation
// unconditionally, which would be advice about a role to somebody who named none.
func TestAnAdvisoryRoleSaysThereIsNoWayToTurnItOff(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "watcher", "--advisory")

	r := arxi(t, dir, "agent", "create", "eyes", "--model", "m", "--role", "watcher")
	if r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}
	for _, want := range []string{
		"caveat: advisory",
		"the role is what made it advisory, and there is no --no-advisory",
		"an agent that must work names another role, or none.",
		"use it:",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("agent create --role watcher does not say %q:\n%s\n"+
				"  consequence: the operator learns their agent takes no turns from "+
				"a run that does nothing, and cannot tell that the role is why.",
				want, r.out)
		}
	}
	if strings.Contains(r.out, "run it:") {
		t.Errorf("an advisory agent was given a run command:\n%s\n"+
			"  consequence: the printed command enters the stage, activates nobody "+
			"and goes quiescent, which looks like a broken run rather than a trait.", r.out)
	}

	solo := arxi(t, dir, "agent", "create", "solo", "--model", "m", "--advisory")
	if solo.code != 0 {
		t.Fatalf("agent create --advisory: exit %d, want 0:\n%s", solo.code, solo.out)
	}
	if strings.Contains(solo.out, "the role is what made it advisory") {
		t.Errorf("the role explanation printed for an agent created with no role:\n%s\n"+
			"  consequence: the caveat blames a role the command line never named.",
			solo.out)
	}
}

// TestASecondRoleDefineNeverOverwrites, and the bytes are the assertion.
//
// `role define` is Mutates and not Idempotent in the registry, so a second call
// was never promised to be a no-op -- and there is no --force to offer, because
// the declaration has three parameters and that is not one of them.
//
// An overwrite is refused rather than merged for a reason the file cannot show:
// the agents created from the old definition copied it as they were written, and
// nothing on disk records which definition an agent came from. So a replaced role
// cannot be reviewed against the agents that inherited the previous one, and the
// only safe answer is to leave both the file and those agents alone.
func TestASecondRoleDefineNeverOverwrites(t *testing.T) {
	dir := workdir(t)
	defineRole(t, dir, "auditor", "--tools", "read")

	path := filepath.Join(dir, "roles", "auditor.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	r := arxi(t, dir, "role", "define", "auditor", "--tools", "bash", "--advisory")
	if r.code != 2 {
		t.Fatalf("a second role define: exit %d, want 2:\n%s\n"+
			"  consequence: exit 0 would report a definition that was not written, "+
			"and exit 1 would call a name collision an operational failure.", r.code, r.out)
	}
	for _, want := range []string{
		"role already exists",
		filepath.Join("roles", "auditor.json"),
		"never overwrites",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: there is no `role show`, so the file it points at is "+
				"the only way to read what is already defined.", want, r.out)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the refused define rewrote the file: %q -> %q\n"+
			"  consequence: every agent already created from the old definition "+
			"copied rules that can no longer be read anywhere.", before, after)
	}
}

// TestAMistypedToolIsRefusedBeforeRolesIsCreated checks the order of two gates.
//
// The grant is validated before the store is opened, so a command that cannot
// succeed leaves no directory behind. That ordering is not cosmetic: the same
// argument as readRoles, one layer up -- a verb that creates roles/ and then
// refuses has made a change on a failed invocation, and in a checkout the user
// cannot write to it fails with an error about the directory instead of the typo.
//
// Both bad names must be in the message. tool.ValidateGrants reports them all at
// once, so `--tools reed,gerp` is one round trip, and a validator that stopped at
// the first would turn a single fix into two.
func TestAMistypedToolIsRefusedBeforeRolesIsCreated(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "role", "define", "typo", "--tools", "reed,gerp")
	if r.code != 2 {
		t.Fatalf("role define --tools reed,gerp: exit %d, want 2:\n%s\n"+
			"  consequence: a typo on the command line is misuse, and exit 1 would "+
			"tell a CI job the filesystem failed.", r.code, r.out)
	}
	for _, want := range []string{"reed", "gerp", "known tools:"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not mention %q:\n%s\n"+
				"  consequence: fixing one name at a time makes a single typo pair "+
				"into two failed commands.", want, r.out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "roles")); !os.IsNotExist(err) {
		t.Errorf("a refused define created roles/ (stat err: %v)\n"+
			"  consequence: a command that wrote nothing still changed the working "+
			"directory, and would fail on the directory rather than the typo where "+
			"it cannot be created.", err)
	}
}

// TestARoleVerbThatDoesNotExistNamesTheOneThatDoes.
//
// `role list` is a reasonable thing to try and something this tool deliberately
// does not have: a role is read back by opening its file. The answer has to name
// `define` rather than calling the whole path unknown, because "no such command"
// sends the reader hunting for a typo they did not make.
//
// The bare `role` case is a different sentence, and it is checked here so the two
// cannot collapse into one: there is no wrong word to quote, so quoting one would
// blame a word the user got right.
func TestARoleVerbThatDoesNotExistNamesTheOneThatDoes(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "role", "list")
	if r.code != 2 {
		t.Fatalf("arxi role list: exit %d, want 2:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, `"list" is not a role command`) {
		t.Errorf("arxi role list does not blame the verb:\n%s\n"+
			"  consequence: naming the group would tell the user the one word they "+
			"typed correctly is wrong.", r.out)
	}
	if !strings.Contains(r.out, "it accepts: define") {
		t.Errorf("arxi role list does not name the verb that exists:\n%s\n"+
			"  consequence: the reader has to run `arxi surface` to learn there is "+
			"exactly one role verb.", r.out)
	}

	bare := arxi(t, dir, "role")
	if bare.code != 2 {
		t.Fatalf("arxi role: exit %d, want 2:\n%s", bare.code, bare.out)
	}
	if !strings.Contains(bare.out, "usage: arxi role define <name>") {
		t.Errorf("arxi role with no subcommand does not print the usage:\n%s\n"+
			"  consequence: a path that stops one word short is treated as a wrong "+
			"word, which it is not.", bare.out)
	}
}

// TestRoleDefineNamesAFileThatCanBeReadBack, because the file is the only reader.
//
// The surface declares no `role list` and no `role show`, so the path on the
// `file:` line is the entire interface for reading a role back. A wrong or
// missing path there is not a cosmetic defect: it is the difference between a
// definition a team can review and one nobody can find.
//
// The path is followed rather than pattern-matched, and the JSON is parsed rather
// than grepped, so this fails if the name is right and the contents are not.
//
// The mode is 0644 on purpose and is asserted for the contrast with providers/,
// which modelstore writes 0600 because it holds credentials. A role holds a tool
// grant a team is expected to read and commit; making it 0600 would be a
// permission that stops a reviewer without protecting anything.
func TestRoleDefineNamesAFileThatCanBeReadBack(t *testing.T) {
	dir := workdir(t)
	def := defineRole(t, dir, "auditor", "--tools", "read,write", "--advisory")

	line := ""
	for _, l := range strings.Split(def.out, "\n") {
		if i := strings.Index(l, "file:"); i >= 0 {
			line = strings.TrimSpace(l[i+len("file:"):])
		}
	}
	if line == "" {
		t.Fatalf("role define printed no `file:` line:\n%s\n"+
			"  consequence: there is no other way to read a role back.", def.out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, line))
	if err != nil {
		t.Fatalf("the path role define printed does not open: %v\n"+
			"  consequence: the one place a role can be read is a path that is wrong.", err)
	}

	var got struct {
		Name     string   `json:"name"`
		Advisory bool     `json:"advisory"`
		Tools    []string `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the role file does not parse as JSON: %v\n%s\n"+
			"  consequence: `agent create --role` reads this file, so the verb "+
			"that wrote it produced something the verb that uses it will refuse.",
			err, raw)
	}
	if got.Name != "auditor" || !got.Advisory ||
		strings.Join(got.Tools, ",") != "read,write" {
		t.Errorf("the role file holds %+v, want auditor/advisory/[read write]\n%s\n"+
			"  consequence: the flags were accepted and something else was stored, "+
			"and every agent that names this role inherits the difference.", got, raw)
	}

	info, err := os.Stat(filepath.Join(dir, line))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("the role file is mode %04o, want 0644\n"+
			"  consequence: a role is a default a team commits and reads; a "+
			"credential-grade mode locks out the review without protecting a secret.",
			perm)
	}
}
