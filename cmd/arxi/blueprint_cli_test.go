package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// storeAgent runs the real `agent create` and fails the test if it refuses.
//
// The store is never written to directly, for the reason defineRole gives next
// door: a walk that composes a team out of files the test wrote itself cannot
// notice the day `agent create` stops producing a file `blueprint create` can
// copy, and that is the only defect this pair of verbs can have that neither has
// alone.
//
// A model is always passed. `blueprint create` appends `--model <id>` to the
// `run it:` line when any member lacks one and runItArgs substitutes only the
// quoted objective, so a modelless team would leave the walk running a command
// with the literal word `<id>` in it.
func storeAgent(t *testing.T, dir string, args ...string) result {
	t.Helper()

	argv := append([]string{"agent", "create"}, args...)
	argv = append(argv, "--model", "claude-sonnet-4-6")
	r := arxi(t, dir, argv...)
	if r.code != 0 {
		t.Fatalf("agent create %v: exit %d, want 0:\n%s", args, r.code, r.out)
	}
	return r
}

// storeTeam is the two workers and one adviser most walks below start from.
//
// backend holds `write` and frontend does not, so the resolved workspace and the
// printed grants differ per member -- both of them facts the create screen is
// supposed to surface rather than leave for the first run to discover.
func storeTeam(t *testing.T, dir string) {
	t.Helper()

	storeAgent(t, dir, "backend", "--role", "implementer", "--tools", "read,write")
	storeAgent(t, dir, "frontend", "--role", "implementer", "--tools", "read")
	storeAgent(t, dir, "advisor", "--tools", "read", "--advisory")
}

// writeAgentFile puts an agent file on disk that `agent create` cannot write.
//
// Three of the refusals below are about files the store will not produce: one
// whose member is named differently from the file it lives in, one with no
// members at all, and one whose member carries a `stages:` list. Create
// validates and renders exactly one member named after the file, so those can
// only arrive from a hand edit -- which is how they will arrive, since the store
// writes 0644 and the file's own header invites editing it.
//
// The returned path is repository-relative, because that is the spelling the
// refusals print and therefore the thing worth comparing them against.
func writeAgentFile(t *testing.T, dir, name, body string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("agents", name+".yaml")
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

// turnTakers is who a run asked to work, in the order it asked them.
//
// agent.activated rather than agent.turn_done: the question is who the blueprint
// gave a turn to, and a turn that opened and then failed is still a member that
// was activated and paid for. In order, because an unordered set is also
// satisfied by a run that activated one member twice and the other never.
func turnTakers(t *testing.T, dir, run string) []string {
	t.Helper()

	var who []string
	for _, ev := range allEvents(t, dir, run) {
		if ev["type"] == "agent.activated" {
			actor, _ := ev["actor"].(string)
			who = append(who, actor)
		}
	}
	return who
}

// noTeamFile fails the test if a refusal left a file behind.
//
// Every refusal below claims nothing was written, and a half-composed team is
// worse than either outcome: `agent list` shows it, `run start` accepts it, and
// it is the team the operator was told they did not get.
func noTeamFile(t *testing.T, dir, name string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(dir, "agents", name+".yaml")); !os.IsNotExist(err) {
		t.Errorf("agents/%s.yaml exists after a refusal (stat error: %v)\n"+
			"  consequence: the screen said nothing was written, so `agent list` "+
			"would be showing a team composed by a command that exited non-zero.",
			name, err)
	}
}

