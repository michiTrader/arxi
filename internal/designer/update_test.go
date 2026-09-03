package designer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/kernel"
)

// picker is the listing these tests fold keys against: two agents that can be
// members, a third whose member name collides with the first, a team, and a file
// that did not load. Five rows because every refusal this screen makes needs a
// row to make it about.
func picker() Model {
	return New(Candidates([]agentstore.Entry{
		stored("api", member("api")),
		stored("reviewer", member("reviewer")),
		stored("legacy-api", member("api")),
		stored("platform", member("api"), member("web")),
		broken("half-written", errors.New("members[0]: model is required")),
	}))
}

// fold plays keys into the model the way the loop does, collecting every command
// they produced along the way.
//
// The commands are the assertion in most of these tests. A Write is a file, so
// WHERE one appears matters as much as what is inside it: a Write emitted on the
// way to the review screen is a blueprint nobody looked at.
func fold(m Model, ks ...Key) (Model, []Command) {
	var out []Command
	for _, k := range ks {
		var cs []Command
		m, cs = Update(m, KeyPress{k})
		out = append(out, cs...)
	}
	return m, out
}

// typeIn is the keys a phrase arrives as, one per rune.
func typeIn(s string) []Key {
	out := make([]Key, 0, len(s))
	for _, r := range s {
		out = append(out, Typed(r))
	}
	return out
}

