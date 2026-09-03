package agentstore

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
)

// TestAComposedTeamRunsAndActivatesEveryMember is the point of Team, and it is
// the same assertion TestTheRenderedAgentEntersAStageAndActivatesItsMember makes
// about one member, for the same reason: the file that cannot run is the failure
// this store exists to prevent, and every other check passes it.
//
// Through Decide rather than by reading the rendered text, because "declares a
// stage" is the mechanism and "everybody gets a turn" is the property. A team is
// where getting it wrong is most expensive: a stage that activates two of three
// members starts, spends money on two turns, and then waits for a third that
// `advance_when: all` will never see submit -- a run that is neither finished nor
// progressing, and whose log shows nothing wrong.
func TestAComposedTeamRunsAndActivatesEveryMember(t *testing.T) {
	raw, err := Team{Name: "feature-team", Members: []kernel.MemberConfig{
		{Name: "backend", Role: "implementer", Tools: []string{"read", "write", "bash"}},
		{Name: "frontend", Role: "implementer", Tools: []string{"read", "write"}},
		{Name: "security", Role: "reviewer", Tools: []string{"read"}, Advisory: true},
	}}.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	bp, err := blueprint.Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	st, fx := kernel.Decide(kernel.State{}, kernel.Event{
		Seq: 1, ID: "e1", Type: kernel.RunStarted, Scope: "run:r1",
		Source:  kernel.SourceRuntime,
		Payload: map[string]any{"run_id": "r1", "actor": "backend", "budget_usd": 5.0},
	}, bp.Config)

	entered := firstEmit(fx, kernel.StageEntered)
	if entered == nil {
		t.Fatalf("run.started on a composed team enters no stage; effects were %s\n"+
			"  consequence: applyRunStarted returns nil for a blueprint with no "+
			"stages, so a team of three activates nobody and records run.quiescent "+
			"after zero turns.\n"+
			"  fix: Team.Render writes a stages: block.", effectTypes(fx))
	}

	next := *entered
	next.Seq, next.ID = 2, "e2"
	_, fx = kernel.Decide(st, next, bp.Config)

	spawned := map[string]bool{}
	for _, f := range fx {
		if sp, ok := f.(kernel.SpawnTurn); ok {
			spawned[sp.Agent] = true
		}
	}
	for _, want := range []string{"backend", "frontend"} {
		if !spawned[want] {
			t.Errorf("entering stage %q spawns no turn for %q; effects were %s\n"+
				"  consequence: `advance_when: all` waits for a member that was "+
				"never activated, so the run pays for the others and then hangs "+
				"with nothing in the log to explain it.\n"+
				"  fix: a member with no stages list must take part in every stage.",
				entered.Str("stage"), want, effectTypes(fx))
		}
	}

	// The advisory member is expected NOT to get a turn, and this assertion is
	// here rather than in internal/kernel because it is what `--members
	// backend,frontend,security` composes: applyStageEntered skips advisory
	// members, and quorumMet does not count them, so `advance_when: all` means
	// "both implementers" and the run does not hang waiting for an observer.
	//
	// It also fixes the shape of this test, which asserted three turns first.
	// Three would mean a paid turn for a member that cannot advance the stage --
	// TestStageEnteredActivatesOnlyNonAdvisory in internal/kernel pins the
	// opposite deliberately, and a store test contradicting it would have been an
	// argument for changing the reducer to bill for nothing.
	if spawned["security"] {
		t.Errorf("entering stage %q spawns a turn for the advisory member; effects were %s\n"+
			"  consequence: an advisory member does not count toward any advance "+
			"rule, so a turn opened for it is money spent on an opinion the stage "+
			"cannot wait for.", entered.Str("stage"), effectTypes(fx))
	}
}

