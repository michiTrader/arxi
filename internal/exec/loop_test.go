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