// TestTheComposedTeamIsARunThatWorks is the test this verb shipped without.
//
// `blueprint create` had no process-level coverage of any kind.
// internal/agentstore checks that Render writes back the members it was handed,
// which cannot see the property that matters: that the file it wrote is one the
// runtime drives. `agent create` shipped a defect straight through that same gap
// -- a stageless blueprint that loaded, validated, printed a `run it:` line,
// started a run and opened no turn in it -- and this is that same walk taken for
// a team, where there is more to get wrong: three members, a stage list, and an
// advance rule that has to be satisfiable by whoever is left after the advisory
// ones are set aside.
//
// Asserted through the log rather than the YAML. The stage list is the mechanism;
// being asked to work is the property.
//
// The run is started by the command the screen printed, parsed back out of the
// output by runItArgs, because the defect class this catches is one verb printing
// a command another verb cannot honour.
func TestTheComposedTeamIsARunThatWorks(t *testing.T) {
	dir := workdir(t)
	storeTeam(t, dir)

	c := arxi(t, dir, "blueprint", "create", "feature-team",
		"--members", "backend,frontend,advisor")
	if c.code != 0 {
		t.Fatalf("blueprint create: exit %d, want 0:\n%s", c.code, c.out)
	}

	args := append(runItArgs(t, c.out, "ship the thing"), "--sim", "--run-id", "walk")
	r := arxi(t, dir, args...)
	if r.code != 0 {
		t.Fatalf("the command `blueprint create` printed exits %d:\n  arxi %s\n%s",
			r.code, strings.Join(args, " "), r.out)
	}

	// Both workers and neither the adviser, as one assertion: the two failures
	// are asymmetric and both matter. A worker the stage forgot is a team that
	// silently does half the work it was composed for; the adviser among them is
	// a paid turn for a member whose submit cannot count toward anything.
	if got, want := turnTakers(t, dir, "walk"), []string{"backend", "frontend"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the run activated %v, want %v\n"+
			"  consequence: the file this command composes is what decides who "+
			"works. either way round, the operator paid for a team and got "+
			"something else.\n%s", got, want, r.out)
	}
	if n := countEvents(t, dir, "walk", "stage.entered"); n != 1 {
		t.Errorf("stage.entered × %d, want 1\n"+
			"  consequence: without --stages the verb promises one stage named "+
			"work with everybody in it. none is the stageless defect again; two is "+
			"the whole team paid for twice.\n%s", n, r.out)
	}
	if n := countEvents(t, dir, "walk", "run.quiescent"); n != 0 {
		t.Errorf("run.quiescent × %d, want 0\n"+
			"  consequence: \"nobody is working and nobody can start\" said of a "+
			"run that was working. every multi-member team reported failed on "+
			"every --sim run until a commissioned turn began counting as busy.\n%s",
			n, r.out)
	}
	if !strings.Contains(r.out, "succeeded") {
		t.Errorf("the run this verb composed did not report success:\n%s\n"+
			"  consequence: the first thing the create screen promises is a file "+
			"that runs, and the status line is what the operator reads to believe "+
			"it.", r.out)
	}
}