// seq joins groups of keys so that a test reads like the session it describes.
func seq(groups ...[]Key) []Key {
	var out []Key
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// writes is the teams the commands would write. Every command is a Write -- the
// package has no others -- and a test that reached for a type switch would be
// asserting that, so it does it here once.
func writes(t *testing.T, cmds []Command) []agentstore.Team {
	t.Helper()
	out := make([]agentstore.Team, 0, len(cmds))
	for _, c := range cmds {
		w, ok := c.(Write)
		if !ok {
			t.Fatalf("a command of type %T came out of the designer; the only one is Write", c)
		}
		out = append(out, w.Team)
	}
	return out
}

// TestATypedSessionComposesTheTeamAndWritesNothingBeforeReview is the designer
// working, and it is one test rather than four because the walk is the claim: a
// name, some members, some stages, and a file at the end that contains what was
// typed on the way.
//
// The two halves are separated for the reason the second half exists. Everything
// up to the review screen must produce NO command at all -- a Write is a file,
// and a file written by any keystroke before the last one is a blueprint nobody
// saw. Then one enter, one Write, and the team inside it is the team that was on
// screen.
func TestATypedSessionComposesTheTeamAndWritesNothingBeforeReview(t *testing.T) {
	m, cmds := fold(picker(), seq(
		typeIn("release"),
		[]Key{Press(KeyEnter)},
		// reviewer first and api second, which is neither the directory order
		// nor alphabetical, so the composed order can only come from the picks.
		[]Key{Press(KeyDown), Typed(' '), Press(KeyUp), Typed(' '), Press(KeyEnter)},
		typeIn("draft"), []Key{Press(KeyEnter)},
		typeIn("review"), []Key{Press(KeyEnter)},
		[]Key{Press(KeyEnter)}, // nothing typed: that is all the stages there are
	)...)

	if m.Screen != ScreenReview {
		t.Fatalf("four questions answered and the designer is on the %s screen, want review\n"+
			"  the walk is name -> members -> stages -> review; landing anywhere else\n"+
			"  means a key that should have advanced did not, and the operator is stuck\n"+
			"  on a screen they have already answered.\n  err on screen: %q", m.Screen, m.Err)
	}
	if len(cmds) != 0 {
		t.Fatalf("%d commands were produced before anything was reviewed: %+v\n"+
			"  a Write is a file. Emitted on the way here, it is a blueprint composed\n"+
			"  from a half-answered model, and the review screen it skipped is the only\n"+
			"  place the operator could have said no.", len(cmds), cmds)
	}

	m, cmds = fold(m, Press(KeyEnter))
	got := writes(t, cmds)
	if len(got) != 1 {
		t.Fatalf("enter on the review screen produced %d writes, want 1 (err on screen: %q)\n"+
			"  none means the reviewed team cannot be written and the designer is a\n"+
			"  read-only tour; two means two files, and the second fails as existing.",
			len(got), m.Err)
	}
	team := got[0]
	if team.Name != "release" {
		t.Errorf("the team being written is called %q, want release\n"+
			"  the name is the filename: this writes agents/%s.yaml, which is not the\n"+
			"  one the operator typed and not the one they will run.", team.Name, team.Name)
	}
	if names := strings.Join(memberNames(team.Members), ","); names != "reviewer,api" {
		t.Errorf("the team is composed of %v, want [reviewer api]\n"+
			"  picked reviewer then api. The order in the file is the order `agent show`\n"+
			"  prints and the order a reader takes to be meaningful, and sorting it\n"+
			"  discards a choice made with two keystrokes.", names)
	}
	if stages := strings.Join(team.Stages, ","); stages != "draft,review" {
		t.Errorf("the team's stages are %v, want [draft review]\n"+
			"  stages are a sequence and a run enters the first one, so a team that\n"+
			"  lost or reordered them reviews before it drafts -- and every validator\n"+
			"  downstream passes it, because both names are legitimate.", stages)
	}
}

// TestANameTheStoreAlreadyHoldsIsRefusedWhileItCanStillBeChanged is most of the
// argument for having a name screen at all.
//
// `blueprint create platform --members api,reviewer` types the whole command,
// then learns the name is taken. Here the refusal lands on the keystroke that
// tried to leave the screen, with the name still under the cursor -- and it names
// the file, because an operator told "platform already exists" goes looking for
// it and one told "agents/platform.yaml already exists" has already found it.
func TestANameTheStoreAlreadyHoldsIsRefusedWhileItCanStillBeChanged(t *testing.T) {
	m, cmds := fold(picker(), seq(typeIn("platform"), []Key{Press(KeyEnter)})...)
	if m.Screen != ScreenName {
		t.Fatalf("a taken name advanced to the %s screen\n"+
			"  the whole team would then be composed and reviewed against a name\n"+
			"  CreateTeam refuses, so the refusal arrives after the last keystroke\n"+
			"  instead of the first -- which is the failure this screen exists to fix.",
			m.Screen)
	}
	if len(cmds) != 0 {
		t.Fatalf("a refused name produced commands: %+v", cmds)
	}
	if !strings.Contains(m.Err, "agents/platform.yaml") {
		t.Errorf("the refusal is %q; it has to name the file that holds the name\n"+
			"  otherwise the operator's next move is to go and look for it, and the\n"+
			"  designer knew where it was.", m.Err)
	}

	// The fix, and the message going away when the reason does. A refusal still
	// on screen after the problem is gone teaches the operator to stop reading
	// the line, which costs the next refusal too.
	m, _ = fold(m, Typed('2'))
	if m.Err != "" {
		t.Errorf("the refusal survived the keystroke that fixed it: %q\n"+
			"  it is now a false statement under a name that is free, and every\n"+
			"  message on that row is worth less for it.", m.Err)
	}
	m, cmds = fold(m, Press(KeyEnter))
	if m.Screen != ScreenMembers || len(cmds) != 0 {
		t.Errorf("`platform2` did not advance: screen %s, err %q\n"+
			"  no file holds that name, so nothing about it is refusable.", m.Screen, m.Err)
	}
}

// TestANameThatCouldNotBeAFilenameIsRefusedByTheStoresOwnRule.
//
// Not the designer's rule. ValidateName is what CreateTeam calls, so a name the
// designer accepts is a name the writer accepts; a second copy of the rule here
// would drift, and the direction it drifts is the expensive one -- a designer
// that accepts `../escape` reaches the store's refusal after the operator has
// composed a whole team.
func TestANameThatCouldNotBeAFilenameIsRefusedByTheStoresOwnRule(t *testing.T) {
	// "" is the state the designer starts in, so enter as the very first key is
	// the likeliest of these by a wide margin.
	for _, bad := range []string{"", "../escape", "a/b", "..", " leading", "trailing "} {
		m, cmds := fold(picker(), seq(typeIn(bad), []Key{Press(KeyEnter)})...)
		if m.Screen != ScreenName || len(cmds) != 0 {
			t.Errorf("the name %q advanced to %s\n"+
				"  agentstore.ValidateName refuses it, so the file cannot be written:\n"+
				"  the refusal arrives after the members and the stages have been\n"+
				"  typed, about a screen three steps back.", bad, m.Screen)
		}
		if m.Err == "" {
			t.Errorf("the name %q was refused with nothing on screen to say why\n"+
				"  the operator presses enter again, gets the same nothing, and the\n"+
				"  designer looks broken rather than particular.", bad)
		}
	}
}

// onMembers is the model after a legal name has been accepted.
func onMembers(t *testing.T) Model {
	t.Helper()
	m, _ := fold(picker(), seq(typeIn("release"), []Key{Press(KeyEnter)})...)
	if m.Screen != ScreenMembers {
		t.Fatalf("the fixture never reached the members screen: %s (%q)", m.Screen, m.Err)
	}
	return m
}

// down is n presses of the down arrow, which is how a row gets reached.
func down(n int) []Key {
	out := make([]Key, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Press(KeyDown))
	}
	return out
}

