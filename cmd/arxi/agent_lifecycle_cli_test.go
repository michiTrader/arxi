package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// runItArgs turns the `run it:` line `agent create` printed into arguments.
//
// Parsed out of the output rather than rebuilt from the agent's name, because a
// retyped command tests the test author's memory of the screen. The defect class
// here is one verb printing a command another verb cannot honour, and the only
// way to catch that is to run the command that was actually printed.
//
// The objective is a placeholder token in quotes, and strings.Fields keeps it as
// one field because there is no space inside `"<objective>"`. Substituting that
// token beats splitting the line with quote-aware parsing: the subject is the
// command, and a shell-quoting parser here would be a second implementation of
// one to get wrong.
func runItArgs(t *testing.T, out, objective string) []string {
	t.Helper()

	line := ""
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "run it:"); i >= 0 {
			line = strings.TrimSpace(l[i+len("run it:"):])
		}
	}
	if line == "" {
		t.Fatalf("agent create printed no `run it:` line, so there is no "+
			"command to run:\n%s", out)
	}

	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "arxi" {
		t.Fatalf("the printed command is not an arxi invocation: %q", line)
	}
	args := fields[1:]
	for i, a := range args {
		if strings.HasPrefix(a, `"`) {
			args[i] = objective
		}
	}
	return args
}

// sha256Of is the digest blueprint.Load records for a file, computed the same way.
//
// Recomputed from the bytes on disk rather than read back out of the tool, so
// `agent show`, a run's blueprint_sha and the frozen snapshot are each compared
// against the file instead of against one another.
func sha256Of(t *testing.T, path string) (string, []byte) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw
}

// agentDoc is the shape both `agent list --json` and `agent show --json` emit.
//
// One local type for both, mirroring the single agentJSON the binary marshals:
// if the two verbs ever grow separate shapes, the test that unmarshals them into
// this struct is the one that notices.
type agentDoc struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	SHA     string   `json:"sha"`
	Stages  []string `json:"stages"`
	Members []struct {
		Name       string   `json:"name"`
		Model      string   `json:"model"`
		Tools      []string `json:"tools"`
		Activation string   `json:"activation"`
		Advisory   bool     `json:"advisory"`
		Resolved   []struct {
			Tool   string `json:"tool"`
			Policy string `json:"policy"`
		} `json:"resolved"`
	} `json:"members"`
}

// createReviewer stores the agent most walks below start from.
func createReviewer(t *testing.T, dir string) result {
	t.Helper()

	r := arxi(t, dir, "agent", "create", "reviewer",
		"--model", "claude-sonnet-4-6", "--tools", "read,grep")
	if r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}
	return r
}