// TestNamedStagesAreDeclaredInOrderAndWaitForEverybody.
//
// Order is the assertion that matters. `--stages build,review` is a sequence, and
// a store that rendered it as a set would produce a team that reviews before it
// builds -- a file that validates, runs, and does the work backwards. Nothing
// downstream could detect it, because both stage names are legitimate.
//
// `advance_when: all` on each, read off the loaded config rather than the text.
// `any` would advance on whichever member finished first and ship a stage's worth
// of half-done work; `quorum:N` would be a number this command was never given.
// §20.4's review stage does use quorum:2, and that is a hand edit in a file
// written to be edited.
func TestNamedStagesAreDeclaredInOrderAndWaitForEverybody(t *testing.T) {
	raw, err := Team{Name: "shipping", Stages: []string{"build", "review", "ship"},
		Members: []kernel.MemberConfig{{Name: "dev", Tools: []string{"write"}}}}.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	bp, err := blueprint.Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got []string
	for _, s := range bp.Config.Stages {
		got = append(got, s.Name)
		if s.AdvanceWhen != "all" {
			t.Errorf("stage %q advances on %q, want all\n"+
				"  consequence: `any` advances as soon as one member submits, so a "+
				"stage ships whatever the others had not finished; `quorum:N` would "+
				"be a threshold this command was never told.", s.Name, s.AdvanceWhen)
		}
	}
	if strings.Join(got, ",") != "build,review,ship" {
		t.Errorf("stages are %v, want [build review ship]\n"+
			"  consequence: stages are a sequence, and a run enters the first one. "+
			"Reordered, this team reviews before it builds -- and every validator "+
			"passes it, because both names are legitimate.", got)
	}
}

// TestACopiedMemberKeepsWhatTheAgentDeclared is why Members is
// []kernel.MemberConfig and not []Record.
//
// A stored agent is a file designed to be hand-edited, so it may carry
// `activation: queue` or `stages: [build]` -- neither reachable from any `agent
// create` flag, both meaningful to the reducer. Composing through Record's five
// fields would drop them silently, and the member in the team would then behave
// differently from the agent whose name it carries: `queue` opens one turn per
// reason where `coalesce` opens one carrying all of them, which is a difference
// measured in dollars.
//
// activation is checked against "queue" and not against "" for a reason: kernel's
// config defaulting writes "coalesce" into an empty Activation, so a dropped field
// comes back looking like a deliberate choice.
func TestACopiedMemberKeepsWhatTheAgentDeclared(t *testing.T) {
	raw, err := Team{Name: "pair", Stages: []string{"build", "review"},
		Members: []kernel.MemberConfig{
			{Name: "backend", Role: "implementer", Model: "claude-sonnet-4-6",
				Tools: []string{"read", "write"}, Activation: "queue", Stages: []string{"build"}},
			{Name: "reviewer", Tools: []string{"read"}, Advisory: true},
		}}.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	bp, err := blueprint.Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	m := bp.Config.Members[0]
	if m.Activation != "queue" {
		t.Errorf("the copied member's activation is %q, want queue\n"+
			"  consequence: kernel defaults an empty Activation to coalesce, so a "+
			"dropped field is indistinguishable from a chosen one -- and the member "+
			"bills one turn per reason instead of one carrying all of them.", m.Activation)
	}
	if strings.Join(m.Stages, ",") != "build" {
		t.Errorf("the copied member takes part in %v, want [build]\n"+
			"  consequence: dropping the list makes this member a member of every "+
			"stage, so a build-only agent is also activated for review -- and paid "+
			"for there.", m.Stages)
	}
	if m.Model != "claude-sonnet-4-6" || m.Role != "implementer" {
		t.Errorf("the copied member is role=%q model=%q, want implementer/claude-sonnet-4-6\n"+
			"  consequence: the team runs the same names against a different model "+
			"than the agents it was built from.", m.Role, m.Model)
	}
	if !bp.Config.Members[1].Advisory {
		t.Errorf("the advisory member came back non-advisory\n" +
			"  consequence: advisory is the one member field that changes whether a " +
			"stage can advance, so a reviewer that was meant to have no vote now " +
			"blocks the advance it was added to observe.")
	}
}