// TestTheComposedFileIsAnOrdinaryAgentFile checks the sentence the usage prints.
//
// "composes agents already in agents/ into one file of the same kind, in the same
// directory" is a promise about a directory rather than about a format: the team
// lands beside the files it was composed from, so every verb that reads that
// directory has to keep working over it. The one that breaks first is `agent
// list`, which prints a row per file and has to say something truthful about a
// file holding three members.
//
// `blueprint validate` is run on the path the create screen printed, not on one
// this test built, because the screen's last line tells the operator to run
// exactly that and a path only the test knows is a path nobody will type.
func TestTheComposedFileIsAnOrdinaryAgentFile(t *testing.T) {
	dir := workdir(t)
	storeTeam(t, dir)

	c := arxi(t, dir, "blueprint", "create", "feature-team",
		"--members", "backend,frontend,advisor")
	if c.code != 0 {
		t.Fatalf("blueprint create: exit %d, want 0:\n%s", c.code, c.out)
	}

	rel := filepath.Join("agents", "feature-team.yaml")
	want, _ := sha256Of(t, filepath.Join(dir, rel))

	var listed []agentDoc
	l := arxi(t, dir, "agent", "list", "--json")
	if err := json.Unmarshal([]byte(l.out), &listed); err != nil {
		t.Fatalf("agent list --json stopped parsing once a team was in the "+
			"directory: %v\n%s", err, l.out)
	}
	found := false
	for _, d := range listed {
		if d.Name != "feature-team" {
			continue
		}
		found = true
		if len(d.Members) != 3 {
			t.Errorf("agent list reports %d members for the team, want 3\n"+
				"  consequence: the row is how the operator finds the team again "+
				"after the create screen has scrolled away.", len(d.Members))
		}
		if d.SHA != want {
			t.Errorf("agent list reports sha %q, the file hashes to %q\n"+
				"  consequence: the sha is what `run start` freezes into the "+
				"snapshot, so a listing that reports a different one cannot be "+
				"used to tell which team a finished run actually ran.", d.SHA, want)
		}
	}
	if !found {
		t.Errorf("agent list does not mention feature-team:\n%s\n"+
			"  consequence: the usage text says `arxi agent list` shows it. a team "+
			"the store will not list is a file the operator has to remember by "+
			"hand.", l.out)
	}

	if !strings.Contains(c.out, "arxi blueprint validate "+rel) {
		t.Fatalf("the create screen does not offer `blueprint validate %s`:\n%s", rel, c.out)
	}
	if v := arxi(t, dir, "blueprint", "validate", rel); v.code != 0 {
		t.Errorf("blueprint validate refuses the file this command wrote: exit %d\n%s\n"+
			"  consequence: the create screen's closing advice is to run exactly "+
			"that, so a file it rejects turns the last line of a successful command "+
			"into a dead end.", v.code, v.out)
	}
}

// TestTheCreateScreenPrintsEachMemberResolvedGrant is why the screen is long.
//
// `--tools read,write` looks like two equal grants and `write` resolves to ask, so
// a run stops for an approval nobody was expecting. A team multiplies that: three
// members can carry three tool lists written on three different days, and the file
// the operator is about to run is the first screen where all of them appear
// together. The source path shares the line because it is the one thing this
// screen knows that the new file does not record -- members are copied, so
// nothing in agents/feature-team.yaml says where backend came from.
func TestTheCreateScreenPrintsEachMemberResolvedGrant(t *testing.T) {
	dir := workdir(t)
	storeTeam(t, dir)

	c := arxi(t, dir, "blueprint", "create", "feature-team",
		"--members", "backend,frontend,advisor")
	if c.code != 0 {
		t.Fatalf("blueprint create: exit %d, want 0:\n%s", c.code, c.out)
	}

	for _, want := range []string{
		"- backend: tools: read (allow), write (ask)",
		"copied from " + filepath.Join("agents", "backend.yaml"),
		"counts toward advance",
		"advisor is advisory",
		"stages: work",
	} {
		if !strings.Contains(c.out, want) {
			t.Errorf("the create screen does not contain %q:\n%s\n"+
				"  consequence: this is the only screen that shows the whole team "+
				"at once. a grant, a source file or an advisory member missing from "+
				"it is a surprise the first run pays for.", want, c.out)
		}
	}
}

