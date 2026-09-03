package agentstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
)

// open makes a store in a disposable directory, under the name a real
// repository would use.
//
// filepath.Join(TempDir, DefaultDir) rather than the temp dir itself, so every
// test below goes through the same Path() the CLI does. A store rooted at the
// bare temp dir would keep passing while Path was wrong.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestACreatedAgentIsABlueprintTheRunnerCanExecute is the point of the package.
//
// It asserts against the loaded kernel.Config rather than against the rendered
// bytes, because what `agent create` promises is not "a file appeared" -- it is
// that `run start reviewer` will execute this member, with this model and these
// tools. A test that compared YAML text would pass just as well if the parser
// read none of it back, which is the failure that matters.
func TestACreatedAgentIsABlueprintTheRunnerCanExecute(t *testing.T) {
	s := open(t)

	path, err := s.Create(Record{
		Name:  "reviewer",
		Model: "claude-sonnet-4-6",
		Role:  "review the diff in HEAD and list real risks",
		Tools: []string{"read", "grep"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := s.Path("reviewer"); path != want {
		t.Errorf("Create returned %q, want %q\n"+
			"  consequence: `agent create` prints this path as the next thing to "+
			"open, and the user would edit a file nothing reads.\n"+
			"  fix: return s.Path(r.Name), the same join Load uses.", path, want)
	}

	bp, err := s.Load("reviewer")
	if err != nil {
		t.Fatalf("Load right after Create: %v\n"+
			"  consequence: the store wrote a file it cannot read back, so "+
			"`agent show reviewer` and `run start reviewer` both fail on bytes "+
			"this package produced and the user cannot correct.", err)
	}
	if bp.Name != "reviewer" {
		t.Errorf("blueprint name is %q, want %q\n"+
			"  consequence: the run is named after the blueprint, so `run list` "+
			"would show a run belonging to no agent.", bp.Name, "reviewer")
	}
	if n := len(bp.Config.Members); n != 1 {
		t.Fatalf("the rendered agent has %d members, want exactly 1\n"+
			"  consequence: an agent IS a one-member blueprint. Zero members is a "+
			"run with nobody to open a turn; two is a team nobody declared.", n)
	}
	m := bp.Config.Members[0]
	if m.Name != "reviewer" || m.Model != "claude-sonnet-4-6" {
		t.Errorf("member is {name:%q model:%q}, want {reviewer claude-sonnet-4-6}\n"+
			"  consequence: `run steer reviewer` addresses members by name, and "+
			"the model decides what is actually billed.", m.Name, m.Model)
	}
	if got := strings.Join(m.Tools, ","); got != "read,grep" {
		t.Errorf("member tools are %q, want %q\n"+
			"  consequence: the grant list is checked first by tool.Resolve, so a "+
			"lost tool is a refusal mid-run for a tool the user granted.", got, "read,grep")
	}
}

// TestTheRenderedAgentEntersAStageAndActivatesItsMember is the test that says
// the file can run, and it used to assert the opposite.
//
// It was TestTheRenderedAgentDeclaresNoStages, arguing that one member has
// nothing to advance between and that a stage nobody wrote is a surprise in the
// diff. Both true, and both beside the point: applyRunStarted activates the
// members OF THE ENTERED STAGE, and returns nil when len(c.Stages) == 0. So the
// stageless file it was protecting started, activated nobody, and recorded
// run.quiescent -- "nobody is working and nobody can start" -- as the second
// event, after zero turns. `blueprint validate` passed it, `agent show` printed
// it, and the only symptom was a run that did nothing.
//
// So it is asserted through Decide rather than by counting stages. The stage is
// the mechanism; being activated is the property, and a future reducer that
// activates a lone member without a stage should make this test pass, not fail.
// internal/kernel's TestRunStartedWithoutStagesEntersNothing still pins the
// stageless arm itself -- what changed is that a stored agent no longer takes it.
func TestTheRenderedAgentEntersAStageAndActivatesItsMember(t *testing.T) {
	raw, err := Record{Name: "solo", Model: "claude-sonnet-4-6", Tools: []string{"read"}}.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	bp, err := blueprint.Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Two steps, because activation is not applyRunStarted's job: it emits
	// stage.entered, and applyStageEntered activates that stage's members. The
	// event is fed back the way the runtime feeds it back -- appended, then
	// reduced -- so the property under test is "a run of this file reaches an
	// activation", not "one function returned one effect".
	s, fx := kernel.Decide(kernel.State{}, kernel.Event{
		Seq: 1, ID: "e1", Type: kernel.RunStarted, Scope: "run:r1",
		Source: kernel.SourceRuntime,
		Payload: map[string]any{
			"run_id": "r1", "actor": "solo", "budget_usd": 2.0,
		},
	}, bp.Config)

	entered := firstEmit(fx, kernel.StageEntered)
	if entered == nil {
		t.Fatalf("run.started on a stored agent enters no stage; effects were %s\n"+
			"  consequence: applyRunStarted returns nil for a blueprint with no "+
			"stages, so the run activates nobody and records run.quiescent after "+
			"zero turns -- while `agent create` prints `run it:` and every "+
			"validator passes the file.\n"+
			"  fix: Render writes a stages: block.", effectTypes(fx))
	}

	next := *entered
	next.Seq, next.ID = 2, "e2"
	_, fx = kernel.Decide(s, next, bp.Config)

	// SpawnTurn, not the agent.activated event: the turn is the effect that costs
	// money and does the work, and it is what a stageless file never produced.
	spawned := ""
	for _, f := range fx {
		if st, ok := f.(kernel.SpawnTurn); ok {
			spawned = st.Agent
		}
	}
	if spawned != "solo" {
		t.Errorf("entering stage %q spawns a turn for %q, want solo; effects were %s\n"+
			"  consequence: the run this store exists to make possible enters a "+
			"stage and then goes quiescent, having asked nobody to do anything.\n"+
			"  fix: the rendered member must be a member of the rendered stage.",
			entered.Str("stage"), spawned, effectTypes(fx))
	}

	// The text too, because the file is meant to be grown by hand and a member
	// added to a file with no stage would silently not run either.
	if !strings.Contains(string(raw), "\nstages:") {
		t.Errorf("the rendered file has no stages: block:\n%s\n"+
			"  consequence: a second member added by hand joins a file that "+
			"activates nobody, and the diff gives the author no line to add one to.", raw)
	}
}

// firstEmit returns the first emitted event of one type, or nil.
func firstEmit(fx []kernel.Effect, t kernel.EventType) *kernel.Event {
	for _, f := range fx {
		if em, ok := f.(kernel.Emit); ok && em.Event.Type == t {
			e := em.Event
			return &e
		}
	}
	return nil
}

// effectTypes names what came back, for a failure that has to say what happened
// instead of what did not. "activates nobody" plus [Emit run.quiescent] is a
// diagnosis; "activates nobody" alone is a bug report.
func effectTypes(fx []kernel.Effect) string {
	if len(fx) == 0 {
		return "[]"
	}
	names := make([]string, 0, len(fx))
	for _, f := range fx {
		if em, ok := f.(kernel.Emit); ok {
			names = append(names, string(em.Event.Type))
			continue
		}
		names = append(names, fmt.Sprintf("%T", f))
	}
	return "[" + strings.Join(names, " ") + "]"
}

// hostileNames are names that the YAML subset would read as something other
// than the text they are spelled with, unless they are quoted.
//
// Three separate hazards, and they fail differently, which is why they are all
// here. `yes`/`no`/`on`/`off` in any case are a HARD ERROR in the subset by
// name -- 1.1 reads them as booleans and 1.2 as strings, so the parser refuses
// to pick a side -- and an unquoted one would make `arxi agent create no` write
// a file that the very next command cannot read at all. `true`, `null`, `~` and
// anything numeric would parse, as the wrong Go type, and validation would then
// report "members[0]: name must be text" about a file the user never typed. The
// rest are structural: a leading `-` opens a sequence item, a `#` after a space
// opens a comment, and a `[`, `{` or `,` opens a flow collection.
var hostileNames = []string{
	"no", "No", "NO", "yes", "on", "off", "true", "false", "null", "~",
	"1", "007", "1.5", "-3", "0x10", "1e6",
	"-lead", ".hidden", "a:b", "#lead", "a, b", "[x]", "{x}", "a b",
	"*anchor", "&anchor", "!bang", "|pipe", ">gt", "%pct", "@at", "`tick",
	"'quoted'", `"quoted"`, "ünïcode", "日本語",
}

// TestANameTheParserWouldMisreadReadsBackExactly.
//
// Through Render and blueprint.Load rather than through Create, on purpose: `:`
// and `"` are legal in a POSIX filename and refused by Windows, and the quoting
// rule under test has nothing to do with either. The filesystem half of the
// same question is TestAnAwkwardNameSurvivesTheFilesystemToo below.
//
// Equality against the name that went in is the whole assertion. "It loaded" is
// not enough: `name: 007` loads as the integer 7 and `name: 1e6` as a float, and
// both would produce a validation complaint about a file the user never wrote.
func TestANameTheParserWouldMisreadReadsBackExactly(t *testing.T) {
	for _, name := range hostileNames {
		raw, err := Record{Name: name, Model: "claude-sonnet-4-6"}.Render()
		if err != nil {
			t.Errorf("Render(%q): %v\n"+
				"  consequence: `arxi agent create %s` is refused, or worse writes "+
				"a file the next command cannot read. The name is legal -- it is "+
				"only awkward for the parser -- so quoting is this package's job.\n"+
				"  fix: yamlScalar must quote it; see plainSafe.", name, name, err)
			continue
		}
		bp, err := blueprint.Load(raw)
		if err != nil {
			t.Errorf("Load of the render of %q: %v\n%s", name, err, raw)
			continue
		}
		if bp.Name != name {
			t.Errorf("top-level name round-tripped %q as %q\n%s\n"+
				"  consequence: `run start` resolves the agent by this exact "+
				"string, so the agent could never be started by the name it was "+
				"created with.", name, bp.Name, raw)
		}
		if len(bp.Config.Members) != 1 || bp.Config.Members[0].Name != name {
			t.Errorf("member name round-tripped %q as %+v\n%s\n"+
				"  consequence: `run steer` addresses members by name and "+
				"State.Member() compares it exactly; the member would be "+
				"unaddressable.", name, bp.Config.Members, raw)
		}
	}
}

// TestAnAwkwardNameSurvivesTheFilesystemToo is the other half: the ones that
// are both awkward for the parser and legal as filenames everywhere.
//
// It goes through Create and Load so the name makes a full trip -- rendered,
// quoted, written under name+ext, globbed by Names, read back by LoadFile --
// because Path() and Names() also handle it and neither is exercised above.
func TestAnAwkwardNameSurvivesTheFilesystemToo(t *testing.T) {
	for _, name := range []string{"no", "true", "1", "-lead", ".hidden", "a b"} {
		s := open(t)
		if _, err := s.Create(Record{Name: name, Tools: []string{"read"}}); err != nil {
			t.Errorf("Create(%q): %v", name, err)
			continue
		}
		bp, err := s.Load(name)
		if err != nil {
			t.Errorf("Load(%q): %v\n"+
				"  consequence: the agent exists on disk and no command can reach "+
				"it -- `agent show %s` and `run start %s` both fail.", name, err, name, name)
			continue
		}
		if bp.Name != name {
			t.Errorf("Load(%q) returned an agent named %q", name, bp.Name)
		}

		names, err := s.Names()
		if err != nil {
			t.Errorf("Names after creating %q: %v", name, err)
			continue
		}
		if len(names) != 1 || names[0] != name {
			t.Errorf("Names() is %q after creating %q\n"+
				"  consequence: `agent list` is how an agent is discovered at all. "+
				"An agent missing from it exists only to whoever remembers the name.",
				names, name)
		}
	}
}

// TestCreateRefusesToReplaceAnAgentAndLeavesTheFileAlone.
//
// The surface marks `agent create` Mutates and NOT Idempotent, so a second call
// with the same name is not obliged to be a no-op -- and the only alternative to
// refusing is overwriting, which turns a repeated command into a destructive
// one. This file is designed to be edited by hand, so what an overwrite destroys
// is a member list somebody grew, tools they added and a stage they wrote.
//
// The second assertion is the load-bearing one. A Create that refused AFTER
// truncating would satisfy the error check and still have eaten the file, and
// that failure is invisible until somebody opens it.
func TestCreateRefusesToReplaceAnAgentAndLeavesTheFileAlone(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "backend", Tools: []string{"read"}}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Stand in for the hand editing the package doc promises is expected.
	edited := "# grown by hand\nname: backend\n\nmembers:\n  - name: backend\n    advisory: false\n  - name: helper\n    advisory: true\n"
	if err := os.WriteFile(s.Path("backend"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.Create(Record{Name: "backend", Tools: []string{"write"}})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Create returned %v, want ErrExists\n"+
			"  consequence: the CLI distinguishes three outcomes by sentinel -- "+
			"exists is exit 2 with \"your edits are safe\", missing is exit 1, an "+
			"I/O error is neither. Without the sentinel all three read alike.", err)
	}
	if err != nil && !strings.Contains(err.Error(), s.Path("backend")) {
		t.Errorf("the refusal does not name the file: %v\n"+
			"  consequence: the user is told the name is taken and not where the "+
			"agent already wearing it lives, so they cannot look at it before "+
			"choosing another name.", err)
	}

	after, readErr := os.ReadFile(s.Path("backend"))
	if readErr != nil {
		t.Fatalf("reading the file after a refused Create: %v", readErr)
	}
	if string(after) != edited {
		t.Errorf("the refused Create changed the file on disk:\n--- want\n%s\n--- got\n%s\n"+
			"  consequence: refusing is only safe if it happens before any write. "+
			"A refusal that truncates first destroys exactly the hand edits the "+
			"refusal claims to be protecting.", edited, after)
	}
}

// TestAnUnknownToolNeverBecomesAFile.
//
// internal/tool/policy.go's own comment makes this promise: `--tools reed`
// should say so now, not grant nothing and fail halfway through a paid run. The
// check lives in Record.Validate rather than only in the CLI so the agent-facing
// `arxi_agent_create` passes through it as well.
//
// The second half -- that no file exists afterwards -- is the part worth
// asserting. An agent stored with a grant that resolves to nothing is a member
// that will be refused mid-run for a tool its own file lists.
func TestAnUnknownToolNeverBecomesAFile(t *testing.T) {
	s := open(t)

	_, err := s.Create(Record{Name: "typo", Tools: []string{"read", "reed"}})
	if err == nil {
		t.Fatal("Create accepted the tool \"reed\"\n" +
			"  consequence: the agent is stored with a grant no resolver knows, " +
			"so the refusal arrives mid-run, after the budget has been spent.")
	}
	if !strings.Contains(err.Error(), "reed") {
		t.Errorf("the refusal does not name the bad tool: %v\n"+
			"  consequence: the user retypes the whole --tools list guessing which "+
			"entry was wrong.", err)
	}
	if _, statErr := os.Stat(s.Path("typo")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file exists at %s after a refused Create (%v)\n"+
			"  consequence: `agent list` shows an agent the user was told was not "+
			"created, and the name is now taken by it.", s.Path("typo"), statErr)
	}
}

// TestANameThatWouldEscapeTheStoreIsRefused.
//
// Both directions, because they fail differently. Create with a separator would
// write outside agents/ -- silently, since MkdirAll made the parent -- and Load
// with one is a path traversal reachable from `arxi_agent_show` with an
// agent-supplied argument: agents/../../etc/passwd is not an agent name.
//
// Refused and not sanitised. Somebody who typed a slash meant something by it,
// and quietly renaming their agent is a worse answer than saying it cannot be
// spelled that way.
func TestANameThatWouldEscapeTheStoreIsRefused(t *testing.T) {
	s := open(t)
	for _, name := range []string{"a/b", `a\b`, ".", "..", "../escape", "/abs"} {
		if _, err := s.Create(Record{Name: name}); err == nil {
			t.Errorf("Create accepted the name %q\n"+
				"  consequence: the name becomes a filename, so the agent is "+
				"written outside agents/ and no reader of the store will find it.", name)
		}
		if _, err := s.Load(name); !errors.Is(err, ErrNotExist) {
			t.Errorf("Load(%q) returned %v, want ErrNotExist\n"+
				"  consequence: `arxi_agent_show` takes this argument from an "+
				"agent. A path separator turns it into a reader of any file the "+
				"process can open.", name, err)
		}
	}
}

// TestANameWithInvisibleCharactersIsRefusedRatherThanTrimmed.
//
// The name is the word `run start` is given and the word `run steer` addresses,
// and both compare it exactly. An agent called "reviewer " would fail to be
// addressed for a reason that appears nowhere on screen -- the user sees
// "reviewer" in `agent list` and types "reviewer", and it is a different string.
// Trimming would be worse than refusing: it makes the stored name differ from
// the one that was typed, silently.
//
// A control character is the same problem with three victims: the `#` header of
// the rendered file, the filename itself (a newline is legal in one on unix),
// and the aligned columns of `agent list`. None of the three would name a cause.
func TestANameWithInvisibleCharactersIsRefusedRatherThanTrimmed(t *testing.T) {
	for _, name := range []string{" reviewer", "reviewer ", "\treviewer", "a\nb", "a\x00b", "a\x1bb", ""} {
		if err := (Record{Name: name, Model: "m"}).Validate(); err == nil {
			t.Errorf("Validate accepted the name %q\n"+
				"  consequence: the name is compared exactly by `run start` and "+
				"`run steer`. A name carrying whitespace or a control character "+
				"cannot be typed back, and the failure names no cause.", name)
		}
	}
}

// TestNoAgentsDirectoryIsNotAnError.
//
// `agent list` in a fresh repository is a legitimate question whose answer is
// "none", and the precedent is set by `model list`, `trigger list` and `inbox`,
// which print real empty output rather than complaining about a missing store.
// Open creates the directory, so this is reached through a store whose directory
// was removed -- which is also what a user does when they delete agents/.
func TestNoAgentsDirectoryIsNotAnError(t *testing.T) {
	s := open(t)
	if err := os.RemoveAll(s.Dir()); err != nil {
		t.Fatal(err)
	}

	names, err := s.Names()
	if err != nil {
		t.Fatalf("Names on a missing directory: %v\n"+
			"  consequence: `agent list` fails in every repository that has never "+
			"created an agent, which is every repository at the moment somebody "+
			"first runs it.", err)
	}
	if len(names) != 0 {
		t.Errorf("Names returned %q for a missing directory, want none", names)
	}

	entries, err := s.List()
	if err != nil {
		t.Fatalf("List on a missing directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List returned %d entries for a missing directory", len(entries))
	}
}

// TestAHalfWrittenAgentIsNeverVisible.
//
// Two claims, and the second is the one that survives a refactor. First, that no
// temp file is left behind by a successful write. Second, that the temp NAME
// cannot end in the extension Names() globs for -- because that invariant, not
// the cleanup, is what makes a concurrent reader safe. A truncated YAML file
// would be reported to the user as a validation error against a file they never
// wrote and cannot correct, which is the most confusing failure this package
// could produce.
func TestAHalfWrittenAgentIsNeverVisible(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "backend", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file survived the write: %s\n"+
				"  consequence: `agent list` walks this directory, so the leftover "+
				"is either an agent nobody created or a parse error nobody caused.", e.Name())
		}
	}

	if strings.HasSuffix("backend"+ext+".tmp-123", ext) {
		t.Error("the temp file suffix ends in the extension Names() globs for\n" +
			"  consequence: a reader can see a partially written agent, and reports " +
			"it as a broken blueprint.\n" +
			"  fix: keep the random suffix LAST in the CreateTemp pattern.")
	}
}

