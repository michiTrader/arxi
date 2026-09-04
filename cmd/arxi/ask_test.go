package main

import (
	"os"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// The short path is worth testing at the level of its pure parts, because the
// expensive half of it cannot be: cmdAsk starts a live run and ends in os.Exit.
// What is left is where the mistakes actually live -- which ceiling, whose model,
// which exit code -- and every one of those is silent when it is wrong.

// The first word decides whether this is a prompt or a command, and two of the
// flags it could be mean something else entirely. Excluded here rather than left to
// the order of main's switch, so that reordering the switch cannot turn `arxi
// --version` into a prompt with no prompt in it.
func TestLooksLikeAskLeavesHelpAndVersionAlone(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "--version", "-", "--", "run", "agent", ""} {
		if looksLikeAsk(arg) {
			t.Errorf("%q was taken as a prompt.\n"+
				"  consequence: the user asked for something else and got a run, or a "+
				"parse error about a prompt they never tried to give.", arg)
		}
	}
	for _, arg := range []string{"-p", "--prompt", "-m", "--model", "-b", "--sim"} {
		if !looksLikeAsk(arg) {
			t.Errorf("%q was not taken as a prompt.\n"+
				"  consequence: it falls through to notImplemented, which answers "+
				"%q does not exist in the surface -- true of the word, useless to "+
				"the reader.", arg, arg)
		}
	}
}

// The usage screen must name the ceiling the code will actually use. It is built
// from usd(askBudgetDefault) for that reason, and this pins the join: a screen that
// promises five cents while the constant says fifty is worse than one that promises
// nothing.
func TestAskUsageNamesTheRealDefaultCeiling(t *testing.T) {
	u := askUsage()
	for _, want := range []string{usd(askBudgetDefault), askBudgetEnv, "-p", "--model", defaultAgentName} {
		if !strings.Contains(u, want) {
			t.Errorf("the usage screen does not mention %q, and it is the screen a "+
				"failed parse prints: %s", want, u)
		}
	}
}

// A default ceiling is only defensible because it is announced, so the source note
// is part of the contract and not decoration. It must name the two ways to change
// the number, because the user who reads that line is the user who wants it changed.
func TestAskBudgetDefaultsAndSaysWhereFrom(t *testing.T) {
	got, source, err := askBudget("")
	if err != nil {
		t.Fatalf("an empty %s was refused: %v", askBudgetEnv, err)
	}
	if got != askBudgetDefault {
		t.Errorf("the default ceiling is %v, not %v", got, askBudgetDefault)
	}
	for _, want := range []string{askBudgetEnv, "--budget"} {
		if !strings.Contains(source, want) {
			t.Errorf("the printed source %q does not name %q.\n"+
				"  consequence: the ceiling is visible and unchangeable-looking, which "+
				"is the half of the promise that does not help anybody.", source, want)
		}
	}
}

// An unparseable ceiling is refused rather than ignored. Somebody who exported
// ARXI_BUDGET=0,50 was LOWERING their ceiling; falling back to the built-in default
// would install the number they meant to replace, and the difference arrives on the
// invoice rather than on the terminal.
func TestAskBudgetRefusesACeilingItCannotRead(t *testing.T) {
	for _, v := range []string{"0,50", "cheap", "1.0.0", "$0.50", "0.5USD"} {
		n, _, err := askBudget(v)
		if err == nil {
			t.Errorf("%s=%q was accepted as %v.\n"+
				"  consequence: a typo in a spend ceiling silently becomes the default "+
				"it was written to replace.", askBudgetEnv, v, n)
			continue
		}
		if !strings.Contains(err.Error(), askBudgetEnv) {
			t.Errorf("the refusal does not name %s, so the user does not know which "+
				"of their environment is wrong: %v", askBudgetEnv, err)
		}
	}
}

// Zero is the plausible typo, and reading it as "no limit" would make the most
// cautious number the user could type the most dangerous one. Same rule the parser
// applies to --budget, applied to the environment that can bypass it.
func TestAskBudgetRefusesANonPositiveCeiling(t *testing.T) {
	for _, v := range []string{"0", "0.00", "-1", "-0.5"} {
		if _, _, err := askBudget(v); err == nil {
			t.Errorf("%s=%q was accepted.\n"+
				"  consequence: a run that may not spend anything cannot take a single "+
				"turn, so this is never what was meant.", askBudgetEnv, v)
		}
	}
}