// TestPickingAFileThatCannotBeAMemberIsRefusedWithThatFilesOwnReason.
//
// The rows are listed on purpose, so the cursor reaches them, so space gets
// pressed on them. What comes back has to be the reason THIS file cannot be a
// member and not a general no: `platform` is a dead end but `api, web` is not,
// and the operator can only learn that from the refusal.
func TestPickingAFileThatCannotBeAMemberIsRefusedWithThatFilesOwnReason(t *testing.T) {
	for _, c := range []struct {
		row  int
		want []string
	}{
		{3, []string{"platform", "is itself a team of 2", "api, web"}},
		{4, []string{"half-written", "did not load", "model is required"}},
	} {
		m, _ := fold(onMembers(t), seq(down(c.row), []Key{Typed(' ')})...)
		if len(m.Picked) != 0 {
			t.Fatalf("row %d was picked anyway; the team now carries a zero member\n"+
				"  a member with no name and no model reaches Validate as members[N],\n"+
				"  which refuses the whole team for a reason the operator cannot act on.",
				c.row)
		}
		for _, want := range c.want {
			if !strings.Contains(m.Err, want) {
				t.Errorf("picking row %d says %q, which is missing %q\n"+
					"  the refusal is the only place the alternative appears: a team's\n"+
					"  members are pickable where the team is not, and a file that did\n"+
					"  not load has a loader complaint that says which line to fix.",
					c.row, m.Err, want)
			}
		}
	}
}

// TestTwoFilesThatWouldCarryOneMemberNameAreRefusedNamingBoth is the refusal
// written out by hand instead of left to Team.Validate, and this test is why.
//
// Validate does catch it, and says "members[0] and members[2]". A composed member
// records nothing about the file it came from, so by then both paths are gone --
// and the operator picked FILES. Told the indices, they have to count rows; told
// the two paths, they know which one to drop.
func TestTwoFilesThatWouldCarryOneMemberNameAreRefusedNamingBoth(t *testing.T) {
	m, _ := fold(onMembers(t), seq(
		[]Key{Typed(' ')},          // api, whose member is called api
		down(2), []Key{Typed(' ')}, // legacy-api, whose member is also api
	)...)
	if len(m.Picked) != 1 {
		t.Fatalf("%d files are picked, want 1: the second carries a member name the\n"+
			"  first already used, so a stage would activate a name that matches two\n"+
			"  members and `run steer <run> api` could not say which it meant.", len(m.Picked))
	}
	for _, want := range []string{"agents/api.yaml", "agents/legacy-api.yaml", "api"} {
		if !strings.Contains(m.Err, want) {
			t.Errorf("the refusal is %q, which does not mention %q\n"+
				"  both files are legitimate agents and the operator picked them by\n"+
				"  file name; a message about members[0] and members[2] describes a\n"+
				"  file that does not exist yet.", m.Err, want)
		}
	}
}