// TestAMemberThatCouldNeverTakeATurnIsRefused is the check internal/blueprint
// deliberately does not make, and the one composing members out of other files
// creates the need for.
//
// A stored agent carrying `stages: [build]` composed into a team of `review` and
// `ship` is a member kernel.participates answers false for in every stage. It is
// activated never, and every signal says the file is fine: blueprint.Load accepts
// it (a member's stage list is not cross-checked against the declared stages),
// `blueprint validate` accepts it, and `agent show` lists it among the members.
//
// blueprint.Load is right not to check it -- there, the author wrote both lists
// and can see them. Here the author sees neither, because the file does not exist
// until the command succeeds. Which is also why the refusal has to print both
// lists: the user typed two agent names and a stage list, and the offending stage
// name is in neither.
func TestAMemberThatCouldNeverTakeATurnIsRefused(t *testing.T) {
	s := open(t)
	_, err := s.CreateTeam(Team{Name: "late", Stages: []string{"review", "ship"},
		Members: []kernel.MemberConfig{
			{Name: "reviewer", Tools: []string{"read"}},
			{Name: "builder", Tools: []string{"write"}, Stages: []string{"build"}},
		}})
	if err == nil {
		t.Fatalf("CreateTeam accepted a member whose stages name none of the team's\n" +
			"  consequence: participates returns false for it in every stage, so it " +
			"is never activated and never billed -- and `agent show` still lists it, " +
			"so the run looks like a team of two doing the work of one.")
	}
	for _, want := range []string{"builder", "build", "review", "ship"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v\n"+
				"  consequence: the stage that does not match is in neither the "+
				"--members list nor the --stages list the user typed, so without both "+
				"lists printed there is nothing on screen to compare.", want, err)
		}
	}
	if _, statErr := os.Stat(s.Path("late")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file exists at %s after a refused CreateTeam (%v)\n"+
			"  consequence: the name is taken by a blueprint the user was told was "+
			"not created, so the retry fails with `already exists`.", s.Path("late"), statErr)
	}
}

// TestAMemberWhoseStagesOverlapPartlyIsAccepted is the other side of that check,
// and it is the reason it tests for intersection rather than containment.
//
// A member declaring `[build, review]` in a team that runs only `build` takes part
// in build. It cannot take part in a stage that does not exist, and refusing the
// whole team over the surplus name would block a legitimate composition: an agent
// written for a longer process being reused in a shorter one is exactly what
// copying members is for.
func TestAMemberWhoseStagesOverlapPartlyIsAccepted(t *testing.T) {
	if _, err := (Team{Name: "short", Stages: []string{"build"},
		Members: []kernel.MemberConfig{
			{Name: "dev", Tools: []string{"write"}, Stages: []string{"build", "review"}},
		}}).Render(); err != nil {
		t.Errorf("Render refused a member that takes part in the only stage there is: %v\n"+
			"  consequence: reusing an agent written for a longer process in a "+
			"shorter one is what copying members is for, and this refuses it.", err)
	}
}