// TestTheCommandAgentCreatePrintsIsACommandThatRuns pins the stageless defect.
//
// agent_cli_test.go next door spends fifteen tests on `agent tool policy` and
// covers nothing else: `agent create`, `agent list`, `agent show` and a
// `run start` that takes a name had no process-level coverage in any file, and
// internal/agentstore's own tests cannot see any of what follows, because none of
// it happens in that package.
//
// The gap shipped a defect. `agent create` wrote a stageless blueprint and
// printed `run it:` underneath it; applyRunStarted activates the members of the
// stage it enters and returns nil when the config declares none, so the run
// started, entered nothing, spawned no turn, and recorded run.quiescent as its
// second event. blueprint.Load accepted the file, `blueprint validate` passed it,
// `agent show` printed it, and the store's own test asserted the absence of a
// `stages:` block AS A FEATURE. Starting one by hand was the only thing that
// found it -- which is exactly the step no test in the tree was taking.
//
// Asserted through the log the reducer produced rather than by looking for
// `stages:` in the file: the stage is the mechanism, being asked to work is the
// property. A future runtime that spawns a turn for a lone member with no stage
// declared should make this test pass, not fail.
//
// --sim and --run-id are the only additions to the printed line. This suite
// registers no provider, and reading runs/ to learn what the run was called is a
// second thing to get wrong; neither changes which blueprint is resolved.
func TestTheCommandAgentCreatePrintsIsACommandThatRuns(t *testing.T) {
	dir := workdir(t)
	c := createReviewer(t, dir)

	args := append(runItArgs(t, c.out, "review the diff"), "--sim", "--run-id", "walk")
	r := arxi(t, dir, args...)
	if r.code != 0 {
		t.Fatalf("the command `agent create` printed exits %d:\n  arxi %s\n%s",
			r.code, strings.Join(args, " "), r.out)
	}

	if n := countEvents(t, dir, "walk", "agent.turn_done"); n < 1 {
		t.Errorf("the run took %d turns, want at least 1\n"+
			"  consequence: `agent create` prints a command that starts a run in "+
			"which nobody ever works -- the defect the stageless file shipped, "+
			"which every in-package check passed.\n%s", n, r.out)
	}
	if n := countEvents(t, dir, "walk", "run.quiescent"); n != 0 {
		t.Errorf("run.quiescent × %d, want 0\n"+
			"  consequence: \"nobody is working and nobody can start\" is the line "+
			"the stageless blueprint recorded as its second event, after zero "+
			"turns.\n%s", n, r.out)
	}
	if n := countEvents(t, dir, "walk", "stage.entered"); n != 1 {
		t.Errorf("stage.entered × %d, want 1\n"+
			"  consequence: Render writes exactly one stage, so any other count "+
			"means a run of a stored agent no longer matches the file.\n%s", n, r.out)
	}
}