// TestSpaceTakesAPickBackAndTheRestKeepTheirOrder.
//
// Space is a toggle because a mis-pick has to be undoable without leaving the
// screen: the alternative is an operator who restarts the designer, and the
// screen they restart from is the one that lists forty files.
func TestSpaceTakesAPickBackAndTheRestKeepTheirOrder(t *testing.T) {
	m, _ := fold(onMembers(t), seq(
		down(1), []Key{Typed(' ')}, // reviewer
		[]Key{Press(KeyUp), Typed(' ')}, // api
		down(1), []Key{Typed(' ')},      // reviewer again: taken back
	)...)
	if got := strings.Join(memberNames(m.Members()), ","); got != "api" {
		t.Errorf("the team is %v, want [api]\n"+
			"  reviewer was picked, then unpicked. A toggle that adds a second time\n"+
			"  gives the team two members with one name; one that removes the wrong\n"+
			"  entry silently drops a member the operator still wants.", got)
	}
	if len(m.Picked) != 1 {
		t.Errorf("Picked is %v; the indices and the members have to agree, because\n"+
			"  the frame marks the rows from Picked and the file is written from it.", m.Picked)
	}
}

// TestAdvancingWithNobodyPickedIsRefused.
//
// blueprint.Load accepts a memberless blueprint on purpose, documented there as
// "a run with no members is caught when the run starts" -- which is true, and
// arrives as a run that enters a stage, activates nobody and records
// run.quiescent after zero turns. agentstore.Team.Validate refuses it, so this is
// only about WHERE: here, or after the stages screen and the review screen.
func TestAdvancingWithNobodyPickedIsRefused(t *testing.T) {
	for _, k := range []Key{Press(KeyEnter), Press(KeyTab)} {
		m, cmds := fold(onMembers(t), k)
		if m.Screen != ScreenMembers || len(cmds) != 0 {
			t.Fatalf("%v with nobody picked advanced to %s\n"+
				"  the stages screen and the review screen then ask about a team of\n"+
				"  nobody, and the refusal that was available on the first keystroke\n"+
				"  arrives two screens later.", k, m.Screen)
		}
		if m.Err == "" {
			t.Error("nothing on screen says why enter did nothing; on a screen full of\n" +
				"  files the operator's reading is that the designer is stuck.")
		}
	}
}

// onStages is the model with a name and one member accepted.
func onStages(t *testing.T) Model {
	t.Helper()
	m, _ := fold(onMembers(t), Typed(' '), Press(KeyEnter))
	if m.Screen != ScreenStages {
		t.Fatalf("the fixture never reached the stages screen: %s (%q)", m.Screen, m.Err)
	}
	return m
}

// TestAStageNameIsCheckedByTheStoreAtTheKeystrokeThatAddsIt.
//
// Both refusals come from agentstore.ValidateStages, which is checkStageNames --
// the function CreateTeam runs. A duplicate is the case that argues for asking
// about the whole list rather than the one name: `review` is a legal stage name
// and an illegal addition, and only the list can tell those apart.
func TestAStageNameIsCheckedByTheStoreAtTheKeystrokeThatAddsIt(t *testing.T) {
	m, _ := fold(onStages(t), seq(
		typeIn("build"), []Key{Press(KeyEnter)},
		typeIn("build"), []Key{Press(KeyEnter)},
	)...)
	if strings.Join(m.Stages, ",") != "build" {
		t.Fatalf("the stages are %v, want [build]\n"+
			"  two stages called build is a run that advances from one to the next by\n"+
			"  name, so `run advance` and stage.entered both address two of them.", m.Stages)
	}
	if m.Err == "" || m.Screen != ScreenStages {
		t.Errorf("the duplicate was dropped in silence (screen %s, err %q)\n"+
			"  the operator typed five letters and pressed enter; a list that does not\n"+
			"  grow and a screen that says nothing is indistinguishable from a designer\n"+
			"  that stopped reading the keyboard.", m.Screen, m.Err)
	}
	if m.Stage.String() != "build" {
		t.Errorf("the refused text is now %q, want it left as `build`\n"+
			"  a refusal that also clears the field makes the operator retype what they\n"+
			"  just typed, when what they wanted was to edit one letter of it.",
			m.Stage.String())
	}
}

