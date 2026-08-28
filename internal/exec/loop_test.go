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

// -------------------------------------------------------------------- resume

// A run must be resumable from the log ALONE, because that is the only thing a
// restarted process has. The state is not handed over: it is folded back out of
// the events, which is ADR-0002 in its operational form.
//
// The test runs to completion in two passes and compares against one pass over
// the same blueprint. If the resumed half disagreed with the single pass, then
// `run`, a resumed `run` and `replay` would be three programs that answer
// differently about the same log, and the third one is the one nobody notices is
// broken until they need it to explain an incident.
func TestLoopResumesFromTheLogAlone(t *testing.T) {
	c := teamCfg()

	// One uninterrupted pass, as the reference.
	whole, wholeLog, _, _ := newLoop(c)
	seedRun(t, wholeLog, c, nil)
	refOut, err := whole.Run(context.Background())
	if err != nil {
		t.Fatalf("reference pass: %v", err)
	}

	// Now the same run, stopped early and resumed. MaxSteps is the interruption:
	// it stops the loop mid-run the way a killed process does, leaving a log with
	// events above the cursor.
	part, partLog, _, _ := newLoop(c)
	part.MaxSteps = 3
	seedRun(t, partLog, c, nil)

	first, err := part.Run(context.Background())
	if err == nil {
		t.Fatalf("expected the step limit to interrupt the first pass; it stopped by %q",
			first.StoppedBy)
	}
	if first.Cursor == 0 {
		t.Fatal("the interrupted pass reported cursor 0, so it claims to have " +
			"consumed nothing. A resume would then re-decide run.started and pay " +
			"for every turn of the run a second time")
	}

	// The resumed loop gets the cursor and NOTHING else: no state, no member
	// list, no spend. Everything it knows it folds back out of the log.
	resumed, _, _, _ := newLoop(c)
	resumed.Log = partLog
	resumed.Runner.Log = partLog
	resumed.Cursor = first.Cursor

	second, err := resumed.Run(context.Background())
	if err != nil {
		t.Fatalf("resumed pass: %v", err)
	}

	if second.State.Status != refOut.State.Status {
		t.Errorf("resumed status = %q, uninterrupted = %q.\n"+
			"  consequence: a run that survives a restart reaches a different "+
			"conclusion than one that does not, so the log stops being the truth "+
			"and `replay` cannot be trusted to explain an incident.\n"+
			"  the resumed log was: %v", second.State.Status, refOut.State.Status,
			types(partLog))
	}
	if second.State.Stage != refOut.State.Stage {
		t.Errorf("resumed stage = %q, uninterrupted = %q: the resumed run is in a "+
			"different place in the blueprint", second.State.Stage, refOut.State.Stage)
	}
	if second.State.Turns != refOut.State.Turns {
		t.Errorf("resumed turns = %d, uninterrupted = %d.\n"+
			"  consequence: the two differ in how much WORK happened, so either the "+
			"resume paid for turns twice or it skipped the turn a stage was waiting "+
			"on. Both are silent.", second.State.Turns, refOut.State.Turns)
	}
	if second.State.TreeSpentUSD != refOut.State.TreeSpentUSD {
		t.Errorf("resumed spend = %.4f, uninterrupted = %.4f: a restart changed the "+
			"bill", second.State.TreeSpentUSD, refOut.State.TreeSpentUSD)
	}
}

// Folding the finished log must reproduce the state the loop arrived at live.
// This is the equality the whole architecture rests on: State = fold(Decide,
// State0, events). `replay` and `run why` read the fold, so if the fold and the
// live run disagree, the tool that explains the run contradicts the run.
func TestLoopStateEqualsAFoldOfItsLog(t *testing.T) {
	c := teamCfg()
	loop, log, _, _ := newLoop(c)
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fold the whole log from scratch, with no executor anywhere near it. This is
	// exactly what `replay` does.
	folded, err := log.Fold(c, 0)
	if err != nil {
		t.Fatalf("fold the log: %v", err)
	}

	if folded.Status != out.State.Status {
		t.Errorf("folded status = %q, live status = %q.\n"+
			"  consequence: State = fold(Decide, State0, events) does not hold, so "+
			"`replay` and `run why` describe a run that never happened while the "+
			"live run leaves no way to check.\n  the log was: %v",
			folded.Status, out.State.Status, types(log))
	}
	if folded.Turns != out.State.Turns || folded.TreeSpentUSD != out.State.TreeSpentUSD {
		t.Errorf("folded (turns=%d spend=%.4f) != live (turns=%d spend=%.4f): the "+
			"log does not account for the work the run actually did",
			folded.Turns, folded.TreeSpentUSD, out.State.Turns, out.State.TreeSpentUSD)
	}
	if folded.Stage != out.State.Stage || folded.StageIndex != out.State.StageIndex {
		t.Errorf("folded stage = %q/%d, live = %q/%d", folded.Stage, folded.StageIndex,
			out.State.Stage, out.State.StageIndex)
	}
}