// TestAStoredAgentAndItsPathLoadTheSameBytes is why an agent is a blueprint.
//
// The store renders a one-member blueprint and `run start` resolves a name to
// that file, so both spellings reach blueprint.Load with the same bytes. Byte
// equality rather than "both ran": the frozen snapshot is what `run replay`
// folds, and blueprint_sha is what a reader compares two runs by, so a name that
// loaded an equivalent-but-different config would make two runs of one agent
// incomparable while both looked fine.
func TestAStoredAgentAndItsPathLoadTheSameBytes(t *testing.T) {
	dir := workdir(t)
	createReviewer(t, dir)

	want, raw := sha256Of(t, filepath.Join(dir, "agents", "reviewer.yaml"))

	for _, tc := range []struct{ id, actor string }{
		{"byname", "reviewer"},
		{"bypath", filepath.Join("agents", "reviewer.yaml")},
	} {
		r := arxi(t, dir, "run", "start", tc.actor, "review the diff",
			"--sim", "--budget", "5.00", "--run-id", tc.id)
		if r.code != 0 {
			t.Fatalf("run start %s: exit %d, want 0:\n%s", tc.actor, r.code, r.out)
		}

		snap, err := os.ReadFile(filepath.Join(dir, "runs", tc.id, "blueprint.snapshot.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(snap, raw) {
			t.Errorf("run start %s froze bytes that are not agents/reviewer.yaml\n"+
				"  consequence: `run replay` folds the snapshot, so the two "+
				"spellings of one agent replay as different configs.\n"+
				"--- snapshot\n%s\n--- stored\n%s", tc.actor, snap, raw)
		}

		payload, _ := eventOfType(t, dir, tc.id, "run.started")["payload"].(map[string]any)
		if got := payload["blueprint_sha"]; got != want {
			t.Errorf("run start %s recorded blueprint_sha %v, want %s\n"+
				"  consequence: the sha is how two runs are known to be of the "+
				"same config; by name and by path would read as two agents.",
				tc.actor, got, want)
		}
	}

	// The same digest, in the two lengths the two views print.
	show := arxi(t, dir, "agent", "show", "reviewer")
	if !strings.Contains(show.out, want[:12]) {
		t.Errorf("agent show does not print the file's sha (%s):\n%s\n"+
			"  consequence: that line is what a reader matches against a run's "+
			"blueprint_sha, and it would be describing some other bytes.",
			want[:12], show.out)
	}
	j := arxi(t, dir, "agent", "show", "reviewer", "--json")
	var doc agentDoc
	if err := json.Unmarshal([]byte(j.out), &doc); err != nil {
		t.Fatalf("agent show --json does not parse: %v\n%s", err, j.out)
	}
	if doc.SHA != want {
		t.Errorf("agent show --json sha = %q, want %q\n"+
			"  consequence: a machine reading this cannot join it to the "+
			"blueprint_sha in a run's log.", doc.SHA, want)
	}
}

// TestAgentListAndAgentShowAgreeAboutTheSameAgent.
//
// Both verbs marshal the same agentJSON, and this is what keeps that true. The
// two screens answer "what is stored" and "what will run"; a reader who has to
// reconcile two shapes for one record stops trusting either, and two shapes mean
// two loaders that drift the first time a member field is added.
//
// Only `resolved` differs, deliberately: `agent list` does not read policies/,
// which is what lets it list agents without creating that directory.
func TestAgentListAndAgentShowAgreeAboutTheSameAgent(t *testing.T) {
	dir := workdir(t)
	createReviewer(t, dir)

	l := arxi(t, dir, "agent", "list", "--json")
	var listed []agentDoc
	if err := json.Unmarshal([]byte(l.out), &listed); err != nil {
		t.Fatalf("agent list --json does not parse: %v\n%s", err, l.out)
	}
	if len(listed) != 1 {
		t.Fatalf("agent list --json has %d entries, want 1:\n%s", len(listed), l.out)
	}

	s := arxi(t, dir, "agent", "show", "reviewer", "--json")
	var shown agentDoc
	if err := json.Unmarshal([]byte(s.out), &shown); err != nil {
		t.Fatalf("agent show --json does not parse: %v\n%s", err, s.out)
	}

	got, want := listed[0], shown
	if len(got.Members) != 1 || len(want.Members) != 1 {
		t.Fatalf("a one-member agent is described with %d and %d members",
			len(got.Members), len(want.Members))
	}

	// `resolved` is the one field expected to differ, so it is asserted before it
	// is cleared. Before, and not after: copying the struct copies Members' slice
	// header, so clearing the field on one copy clears it on the other.
	if r := got.Members[0].Resolved; len(r) != 0 {
		t.Errorf("agent list resolved policies: %v\n"+
			"  consequence: listing agents would have to read policies/, and "+
			"reading is the operation that must create nothing.", r)
	}
	if n := len(want.Members[0].Resolved); n != 2 {
		t.Errorf("agent show resolved %d tools, want 2 (grep, read)\n"+
			"  consequence: `agent show` is documented as what `run start` will "+
			"execute, and the policy each granted tool resolves to is half of "+
			"that.\n%s", n, s.out)
	}

	// Then everything else, compared whole, so a field either verb grows later is
	// covered without being named here.
	got.Members[0].Resolved, want.Members[0].Resolved = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list and show describe the same agent differently\n"+
			"  list: %+v\n  show: %+v\n"+
			"  consequence: one record in two shapes is two loaders for a caller, "+
			"and they drift the first time a member field is added.", got, want)
	}
}