// TestTheStagesScreenNeedsNoKeysBeyondEnterAndBackspace.
//
// The empty field is the "that is all of them" answer, which is how every prompt
// that collects a list behaves, and backspace on an empty field takes the last
// one back. Two keys, both already under the operator's fingers, and no cursor on
// this screen -- which is what lets Model.Cursor mean "the row in Cands" and
// nothing else, on every screen.
func TestTheStagesScreenNeedsNoKeysBeyondEnterAndBackspace(t *testing.T) {
	m, _ := fold(onStages(t), seq(
		typeIn("draft"), []Key{Press(KeyEnter)},
		typeIn("review"), []Key{Press(KeyEnter)},
		[]Key{Press(KeyBackspace)}, // nothing typed: take `review` back
	)...)
	if strings.Join(m.Stages, ",") != "draft" {
		t.Fatalf("the stages are %v, want [draft]\n"+
			"  backspace on an empty field is the only way to undo a stage typed by\n"+
			"  mistake; without it the operator restarts the designer, and starts again\n"+
			"  from the screen that lists every file.", m.Stages)
	}
	if m.Screen != ScreenStages {
		t.Fatalf("taking a stage back left the stages screen for %s", m.Screen)
	}

	// Backspace with nothing typed and nothing to take back must not fall
	// through to the field or to the previous screen.
	m2, cmds := fold(m, Press(KeyBackspace), Press(KeyBackspace))
	if len(m2.Stages) != 0 || m2.Screen != ScreenStages || len(cmds) != 0 {
		t.Errorf("backspace past the start of the list gave stages %v on %s\n"+
			"  the second press has nothing to remove; leaving the screen or removing\n"+
			"  something else is a keystroke that did more than it said.",
			m2.Stages, m2.Screen)
	}

	m3, cmds := fold(m, Press(KeyEnter))
	if m3.Screen != ScreenReview || len(cmds) != 0 {
		t.Errorf("enter on an empty stage field went to %s, want review (err %q)\n"+
			"  an empty field is the answer `no more stages`, and a screen that cannot\n"+
			"  be left without typing one more is a screen that demands a stage the\n"+
			"  team does not have.", m3.Screen, m3.Err)
	}
	if strings.Join(m3.Stages, ",") != "draft" {
		t.Errorf("advancing changed the stages to %v; the empty field was the answer,\n"+
			"  not a stage called \"\"", m3.Stages)
	}
}

// TestNoStagesAtAllIsALegalTeamAndTheStoreFillsInTheDefault.
//
// The straight-through path: a name, a member, enter on an empty stage list. The
// team carries no stages, and Team.stageNames defaults it to [work] at Render.
// Refusing an empty list here would demand a decision the store already has an
// answer for, and it is the answer §20.4's own file uses.
func TestNoStagesAtAllIsALegalTeamAndTheStoreFillsInTheDefault(t *testing.T) {
	m, cmds := fold(onStages(t), Press(KeyEnter), Press(KeyEnter))
	if len(cmds) != 1 {
		t.Fatalf("a team with no stages produced %d writes, want 1 (screen %s, err %q)\n"+
			"  no stages is the normal case for a one-member team, and the store\n"+
			"  declares [work] for it.", len(cmds), m.Screen, m.Err)
	}
	if team := writes(t, cmds)[0]; len(team.Stages) != 0 {
		t.Errorf("the written team carries stages %v; nothing was typed, so inventing\n"+
			"  one here would put a name in the file that nobody chose -- the default\n"+
			"  belongs to stageNames, where Validate can see it too.", team.Stages)
	}
}

// onReview is a whole answered team: the name `release`, the member `api`, the
// stage `draft`.
func onReview(t *testing.T) Model {
	t.Helper()
	m, cmds := fold(onStages(t), seq(typeIn("draft"),
		[]Key{Press(KeyEnter), Press(KeyEnter)})...)
	if m.Screen != ScreenReview || len(cmds) != 0 {
		t.Fatalf("the fixture never reached the review screen: %s, %d commands (%q)",
			m.Screen, len(cmds), m.Err)
	}
	return m
}