// TestATeamDoesNotOverwriteAnAgentOfTheSameName.
//
// The hazard of one directory for both. `arxi agent create reviewer` and `arxi
// blueprint create reviewer --members a,b` compete for agents/reviewer.yaml, and
// the loser is a file somebody grew by hand. Create and CreateTeam share
// createRaw so the guard cannot be present on one path and absent on the other --
// which is the plausible bug, since only one of the two was written first.
//
// Both directions, because they fail differently in the same way: whichever verb
// is second is the one that would destroy work, and neither is more entitled to
// the name.
func TestATeamDoesNotOverwriteAnAgentOfTheSameName(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "reviewer", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := os.ReadFile(s.Path("reviewer"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	_, err = s.CreateTeam(Team{Name: "reviewer", Members: []kernel.MemberConfig{
		{Name: "a", Tools: []string{"read"}}, {Name: "b", Tools: []string{"read"}},
	}})
	if !errors.Is(err, ErrExists) {
		t.Errorf("CreateTeam over an existing agent returned %v, want ErrExists\n"+
			"  consequence: the agent's file is replaced by a team, so the tool grant "+
			"and any hand edits are gone and the command that did it printed success.", err)
	}
	after, err := os.ReadFile(s.Path("reviewer"))
	if err != nil {
		t.Fatalf("read back after the refusal: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the file changed under a refused CreateTeam\n  before:\n%s\n  after:\n%s\n"+
			"  consequence: a refusal that still writes is worse than an overwrite -- "+
			"the user was told nothing happened.", before, after)
	}

	// The other order: a team first, then an agent of the same name.
	if _, err := s.CreateTeam(Team{Name: "team", Members: []kernel.MemberConfig{
		{Name: "a", Tools: []string{"read"}},
	}}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := s.Create(Record{Name: "team", Tools: []string{"read"}}); !errors.Is(err, ErrExists) {
		t.Errorf("Create over an existing team returned %v, want ErrExists\n"+
			"  consequence: a composed team of several members is replaced by a "+
			"one-member agent, and `run start team` then runs one agent where the "+
			"user expects the team they built.", err)
	}
}

// TestATeamThatCouldNotRunIsRefusedBeforeItIsAFile collects the refusals whose
// shared property is that the file would have loaded.
//
// Every case here passes blueprint.Load. That is what makes them Team's business:
// a refusal from the schema needs no help, and a file that validates and cannot
// work needs all of it. The empty member list is the clearest -- internal/blueprint
// accepts it on purpose, documented there as "a run with no members is caught when
// the run starts", which is true and arrives after `blueprint create` has reported
// success.
func TestATeamThatCouldNotRunIsRefusedBeforeItIsAFile(t *testing.T) {
	cases := []struct {
		why  string
		team Team
	}{
		{"no members at all", Team{Name: "empty"}},
		{"two members with one name", Team{Name: "dup", Members: []kernel.MemberConfig{
			{Name: "a", Tools: []string{"read"}}, {Name: "a", Tools: []string{"write"}}}}},
		{"two stages with one name", Team{Name: "dupstage", Stages: []string{"x", "x"},
			Members: []kernel.MemberConfig{{Name: "a"}}}},
		{"a stage with no name", Team{Name: "blankstage", Stages: []string{"build", "  "},
			Members: []kernel.MemberConfig{{Name: "a"}}}},
		{"a member with no name", Team{Name: "blankmember",
			Members: []kernel.MemberConfig{{Name: ""}}}},
		{"a member whose name cannot be typed back", Team{Name: "invisible",
			Members: []kernel.MemberConfig{{Name: "backend "}}}},
		{"a tool that does not exist", Team{Name: "typo",
			Members: []kernel.MemberConfig{{Name: "a", Tools: []string{"reed"}}}}},
		{"a name that would escape the store", Team{Name: "../escape",
			Members: []kernel.MemberConfig{{Name: "a"}}}},
	}
	s := open(t)
	for _, c := range cases {
		if _, err := s.CreateTeam(c.team); err == nil {
			t.Errorf("CreateTeam accepted a team with %s\n"+
				"  consequence: every case here renders a file blueprint.Load "+
				"ACCEPTS, so nothing downstream refuses it -- the failure arrives as "+
				"a run that does nothing, or as an error naming a line the user did "+
				"not write.", c.why)
		}
	}
}

// TestStageNamesCanBeCheckedBeforeThereIsATeamToCheckThem is the claim
// ValidateStages exists for: a caller that has typed some stages and nothing
// else can find out whether they are legal, and cannot be told no for a reason
// about somebody else.
func TestStageNamesCanBeCheckedBeforeThereIsATeamToCheckThem(t *testing.T) {
	if err := ValidateStages([]string{"draft", "review"}); err != nil {
		t.Fatalf("a legal stage list was refused: %v", err)
	}
	if err := ValidateStages(nil); err != nil {
		t.Errorf("an empty list was refused: %v\n"+
			"  no stages is the normal case: stageNames defaults it to [work].", err)
	}
	for _, bad := range [][]string{{"x", "x"}, {"  "}, {"build "}, {"a\nb"}} {
		if ValidateStages(bad) == nil {
			t.Errorf("ValidateStages accepted %q, and CreateTeam would refuse it", bad)
		}
	}

	// The reason the wrapper is not just Team.Validate. This member takes part
	// only in `build`, which the team does not declare, so Validate refuses --
	// correctly, at review. But an interactive caller types `draft` first on the
	// way to [draft, build], and refusing THAT keystroke would make a legal team
	// unreachable.
	partial := Team{Name: "release", Stages: []string{"draft"},
		Members: []kernel.MemberConfig{{Name: "a", Stages: []string{"build"}}}}
	if partial.Validate() == nil {
		t.Error("Validate accepted a member that takes part in no declared stage; " +
			"the cross-check at review is what ValidateStages deliberately leaves to it")
	}
	if err := ValidateStages(partial.Stages); err != nil {
		t.Errorf("ValidateStages refused %q: %v\n"+
			"  the stage name is legal. This wrapper must answer about the names it "+
			"was given and nothing else, or the first stage typed toward a legal "+
			"team is refused as though it were illegal.", partial.Stages, err)
	}
}