// TestAFileInTheWorkingDirectoryWinsOverAStoredAgentOfTheSameName.
//
// resolveActor stats the argument before it consults the store, so a stored agent
// can never shadow the file being edited. The direction matters: a name that
// silently preferred agents/ would run yesterday's copy of the blueprint the user
// is looking at, and every screen would agree it had done the right thing.
//
// The local file has no extension, which is the case that has to work for the
// rule to be about existence rather than about spelling.
func TestAFileInTheWorkingDirectoryWinsOverAStoredAgentOfTheSameName(t *testing.T) {
	dir := workdir(t)
	createReviewer(t, dir)

	body := []byte("name: from-the-file\n" +
		"members:\n  - {name: solo, role: implementer, tools: [read]}\n" +
		"stages:\n  - {name: work, advance_when: all}\n")
	if err := os.WriteFile(filepath.Join(dir, "reviewer"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	r := arxi(t, dir, "run", "start", "reviewer", "review the diff",
		"--sim", "--budget", "5.00", "--run-id", "filewins")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", r.code, r.out)
	}

	payload, _ := eventOfType(t, dir, "filewins", "run.started")["payload"].(map[string]any)
	if payload["actor"] != "from-the-file" {
		t.Errorf("the run's actor is %v, want from-the-file\n"+
			"  consequence: a stored agent shadowed the file in the working "+
			"directory, so `run start reviewer` ran something the user is not "+
			"editing.\n%s", payload["actor"], r.out)
	}
	snap, err := os.ReadFile(filepath.Join(dir, "runs", "filewins", "blueprint.snapshot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap, body) {
		t.Errorf("the frozen snapshot is not the file that won:\n%s\n"+
			"  consequence: './name.yaml' would stop being a way to say which of "+
			"the two you meant.", snap)
	}
}

// TestANameNothingAnswersToIsNotCalledAnInvalidBlueprint.
//
// "blueprint is not valid" plus a parse error is the right sentence for a broken
// file and the wrong one for a typo: it sends the reader to inspect a file that is
// not there. Now that a bare name is a legal argument the typo is the common case,
// so the refusal names both places it looked and what to ask next.
func TestANameNothingAnswersToIsNotCalledAnInvalidBlueprint(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "run", "start", "ghost", "review the diff", "--sim", "--budget", "5.00")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", r.code, r.out)
	}
	if strings.Contains(r.out, "blueprint is not valid") {
		t.Errorf("a name nothing answers to is reported as an invalid blueprint:\n%s\n"+
			"  consequence: the reader is sent to debug a file that does not "+
			"exist, over a validator error about bytes nobody wrote.", r.out)
	}
	for _, want := range []string{
		"no such blueprint file and no such stored agent",
		"looked for a file at ghost",
		"and a stored agent at " + filepath.Join("agents", "ghost.yaml"),
		"arxi agent list",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: a mistyped name does not say which two places "+
				"were searched, so a typo cannot be told from a missing file.",
				want, r.out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); !os.IsNotExist(err) {
		t.Errorf("a failed resolution left runs/ behind (err=%v)\n"+
			"  consequence: the run directory would be created before the actor is "+
			"known to exist, so `run list` grows an entry for a run that never "+
			"started.", err)
	}
}

// TestADottedAgentNameIsResolvedFromTheStore is the filepath.Ext trap.
//
// looksLikePath accepts any separator, then only .yaml/.yml, because
// filepath.Ext("v1.2") is ".2": a version-numbered agent would otherwise be
// looked for on the filesystem and reported missing while its file sat in
// agents/. yamlScalar allows the dot, so the name is legal to create -- which is
// what makes the failure reachable rather than theoretical.
func TestADottedAgentNameIsResolvedFromTheStore(t *testing.T) {
	dir := workdir(t)

	if r := arxi(t, dir, "agent", "create", "v1.2", "--tools", "read"); r.code != 0 {
		t.Fatalf("agent create v1.2: exit %d, want 0:\n%s", r.code, r.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "v1.2.yaml")); err != nil {
		t.Fatalf("the store did not write agents/v1.2.yaml: %v", err)
	}

	r := arxi(t, dir, "run", "start", "v1.2", "review the diff",
		"--sim", "--budget", "5.00", "--run-id", "dotted")
	if r.code != 0 {
		t.Fatalf("run start v1.2: exit %d, want 0:\n%s\n"+
			"  consequence: an agent this tool created cannot be run by the name "+
			"it was created with.", r.code, r.out)
	}
	if n := countEvents(t, dir, "dotted", "agent.turn_done"); n < 1 {
		t.Errorf("the run took %d turns, want at least 1\n"+
			"  consequence: the name resolved to something that does no work, "+
			"which is a pass for the wrong reason.", n)
	}
}