// TestAMemberThatIsATeamIsRefusedByNamingItsMembers guards a silent composition.
//
// Splicing a team in would compose more members than the command line names --
// `--members platform,security` quietly becoming five -- and would drop the
// stages that made that file a team, which are the part of it saying who works
// when. Hence the refusal, and hence the suggestion: the message prints the flag
// the operator should have typed.
//
// That flag is parsed out of the refusal and run, rather than retyped here.
// Advice that does not work is worse than no advice in a message whose whole job
// is to unblock somebody, and a retyped copy would only prove the test author can
// read.
func TestAMemberThatIsATeamIsRefusedByNamingItsMembers(t *testing.T) {
	dir := workdir(t)
	storeTeam(t, dir)
	if c := arxi(t, dir, "blueprint", "create", "feature-team",
		"--members", "backend,frontend,advisor"); c.code != 0 {
		t.Fatalf("blueprint create: exit %d, want 0:\n%s", c.code, c.out)
	}

	r := arxi(t, dir, "blueprint", "create", "super", "--members", "feature-team")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2: a team named as a member is the command line "+
			"naming the wrong kind of thing, and no change to the store makes the "+
			"same invocation right\n%s", r.code, r.out)
	}
	noTeamFile(t, dir, "super")

	for _, want := range []string{
		`member "feature-team" is itself a team of 3 (backend, frontend, advisor)`,
		"would drop the stages that decide which of them works when",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: \"cannot use that member\" leaves the operator "+
				"guessing which of the three files they named is the team.", want, r.out)
		}
	}

	_, suggested, ok := strings.Cut(r.out, "name its members instead: --members ")
	if !ok {
		t.Fatalf("the refusal suggests no --members line:\n%s", r.out)
	}
	suggested = strings.TrimSpace(strings.SplitN(suggested, "\n", 2)[0])
	if s := arxi(t, dir, "blueprint", "create", "super", "--members", suggested); s.code != 0 {
		t.Errorf("the flag the refusal printed (--members %s) exits %d:\n%s\n"+
			"  consequence: the refusal's one job is to unblock the operator, and a "+
			"suggestion that fails leaves them worse off than a bare error.",
			suggested, s.code, s.out)
	}
}

// TestTwoMembersWithOneNameAreRefusedNamingBothFiles is about the second file.
//
// A member's name is how a stage activates it and how `run steer` addresses it, so
// two members sharing one is a team the kernel cannot address. What makes this
// worth a process test is where the collision comes from: the name inside a file
// need not match the filename, so `--members backend,twin` names two different
// files and collides anyway. An operator staring at that command line sees no
// duplicate at all, which is why the refusal has to print both paths and say the
// name lives inside the file.
func TestTwoMembersWithOneNameAreRefusedNamingBothFiles(t *testing.T) {
	dir := workdir(t)
	storeAgent(t, dir, "backend", "--tools", "read,write")

	twin := writeAgentFile(t, dir, "twin", "name: twin\n\nmembers:\n"+
		"  - name: backend\n    tools: [read]\n\n"+
		"stages:\n  - {name: work, advance_when: all}\n")

	r := arxi(t, dir, "blueprint", "create", "pair", "--members", "backend,twin")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2: two members with one name is the invocation "+
			"being wrong -- both files load and both are valid alone\n%s", r.code, r.out)
	}
	noTeamFile(t, dir, "pair")

	for _, want := range []string{
		`two members would be named "backend"`,
		filepath.Join("agents", "backend.yaml") + " and " + twin,
		"arxi agent show twin",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: the duplicate is invisible on the command line, so a "+
				"message that does not name both files and the verb that reads them "+
				"leaves the operator opening agents/ by hand.", want, r.out)
		}
	}
}

// TestAMemberNoStageActivatesIsRefusedBeforeAnythingIsWritten is the quiet one.
//
// A member whose `stages:` names none of the team's is activated in no stage at
// all: it is copied in, listed by `agent show`, counted by `agent list`, and never
// takes a turn. Nothing about the resulting file looks wrong, which is the same
// shape as the advance rule that asks for three submits from two members -- the
// failure mode ADR-0004 exists for, arriving from a blueprint instead of a bug.
//
// The refusal has to land before the file exists, because a team on disk gets run.
// It also lists the source paths of every member, including the one the operator
// did not type wrong: a name that means nothing until you know which file it came
// out of is what makes this one hard to fix by reading the command line.
func TestAMemberNoStageActivatesIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	dir := workdir(t)
	storeAgent(t, dir, "backend", "--tools", "read,write")

	staged := writeAgentFile(t, dir, "staged", "name: staged\n\nmembers:\n"+
		"  - name: staged\n    tools: [read]\n    stages: [build]\n\n"+
		"stages:\n  - {name: work, advance_when: all}\n")

	r := arxi(t, dir, "blueprint", "create", "mismatch",
		"--members", "backend,staged", "--stages", "review")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2: the member and the --stages flag disagree, and "+
			"which of the two is wrong is a question about the command line\n%s",
			r.code, r.out)
	}
	noTeamFile(t, dir, "mismatch")

	for _, want := range []string{
		`member "staged" only takes part in stages [build]`,
		"this blueprint declares [review]",
		"it would be activated in no stage at all",
		"    staged: " + staged,
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: composed anyway, this team runs, spends, and leaves "+
				"one member out with nothing in the log to say so.", want, r.out)
		}
	}
}