// The cursor must LAG on a failed step, never lead. This is the one property of
// the cursor that no other test pins down, and the direction is a deliberate
// asymmetry rather than an accident of where the assignment sits.
//
// A cursor that lags re-executes the effects of the event it stopped on. A cursor
// that leads skips them. Re-spawning a turn costs money, which is bad and
// visible; skipping the turn a stage is waiting on costs the whole run, because
// the stage waits forever for a result nobody will produce and the failure is
// silent. Paying twice is recoverable, waiting forever is not.
//
// The step is failed by making the log refuse the write, which is the realistic
// version: the effects were carried out, the events describing them could not be
// recorded, so the loop must not claim that event as consumed.
func TestLoopCursorLagsOnAFailedStepInsteadOfLeading(t *testing.T) {
	c := teamCfg()
	loop, log, _, _ := newLoop(c)
	seedRun(t, log, c, nil)

	// Let the run get going, then break the log. Failing from the very first
	// event would leave the cursor at 0 and prove nothing about the direction.
	loop.MaxSteps = 2
	first, err := loop.Run(context.Background())
	if err == nil {
		t.Fatalf("expected the step limit to stop the first pass, stopped by %q",
			first.StoppedBy)
	}

	resume, _, _, _ := newLoop(c)
	resume.Log = log
	resume.Runner.Log = log
	resume.Cursor = first.Cursor

	// The next append fails. Whatever event the loop is on when that happens must
	// remain unconsumed.
	log.failAppend = errors.New("disk full")

	out, err := resume.Run(context.Background())
	if err == nil {
		t.Fatal("a log that refuses the write must fail the step: continuing " +
			"would fold the next event against a world the log does not describe, " +
			"and the run would be reasoning from a state no reader can rebuild")
	}

	// The cursor may well have advanced: the events the earlier pass left behind
	// are folded successfully until one of them needs a write. What must hold is
	// that the event that FAILED is not among the consumed ones, and the way to
	// establish that without reaching into the loop is to repair the log and
	// resume: if the failed event is still unread, its effects happen now and the
	// run reaches the same place an uninterrupted one does.
	log.failAppend = nil

	final, _, _, _ := newLoop(c)
	final.Log = log
	final.Runner.Log = log
	final.Cursor = out.Cursor

	done, err := final.Run(context.Background())
	if err != nil {
		t.Fatalf("resuming after the log recovered: %v", err)
	}

	// The reference: the same blueprint, never interrupted.
	ref, refLog, _, _ := newLoop(c)
	seedRun(t, refLog, c, nil)
	refOut, err := ref.Run(context.Background())
	if err != nil {
		t.Fatalf("reference pass: %v", err)
	}

	if done.State.Status != refOut.State.Status || done.State.Stage != refOut.State.Stage {
		t.Errorf("after a failed write and a resume the run is %q/%q, "+
			"uninterrupted it is %q/%q.\n"+
			"  consequence: the cursor claimed an event whose effects were never "+
			"recorded, so the resume skipped them. If one was the turn a stage "+
			"waits on, the stage waits forever for a result nobody will produce "+
			"and nothing in the log says why.\n  the log was: %v",
			done.State.Status, done.State.Stage,
			refOut.State.Status, refOut.State.Stage, types(log))
	}
	if done.State.Turns < refOut.State.Turns {
		t.Errorf("the recovered run did %d turns, the uninterrupted one %d: work "+
			"the stage is waiting on was skipped rather than redone. A cursor that "+
			"lags pays twice, which is recoverable; one that leads loses the turn, "+
			"which is not", done.State.Turns, refOut.State.Turns)
	}
}

