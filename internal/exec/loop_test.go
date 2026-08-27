package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/michiTrader/iash/internal/kernel"
)

// ------------------------------------------------------------------ helpers

// teamCfg is the blueprint the loop tests drive: two stages with different
// advance rules, two working members and one advisory, and a watcher on
// quiescence.
//
// It is deliberately the interesting blueprint rather than the minimal one.
// Coordination bugs do not appear with one agent and one stage: a single member
// satisfies `all` the moment it submits, so the advance rule, the advisory
// exclusion and the quiescence diagnosis would each be exercised only in their
// trivial form.
func teamCfg() kernel.Config {
	return kernel.Config{
		Blueprint: "feature-team",
		Members: []kernel.MemberConfig{
			{Name: "backend", Role: "coordinator", Tools: []string{"write"}},
			{Name: "frontend", Tools: []string{"write"}},
			{Name: "security", Advisory: true},
		},
		Stages: []kernel.StageConfig{
			{Name: "build", AdvanceWhen: "all", TimeoutMs: 1_800_000, OnTimeout: "escalate"},
			{Name: "review", AdvanceWhen: "any"},
		},
		Watchers: []kernel.Watcher{
			{Agent: "security", Pattern: "run.quiescent", Action: "notify"},
		},
	}.ResolveDefaults()
}

// newLoop wires a loop over the in-memory log, the fake executor and the virtual
// clock. This is the same wiring `run --sim` uses, which is the point: the tests
// exercise the shipped simulation path rather than a test-only arrangement.
func newLoop(c kernel.Config) (*Loop, *memLog, *Fake, *VirtualClock) {
	log := newMemLog()
	fake := NewFake()
	clock := NewVirtualClock()
	runner := &Runner{Log: log, Clock: clock, Executor: fake, Config: c}
	return &Loop{
		Runner: runner,
		Log:    log,
		Time:   VirtualTime{C: clock},
		Config: c,
	}, log, fake, clock
}