// TestOnlyEnterWritesOnTheReviewScreen, and tab in particular does not.
//
// Tab has meant "the next question" on all three screens before this one, which
// makes it the key an operator presses without looking. Here the next thing is a
// file, so the deliberate key is the only one that gets it: everything else on
// this screen -- tab, the arrows, the space that picked members two screens ago,
// an ordinary letter -- has to do nothing at all.
func TestOnlyEnterWritesOnTheReviewScreen(t *testing.T) {
	base := onReview(t)
	for _, k := range []Key{Press(KeyTab), Press(KeyDown), Press(KeyUp),
		Typed(' '), Typed('y'), Press(KeyDelete)} {
		m, cmds := fold(base, k)
		if len(cmds) != 0 {
			t.Errorf("%v on the review screen wrote a file: %+v\n"+
				"  tab advanced the screen three times on the way here, so a tab that\n"+
				"  writes is a blueprint created by muscle memory -- and the name is\n"+
				"  then taken, so the retry fails as existing.", k, cmds)
		}
		if m.Screen != ScreenReview {
			t.Errorf("%v moved off the review screen to %s\n"+
				"  the two ways out are enter, which writes, and esc, which goes back.",
				k, m.Screen)
		}
	}
	if _, cmds := fold(base, Press(KeyEnter)); len(cmds) != 1 {
		t.Errorf("enter produced %d writes, want 1; the deliberate key has to be the\n"+
			"  one that works, or the screen is a dead end", len(cmds))
	}
}

// TestReviewRefusesTheTeamThatValidateRefusesAndWritesNothing.
//
// The cross-check that only exists because members are copied: a stored agent
// carrying `stages: [build]`, composed into a team whose only stage is `review`,
// is a member kernel.participates answers false for everywhere. It is activated
// never, and every other signal says the file is fine.
//
// Run through agentstore.Team.Validate on exactly the value the Write would
// carry, so nothing can be confirmed here and refused by the store afterwards.
func TestReviewRefusesTheTeamThatValidateRefusesAndWritesNothing(t *testing.T) {
	m, cmds := fold(New(Candidates([]agentstore.Entry{
		stored("builder", kernel.MemberConfig{Name: "builder",
			Model: "claude-sonnet-4", Stages: []string{"build"}}),
	})), seq(
		typeIn("late"), []Key{Press(KeyEnter), Typed(' '), Press(KeyEnter)},
		typeIn("review"), []Key{Press(KeyEnter), Press(KeyEnter)},
	)...)
	if m.Screen != ScreenReview || len(cmds) != 0 {
		t.Fatalf("the walk ended on %s with %d commands (%q)", m.Screen, len(cmds), m.Err)
	}

	m, cmds = fold(m, Press(KeyEnter))
	if len(cmds) != 0 {
		t.Fatalf("a team whose member takes part in no declared stage was written: %+v\n"+
			"  CreateTeam refuses it, so this is a designer that reports success and\n"+
			"  leaves nothing on disk -- and if it did write, the run would activate\n"+
			"  nobody and record run.quiescent after zero turns.", cmds)
	}
	if m.Screen != ScreenReview {
		t.Errorf("the refusal moved the operator to %s; the team is on the review\n"+
			"  screen and so is the problem with it", m.Screen)
	}
	for _, want := range []string{"builder", "build", "review"} {
		if !strings.Contains(m.Err, want) {
			t.Errorf("the refusal is %q, missing %q\n"+
				"  the stage that does not match was typed on one screen and the member\n"+
				"  picked on another, so the message has to carry both lists.", m.Err, want)
		}
	}
}