// A cancelled context must stop the loop without a terminal status, and without
// pretending the run finished.
//
// Cancellation is the user pressing Ctrl-C, and it is not a failure of the run:
// the work done so far is real, is paid for, and has to remain resumable. If the
// loop marked the run failed or succeeded on the way out, the log would record an
// outcome nobody reached and the run could never be continued, so the money spent
// before the interruption would be thrown away.
func TestLoopCancellationStopsWithoutDecidingTheRun(t *testing.T) {
	c := teamCfg()
	loop, log, fake, _ := newLoop(c)
	seedRun(t, log, c, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the loop must notice before it spends anything

	out, err := loop.Run(ctx)
	if err != nil {
		t.Fatalf("cancellation is not an error, it is a way to stop: %v", err)
	}

	if out.StoppedBy != StopCancelled {
		t.Errorf("stopped by %q, expected %q.\n"+
			"  consequence: the caller cannot tell an interrupted run from a "+
			"finished one, so a resumable run looks complete and nobody resumes it",
			out.StoppedBy, StopCancelled)
	}
	if out.State.Status.Terminal() {
		t.Errorf("status = %q after a cancel.\n"+
			"  consequence: the run records an outcome it never reached and can "+
			"never be continued, so everything paid for before the interruption is "+
			"thrown away", out.State.Status)
	}
	if n := len(fake.Kinds()); n != 0 {
		t.Errorf("a pre-cancelled run still asked the executor for %d things (%v).\n"+
			"  consequence: Ctrl-C keeps spending, which is the one thing the user "+
			"pressed it to stop", n, fake.Kinds())
	}
}

// ------------------------------------------------------- the remaining guards

// TestLoopStepLimitNamesTheCycleAndWhyNothingElseCatchesIt protects the message,
// not just the error.
//
// The step limit is the only bound that can catch a reducer deriving events from
// events forever: --max-turns sees no turns opened and the budget sees no money
// spent, so both report a perfectly healthy run while the log grows until the
// disk fills. That makes this error the sole evidence the user will ever get,
// and an error that only says "step limit exceeded" turns the one available clue
// into a number to tune. The operator raises MaxSteps, waits longer, and fills
// the disk anyway.
//
// So the assertions are on the words: the error must distinguish a CYCLE from a
// long run, say that the other two ceilings cannot see it, and point at the
// place to look.
func TestLoopStepLimitNamesTheCycleAndWhyNothingElseCatchesIt(t *testing.T) {
	c := teamCfg()
	loop, log, _, _ := newLoop(c)
	loop.MaxSteps = 3
	seedRun(t, log, c, nil)

	_, err := loop.Run(context.Background())
	if err == nil {
		t.Fatal("folding past MaxSteps returned no error.\n" +
			"  consequence: the only ceiling that can catch a derivation cycle does " +
			"not report it, so the loop spins at full CPU appending events until " +
			"the disk fills, while --max-turns and the budget both show a healthy run")
	}
	if !errors.Is(err, ErrStepLimit) {
		t.Fatalf("error %v does not wrap ErrStepLimit.\n"+
			"  consequence: the caller cannot separate a reducer bug from a failed "+
			"write or a corrupt log, so `run start` reports all three the same way "+
			"and the bug that needs a maintainer looks like a full disk", err)
	}

	msg := err.Error()
	// Each of these carries a distinct piece of the diagnosis. Together they are
	// the difference between a bug report and a knob.
	for _, want := range []string{
		"cycle",       // it is a cycle, not a run that needed more room
		"--max-turns", // and neither of the other ceilings can see it,
		"budget",      // which is why nobody noticed earlier
		"cause each other",
	} {
		if !contains(msg, want) {
			t.Errorf("the step-limit error does not mention %q.\n  it said: %s\n"+
				"  consequence: this message is the ONLY evidence a derivation cycle "+
				"ever produces. Without naming the cycle and the two ceilings that "+
				"are blind to it, the operator reads it as a limit to raise, raises "+
				"it, and fills the disk on the next run instead of the first.",
				want, msg)
		}
	}
}

// TestLoopIdleOnAHumanIsNotQuiescence is the false positive ADR-0004 calls the
// expensive direction to get wrong.
//
// A run blocked on an unanswered question looks EXACTLY like a run that went
// silent: nobody is thinking, nobody is runnable, no money is moving. The
// difference is that this one is behaving correctly and is waiting for a person.
//
// Emitting quiescence here would be worse than emitting nothing. A signal that
// fires on healthy runs gets ignored, and it gets ignored precisely when it is
// true -- so the one failure mode the whole mechanism exists to catch becomes
// invisible because of the noise made by runs that were fine. And with no
// watcher the run would be FAILED outright: a run cancelled for the offence of
// waiting for the approval it was told to ask for.
func TestLoopIdleOnAHumanIsNotQuiescence(t *testing.T) {
	c := teamCfg()
	c.Watchers = nil // nobody to absorb a false positive: it would kill the run
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "all"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	// backend reaches for bash during its turn and is stopped by policy=ask, so
	// the denial arises where a real one does: inside an open turn, from a member
	// that was thinking. Driving it through the executor rather than appending the
	// event by hand is what makes this a test of the PATH: hand-appending puts the
	// denial before the stage that requested it was entered, a shape no provider
	// can produce.
	//
	// frontend submits normally, which is what makes this the interesting case
	// rather than a run where nothing happened: the stage is genuinely waiting on
	// one specific member, and that member is waiting on one specific person.
	//
	// No canned reply in HumanReplies, so the question stays open. That is the
	// state under test.
	fake.AskTools = map[string]string{"bash": "ask"}
	fake.DenyTurnTool = map[string]string{"backend": "bash"}
	fake.SubmitAgents = map[string]bool{"frontend": true}
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hasType(log, kernel.RunQuiescent) {
		t.Errorf("a run waiting on an unanswered question emitted run.quiescent.\n"+
			"  the log was: %v\n"+
			"  consequence: quiescence fires on runs that are behaving correctly, so "+
			"users learn to ignore it -- and it is then ignored exactly when it is "+
			"true, which is the one case the signal exists for (ADR-0004).\n"+
			"  remedy: keep the unanswered-inbox check in checkQuiescence.",
			types(log))
	}
	if out.State.Status == kernel.StatusFailed {
		t.Errorf("the run FAILED while waiting for a human.\n" +
			"  consequence: the run is cancelled for the offence of asking the " +
			"approval question the blueprint told it to ask, and the work already " +
			"paid for is discarded. Answering the question can no longer resume it.")
	}
	if out.StoppedBy != StopIdle {
		t.Errorf("stopped by %q, expected %q.\n"+
			"  consequence: a run parked on a human must be reported as idle and "+
			"resumable. Any other reason tells the caller there is nothing to come "+
			"back to, so nobody comes back and the answer is never applied.",
			out.StoppedBy, StopIdle)
	}

	// And the diagnosis must be available: idle-on-a-human is only harmless if
	// the user can find out WHO is waiting for WHAT.
	m := out.State.Member("backend")
	if m == nil || m.State != kernel.MemberWaiting {
		t.Fatalf("backend is %v, expected waiting: without the waiting state "+
			"`run why` cannot explain the stop and the user sees a run that is "+
			"merely doing nothing", m)
	}
	if m.BlockedOn == nil || m.BlockedOn["inbox_id"] == nil {
		t.Errorf("the waiting member carries no inbox_id in BlockedOn.\n" +
			"  consequence: `run why` can say the run is blocked but not which " +
			"question unblocks it, so the user has to guess which inbox item to " +
			"answer -- and answering the wrong one leaves the run parked.")
	}
	if !asked(fake) {
		t.Error("no question reached the executor, so nobody can ever answer it: " +
			"the run would sit idle forever with an inbox item that exists only " +
			"in the state")
	}
}

// TestLoopSurvivesAFailingTurn: an effect that fails is a fact about the run,
// not a reason to abandon it.
//
// The loop deliberately does not stop on effect-level errors, and the reason is
// in the blueprint rather than in optimism. A turn that failed is something the
// reducer may legitimately react to -- a watcher on agent.failed, a stage that
// advances without the missing member, a coordinator that reassigns the work --
// and stopping the loop would prevent exactly the recovery the user declared.
//
// The other half is that the failure must be VISIBLE. Continuing quietly would
// be worse than stopping: the run would carry on, reach some conclusion, and
// nothing would record that one member never worked. So the error is collected
// and reported, and the siblings that already spent money keep their events.
func TestLoopSurvivesAFailingTurn(t *testing.T) {
	c := teamCfg()
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "any"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	// A TRANSPORT failure on a tool: the call never landed, so it produces an
	// error and no event. This is the shape that used to be able to abort a run,
	// because unlike a domain failure there is nothing in the log to react to.
	fake.BreakTools["write"] = errors.New("provider connection reset")
	// The watcher turns the write into a real CallTool during the run.
	c.Watchers = []kernel.Watcher{
		{Agent: "security", Pattern: "agent.turn_done", Action: "run_tool", Tool: "write"},
	}
	loop.Config = c
	loop.Runner.Config = c
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())

	// The run must not be abandoned because a tool call could not be delivered.
	if err != nil {
		t.Fatalf("the loop returned a fatal error because an EFFECT failed: %v\n"+
			"  consequence: a single unreachable provider call ends the whole run, "+
			"so the watcher on agent.failed, the stage that could advance without "+
			"the missing member, and every other recovery the blueprint declared "+
			"never get the chance to run.\n"+
			"  remedy: effect errors belong in Outcome.Errs; only step-level "+
			"failures (a refused write, an unordered effect list) stop the loop.", err)
	}
	if len(out.Errs) == 0 {
		t.Errorf("a tool call failed at the transport level and Outcome.Errs is empty.\n"+
			"  consequence: this failure produces NO event -- the call never landed, "+
			"so there is nothing in the log to see it by. If it is not reported "+
			"here it is reported nowhere, and the run reaches a conclusion with a "+
			"member that silently never worked.\n  the log was: %v", types(log))
	}
	if !out.State.Status.Terminal() {
		t.Errorf("status = %q: the run neither finished nor failed after a broken "+
			"tool call, so it is left in a state nobody polls and nobody resumes",
			out.State.Status)
	}
	// The turns that did land must have their events regardless: they spent money,
	// and a log missing them describes a world that did not happen.
	if !hasType(log, kernel.LLMResponse) {
		t.Errorf("no llm.response survived the failing effect.\n"+
			"  consequence: the sibling turns already paid a provider, and dropping "+
			"their events makes the log disagree with the bill.\n  the log was: %v",
			types(log))
	}
}