// A ceiling out of the environment is credited to the environment, and surrounding
// whitespace is not a typo worth refusing -- an exported value picked up from a file
// often carries it.
func TestAskBudgetReadsTheEnvironment(t *testing.T) {
	got, source, err := askBudget(" 0.25 ")
	if err != nil {
		t.Fatalf("a padded value was refused: %v", err)
	}
	if got != 0.25 {
		t.Errorf("the ceiling is %v, not 0.25", got)
	}
	if !strings.Contains(source, askBudgetEnv) {
		t.Errorf("the source %q does not credit %s, so the printed line implies the "+
			"user's own setting was ignored", source, askBudgetEnv)
	}
}

// The printed source has to credit the flag when the flag is what won, or the line
// contradicts the command it is describing. It stops at --, because after that a
// word is prompt: `arxi -p -- --budget` asks about a flag, it does not set one.
func TestBudgetWasTypedStopsAtTheEndOfFlags(t *testing.T) {
	typed := [][]string{
		{"--prompt", "hi", "--budget", "1"},
		{"--budget=1", "--prompt", "hi"},
	}
	for _, args := range typed {
		if !budgetWasTyped(args) {
			t.Errorf("%v was read as not carrying a ceiling.\n"+
				"  consequence: the run prints \"from ARXI_BUDGET\" while honouring the "+
				"flag, so the one number the user chose is credited to something else.",
				args)
		}
	}
	untyped := [][]string{
		{"--prompt", "hi"},
		{"--prompt", "hi", "--", "--budget", "1"},
		{"--", "--budget=1"},
	}
	for _, args := range untyped {
		if budgetWasTyped(args) {
			t.Errorf("%v was read as carrying a ceiling.\n"+
				"  consequence: the printed line credits --budget for a number that came "+
				"from the environment or from the built-in default.", args)
		}
	}
}

// The note names the model the RUN will use, which is the roster's and not the
// flag's: provider.Executor lets a member's own model: win over its DefaultModel, so
// printing --model would name the wrong model on exactly the runs where they differ.
func TestAskModelNoteNamesWhatTheRosterWillUse(t *testing.T) {
	one := kernel.Config{Members: []kernel.MemberConfig{{Name: "a", Model: "p/own"}}}
	if got := askModelNote(one, "p/flag", false); got != "p/own" {
		t.Errorf("the note says %q where the run will call p/own.\n"+
			"  consequence: the line printed above the spend names a model that was "+
			"not billed, which is worse than printing nothing.", got)
	}
	// One entry per DISTINCT model: a team all on one model should read as that
	// model, not as "2 models".
	same := kernel.Config{Members: []kernel.MemberConfig{{Name: "a"}, {Name: "b"}}}
	if got := askModelNote(same, "p/flag", false); got != "p/flag" {
		t.Errorf("two members filled from --model read as %q, not p/flag", got)
	}
	two := kernel.Config{Members: []kernel.MemberConfig{
		{Name: "a", Model: "p/x"}, {Name: "b", Model: "p/y"},
	}}
	if got := askModelNote(two, "", false); !strings.Contains(got, "p/x") ||
		!strings.Contains(got, "p/y") {
		t.Errorf("a two-model team reads as %q, naming neither model", got)
	}
	// A blueprint with no members at all is a single-actor run, and --model is then
	// the only model there is.
	if got := askModelNote(kernel.Config{}, "p/flag", false); got != "p/flag" {
		t.Errorf("a memberless config reads as %q, not p/flag", got)
	}
	if got := askModelNote(one, "", true); !strings.Contains(got, "--sim") {
		t.Errorf("a simulated run reads as %q, which names a model nobody calls", got)
	}
}