// TestAMissingAgentSaysWhereItLookedFor.
//
// Absence is an error here where a missing policy file is not one in
// internal/toolstore, and the difference is what absence means: no policy file
// means "no override", a default the resolver can act on, while no agent file
// means the name does not exist and there is nothing to show or run.
//
// The directory is in the message because the store is relative to the working
// directory. "no such agent" alone is unanswerable from the wrong directory --
// which is the most likely reason to see it at all.
func TestAMissingAgentSaysWhereItLookedFor(t *testing.T) {
	s := open(t)

	_, err := s.Load("ghost")
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("Load of a missing agent returned %v, want ErrNotExist\n"+
			"  consequence: the CLI answers a missing agent with exit 1 (nothing "+
			"to show) and an I/O failure with something else. Without the sentinel "+
			"a permission error reads as \"no such agent\".", err)
	}
	if !strings.Contains(err.Error(), s.Dir()) {
		t.Errorf("the error does not say where it looked: %v\n"+
			"  consequence: agents/ is relative to the working directory, so the "+
			"usual cause of this error is being in the wrong one -- and the message "+
			"gives the user nothing to check.", err)
	}
}

// TestOpenRefusesAnEmptyDirectory.
//
// An empty dir string joins to a relative "" and MkdirAll("") fails with a
// message about no such file, naming nothing the caller can act on. Refusing
// with DefaultDir quoted in the text is the difference between "the directory
// you forgot to pass" and an unexplained syscall error.
func TestOpenRefusesAnEmptyDirectory(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Error("Open accepted a blank directory\n" +
			"  consequence: the store silently resolves to the working directory " +
			"itself, so every *.yaml in the repository becomes an agent.")
	}
}