// TestLoopSurvivesALogRefusingSnapshots: a snapshot is a cache (ADR-0002), so a
// run that cannot write one is still correct, only slower to inspect.
//
// This is the loop-level half of the guarantee. The Runner already declines to
// fail a step over it, but the loop is what turns a step into a run, and if it
// treated the skip as a stop then a read-only disk or a full volume would kill
// runs that are otherwise perfectly healthy -- inverting the ADR and making an
// optimization mandatory.
//
// The run is driven to a real conclusion here, not one step, because the
// interesting claim is that the CONCLUSION is unaffected: same status, same
// stage, same turns, same bill as a run whose snapshots all succeeded.
func TestLoopSurvivesALogRefusingSnapshots(t *testing.T) {
	c := teamCfg()

	// The reference: everything works.
	ref, refLog, _, _ := newLoop(c)
	seedRun(t, refLog, c, nil)
	refOut, err := ref.Run(context.Background())
	if err != nil {
		t.Fatalf("reference pass: %v", err)
	}

	// The same run against a log that refuses every snapshot write.
	loop, log, _, _ := newLoop(c)
	log.failSnapshot = errors.New("read-only file system")
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("the loop failed because a SNAPSHOT could not be written: %v\n"+
			"  consequence: the log is the truth and snapshots are a cache "+
			"(ADR-0002). Failing here inverts that: a full disk or a read-only "+
			"mount kills runs that are entirely correct, and the run is lost for "+
			"the sake of an index that only makes `run show` faster.", err)
	}

	if out.State.Status != refOut.State.Status {
		t.Errorf("status = %q with snapshots failing, %q with them working.\n"+
			"  consequence: whether a run succeeds depends on whether a CACHE could "+
			"be written, so the same events reach different conclusions on two "+
			"machines", out.State.Status, refOut.State.Status)
	}
	if out.State.Turns != refOut.State.Turns {
		t.Errorf("turns = %d with snapshots failing, %d with them working: the "+
			"amount of work done changed because a cache write failed",
			out.State.Turns, refOut.State.Turns)
	}
	if out.State.TreeSpentUSD != refOut.State.TreeSpentUSD {
		t.Errorf("spend = %.4f with snapshots failing, %.4f with them working: a "+
			"failed cache write changed the bill", out.State.TreeSpentUSD,
			refOut.State.TreeSpentUSD)
	}
	if len(log.snapshots) != 0 {
		t.Errorf("%d snapshots were recorded by a log that refuses them, so the "+
			"test is not exercising the failure it claims to", len(log.snapshots))
	}

	// And the state must still be recoverable from the log alone, which is the
	// reason losing snapshots is survivable in the first place.
	folded, err := log.Fold(c, 0)
	if err != nil {
		t.Fatalf("folding the snapshot-less log: %v", err)
	}
	if folded.Status != out.State.Status {
		t.Errorf("the folded log says %q, the run said %q.\n"+
			"  consequence: with no snapshot to fall back on, the fold is the ONLY "+
			"way to recover this run's state. If they disagree, a run that lost its "+
			"cache also lost its history.", folded.Status, out.State.Status)
	}
}