// seedRun appends the run.started event that begins a run, and nothing else. It
// returns nothing because the loop reads it back out of the log: the loop is a
// fold, and handing it the event directly would test a path production never
// uses.
func seedRun(t *testing.T, log *memLog, c kernel.Config, payload map[string]any) {
	t.Helper()
	full := map[string]any{
		"run_id": "r1", "actor": c.Blueprint, "budget_usd": 100.0,
	}
	for k, v := range payload {
		full[k] = v
	}
	if _, err := log.Append([]kernel.Event{{
		ID: "ev-start", Ts: "2026-08-27T00:00:00Z", Type: kernel.RunStarted,
		Scope: "run:r1", Source: kernel.SourceHuman, Payload: full,
	}}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
}

// types returns the event types the log holds, in append order.
func types(log *memLog) []kernel.EventType {
	evs, _ := log.Read(1, 0)
	out := make([]kernel.EventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func countType(log *memLog, want kernel.EventType) int {
	n := 0
	for _, t := range types(log) {
		if t == want {
			n++
		}
	}
	return n
}

func hasType(log *memLog, want kernel.EventType) bool {
	return countType(log, want) > 0
}

// spawnedFor reports whether the fake was asked to open a turn for an agent.
// Assertions read the fake rather than the log because an effect that produced
// no event is exactly the bug worth catching.
func spawnedFor(fake *Fake, agent string) bool {
	for _, call := range fake.Calls {
		if call.Kind == "spawn_turn" && call.Agent == agent {
			return true
		}
	}
	return false
}

// asked reports whether a human was asked anything.
func asked(fake *Fake) bool {
	for _, call := range fake.Calls {
		if call.Kind == "ask_human" {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------ the happy path

// TestLoopDrivesAStagedRunToCompletion is the acceptance test for the whole
// architecture: one run.started event in the log, and the loop alone must carry
// the run through both stages to a result.
//
// Nothing in this test tells the loop what to do next. Every subsequent event --
// entering build, opening the turns, the submits, advancing to review, finishing
// -- is derived by the reducer and executed by the runner, and the loop only
// folds what the log already contains. If any of those links were missing the run
// would stop early, which is what the assertions distinguish.
func TestLoopDrivesAStagedRunToCompletion(t *testing.T) {
	c := teamCfg()
	loop, log, fake, _ := newLoop(c)
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("the loop failed to drive the run: %v", err)
	}
	if err := errors.Join(out.Errs...); err != nil {
		t.Fatalf("effect errors during a run that should be clean: %v", err)
	}

	if out.State.Status != kernel.StatusSucceeded {
		t.Fatalf("run ended %q (stopped by %q, %d steps), expected succeeded.\n"+
			"  consequence: a blueprint that completes in simulation is reported as "+
			"stuck, and the user cannot tell that from a genuinely stuck run.\n"+
			"  the log was: %v",
			out.State.Status, out.StoppedBy, out.Steps, types(log))
	}
	if out.StoppedBy != StopTerminal {
		t.Errorf("StoppedBy = %q, expected %q", out.StoppedBy, StopTerminal)
	}

	// Both stages must have been entered. A run that reached "succeeded" without
	// entering review would mean the advance rule fired on the wrong stage, which
	// is worse than not finishing: a whole stage of work is skipped and the
	// result still claims success.
	if got := countType(log, kernel.StageEntered); got != 2 {
		t.Errorf("stage.entered appeared %d times, expected 2 (build and review); "+
			"the log was: %v", got, types(log))
	}
	if got := countType(log, kernel.StageAdvanced); got != 1 {
		t.Errorf("stage.advanced appeared %d times, expected 1; the log was: %v",
			got, types(log))
	}

	// Exactly one run.result. More than one means the advance rule fired again
	// after the stage had already resolved, so the log would claim the run
	// finished several times and `run why` would have competing answers for one
	// outcome.
	if got := countType(log, kernel.RunResult); got != 1 {
		t.Errorf("run.result appeared %d times, expected exactly 1; a run finishes "+
			"once, and a log that says otherwise gives `run why` competing answers "+
			"for one outcome. The log was: %v", got, types(log))
	}

	// No quiescence on a run that completed normally. ADR-0004 calls the false
	// positive the expensive direction: a signal the user learns to ignore is
	// worse than no signal, because it is ignored exactly when it is true.
	if hasType(log, kernel.RunQuiescent) {
		t.Errorf("a run that completed normally was diagnosed as silent; the log "+
			"was: %v", types(log))
	}

	// The advisory must NOT have been asked to work. That is the concrete
	// consequence of the trait: an advisory gives its opinion when called, and
	// waking it when a stage opens means paying for an opinion nobody requested.
	if spawnedFor(fake, "security") {
		t.Error("the advisory member took a paid turn; advisory members are woken " +
			"when called, not when a stage opens")
	}
	for _, who := range []string{"backend", "frontend"} {
		if !spawnedFor(fake, who) {
			t.Errorf("no turn was ever opened for %q; the fake saw: %v", who, fake.Kinds())
		}
	}

	// The timer armed by stage build must not still be armed: the run finished,
	// and a live timer would let a finished run "time out" afterwards.
	if out.State.ActiveTimer != "" {
		t.Errorf("ActiveTimer = %q after the run succeeded; a finished run with an "+
			"armed timer can still fire a stage timeout", out.State.ActiveTimer)
	}
}

// The loop must terminate in proportion to the work, not merely terminate. This
// is the cheap guard against a derivation cascade: the run still ends, so
// nothing looks broken, while CPU and disk are consumed deriving events from
// events.
func TestLoopTerminatesInProportionToTheWork(t *testing.T) {
	c := teamCfg()
	loop, log, _, _ := newLoop(c)
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Steps > 50 {
		t.Fatalf("the loop folded %d events to run two stages.\n"+
			"  consequence: events are being derived from events in a cycle; the run "+
			"still ends, so nothing looks broken while CPU and disk are consumed.\n"+
			"  the log was: %v", out.Steps, types(log))
	}
}

// -------------------------------------------------------------- quiescence

// TestLoopStopsOnQuiescenceWithoutObserver is the failure mode the whole design
// exists to catch, driven end to end.
//
// The stage advances with `all` and one member never submits, so the rule can
// never hold. Nobody is busy, nothing is armed, and the run would sit silent
// forever. ADR-0004 says the answer is to notify rather than to die, and with no
// watcher declared the run fails carrying the diagnosis.
func TestLoopStopsOnQuiescenceWithoutObserver(t *testing.T) {
	c := teamCfg()
	c.Watchers = nil // nobody observes
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "all"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	// frontend never submits, so `all` is unreachable. This is the realistic
	// shape of a stuck run: the turns happen and cost money, and the stage still
	// does not advance.
	fake.SubmitAgents = map[string]bool{"backend": true}
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasType(log, kernel.RunQuiescent) {
		t.Fatalf("an unmet advance rule with nobody working produced no run.quiescent.\n"+
			"  consequence: the run goes silent with no diagnosis, which is the most "+
			"expensive failure mode in the system: the user discovers it the next "+
			"morning with the money already spent.\n  the log was: %v", types(log))
	}
	if out.State.Status != kernel.StatusFailed {
		t.Errorf("status = %q, expected failed: quiescence with nobody observing has "+
			"to end the run rather than wait forever", out.State.Status)
	}
	if out.State.Result == "" {
		t.Error("the run failed with an empty Result; the diagnosis is what `run why` " +
			"reads, and without it the user is told only that something went wrong")
	}
}

// The diagnosis has to name the rule and who is missing. "The run is idle" is
// true and useless: the user is left staring at an apparently correct blueprint.
func TestLoopQuiescenceDiagnosisNamesTheRuleAndTheMissing(t *testing.T) {
	c := teamCfg()
	c.Watchers = nil
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "all"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	fake.SubmitAgents = map[string]bool{"backend": true}
	seedRun(t, log, c, nil)

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	evs, _ := log.Read(1, 0)
	var diag string
	for _, e := range evs {
		if e.Type == kernel.RunQuiescent {
			diag = e.Str("diagnosis")
		}
	}
	if diag == "" {
		t.Fatalf("run.quiescent carried no diagnosis; the log was: %v", types(log))
	}
	for _, want := range []string{"build", "all", "frontend"} {
		if !contains(diag, want) {
			t.Errorf("the diagnosis %q does not mention %q.\n"+
				"  consequence: the user is told the run is idle and not which rule is "+
				"unmet nor who is missing, which is the hardest case to debug by eye "+
				"because the blueprint looks correct.", diag, want)
		}
	}
}

// With an observer, the same silence must NOT kill the run. This is the half of
// ADR-0004 that is easy to get wrong: notifying and dying look equally "handled"
// from the outside, and only one of them lets a coordinator recover.
func TestLoopQuiescenceWithObserverWakesItInsteadOfDying(t *testing.T) {
	c := teamCfg()
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "all"}}
	c.Watchers = []kernel.Watcher{
		{Agent: "security", Pattern: "run.quiescent", Action: "notify"},
	}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	fake.SubmitAgents = map[string]bool{"backend": true}
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasType(log, kernel.RunQuiescent) {
		t.Fatalf("no run.quiescent was emitted; the log was: %v", types(log))
	}
	if !spawnedFor(fake, "security") {
		t.Errorf("the observer was never woken; the fake saw: %v.\n"+
			"  consequence: quiescence becomes fatal even when somebody declared they "+
			"would handle it, so a recoverable run has to be forked to continue and "+
			"loses the log continuity the user needs to understand what happened.",
			fake.Kinds())
	}
	if out.State.Status == kernel.StatusFailed {
		t.Errorf("the run failed despite an observer being declared; ADR-0004 makes "+
			"quiescence fatal only when NOBODY observes it (result: %q)", out.State.Result)
	}
}

// Quiescence is emitted at most once. Without QuiescentEmitted the detector
// re-fires on every subsequent step, and a watcher on run.quiescent then opens a
// paid turn each time: an infinite loop with a credit card attached.
func TestLoopQuiescenceIsEmittedOnce(t *testing.T) {
	c := teamCfg()
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "all"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	fake.SubmitAgents = map[string]bool{"backend": true}
	seedRun(t, log, c, nil)

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := countType(log, kernel.RunQuiescent); got != 1 {
		t.Errorf("run.quiescent appeared %d times, expected exactly 1.\n"+
			"  consequence: a watcher on run.quiescent opens a paid turn for each "+
			"one, so a stuck run bills repeatedly for noticing it is stuck.\n"+
			"  the log was: %v", got, types(log))
	}
}

// ---------------------------------------------------------------- the clock

// TestLoopFiresStageTimeoutThroughTheClock proves the loop advances time rather
// than only reacting to events.
//
// The stage arms a 30-minute timer and nobody submits. There are no unread events
// and nobody busy, so a loop that only folded events would stop here and call the
// run idle. It has to notice the armed deadline, move the clock to it, and turn
// what fired into an event.
func TestLoopFiresStageTimeoutThroughTheClock(t *testing.T) {
	c := teamCfg()
	c.Stages = []kernel.StageConfig{
		{Name: "build", AdvanceWhen: "all", TimeoutMs: 1_800_000, OnTimeout: "advance"},
		{Name: "review", AdvanceWhen: "any"},
	}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	fake.Submits = false // nobody submits, so only the deadline can move the run
	seedRun(t, log, c, nil)

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasType(log, kernel.TimerTick) {
		t.Fatalf("no timer.tick reached the log.\n"+
			"  consequence: the loop never advanced the clock, so a stage with a "+
			"timeout waits forever and the run is reported as silent rather than "+
			"timed out.\n  the log was: %v", types(log))
	}
	if !hasType(log, kernel.StageTimeout) {
		t.Fatalf("a timer fired but no stage.timeout was derived.\n"+
			"  consequence: every on_timeout branch is dead code, so escalate, "+
			"advance and fail are configuration the run silently ignores.\n"+
			"  the log was: %v", types(log))
	}
}

// A fired timer must be translated by the REDUCER, not by the loop. The loop
// appends timer.tick and nothing else, so `run`, `--sim`, resume and replay all
// derive stage.timeout from the same rule. A loop that emitted stage.timeout
// directly would leave replay -- which appends nothing -- unable to agree.
func TestLoopAppendsTicksNotDomainEvents(t *testing.T) {
	c := teamCfg()
	c.Stages = []kernel.StageConfig{
		{Name: "build", AdvanceWhen: "all", TimeoutMs: 1000, OnTimeout: "fail"},
	}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	fake.Submits = false
	seedRun(t, log, c, nil)

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The tick must precede the timeout it caused: the reducer derives the domain
	// event from the tick, so the reverse order would mean the loop invented it.
	seenTick := false
	for _, ty := range types(log) {
		if ty == kernel.TimerTick {
			seenTick = true
		}
		if ty == kernel.StageTimeout && !seenTick {
			t.Fatalf("stage.timeout appears before any timer.tick, so the loop "+
				"invented a domain event instead of reporting that a deadline "+
				"passed; replay could not reproduce it. The log was: %v", types(log))
		}
	}
}

// A tick is runtime, not agent, and that classification is load-bearing: runtime
// events do not re-trigger watchers. A tick marked otherwise would let a watcher
// on `*` react to the mere passage of time, which is an infinite loop with a
// credit card attached.
func TestLoopTicksAreRuntimeSourced(t *testing.T) {
	c := teamCfg()
	c.Stages = []kernel.StageConfig{
		{Name: "build", AdvanceWhen: "all", TimeoutMs: 1000, OnTimeout: "fail"},
	}
	c.Watchers = []kernel.Watcher{{Agent: "security", Pattern: "*", Action: "notify"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	fake.Submits = false
	seedRun(t, log, c, nil)

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	evs, _ := log.Read(1, 0)
	for _, e := range evs {
		if e.Type == kernel.TimerTick && e.Source != kernel.SourceRuntime {
			t.Errorf("timer.tick carried source %q, expected %q: a watcher on `*` "+
				"would then react to the passage of time and open a paid turn per "+
				"tick", e.Source, kernel.SourceRuntime)
		}
	}
}

// ----------------------------------------------------------------- ceilings

// The turn ceiling has to be reachable end to end. --max-turns exists to stop a
// looping blueprint, so if the loop never enforces it the flag is decorative and
// a cycle runs until somebody notices by hand.
func TestLoopEnforcesMaxTurns(t *testing.T) {
	c := teamCfg()
	loop, log, _, _ := newLoop(c)
	seedRun(t, log, c, map[string]any{"max_turns": 1.0})

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if out.State.Status != kernel.StatusExpired {
		t.Fatalf("status = %q with max_turns=1, expected expired.\n"+
			"  consequence: --max-turns is decorative end to end, and a looping "+
			"blueprint runs until somebody notices by hand.\n  the log was: %v",
			out.State.Status, types(log))
	}
	if !hasType(log, kernel.RunExpired) {
		t.Errorf("no run.expired in the log; the ceiling has to be a recorded fact, "+
			"not a status somebody set. The log was: %v", types(log))
	}
}

// The budget must block and ASK rather than kill the run. The work already done
// cost money and a human may want to pay for more; ending the run throws that
// away with no way back. This is deliberately asymmetric with the turn ceiling,
// where asking would just move the loop to somebody's inbox.
func TestLoopBudgetExhaustionBlocksAndAsks(t *testing.T) {
	c := teamCfg()
	loop, log, fake, _ := newLoop(c)
	fake.TurnCostUSD = 10.0
	seedRun(t, log, c, map[string]any{"budget_usd": 5.0})

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasType(log, kernel.BudgetExceeded) {
		t.Fatalf("spending 10.00 against a 5.00 budget emitted no budget.exceeded.\n"+
			"  consequence: --budget is not a ceiling but a suggestion, and the user "+
			"learns the real number from the invoice.\n  the log was: %v", types(log))
	}
	if out.State.Status.Terminal() {
		t.Errorf("status = %q: an exhausted budget must block and ask, not end the "+
			"run. The work already done cost money and a human may want to pay for "+
			"more; killing it throws that away with no way back", out.State.Status)
	}
	if !asked(fake) {
		t.Errorf("nobody was asked about the exhausted budget; the fake saw: %v",
			fake.Kinds())
	}

	// The ceiling has to STOP the spending, and this is the assertion that makes
	// the test worth having. Every check above already passed while the run went
	// on to spend 40.00 against a 5.00 budget: budget.exceeded was in the log, a
	// human had been asked, and the reducer then entered the next stage and
	// opened four more paid turns because StatusBlocked was a label no spawn site
	// consulted. Asserting on the log and the inbox measures whether iash NOTICED
	// the breach; only the spend measures whether it did anything about it.
	//
	// The bound is not the budget itself. Cost is known when a turn ends, so the
	// turns already in flight when the ceiling breaks are paid for and no design
	// avoids that. What must not happen is a turn opened AFTER the breach was
	// folded, so the bound is the work in flight (two members, one turn each) and
	// nothing beyond it.
	const inFlight = 2 * 10.0
	if out.State.TreeSpentUSD > inFlight {
		t.Errorf("spent %.2f USD against a 5.00 budget, over the %.2f already in "+
			"flight when the ceiling broke.\n"+
			"  consequence: turns were opened AFTER the run was blocked, so the "+
			"budget is enforced by the invoice rather than by the reducer.\n"+
			"  the log was: %v", out.State.TreeSpentUSD, inFlight, types(log))
	}

	// One breach, one question. Being over the ceiling stays true for every later
	// cost event, so an unguarded check re-asks on each one. Each copy is an inbox
	// item with OnTimeout "fail", so a duplicate is not noise: it is another way
	// to fail a run whose budget a human already agreed to raise, and the person
	// answering cannot tell which copy is live.
	if n := countType(log, kernel.BudgetExceeded); n != 1 {
		t.Errorf("budget.exceeded appears %d times, expected exactly 1.\n"+
			"  consequence: every surplus copy asks another human a question that "+
			"fails the run on timeout, so one unanswered duplicate kills a run "+
			"somebody already paid to continue.\n  the log was: %v", n, types(log))
	}
}

// Blocking on the budget is only half a decision: the withheld work has to still
// be there when somebody pays. The reducer parks the causes of the turns it
// refused to open instead of dropping them, and that choice is invisible until a
// human answers.
//
// If the causes were dropped, answering "raise" would resume a run with nothing
// left to do. The stage would then sit with nobody working and nobody wakeable,
// quiescence would fire, and the diagnosis would blame the advance rule of a
// blueprint that was never at fault -- sending the user to debug their YAML over
// a decision taken in spawnCauses.
func TestLoopResumesTheWithheldWorkWhenTheBudgetIsRaised(t *testing.T) {
	c := teamCfg()
	loop, log, fake, _ := newLoop(c)
	fake.TurnCostUSD = 10.0
	seedRun(t, log, c, map[string]any{"budget_usd": 5.0})

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.State.Status != kernel.StatusBlocked {
		t.Fatalf("status = %q, expected blocked before testing the resume; "+
			"the log was: %v", out.State.Status, types(log))
	}

	// Somebody parked. A blocked run that holds no cause has nothing to resume,
	// so this is the precondition the whole resume depends on.
	parked := 0
	for _, m := range out.State.Members {
		parked += len(m.PendingCauses)
	}
	if parked == 0 {
		t.Fatalf("the blocked run parked no causes.\n" +
			"  consequence: raising the budget resumes a run with no work left, " +
			"the stage dies of silence, and the diagnosis blames the blueprint " +
			"for a turn this reducer declined to open")
	}

	// Answer the question the way a human who wants to keep going answers it.
	var inboxID string
	evs, _ := log.Read(1, 0)
	for _, e := range evs {
		if e.Type == kernel.InboxCreated {
			inboxID = e.Str("inbox_id")
		}
	}
	if inboxID == "" {
		t.Fatalf("no inbox item to reply to; the log was: %v", types(log))
	}

	fake.TurnCostUSD = 0
	if _, err := log.Append([]kernel.Event{{
		ID: "ev-raise", Ts: "2026-08-27T00:01:00Z", Type: kernel.InboxReplied,
		Scope: "run:r1", Source: kernel.SourceHuman,
		Payload: map[string]any{"inbox_id": inboxID, "reply": "raise"},
	}}); err != nil {
		t.Fatalf("append the reply: %v", err)
	}

	// Resume from where the first pass stopped. Outcome.Cursor is exactly what a
	// resumed process has, which is why the test uses it rather than re-reading
	// from zero: starting at zero would re-execute every effect and pay twice.
	loop.Cursor = out.Cursor
	before := len(fake.Kinds())
	out2, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on resume: %v", err)
	}

	if len(fake.Kinds()) <= before {
		t.Errorf("the raised budget opened no turn: the executor saw %v.\n"+
			"  consequence: the parked causes were dropped rather than queued, so "+
			"paying for more work buys silence.\n  the log was: %v",
			fake.Kinds(), types(log))
	}
	if out2.State.Status == kernel.StatusBlocked {
		t.Errorf("status is still blocked after the reply; a run that cannot be " +
			"unblocked by answering the question has no way back and the work is lost")
	}
}