// TestAnAdvisoryAgentIsNotPromisedARunItCannotDo.
//
// The same class of false promise as the stageless file, and this one cannot be
// fixed in the file: an advisory member starts MemberInactive, so a run of that
// agent alone enters the stage, activates nobody and goes quiescent after zero
// turns. The run below is that measurement, kept here so the caveat `agent create`
// prints cannot outlive the behaviour it describes.
//
// If a future reducer gives a lone advisory member a turn, this is the test that
// fails, and the fix is to delete the caveat and print `run it:` again.
func TestAnAdvisoryAgentIsNotPromisedARunItCannotDo(t *testing.T) {
	dir := workdir(t)

	c := arxi(t, dir, "agent", "create", "watcher",
		"--model", "claude-sonnet-4-6", "--tools", "read", "--advisory")
	if c.code != 0 {
		t.Fatalf("agent create --advisory: exit %d, want 0:\n%s", c.code, c.out)
	}
	if strings.Contains(c.out, "run it:") {
		t.Errorf("an advisory agent is offered a command that cannot work:\n%s\n"+
			"  consequence: the printed run enters the stage, activates nobody, "+
			"and is quiescent after zero turns.", c.out)
	}
	for _, want := range []string{"caveat: advisory", "use it:"} {
		if !strings.Contains(c.out, want) {
			t.Errorf("the create output does not say %q:\n%s\n"+
				"  consequence: withholding `run it:` without saying why reads as "+
				"an omission, and the user runs it anyway.", want, c.out)
		}
	}

	// The behaviour the caveat states in prose, in the log.
	r := arxi(t, dir, "run", "start", "watcher", "watch it",
		"--sim", "--budget", "5.00", "--run-id", "advisory")
	if r.code != 0 {
		t.Fatalf("run start: exit %d, want 0:\n%s", r.code, r.out)
	}
	var types []string
	for _, ev := range allEvents(t, dir, "advisory") {
		types = append(types, fmt.Sprint(ev["type"]))
	}
	if want := []string{"run.started", "stage.entered", "run.quiescent"}; !reflect.DeepEqual(types, want) {
		t.Errorf("the log is %v, want %v\n"+
			"  consequence: the caveat describes this exact sequence, so if it has "+
			"changed then the sentence `agent create` prints is now wrong.",
			types, want)
	}
}