// The one-off team has to be a real blueprint, because executeRun freezes bp.Raw
// into the run directory as blueprint.snapshot.yaml. A Config assembled in place
// would leave Raw empty, the snapshot unparseable, and `arxi run why` unable to fold
// the run the user just paid for.
func TestEphemeralAgentIsARunnableBlueprint(t *testing.T) {
	bp, err := ephemeralAgent(defaultAgentName, "openai/gpt-4o-mini")
	if err != nil {
		t.Fatalf("a one-off agent could not be built: %v", err)
	}
	if len(bp.Raw) == 0 {
		t.Error("bp.Raw is empty.\n" +
			"  consequence: executeRun writes an empty blueprint.snapshot.yaml, and the " +
			"run cannot be replayed or explained afterwards.")
	}
	if len(bp.Config.Stages) == 0 {
		t.Error("the one-off team has no stages, so the loop has nothing to enter")
	}
	if len(bp.Config.Members) != 1 {
		t.Fatalf("the one-off team has %d members, not 1", len(bp.Config.Members))
	}
	m := bp.Config.Members[0]
	if m.Model != "openai/gpt-4o-mini" {
		t.Errorf("the member's model is %q, so --model was dropped on the way in", m.Model)
	}
	// No tools, deliberately. Tool policy is automatic, so a granted bash would let a
	// one-liner run commands in a directory the user never chose to hand over -- and
	// an `ask` becomes a blocking inbox question instead of an answer.
	if len(m.Tools) != 0 {
		t.Errorf("the one-off team was granted %v.\n"+
			"  consequence: a single-line invocation can touch the working directory, "+
			"which nobody asked it to do.", m.Tools)
	}
}

// Nothing is stored. A run that needed a model for one question should not leave a
// team behind under agents/ for the user to find later and wonder about.
func TestEphemeralAgentWritesNothing(t *testing.T) {
	dir := t.TempDir()
	defer func(old string) { agentDir = old }(agentDir)
	agentDir = dir

	if _, err := ephemeralAgent(defaultAgentName, "p/m"); err != nil {
		t.Fatalf("a one-off agent could not be built: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(ents) != 0 {
		t.Errorf("%d file(s) appeared under agents/: the one-off was persisted", len(ents))
	}
}

// The error a newcomer meets is the documentation they read, so it names both fixes:
// one for the user who wants this to work today, one for the user who wants it to
// work tomorrow without flags.
func TestAskNoModelMessageNamesBothFixes(t *testing.T) {
	msg := askNoModelMessage(defaultAgentName, "agents/default.yaml")
	for _, want := range []string{
		"arxi agent create " + defaultAgentName,
		"--model",
		"agents/default.yaml",
		"arxi model list",
		"arxi provider add",
		"on purpose",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q.\n"+
				"  consequence: this is the first thing a new user sees, and every fix "+
				"it leaves out is a fix they have to find elsewhere: %s", want, msg)
		}
	}
}

// The premise of the whole alias: with the actor already seeded, every remaining word
// is prompt. Otherwise `arxi -m X "hola mundo"` goes looking for an agent named hola
// and refuses with a name the user never typed.
func TestAskSeedTurnsEveryWordIntoPrompt(t *testing.T) {
	seed := func() startFlags {
		return startFlags{
			workspace: "auto", actor: defaultAgentName,
			budget: askBudgetDefault, budgetSet: true,
		}
	}
	expand := func(t *testing.T, args []string) []string {
		t.Helper()
		out, err := expandShort(surface.Lookup("run", "start"), args)
		if err != nil {
			t.Fatalf("expanding %v: %v", args, err)
		}
		return out
	}

	f, err := parseStartArgs(expand(t, []string{"-p", "hola mundo"}), seed())
	if err != nil {
		t.Fatalf(`arxi -p "hola mundo" was rejected: %v`, err)
	}
	if f.actor != defaultAgentName {
		t.Errorf("the actor is %q, not the seeded %q", f.actor, defaultAgentName)
	}
	if f.prompt != "hola mundo" {
		t.Errorf("the prompt is %q, not \"hola mundo\"", f.prompt)
	}
	if f.budget != askBudgetDefault {
		t.Errorf("the ceiling is %v, so the seeded default was dropped", f.budget)
	}

	// An unquoted positional prompt is joined rather than truncated, and -m does not
	// consume the first word of it.
	f, err = parseStartArgs(expand(t, []string{"-m", "p/x", "hola", "mundo"}), seed())
	if err != nil {
		t.Fatalf(`arxi -m p/x hola mundo was rejected: %v`, err)
	}
	if f.model != "p/x" {
		t.Errorf("the model is %q, not p/x", f.model)
	}
	if f.prompt != "hola mundo" {
		t.Errorf("the prompt is %q: an unquoted prompt was truncated or the model ate "+
			"a word of it", f.prompt)
	}
}

// askRunReply's answer comes from llm.response and not from Outcome.State.Result,
// which the reducer sets to "all stages completed". These events are the shape the
// provider executor actually writes.
func askEvent(t kernel.EventType, payload map[string]any) kernel.Event {
	return kernel.Event{Type: t, Payload: payload}
}