// TestEscGoesBackOneScreenWithoutLosingTheAnswerThere.
//
// Going back is only useful if what is behind is still there. An esc that reset
// the screen it returns to would make the key a trap: the operator who wanted to
// change one member would find the name gone, and the one who wanted to see the
// members again would have to pick all of them a second time.
func TestEscGoesBackOneScreenWithoutLosingTheAnswerThere(t *testing.T) {
	m, cmds := fold(onReview(t), Press(KeyEsc))
	if m.Screen != ScreenStages || strings.Join(m.Stages, ",") != "draft" {
		t.Fatalf("esc from review reached %s with stages %v, want stages [draft]",
			m.Screen, m.Stages)
	}
	m, _ = fold(m, Press(KeyEsc))
	if m.Screen != ScreenMembers || strings.Join(memberNames(m.Members()), ",") != "api" {
		t.Fatalf("esc from stages reached %s with members %v, want [api]",
			m.Screen, memberNames(m.Members()))
	}
	m, _ = fold(m, Press(KeyEsc))
	if m.Screen != ScreenName || m.Name.String() != "release" {
		t.Fatalf("esc from members reached %s with the name %q, want release",
			m.Screen, m.Name.String())
	}

	// And forward again, to the same team: going back must not have quietly
	// dropped something that only shows up in the file.
	m, cmds = fold(m, Press(KeyEnter), Press(KeyEnter), Press(KeyEnter), Press(KeyEnter))
	if len(cmds) != 1 {
		t.Fatalf("walking forward after three escs produced %d writes (screen %s, err %q)",
			len(cmds), m.Screen, m.Err)
	}
	team := writes(t, cmds)[0]
	if team.Name != "release" || strings.Join(memberNames(team.Members), ",") != "api" ||
		strings.Join(team.Stages, ",") != "draft" {
		t.Errorf("the team after going back and forward is %+v, and it was\n"+
			"  {release [api] [draft]} before: esc is a way to look, and a look that\n"+
			"  changes the answer is worse than no way back at all.", team)
	}
}

// TestEscOnTheFirstScreenLeavesHavingWrittenNothing.
//
// There is nothing behind the name screen, and esc is the key an operator reaches
// for on arriving somewhere they did not mean to be. The honest answer is to
// leave -- with Quit rather than a command, because the adapter restores the
// terminal on every exit path anyway.
func TestEscOnTheFirstScreenLeavesHavingWrittenNothing(t *testing.T) {
	m, cmds := fold(picker(), seq(typeIn("release"), []Key{Press(KeyEsc)})...)
	if !m.Quit {
		t.Errorf("esc on the first screen did not quit (screen %s)\n"+
			"  the only other way out of the name screen is answering it, so a designer\n"+
			"  that keeps the terminal here is one you leave with ctrl+c.", m.Screen)
	}
	if len(cmds) != 0 {
		t.Errorf("leaving wrote something: %+v", cmds)
	}
}

// TestCtrlCLeavesFromEveryScreenAndWritesNothing.
//
// Answered before any screen sees it, including the review screen where enter
// writes a file. A screen that could swallow ctrl+c is a designer you cannot
// leave, and the last screen that could swallow it is the one holding a whole
// composed team.
func TestCtrlCLeavesFromEveryScreenAndWritesNothing(t *testing.T) {
	for _, m := range []Model{picker(), onMembers(t), onStages(t), onReview(t)} {
		at := m.Screen
		out, cmds := fold(m, Press(KeyInterrupt))
		if !out.Quit {
			t.Errorf("ctrl+c on the %s screen did not quit", at)
		}
		if len(cmds) != 0 {
			t.Errorf("ctrl+c on the %s screen produced %+v; it means leave now, having\n"+
				"  written nothing", at, cmds)
		}
		if out.Wrote != "" {
			t.Errorf("ctrl+c on the %s screen reported a written file at %q", at, out.Wrote)
		}
	}
}