// TestAHandEditedAgentIsAuthoritativeOverTheRecordThatMadeIt.
//
// Load returns a *blueprint.Blueprint and not a Record, and this is the reason.
// The file is the definition: somebody may have added a second member, a stage
// or a watcher, and reconstructing a five-field Record would have to throw all of
// that away. `agent show` then describes what is actually there, and `run start`
// executes the same bytes.
func TestAHandEditedAgentIsAuthoritativeOverTheRecordThatMadeIt(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "backend", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	grown := "name: backend\n\nmembers:\n  - name: backend\n    tools: [read, write]\n    stages: [build]\n  - name: reviewer\n    advisory: true\n\nstages:\n  - name: build\n    advance_when: all\n"
	if err := os.WriteFile(s.Path("backend"), []byte(grown), 0o644); err != nil {
		t.Fatal(err)
	}

	bp, err := s.Load("backend")
	if err != nil {
		t.Fatalf("Load of a grown agent: %v\n"+
			"  consequence: the package doc promises hand editing is expected and "+
			"that adding members or stages grows an agent into a team without "+
			"moving it. A reader that refuses the result breaks that promise.", err)
	}
	if len(bp.Config.Members) != 2 || len(bp.Config.Stages) != 1 {
		t.Errorf("the grown agent loaded as %d member(s) and %d stage(s), want 2 and 1\n"+
			"  consequence: `agent show` would describe a file other than the one "+
			"on disk, and `run start` would execute something the user cannot see.",
			len(bp.Config.Members), len(bp.Config.Stages))
	}
}