// TestATeamCannotOverwriteAnAgentThatExists checks the bytes, not the message.
//
// An agent and a team compete for one filename, so `blueprint create backend`
// would silently replace a member three other teams already copied -- and copies
// are the reason it would be silent: those teams keep working, so nothing fails,
// and the only loss is the original file. The sha is compared before and after
// because a refusal that prints correctly and truncates the file anyway is the
// worst version of this bug and the one a message-only assertion cannot see.
func TestATeamCannotOverwriteAnAgentThatExists(t *testing.T) {
	dir := workdir(t)
	storeTeam(t, dir)

	path := filepath.Join(dir, "agents", "backend.yaml")
	before, _ := sha256Of(t, path)

	r := arxi(t, dir, "blueprint", "create", "backend", "--members", "frontend,advisor")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2: the name on the command line is the thing that "+
			"is wrong, and every member it names is fine\n%s", r.code, r.out)
	}
	if after, _ := sha256Of(t, path); after != before {
		t.Errorf("agents/backend.yaml changed under a command that exited 2 "+
			"(%q -> %q)\n  consequence: the agent every other team was composed "+
			"from is gone, and the teams holding copies of it keep running, so "+
			"nothing reports the loss.", before, after)
	}
	for _, want := range []string{
		"agent already exists: " + filepath.Join("agents", "backend.yaml"),
		"arxi agent show backend",
		"an agent and a team compete for the same file",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: without the last line the operator reads \"already "+
				"exists\" about a team they never created and assumes the store is "+
				"broken.", want, r.out)
		}
	}
}

// TestAMissingMemberIsRefusedWithSomewhereToLook pins the other exit code.
//
// Exit 1, where a team named as a member is exit 2, and the line between them is
// worth keeping: this invocation becomes correct the moment `agent create nope`
// runs, without a character changing, while `--members feature-team` can never
// succeed as typed. That is the same distinction `blueprint validate` draws
// between "you called this wrong" and "the file is wrong", and a CI job reading
// only the code needs it to hold.
func TestAMissingMemberIsRefusedWithSomewhereToLook(t *testing.T) {
	dir := workdir(t)
	storeAgent(t, dir, "backend", "--tools", "read")

	r := arxi(t, dir, "blueprint", "create", "pair", "--members", "backend,nope")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1: the flags are well formed and the store is what "+
			"is missing a file\n%s", r.code, r.out)
	}
	noTeamFile(t, dir, "pair")

	for _, want := range []string{
		`member "nope": no such agent`,
		"arxi agent list",
		"arxi agent create nope",
		"nothing was written",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: a typo and a member that was never stored read "+
				"identically here, and both are answered by listing what is there "+
				"or writing the missing one.", want, r.out)
		}
	}
}