// TestTheStoresAnswerDecidesWhichScreenComesNext is why Written carries an error
// and not a message.
//
// ErrExists is the one refusal the review screen could not have caught: the
// candidates are read once before raw mode, so a file that appeared in between is
// invisible until the write. It is also the only one the operator can fix, and the
// name is on the first screen -- so that is where they land, with the name they
// typed still in the field. Anything else is about the team, and review is where
// the team is.
func TestTheStoresAnswerDecidesWhichScreenComesNext(t *testing.T) {
	base := onReview(t)

	done, cmds := Update(base, Written{Path: "agents/release.yaml"})
	if done.Screen != ScreenDone || done.Wrote != "agents/release.yaml" {
		t.Errorf("a successful write left the designer on %s with Wrote=%q\n"+
			"  the path is the whole receipt: it is what the operator opens to hand-edit\n"+
			"  the timeouts and what `arxi run start` is given next.", done.Screen, done.Wrote)
	}
	if done.Err != "" || len(cmds) != 0 {
		t.Errorf("a successful write left err %q and %d commands", done.Err, len(cmds))
	}

	taken, _ := Update(base, Written{
		Err: fmt.Errorf("blueprint %q: %w", "release", agentstore.ErrExists)})
	if taken.Screen != ScreenName {
		t.Errorf("a name taken at write time left the operator on %s\n"+
			"  the name is on the first screen, and it is the only thing that can be\n"+
			"  changed to make this write succeed. Anywhere else, the only move left is\n"+
			"  ctrl+c and start again.", taken.Screen)
	}
	if !strings.Contains(taken.Err, "already exists") {
		t.Errorf("the store's refusal became %q, which does not say the name is taken",
			taken.Err)
	}
	if taken.Wrote != "" {
		t.Errorf("a refused write reported a file at %q", taken.Wrote)
	}

	other, _ := Update(base, Written{Err: errors.New("open agents: permission denied")})
	if other.Screen != ScreenReview || !strings.Contains(other.Err, "permission denied") {
		t.Errorf("a write that failed for its own reason left %s / %q\n"+
			"  a full disk or a read-only directory is not about the name, so sending\n"+
			"  the operator to the name screen would have them rename a team to fix a\n"+
			"  permission.", other.Screen, other.Err)
	}
}

// TestTheDoneScreenIsAReceiptAndAnyKeyLeavesIt.
//
// The file exists by now. There is nothing on this screen to get wrong, and a
// screen that insists on one particular key to acknowledge a success is a screen
// the operator is stuck on for as long as it takes to guess which.
func TestTheDoneScreenIsAReceiptAndAnyKeyLeavesIt(t *testing.T) {
	done, _ := Update(onReview(t), Written{Path: "agents/release.yaml"})
	for _, k := range []Key{Press(KeyEnter), Press(KeyEsc), Typed('q'), Press(KeyTab)} {
		out, cmds := fold(done, k)
		if !out.Quit {
			t.Errorf("%v on the done screen did not leave", k)
		}
		if len(cmds) != 0 {
			t.Errorf("%v on the done screen wrote again: %+v\n"+
				"  the second write fails as existing, and the message would be about a\n"+
				"  name the operator just saw succeed.", k, cmds)
		}
	}
}

// TestAnEarlierModelIsNotRewrittenByALaterKeystroke.
//
// The one way a fold of values can mutate the past: append into spare capacity
// writes through every copy that shares the array. The loop keeps no old models,
// but the tests here do, the render pass reads one while the next is computed, and
// an undo would keep all of them -- so the bug would surface as a stage list that
// changes under a screen nobody touched.
func TestAnEarlierModelIsNotRewrittenByALaterKeystroke(t *testing.T) {
	one, _ := fold(onMembers(t), Typed(' '))
	// The later keystroke, deliberately discarded: the assertion is about `one`.
	fold(one, Press(KeyDown), Typed(' '))
	if got := strings.Join(memberNames(one.Members()), ","); got != "api" {
		t.Errorf("picking a second member changed the earlier model to %v, want [api]\n"+
			"  Picked was appended in place, so the model the frame was drawn from\n"+
			"  now disagrees with the frame.", got)
	}

	draft, _ := fold(onStages(t), seq(typeIn("draft"), []Key{Press(KeyEnter)})...)
	withReview, _ := fold(draft, seq(typeIn("review"), []Key{Press(KeyEnter)})...)
	withShip, _ := fold(draft, seq(typeIn("ship"), []Key{Press(KeyEnter)})...)
	if got := strings.Join(withReview.Stages, ","); got != "draft,review" {
		t.Errorf("the stages are %v, want [draft review]\n"+
			"  a second fold from the same model appended into the same array, so this\n"+
			"  team's second stage is the other team's.", got)
	}
	if got := strings.Join(withShip.Stages, ","); got != "draft,ship" {
		t.Errorf("the stages are %v, want [draft ship]", got)
	}
}