// TestLoopSurvivesALogThatRefusesSnapshots: a snapshot is a cache, so a run that
// cannot write one is still a correct run (ADR-0002).
//
// This is the ADR's easy half to get backwards. Snapshots exist to make a long
// log cheap to inspect, and the temptation on a failed write is to treat it like
// any other log error and stop -- which inverts the ADR and turns an
// optimization into a requirement. A read-only disk would then kill runs that
// are otherwise perfectly healthy, and the money already spent goes with them.
//
// The other half is that "not fatal" must not become "not mentioned". The count
// has to reach the caller, or a permanently failing cache degrades every run
// invisibly and `run show` just gets slower forever with nothing to point at.
func TestLoopSurvivesALogThatRefusesSnapshots(t *testing.T) {
	c := teamCfg()

	// The reference: the same run with a log that accepts snapshots. Every
	// assertion below compares against this, because the claim is not merely
	// "it did not crash" -- it is that the run reached the SAME conclusion.
	ref, refLog, _, _ := newLoop(c)
	seedRun(t, refLog, c, nil)
	want, err := ref.Run(context.Background())
	if err != nil {
		t.Fatalf("reference pass: %v", err)
	}
	if want.SnapshotSkipped != 0 {
		t.Fatalf("setup: the reference skipped %d snapshots, so it is not a clean "+
			"baseline to compare against", want.SnapshotSkipped)
	}

	loop, log, _, _ := newLoop(c)
	log.failSnapshot = errors.New("read-only file system")
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("the loop FAILED because a snapshot could not be written: %v\n"+
			"  consequence: a cache write turns into a dead run. ADR-0002 says the "+
			"log is the truth and the snapshot is an optimization, so a read-only "+
			"disk would kill runs that are entirely correct and discard everything "+
			"already paid for.\n"+
			"  remedy: keep runSnapshot counting into SnapshotSkipped instead of "+
			"returning an error.", err)
	}

	if out.State.Status != want.State.Status {
		t.Errorf("status = %q with snapshots failing, %q with them working.\n"+
			"  consequence: whether a run succeeds depends on whether its CACHE "+
			"could be written, so the same events reach two different conclusions "+
			"and the log stops being the truth.",
			out.State.Status, want.State.Status)
	}
	if out.State.Turns != want.State.Turns || out.State.TreeSpentUSD != want.State.TreeSpentUSD {
		t.Errorf("work differs: %d turns / %.4f USD with snapshots failing, "+
			"%d / %.4f with them working.\n"+
			"  consequence: a failing cache changed how much work happened and what "+
			"it cost, which means the snapshot is no longer an optimization but a "+
			"participant in the run.",
			out.State.Turns, out.State.TreeSpentUSD,
			want.State.Turns, want.State.TreeSpentUSD)
	}

	// The log itself must be unharmed: it is the artifact `replay` and `run why`
	// read, and a run whose log is short a few events is one that cannot be
	// explained afterwards.
	if got, exp := len(types(log)), len(types(refLog)); got != exp {
		t.Errorf("the log holds %d events, the reference holds %d.\n"+
			"  got:  %v\n  want: %v\n"+
			"  consequence: a failed snapshot cost the log real events, so the run "+
			"can never be replayed or explained -- which is precisely what the "+
			"snapshot was only ever supposed to make FASTER.",
			got, exp, types(log), types(refLog))
	}

	// And it must be reported.
	if out.SnapshotSkipped == 0 {
		t.Error("SnapshotSkipped is 0 although every snapshot write was refused.\n" +
			"  consequence: the loop swallows the Runner's count, so a permanently " +
			"broken cache is invisible. Every run degrades, `run show` and `run why` " +
			"get slower and slower, and there is nothing anywhere for an operator " +
			"to point at -- the failure hides behind being optional.\n" +
			"  remedy: accumulate res.SnapshotSkipped into the Outcome.")
	}
}