// TestAMemberlessFileIsRefusedRatherThanCopiedEmpty covers a file that loads.
//
// An agent file with no `members:` block parses, validates as YAML and yields
// nothing to copy. Composing from it would produce a team one member short of the
// count on the command line, and the shortfall is invisible: the team is valid, it
// runs, and the missing worker never appears anywhere. Exit 1, because the
// invocation was right and the file is what is empty.
func TestAMemberlessFileIsRefusedRatherThanCopiedEmpty(t *testing.T) {
	dir := workdir(t)
	storeAgent(t, dir, "backend", "--tools", "read")

	hollow := writeAgentFile(t, dir, "hollow",
		"name: hollow\n\nstages:\n  - {name: work, advance_when: all}\n")

	r := arxi(t, dir, "blueprint", "create", "pair", "--members", "backend,hollow")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1: the command line is right and the file it names "+
			"is empty\n%s", r.code, r.out)
	}
	noTeamFile(t, dir, "pair")

	for _, want := range []string{
		`member "hollow" has no members of its own`,
		hollow + " loads and is empty",
		"arxi agent show hollow",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: the file loads, so \"cannot read it\" would send the "+
				"operator looking for a syntax error that is not there.", want, r.out)
		}
	}
}

// TestAnAdvisoryOnlyTeamIsWarnedAboutAndTheWarningIsTrue runs the caveat.
//
// Created, not refused: every member is a valid agent, the file is valid, and one
// `advisory: true` deleted makes it a working team -- so the verb writes it and
// says what will happen. What it must not do is print `run it:` underneath, which
// would be an invitation to spend a budget on a run that enters work, activates
// nobody and stops.
//
// Then the caveat is checked by doing what it describes. A warning is a claim about
// a run, and a claim about a run that no test ever performs is the kind of sentence
// that stays in the source for a year after it stopped being true. The diagnosis is
// asserted too, because "goes quiescent after zero turns" is only useful if the run
// itself explains why: the advance rule is unsatisfiable, and it says so.
func TestAnAdvisoryOnlyTeamIsWarnedAboutAndTheWarningIsTrue(t *testing.T) {
	dir := workdir(t)
	storeAgent(t, dir, "advisor", "--tools", "read", "--advisory")

	c := arxi(t, dir, "blueprint", "create", "council", "--members", "advisor")
	if c.code != 0 {
		t.Fatalf("exit %d, want 0: an advisory-only team is one line away from "+
			"working, so it is written with a caveat rather than refused\n%s",
			c.code, c.out)
	}
	if !strings.Contains(c.out, "caveat: every member is advisory") {
		t.Errorf("the create screen carries no advisory-only caveat:\n%s\n"+
			"  consequence: the file looks like every other team, and the operator "+
			"finds out at the end of a run that bought nothing.", c.out)
	}
	if strings.Contains(c.out, "run it:") {
		t.Errorf("the create screen offers `run it:` for a team that cannot work:\n%s\n"+
			"  consequence: --budget is mandatory precisely so nobody spends by "+
			"accident, and this would be the tool spending it on a run it has "+
			"already diagnosed.", c.out)
	}

	r := arxi(t, dir, "run", "start", "council", "advise me",
		"--budget", "5.00", "--sim", "--run-id", "council")
	if r.code != 0 {
		t.Fatalf("run start: exit %d, want 0:\n%s", r.code, r.out)
	}
	if n := countEvents(t, dir, "council", "agent.activated"); n != 0 {
		t.Errorf("agent.activated × %d, want 0\n"+
			"  consequence: the caveat says \"activates nobody\". a turn here means "+
			"the screen is wrong about advisory members and money moves on a "+
			"submit that counts toward nothing.\n%s", n, r.out)
	}
	if n := countEvents(t, dir, "council", "run.quiescent"); n != 1 {
		t.Fatalf("run.quiescent × %d, want 1\n"+
			"  consequence: the caveat says the run goes quiescent. no event means a "+
			"run that stopped with no explanation in its own log, which is the "+
			"failure mode the diagnosis exists to prevent.\n%s", n, r.out)
	}
	payload, _ := eventOfType(t, dir, "council", "run.quiescent")["payload"].(map[string]any)
	if d, _ := payload["diagnosis"].(string); !strings.Contains(d, "unsatisfiable") {
		t.Errorf("the diagnosis is %q, want it to name the rule as unsatisfiable\n"+
			"  consequence: \"nobody can start\" sends the operator looking for a "+
			"stuck member. the truth is that this blueprint can never advance, and "+
			"the diagnosis is the only thing that tells waiting from never.", d)
	}
}