// TestAToolAddedByHandLoadsEvenThoughCreateWouldRefuseIt pins a deliberate
// asymmetry, which is the kind of thing a later reader deletes as a bug.
//
// Create validates the grant list; Load does not. The blueprint schema accepts
// any string list for `tools`, so a stored agent with a hand-added tool loads for
// `run start` -- and a reader stricter than the thing that runs the file is the
// wrong way round for the two to disagree. The refusal belongs where the name is
// typed, not where it is read back.
func TestAToolAddedByHandLoadsEvenThoughCreateWouldRefuseIt(t *testing.T) {
	s := open(t)

	if err := (Record{Name: "backend", Tools: []string{"telepathy"}}).Validate(); err == nil {
		t.Fatal("the premise is wrong: Validate accepts an unknown tool, so this " +
			"test proves nothing about the asymmetry it is here to pin")
	}

	raw := "name: backend\n\nmembers:\n  - name: backend\n    tools: [read, telepathy]\n"
	if err := os.WriteFile(s.Path("backend"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	bp, err := s.Load("backend")
	if err != nil {
		t.Fatalf("Load of an agent with a hand-added tool: %v\n"+
			"  consequence: this store would refuse a file that `run start "+
			"./backend.yaml` accepts, so the same bytes would be valid by path and "+
			"invalid by name -- and the name is the shorter route the user reaches "+
			"for first.", err)
	}
	if got := strings.Join(bp.Config.Members[0].Tools, ","); got != "read,telepathy" {
		t.Errorf("tools loaded as %q, want %q", got, "read,telepathy")
	}
}

// TestOneGarbledAgentDoesNotHideTheOthers.
//
// Entry.Err travels with the entry instead of aborting the walk. A `agent list`
// that gave up on the first unparseable file would answer a question about the
// whole directory with a complaint about one member of it, and the operator would
// never learn that their other agents are fine -- while the one thing they need,
// the name of the broken file, is the thing they would have to guess.
func TestOneGarbledAgentDoesNotHideTheOthers(t *testing.T) {
	s := open(t)
	for _, name := range []string{"alpha", "gamma"} {
		if _, err := s.Create(Record{Name: name, Tools: []string{"read"}}); err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
	}
	// Sorts between the two, so a walk that stops on the first error stops with
	// one good entry already collected -- the case a "did it return anything"
	// assertion would pass.
	if err := os.WriteFile(s.Path("beta"), []byte("members: [oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := s.List()
	if err != nil {
		t.Fatalf("List returned a directory-level error for one bad file: %v\n"+
			"  consequence: one file nobody can parse makes `agent list` refuse to "+
			"report the agents that are fine.", err)
	}
	if len(entries) != 3 {
		t.Fatalf("List returned %d entries, want 3 (two good, one broken)", len(entries))
	}

	for _, e := range entries {
		switch e.Name {
		case "beta":
			if e.Err == nil || e.Blueprint != nil {
				t.Errorf("the broken agent reported Err=%v Blueprint=%v\n"+
					"  consequence: a nil blueprint with no error is a nil "+
					"dereference in every caller that trusts the pair.", e.Err, e.Blueprint)
			}
		default:
			if e.Err != nil {
				t.Errorf("agent %q reported an error next to a broken sibling: %v\n"+
					"  consequence: the operator is told their working agents are "+
					"broken because a different file is.", e.Name, e.Err)
			}
			if e.Blueprint == nil {
				t.Errorf("agent %q loaded as nil with no error", e.Name)
			}
		}
	}
}

// TestTheStoredFileIsReadableCommittableAndSaysWhatItIs.
//
// CreateTemp makes a file 0600, and an agent definition is not a secret: it is a
// file the whole team is expected to read, diff and commit. Leaving it
// unreadable would make agents/reviewer.yaml behave differently from every other
// blueprint in the repository, for no reason the user could see.
//
// The header is asserted too, because it is the only place the file explains
// itself. Somebody who finds agents/reviewer.yaml in a diff has to be able to
// tell what wrote it and what to do with it without leaving the file.
func TestTheStoredFileIsReadableCommittableAndSaysWhatItIs(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "reviewer", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if runtime.GOOS != "windows" {
		// Skipped on Windows, where Chmod only moves the read-only bit and the
		// mode read back is not the mode requested. The invariant is a unix one.
		info, err := os.Stat(s.Path("reviewer"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("the stored agent is mode %04o, want 0644\n"+
				"  consequence: CreateTemp's 0600 would leave a committed, "+
				"reviewed blueprint unreadable to everyone but its author.", perm)
		}
	}

	raw, err := os.ReadFile(s.Path("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"# reviewer", "arxi run start", "blueprint validate"} {
		if !strings.Contains(text, want) {
			t.Errorf("the stored file does not mention %q:\n%s\n"+
				"  consequence: the header is the only thing that tells a reader "+
				"who finds this file in a diff what wrote it, how to run it and "+
				"what checks it.", want, text)
		}
	}
}

// TestRenderRefusesWhatTheBlueprintValidatorWouldRefuse.
//
// The load inside Render is the reason rendering was chosen over templating, and
// this test is the only thing that proves it is not decorative. A model with two
// slashes passes Record.Validate -- which knows nothing about model shape -- and
// is refused by the blueprint validator, so it can only be caught by actually
// loading the bytes.
//
// This store must be incapable of producing a file that `arxi blueprint validate`
// rejects, because a refusal is visible now while a broken file is discovered
// later, by `run start`, as a complaint about a file the user never wrote.
func TestRenderRefusesWhatTheBlueprintValidatorWouldRefuse(t *testing.T) {
	s := open(t)

	_, err := s.Create(Record{Name: "backend", Model: "a/b/c"})
	if err == nil {
		t.Fatal("Create accepted the model \"a/b/c\"\n" +
			"  consequence: agents/backend.yaml would exist and fail its own " +
			"validator, so `agent show` and `run start` report a problem in a file " +
			"the store wrote and the user cannot correct.")
	}
	if !strings.Contains(err.Error(), "a/b/c") {
		t.Errorf("the refusal does not name the offending value: %v", err)
	}
	if _, statErr := os.Stat(s.Path("backend")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file exists after Render refused (%v)\n"+
			"  consequence: the name is taken by an agent that was never created.", statErr)
	}
}

// TestPlainSafeIsAnAllowlistAndKeepsTheSlash.
//
// Two directions in one test because they trade off against each other. Model
// ids are spelled `provider/id` and versions carry dots and dashes, so those
// characters must stay plain or every rendered file would be a wall of quotes;
// everything else must be quoted, because a denylist of the subset's specials is
// one omission away from writing a file that misparses into something plausible.
func TestPlainSafeIsAnAllowlistAndKeepsTheSlash(t *testing.T) {
	for _, s := range []string{"reviewer", "anthropic/claude-sonnet-4-6", "gpt-4.1", "a_b", "x2"} {
		if !plainSafe(s) {
			t.Errorf("plainSafe(%q) is false\n"+
				"  consequence: every model id and hyphenated name renders quoted. "+
				"Correct, but the file is meant to be read and edited by hand.", s)
		}
	}
	for _, s := range []string{"", "-x", ".x", "a b", "a:b", "#x", "1", "1.5", "yes", "NO", "true", "null"} {
		if plainSafe(s) {
			t.Errorf("plainSafe(%q) is true\n"+
				"  consequence: rendered unquoted, this parses as a comment, a "+
				"sequence item, a number, a bool -- or is a hard error in the "+
				"subset. Any of the four writes a file the next command misreads.", s)
		}
	}
}

// TestAtDoesNotCreateTheDirectoryAndStillAnswers.
//
// `agent list` and `agent show` read through At for this: a command that reports
// on agents/ must not create it. The absence is asserted together with the two
// answers, because a constructor that skipped the MkdirAll and then failed every
// read would be worse than the side effect it avoids -- and in a checkout the
// user cannot write to, At is the difference between "no agents yet" and
// "permission denied" about a directory nobody asked to make.
func TestAtDoesNotCreateTheDirectoryAndStillAnswers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDir)
	s, err := At(dir)
	if err != nil {
		t.Fatalf("At: %v", err)
	}

	names, err := s.Names()
	if err != nil || len(names) != 0 {
		t.Errorf("Names on a store whose directory does not exist = %v, %v; want none and no error\n"+
			"  consequence: `agent list` in a fresh repository complains instead of "+
			"answering \"none\", which is what model list, trigger list and inbox do.", names, err)
	}
	if _, err := s.Load("ghost"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Load through At = %v; want ErrNotExist\n"+
			"  consequence: `agent show ghost` exits on an I/O error instead of "+
			"exit 1 with the name it looked for.", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists after two reads through At (%v)\n"+
			"  consequence: reporting on the store creates it. Open is for writers "+
			"and does that on purpose; a reader that does it too leaves an empty "+
			"directory behind in every repository somebody runs `agent list` in.", dir, err)
	}
}