// TestLoopCarriesOnAfterATurnThatCouldNotBeReached: one unreachable provider
// must not abandon the run.
//
// This is the most ordinary failure a real run meets -- a 500, a rate limit, a
// dropped connection -- and it is the one the loop deliberately does NOT stop
// on. The reasoning is in Outcome.Errs: an effect that failed is a fact the
// reducer may legitimately react to, and stopping the fold would prevent exactly
// the recovery a blueprint declared. Aborting the whole run because one member's
// turn did not connect throws away every other member's finished, paid-for work.
//
// So the assertions are that the run KEEPS GOING, that the failure is REPORTED
// rather than swallowed, and that the log stays replayable. The last is the
// subtle one: a broken turn must leave nothing behind, because a member the
// reducer believes is thinking has an open turn that can never close, and it
// would then look eternally busy and mask every later silence.
func TestLoopCarriesOnAfterATurnThatCouldNotBeReached(t *testing.T) {
	c := teamCfg()
	c.Watchers = nil
	c.Stages = []kernel.StageConfig{{Name: "build", AdvanceWhen: "any"}}
	c = c.ResolveDefaults()

	loop, log, fake, _ := newLoop(c)
	// backend's provider is down; frontend's is fine. `any` means the stage can
	// still be satisfied, which is the point: the run has a way forward and the
	// loop must take it rather than stopping at the first error.
	fake.BreakTurns = map[string]error{"backend": errors.New("503 from the provider")}
	seedRun(t, log, c, nil)

	out, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("the loop returned a fatal error for ONE unreachable provider: %v\n"+
			"  consequence: a single 503 abandons the whole run, discarding the "+
			"finished and already-paid-for work of every other member. A rate limit "+
			"on one agent would kill a run that could still succeed.\n"+
			"  remedy: keep effect-level failures in Outcome.Errs and keep folding.",
			err)
	}

	// Reported, not swallowed.
	if len(out.Errs) == 0 {
		t.Error("the failed turn produced no entry in Outcome.Errs.\n" +
			"  consequence: the run is missing work nobody is told about. A user " +
			"sees a result that silently excludes an agent, and a retry cannot be " +
			"decided on because there is no record that anything failed.")
	}
	if !spawnedFor(fake, "backend") {
		t.Error("no turn was even attempted for backend: the failure has to come " +
			"from a real attempt, or this test is not exercising the path it claims")
	}

	// The run must have gone somewhere. `any` is satisfiable by frontend alone.
	if !hasType(log, kernel.StageSubmitted) {
		t.Errorf("no submit reached the log after one provider failed.\n"+
			"  the log was: %v\n"+
			"  consequence: one broken provider stopped the members that were "+
			"working, so a stage that only needed ANY submit got none.", types(log))
	}
	if out.State.Status == kernel.StatusFailed {
		t.Errorf("the run failed although its advance rule was `any` and frontend "+
			"was reachable.\n  result: %q\n"+
			"  consequence: a blueprint that explicitly tolerates one member not "+
			"delivering is defeated by the transport, so `any` means `all` whenever "+
			"a provider hiccups.", out.State.Result)
	}

	// And backend must not be left looking busy forever.
	if m := out.State.Member("backend"); m != nil && m.TurnOpen {
		t.Error("backend still has TurnOpen after its turn failed to start.\n" +
			"  consequence: Busy() stays true forever, so quiescence can never fire " +
			"again for this run and a genuine stall later on goes unreported -- the " +
			"one failure mode that costs a whole night.")
	}

	// The log has to remain foldable: it is what `replay` and `run why` read.
	folded, ferr := log.Fold(c, 0)
	if ferr != nil {
		t.Fatalf("the log cannot be folded after a failed turn: %v\n"+
			"  consequence: the run cannot be replayed or explained, so the incident "+
			"that most needs investigating is the one that destroyed the evidence.",
			ferr)
	}
	if folded.Status != out.State.Status {
		t.Errorf("folding the log gives status %q, the live run ended at %q.\n"+
			"  consequence: State = fold(log) is broken by a failed effect, so "+
			"`run why` contradicts the run it is explaining.",
			folded.Status, out.State.Status)
	}
}