// The status fields are the LAST response's, because that is the turn the run stopped
// on. The text is the last NON-EMPTY one, because a final turn that only called a
// tool carries no text and must not blank an answer already given.
func TestLastReplyKeepsTheLastTextAndTheLastStatus(t *testing.T) {
	rep := lastReply([]kernel.Event{
		askEvent(kernel.RunStarted, map[string]any{"simulated": false}),
		askEvent(kernel.LLMResponse, map[string]any{
			"ok": true, "text": "first", "model": "p/a", "finish_reason": "stop",
		}),
		askEvent(kernel.LLMResponse, map[string]any{
			"ok": true, "text": "second", "model": "p/a", "finish_reason": "stop",
		}),
		askEvent(kernel.LLMResponse, map[string]any{
			"ok": true, "model": "p/b", "finish_reason": "tool_use",
		}),
	})
	if !rep.found || !rep.ok {
		t.Fatalf("a successful run reads as found=%v ok=%v", rep.found, rep.ok)
	}
	if rep.text != "second" {
		t.Errorf("the reply is %q.\n"+
			"  consequence: a last turn that only called a tool blanks the answer, so "+
			"stdout is empty on a run that succeeded and was paid for.", rep.text)
	}
	if rep.model != "p/b" || rep.finish != "tool_use" {
		t.Errorf("the status is %q/%q, not the last response's p/b/tool_use",
			rep.model, rep.finish)
	}
	if rep.sim {
		t.Error("a live run reads as simulated")
	}
}

// --sim is read off run.started because kernel.State does not carry it: the reducer
// has no use for the distinction, which is exactly what makes --sim worth trusting.
func TestLastReplyReadsSimOffRunStarted(t *testing.T) {
	rep := lastReply([]kernel.Event{
		askEvent(kernel.RunStarted, map[string]any{"simulated": true}),
		askEvent(kernel.LLMResponse, map[string]any{
			"agent": "default", "cost_usd": 0.0, "simulated": true,
		}),
	})
	if !rep.sim {
		t.Error("a simulated run does not read as simulated.\n" +
			"  consequence: it exits 1 on the branch below, so `arxi -p --sim` fails in " +
			"CI on every green run.")
	}
	if rep.ok {
		t.Error("the Fake writes no ok key, so ok must stay false and be handled by " +
			"the sim branch rather than papered over here")
	}
}

// The ordering is the contract for a command meant to live in a pipe, and it was
// measured against exec.Fake rather than reasoned about: the Fake writes no ok key at
// all, so a simulated run reads as found-and-not-ok and the obvious ordering made
// every --sim run exit 1.
func TestAskExitCodePutsSimBeforeFailure(t *testing.T) {
	sim := askReply{found: true, sim: true}
	if got := askExitCode(sim, nil); got != 0 {
		t.Errorf("a simulated run exits %d.\n"+
			"  consequence: `arxi -p \"...\" --sim` is the one invocation that costs "+
			"nothing and is safe in CI, and it would fail every time.", got)
	}
	// A loop error outranks even that: the run did not finish, whatever it recorded.
	if got := askExitCode(sim, os.ErrClosed); got != 1 {
		t.Errorf("a run that stopped early exits %d", got)
	}
}

// Text is success, a recorded failure is a failure even when an earlier turn produced
// text, and no text with no error is still 1 -- a caller piping stdout into something
// needs to know the pipe came up empty.
func TestAskExitCodeReportsWhatTheCallerCanSee(t *testing.T) {
	cases := []struct {
		name string
		rep  askReply
		want int
	}{
		{"a reply", askReply{found: true, ok: true, text: "hola"}, 0},
		{"a failed call", askReply{found: true, failed: "429 rate limited"}, 1},
		{"a failure after text", askReply{found: true, text: "half", failed: "eof"}, 1},
		{"nothing recorded", askReply{}, 1},
		{"found, ok, and silent", askReply{found: true, ok: true}, 1},
	}
	for _, c := range cases {
		if got := askExitCode(c.rep, nil); got != c.want {
			t.Errorf("%s exits %d, want %d.\n"+
				"  consequence: `arxi -p \"...\" && next` either runs on an empty answer "+
				"or refuses to run on a good one.", c.name, got, c.want)
		}
	}
}