// TestAnEmptyMembersListIsAUsageErrorWithTheUsage covers the reachable empty.
//
// `--members ,` satisfies the flag's requiredness and then splits to nothing, which
// is the only way to reach this branch: an absent flag is caught by the parser with
// a different message. The team it would compose enters its first stage, activates
// nobody and goes quiescent after zero turns -- the same dead run as the
// advisory-only case, arrived at from the other direction.
//
// The usage block is printed with it, and that is a deliberate difference from
// every other refusal in this file: the others are about a member, and the operator
// only needs to hear which one. This one means the argument they got wrong is the
// shape of the command, so the shape is what they are shown.
func TestAnEmptyMembersListIsAUsageErrorWithTheUsage(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "blueprint", "create", "empty", "--members", ",")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2:\n%s", r.code, r.out)
	}
	noTeamFile(t, dir, "empty")

	for _, want := range []string{
		"--members is empty",
		"goes quiescent after zero turns",
		"usage: arxi blueprint create <name> --members a,b,c",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not contain %q:\n%s\n"+
				"  consequence: `--members ,` looks like a flag that was supplied, so "+
				"without the usage the operator re-reads their shell quoting instead "+
				"of the argument.", want, r.out)
		}
	}
}

// TestEditingAMemberDoesNotChangeATeamAlreadyComposed is why members are copied.
//
// The usage says it in capitals -- members are COPIED, not referenced -- and it is
// not a convenience. `run start` freezes runs/<id>/blueprint.snapshot.yaml and
// records its sha (ADR-0001, ADR-0002), so a team that referenced agents/backend.yaml
// would have part of its rules outside the bytes that were hashed: replaying last
// week's run would use today's tools, which is the same as having no replay.
//
// Widening a member is the case that shows the cost of the alternative. `bash` added
// to backend after the fact would reach every team ever composed from it, without a
// single team file changing and with nothing in any run's log to point at.
//
// This is also the reason the refusals above have to exist. Nothing revisits
// agents/backend.yaml later to notice a problem, so the only moment a team can be
// checked against the files it came from is the moment it is composed.
func TestEditingAMemberDoesNotChangeATeamAlreadyComposed(t *testing.T) {
	dir := workdir(t)
	storeTeam(t, dir)

	if c := arxi(t, dir, "blueprint", "create", "feature-team",
		"--members", "backend,frontend,advisor"); c.code != 0 {
		t.Fatalf("blueprint create: exit %d, want 0:\n%s", c.code, c.out)
	}
	team := filepath.Join(dir, "agents", "feature-team.yaml")
	before, _ := sha256Of(t, team)

	body := agentYAML(t, dir, "backend")
	widened := strings.Replace(body, "tools: [read, write]", "tools: [read, write, bash]", 1)
	if widened == body {
		t.Fatalf("the rendered tools line did not match, so nothing was widened "+
			"and this test would pass without testing anything:\n%s", body)
	}
	writeAgentFile(t, dir, "backend", widened)

	if after, _ := sha256Of(t, team); after != before {
		t.Errorf("the team file changed when its source agent was edited (%q -> %q)\n"+
			"  consequence: nothing wrote to that path, so this can only mean the "+
			"team is reading the member back. a run's frozen snapshot would then be "+
			"missing part of the rules its sha is supposed to cover.", before, after)
	}
	show := arxi(t, dir, "agent", "show", "feature-team")
	if strings.Contains(show.out, "bash") {
		t.Errorf("the composed team now reports bash:\n%s\n"+
			"  consequence: one edit to one agent would widen every team ever "+
			"composed from it, with no team file changed and nothing in any run's "+
			"log to point at.", show.out)
	}
}