// TestReadingTheAgentStoreCreatesNothing.
//
// `agent list` in a directory with no agents answers, and leaves the directory as
// it found it. Worth a test because the natural implementation breaks it: every
// writer here opens the store through a constructor that calls MkdirAll, so a
// reader opening it the same way creates agents/ as a side effect of being asked
// what is in it. A fresh checkout then grows directories by being inspected, and
// .gitignore acquires entries for debris.
func TestReadingTheAgentStoreCreatesNothing(t *testing.T) {
	dir := workdir(t)

	l := arxi(t, dir, "agent", "list")
	if l.code != 0 {
		t.Fatalf("agent list: exit %d, want 0:\n%s", l.code, l.out)
	}
	if !strings.Contains(l.out, "no agents in") {
		t.Errorf("agent list in an empty directory says:\n%s\n"+
			"  consequence: nothing tells the user where agents live or how to "+
			"make one.", l.out)
	}
	if s := arxi(t, dir, "agent", "show", "ghost"); s.code != 1 {
		t.Errorf("agent show of an unknown agent exits %d, want 1:\n%s", s.code, s.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents")); !os.IsNotExist(err) {
		t.Fatalf("reading the store created agents/ (err=%v)\n"+
			"  consequence: two read-only verbs leave a directory behind, so a "+
			"clone grows one by being looked at.", err)
	}

	// Showing a real agent does not create policies/ either: the override lookup
	// stats the directory first, so an agent with no overrides reads nothing.
	createReviewer(t, dir)
	if s := arxi(t, dir, "agent", "show", "reviewer"); s.code != 0 {
		t.Fatalf("agent show: exit %d, want 0:\n%s", s.code, s.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "policies")); !os.IsNotExist(err) {
		t.Errorf("agent show created policies/ (err=%v)\n"+
			"  consequence: the directory `agent tool policy` writes appears "+
			"before any policy exists, so its presence stops meaning anything.", err)
	}
}

// TestASecondCreateNeverOverwrites.
//
// `agent create` is the only verb that writes agents/, and the file it writes
// carries tool grants. A silent overwrite would widen or narrow a grant under a
// name somebody is already running, which is the one mistake here that cannot be
// noticed by reading the output. The sha is compared rather than the text, so the
// assertion is about the bytes and not about Render's formatting.
func TestASecondCreateNeverOverwrites(t *testing.T) {
	dir := workdir(t)

	if r := arxi(t, dir, "agent", "create", "security", "--tools", "read"); r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}
	path := filepath.Join(dir, "agents", "security.yaml")
	before, _ := sha256Of(t, path)

	r := arxi(t, dir, "agent", "create", "security", "--tools", "write")
	if r.code != 2 {
		t.Errorf("a create over an existing agent exits %d, want 2:\n%s\n"+
			"  consequence: a script cannot tell that the name was taken.",
			r.code, r.out)
	}
	if after, _ := sha256Of(t, path); after != before {
		t.Errorf("the stored agent changed: %s -> %s\n"+
			"  consequence: a tool grant was rewritten under a name already in "+
			"use, and nothing in the output says so.", before, after)
	}
	for _, want := range []string{"already exists", "arxi agent show security"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: the user is not told the name is taken, nor how to "+
				"read what is already under it.", want, r.out)
		}
	}
}

// TestAgentShowMarksAnOverrideEvenWhenItMatchesTheDefault.
//
// The `override` mark is keyed on presence in the policy map, not on difference
// from today's default, and this is the case that shows why. `agent tool policy
// --allow read` answers "which is already the default. the override is recorded
// but changes nothing", so from then on `agent show` is the only screen that says
// a file exists at all -- one that keeps applying if the default changes, and that
// `--reset` still removes.
func TestAgentShowMarksAnOverrideEvenWhenItMatchesTheDefault(t *testing.T) {
	dir := workdir(t)

	if r := arxi(t, dir, "agent", "create", "reviewer",
		"--model", "claude-sonnet-4-6", "--tools", "read,write"); r.code != 0 {
		t.Fatalf("agent create: exit %d, want 0:\n%s", r.code, r.out)
	}
	if r := arxi(t, dir, "agent", "tool", "policy",
		"--agent", "reviewer", "--allow", "read"); r.code != 0 {
		t.Fatalf("agent tool policy: exit %d, want 0:\n%s", r.code, r.out)
	}

	s := arxi(t, dir, "agent", "show", "reviewer")
	if s.code != 0 {
		t.Fatalf("agent show: exit %d, want 0:\n%s", s.code, s.out)
	}
	if !strings.Contains(s.out, "read (allow, override)") {
		t.Errorf("the tool line does not mark the override:\n%s\n"+
			"  consequence: an override that agrees with the default is "+
			"indistinguishable from the default, so the file that would outlive a "+
			"change to it is invisible.", s.out)
	}
	if !strings.Contains(s.out, "write (ask)") {
		t.Errorf("the tool with no override is not shown at its default:\n%s\n"+
			"  consequence: marking one tool while mis-stating another makes the "+
			"screen worse than silent.", s.out)
	}
	for _, want := range []string{
		filepath.Join("policies", "reviewer.json"),
		"arxi agent tool policy --agent reviewer --reset read",
	} {
		if !strings.Contains(s.out, want) {
			t.Errorf("the policy note does not say %q:\n%s\n"+
				"  consequence: the mark says a file applies without saying where "+
				"it is or how to undo it.", want, s.out)
		}
	}
}
