package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ------------------------------------------------------------------ helpers

// seq is the sequence counter for the tests. Every test that compares a golden
// has to reset it (see TestGolden): otherwise the result depends on which other
// tests ran before, which is the worst class of fragile test.
var seq int64

func nextSeq() int64 { seq++; return seq }

func ev(t EventType, actor string, payload map[string]any) Event {
	n := nextSeq()
	return Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10),
		Ts: "2026-08-26T00:00:00Z", Type: t, Scope: "run:r1",
		Source: SourceAgent, Actor: actor, Payload: payload,
	}
}

// started starts a run and returns the already-initialized state. It takes the
// Config because the members of the state are derived from the frozen blueprint.
func started(c Config) State {
	n := nextSeq()
	e := Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10),
		Ts: "2026-08-26T00:00:00Z", Type: RunStarted, Scope: "run:r1",
		Source: SourceRuntime,
		Payload: map[string]any{
			"run_id": "r1", "actor": "team", "budget_usd": 5.0,
			"blueprint_sha": "abc123",
		},
	}
	s, _ := Decide(State{}, e, c)
	return s
}

// bp is the test blueprint: four members (one advisory), two stages with
// different advance rules, three watchers. It is deliberately the interesting
// case and not the minimal one, because coordination bugs do not show up with a
// single agent and a single stage.
func bp() Config {
	return Config{
		Blueprint: "test",
		Members: []MemberConfig{
			{Name: "backend", Role: "coordinator", Tools: []string{"write", "bash"}},
			{Name: "designer"},
			{Name: "frontend", Tools: []string{"write"}},
			{Name: "mediator", Advisory: true},
		},
		Stages: []StageConfig{
			{Name: "execute", AdvanceWhen: "all", TimeoutMs: 1_800_000, OnTimeout: "escalate"},
			{Name: "integrate", AdvanceWhen: "any", OnConflict: "merge"},
		},
		Watchers: []Watcher{
			{Agent: "mediator", Pattern: "stage.timeout", Action: "notify"},
			{Agent: "mediator", Pattern: "resource.conflict", Action: "notify"},
			{Agent: "mediator", Pattern: "run.quiescent", Action: "notify"},
		},
	}.ResolveDefaults()
}

func countEffects[T Effect](fx []Effect) int {
	n := 0
	for _, f := range fx {
		if _, ok := f.(T); ok {
			n++
		}
	}
	return n
}

func firstEffect[T Effect](fx []Effect) (T, bool) {
	for _, f := range fx {
		if v, ok := f.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// drive does what the executor does: if the reducer returned Emit, those events
// are written to the log and go through Decide again.
//
// This helper exists because the first version of several tests was WRONG: they
// asserted "the stage advanced" by looking at the State that Decide returned,
// and the stage does not advance in that step - an event is emitted that
// advances the stage in the following step. Without this helper, the tests
// verify something different from what happens in production.
func drive(s State, e Event, c Config) (State, []Effect) {
	s, fx := Decide(s, e, c)
	all := append([]Effect(nil), fx...)
	for _, f := range fx {
		if em, ok := f.(Emit); ok {
			em.Event.Seq = nextSeq()
			em.Event.ID = "d" + strconv.FormatInt(em.Event.Seq, 10)
			var sub []Effect
			s, sub = drive(s, em.Event, c)
			all = append(all, sub...)
		}
	}
	return s, all
}

// ------------------------------------------------------------------ startup

func TestRunStartedInitializesMembers(t *testing.T) {
	c := bp()
	s := started(c)

	if s.Status != StatusRunning {
		t.Fatalf("status = %q", s.Status)
	}
	if len(s.Members) != 4 {
		t.Fatalf("members = %d", len(s.Members))
	}
	if s.StageIndex != -1 {
		t.Errorf("StageIndex = %d, expected -1 (has not entered any stage yet)", s.StageIndex)
	}
	if m := s.Member("mediator"); m.State != MemberInactive {
		t.Errorf("advisory started in %q, expected inactive", m.State)
	}
	if m := s.Member("backend"); m.State != MemberIdle {
		t.Errorf("non-advisory started in %q", m.State)
	}
	if s.BlueprintSHA != "abc123" {
		t.Errorf("the run did not pin the blueprint: %q", s.BlueprintSHA)
	}
}

// Protects the default that avoids the most expensive hole: two agents with
// `write` over the same directory overwrite each other and the KV store lock
// does not prevent it.
func TestWorkspaceWorktreeByDefault(t *testing.T) {
	c := bp()
	if c.Workspace != "worktree" {
		t.Fatalf("workspace = %q; with members that have write/bash the safe default is worktree", c.Workspace)
	}
	without := Config{Members: []MemberConfig{{Name: "a"}, {Name: "b"}}}.ResolveDefaults()
	if without.Workspace != "none" {
		t.Errorf("without write/bash the workspace should be none, was %q", without.Workspace)
	}
}

// ------------------------------------------------------------------- stages

func TestStageEnteredActivatesOnlyNonAdvisory(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	if got := countEffects[SpawnTurn](fx); got != 3 {
		t.Fatalf("turns opened = %d, expected 3 (the 4 members minus the advisory)", got)
	}
	for _, f := range fx {
		if sp, ok := f.(SpawnTurn); ok && sp.Agent == "mediator" {
			t.Error("a paid turn was opened for the advisory")
		}
	}
	if countEffects[SetTimer](fx) != 1 {
		t.Error("the stage has a timeout but the timer was not armed")
	}
	if s.Stage != "execute" {
		t.Errorf("stage = %q", s.Stage)
	}
}

// Resolves AI A's gap: the executor needs to know what to run in order and what
// in parallel. If a SpawnTurn slipped in before the SetTimer, the turn could
// finish before the timer that watches it even exists.
func TestEffectOrderControlBeforeIndependent(t *testing.T) {
	c := bp()
	s := started(c)
	_, fx := Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	seenIndependent := false
	for _, f := range fx {
		if f.Class() == ClassIndependent {
			seenIndependent = true
		} else if seenIndependent {
			t.Fatalf("a control effect appeared after an independent one: %T", f)
		}
	}
}

func TestQuorumAllAdvancesAndCancelsTimer(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	var fx []Effect
	for _, who := range []string{"backend", "designer", "frontend"} {
		s, fx = Decide(s, ev(StageSubmitted, who, nil), c)
	}

	if countEffects[CancelTimer](fx) != 1 {
		t.Error("the stage advanced but the timer stayed armed: the run could 'expire' after finishing")
	}
	if s.ActiveTimer != "" {
		t.Errorf("ActiveTimer = %q after advancing", s.ActiveTimer)
	}
	if countEffects[Emit](fx) < 2 {
		t.Errorf("expected stage.advanced and stage.entered, there were %d emits", countEffects[Emit](fx))
	}
}

// Protects the concrete consequence of the advisory trait: it does not count
// toward the quorum.
func TestAdvisoryDoesNotCountTowardQuorum(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = drive(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s, _ = drive(s, ev(StageSubmitted, "mediator", nil), c)
	if s.Stage != "execute" {
		t.Fatal("an advisory submit made the stage advance")
	}
	for _, who := range []string{"backend", "designer", "frontend"} {
		s, _ = drive(s, ev(StageSubmitted, who, nil), c)
	}
	if s.Stage != "integrate" {
		t.Errorf("stage = %q; with the 3 non-advisory it should already be in integrate", s.Stage)
	}
}

// Protects the default that avoids training the user to set absurd timeouts: a
// stage timeout escalates, it does not kill the run.
func TestStageTimeoutEscalatesDoesNotFail(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s2, fx := Decide(s, ev(StageTimeout, "", map[string]any{"stage": "execute"}), c)
	if s2.Status == StatusFailed {
		t.Fatal("a stage timeout killed the run; the default has to be to escalate")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Errorf("expected to wake the mediator, effects: %#v", fx)
	}
}

func TestStageTimeoutWithoutObserverAsksTheHuman(t *testing.T) {
	c := bp()
	c.Watchers = nil
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s, fx := Decide(s, ev(StageTimeout, "", map[string]any{"stage": "execute"}), c)
	if countEffects[AskHuman](fx) != 1 {
		t.Fatal("without an observer we have to ask, not throw the work in the trash")
	}
	if s.Status != StatusBlocked {
		t.Errorf("status = %q, expected blocked", s.Status)
	}
}

// TestRunStartedEntersTheFirstStage protects the entry point of every staged
// run.
//
// Without this derivation a staged run starts with everybody idle, no timer
// armed and no pending effect, so the very next step satisfies every condition
// in checkQuiescence: the run is declared silent and dies before a single agent
// was asked to work. The symptom reads as "my blueprint is broken" while the
// blueprint is fine, which is the most expensive kind of wrong error message.
func TestRunStartedEntersTheFirstStage(t *testing.T) {
	c := bp()
	n := nextSeq()
	e := Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10), Type: RunStarted,
		Scope: "run:r1", Source: SourceRuntime,
		Payload: map[string]any{"run_id": "r1", "actor": "team", "budget_usd": 5.0},
	}

	_, fx := Decide(State{}, e, c)

	em, ok := firstEffect[Emit](fx)
	if !ok {
		t.Fatalf("run.started emitted no stage.entered for a blueprint with %d stages.\n"+
			"  consequence: nobody enters stage %q, so no turn is opened, no timer is "+
			"armed, and the next step declares the run quiescent before any work happened.\n"+
			"  remedy: applyRunStarted must Emit stage.entered with index 0.",
			len(c.Stages), c.Stages[0].Name)
	}
	if em.Event.Type != StageEntered {
		t.Fatalf("run.started emitted %q, expected %q", em.Event.Type, StageEntered)
	}
	if got := em.Event.Str("stage"); got != "execute" {
		t.Errorf("entered stage %q, expected the FIRST stage %q; entering any other "+
			"stage skips work the blueprint declared", got, "execute")
	}
	if got := em.Event.Num("index"); got != 0 {
		t.Errorf("entered index %v, expected 0", got)
	}
}

// An unstaged blueprint must NOT get a stage.entered: there is no stage to
// enter, and inventing one would make the single-agent run of §20.1 fold into a
// state referring to a stage its blueprint never declared.
func TestRunStartedWithoutStagesEntersNothing(t *testing.T) {
	c := Config{Members: []MemberConfig{{Name: "reviewer"}}}.ResolveDefaults()
	n := nextSeq()
	e := Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10), Type: RunStarted,
		Scope: "run:r1", Source: SourceRuntime,
		Payload: map[string]any{"run_id": "r1", "actor": "reviewer", "budget_usd": 2.0},
	}

	_, fx := Decide(State{}, e, c)

	for _, f := range fx {
		if em, ok := f.(Emit); ok && em.Event.Type == StageEntered {
			t.Fatalf("an unstaged blueprint entered stage %q.\n"+
				"  consequence: the state names a stage the blueprint never declared, and "+
				"advance rules would be evaluated against it.\n"+
				"  remedy: applyRunStarted returns nil when len(c.Stages) == 0.",
				em.Event.Str("stage"))
		}
	}
}

// ---------------------------------------------------------------- the clock

// TestTimerTickBecomesStageTimeout protects the translation that makes every
// on_timeout branch reachable.
//
// A tick that only cleared ActiveTimer left the stage waiting forever with its
// deadline gone, and — because clearing the timer removes the last reason
// checkQuiescence stays quiet — the run was then reported as mysteriously silent
// instead of as timed out. `escalate`, `advance`, `fail` and the AskHuman
// fallback were unreachable code that only a hand-written event could enter.
func TestTimerTickBecomesStageTimeout(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	if s.ActiveTimer != "stage:execute" {
		t.Fatalf("ActiveTimer = %q, expected stage:execute", s.ActiveTimer)
	}

	s2, fx := Decide(s, ev(TimerTick, "", map[string]any{"timer_id": "stage:execute"}), c)

	em, ok := firstEffect[Emit](fx)
	if !ok {
		t.Fatalf("a fired stage timer produced no event.\n" +
			"  consequence: the stage loses its deadline and waits forever, and the run " +
			"is diagnosed as silent rather than as timed out; every on_timeout branch is " +
			"dead code.\n" +
			"  remedy: applyTimerTick must Emit stage.timeout for the active stage timer.")
	}
	if em.Event.Type != StageTimeout {
		t.Fatalf("tick emitted %q, expected %q", em.Event.Type, StageTimeout)
	}
	if got := em.Event.Str("stage"); got != "execute" {
		t.Errorf("timeout names stage %q, expected execute; on_timeout would be read "+
			"from the wrong stage", got)
	}
	if s2.ActiveTimer != "" {
		t.Errorf("ActiveTimer = %q after firing, expected empty: a timer that already "+
			"fired must not be able to fire twice", s2.ActiveTimer)
	}
}

// A tick for a timer that is no longer active is stale by construction: the
// stage advanced and CancelTimer raced the firing. Honouring it would expire a
// stage that already finished, which is the double outcome CancelTimer is
// classified as a control effect to prevent.
func TestStaleTimerTickIsIgnored(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	_, fx := Decide(s, ev(TimerTick, "", map[string]any{"timer_id": "stage:some-old-stage"}), c)

	for _, f := range fx {
		if em, ok := f.(Emit); ok && em.Event.Type == StageTimeout {
			t.Fatalf("a tick naming %q expired the CURRENT stage.\n"+
				"  consequence: a stage that already advanced is timed out by its "+
				"predecessor's timer, so the run ends in two different ways depending on "+
				"scheduling luck.\n"+
				"  remedy: applyTimerTick returns nil unless timer_id == ActiveTimer.",
				"stage:some-old-stage")
		}
	}
}

// A timer whose id carries no `stage:` prefix belongs to some other subsystem.
// Emitting a stage timeout for it would fabricate an event about a stage that
// was never involved.
func TestForeignTimerTickEmitsNoStageTimeout(t *testing.T) {
	c := bp()
	s := started(c)
	s.ActiveTimer = "inbox:7"

	s2, fx := Decide(s, ev(TimerTick, "", map[string]any{"timer_id": "inbox:7"}), c)

	for _, f := range fx {
		if em, ok := f.(Emit); ok && em.Event.Type == StageTimeout {
			t.Fatal("a non-stage timer produced a stage.timeout.\n" +
				"  consequence: the log claims a stage expired when no stage was involved.\n" +
				"  remedy: applyTimerTick requires the stage: prefix before emitting.")
		}
	}
	if s2.ActiveTimer != "" {
		t.Errorf("ActiveTimer = %q; a timer that fired is consumed whether or not it "+
			"maps to a stage, otherwise it fires forever", s2.ActiveTimer)
	}
}

// ------------------------------------------------------------ turn ceiling

// TestMaxTurnsIsEnforced protects a ceiling that was declared and ignored.
//
// MaxTurns was read out of run.started, stored in the State and compared against
// nothing, so `--max-turns 3` was a number the user typed and the reducer never
// used. It is the only bound that catches a run which LOOPS rather than spends: a
// watcher cascade of individually cheap turns can spin for hours without ever
// approaching a dollar ceiling.
func TestMaxTurnsIsEnforced(t *testing.T) {
	c := bp()
	n := nextSeq()
	s, _ := Decide(State{}, Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10), Type: RunStarted,
		Scope: "run:r1", Source: SourceRuntime,
		Payload: map[string]any{
			"run_id": "r1", "actor": "team", "budget_usd": 100.0, "max_turns": 3,
		},
	}, c)

	if s.MaxTurns != 3 {
		t.Fatalf("MaxTurns = %d, expected 3", s.MaxTurns)
	}

	// Two turns are below the ceiling and must not stop the run.
	for i := 0; i < 2; i++ {
		var fx []Effect
		s, fx = Decide(s, ev(AgentActivated, "backend", nil), c)
		for _, f := range fx {
			if em, ok := f.(Emit); ok && em.Event.Type == RunExpired {
				t.Fatalf("the run expired on turn %d of a ceiling of 3: the ceiling is "+
					"off by one and stops work the user paid for", i+1)
			}
		}
	}

	// The third reaches it.
	_, fx := Decide(s, ev(AgentActivated, "designer", nil), c)
	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != RunExpired {
		t.Fatalf("turn 3 of a ceiling of 3 did not expire the run (effects: %#v).\n"+
			"  consequence: --max-turns is decorative, and a cheap watcher loop runs "+
			"until somebody notices by hand.\n"+
			"  remedy: applyActivated must Emit run.expired when Turns reaches MaxTurns.", fx)
	}
	if got := em.Event.Str("reason"); got != "max_turns" {
		t.Errorf("expiry reason = %q, expected max_turns; `run why` reads this field to "+
			"say WHICH limit stopped the run", got)
	}
}

// A run with no declared ceiling must never expire on turn count. MaxTurns is
// optional (`--max-turns 0` is the documented default), and treating 0 as
// "zero turns allowed" would make every unbounded run die on its first activation.
func TestMaxTurnsZeroMeansNoCeiling(t *testing.T) {
	c := bp()
	s := started(c) // no max_turns in the payload

	for i := 0; i < 5; i++ {
		var fx []Effect
		s, fx = Decide(s, ev(AgentActivated, "backend", nil), c)
		for _, f := range fx {
			if em, ok := f.(Emit); ok && em.Event.Type == RunExpired {
				t.Fatalf("a run with no ceiling expired on turn %d.\n"+
					"  consequence: every run without --max-turns dies immediately.\n"+
					"  remedy: applyActivated must treat MaxTurns <= 0 as unbounded.", i+1)
			}
		}
	}
}

// -------------------------------------------------------- turns and causes

// Protects the most direct saving of the design: five reasons to talk to the
// same agent are ONE turn with five causes, not five turns. The difference is
// literally 5x on the bill.
func TestCoalescingOneTurnWithSeveralCauses(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentActivated, "backend", nil), c)

	for i := 0; i < 3; i++ {
		next, fx := Decide(s, ev(RunPrompt, "", map[string]any{"to": "backend"}), c)
		if countEffects[SpawnTurn](fx) != 0 {
			t.Fatal("a turn was opened for a busy agent")
		}
		s = next
	}
	n := len(s.Member("backend").PendingCauses)
	if n != 3 {
		t.Fatalf("queued causes = %d, expected 3: lost causes are the user's work thrown in the trash", n)
	}

	s, fx := Decide(s, ev(AgentTurnDone, "backend", nil), c)
	if got := countEffects[SpawnTurn](fx); got != 1 {
		t.Fatalf("turns = %d, expected 1 turn with the %d causes merged", got, n)
	}
	sp, _ := firstEffect[SpawnTurn](fx)
	if len(sp.CauseEvents) != n {
		t.Errorf("causes in the turn = %d, expected %d", len(sp.CauseEvents), n)
	}
	if sp.Coalesced != n {
		t.Errorf("Coalesced = %d; the number has to be auditable", sp.Coalesced)
	}
	if len(s.Member("backend").PendingCauses) != 0 {
		t.Error("the causes were not drained: they are going to be reprocessed forever")
	}
}

// Protects that `on_busy: queue` really queues and does not open a parallel turn
// that competes with the one already running.
func TestSteerBusyAgentQueuesDoesNotOpenTurn(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentActivated, "backend", nil), c)

	s, fx := Decide(s, ev(AgentSteered, "", map[string]any{"to": "backend"}), c)
	if countEffects[SpawnTurn](fx) != 0 {
		t.Fatal("on_busy: queue means queue, not open a parallel turn")
	}
	if len(s.Member("backend").PendingCauses) != 1 {
		t.Fatal("the steer was lost")
	}
}

func TestSteerToBlueprintWithBroadcast(t *testing.T) {
	c := bp()
	c.Inter.SteerTarget = "broadcast"
	s := started(c)
	_, fx := Decide(s, ev(AgentSteered, "", map[string]any{"text": "change course"}), c)

	// The advisory is inactive: a broadcast does not pay for a turn for every
	// commentator nobody invoked.
	if got := countEffects[SpawnTurn](fx); got != 3 {
		t.Fatalf("broadcast opened %d turns, expected 3", got)
	}
}

// Protects that "coordinator" is not a special kind of agent but a role. A
// single namespace, with no parallel categories.
func TestCoordinatorResolvesByRoleNotByType(t *testing.T) {
	c := bp()
	s := started(c)
	_, fx := Decide(s, ev(RunPrompt, "", map[string]any{"text": "hello"}), c)

	sp, ok := firstEffect[SpawnTurn](fx)
	if !ok {
		t.Fatal("a prompt with no recipient did not wake anybody")
	}
	if sp.Agent != "backend" {
		t.Errorf("the steer went to %q; backend has role=coordinator", sp.Agent)
	}

	// With nobody holding that role, it falls back to the first non-advisory:
	// the system keeps working instead of demanding ceremonial configuration.
	c2 := bp()
	c2.Members[0].Role = ""
	s2 := started(c2)
	_, fx2 := Decide(s2, ev(RunPrompt, "", map[string]any{"text": "hello"}), c2)
	sp2, _ := firstEffect[SpawnTurn](fx2)
	if sp2.Agent != "backend" {
		t.Errorf("with no declared role the fallback should be the first non-advisory, was %q", sp2.Agent)
	}
}

// ----------------------------------------------------------------- watchers

// Protects against the bug that gets billed in dollars: a watcher that reacts to
// its own events is an infinite loop with a credit card.
func TestWatcherSelfExclusion(t *testing.T) {
	c := bp()
	c.Watchers = []Watcher{{Agent: "backend", Pattern: "lock.*", Action: "notify"}}
	s := started(c)

	_, fx := Decide(s, ev(LockAcquired, "backend", map[string]any{"key": "k"}), c)
	for _, f := range fx {
		if sp, ok := f.(SpawnTurn); ok && sp.Agent == "backend" {
			t.Fatal("the watcher woke up on its own event: infinite loop")
		}
	}

	// With include_self it does wake up: the exclusion is a default, not a
	// prohibition. There are legitimate patterns that need to see themselves.
	c.Watchers[0].IncludeSelf = true
	_, fx = Decide(s, ev(LockAcquired, "backend", map[string]any{"key": "k"}), c)
	if countEffects[SpawnTurn](fx) == 0 {
		t.Error("include_self=true did not wake the watcher")
	}
}

// Protects the other cheap filter: without a depth limit, a watcher that reacts
// to what another watcher caused has no bottom.
func TestCausalDepthLimit(t *testing.T) {
	c := bp()
	c.MaxDepth = 3
	c.Watchers = []Watcher{{Agent: "mediator", Pattern: "lock.*", Action: "notify"}}
	s := started(c)

	e := ev(LockAcquired, "backend", map[string]any{"key": "k"})
	e.Depth = 3 // already at the limit
	_, fx := Decide(s, e, c)
	if countEffects[SpawnTurn](fx) != 0 {
		t.Fatal("a watcher was woken past the depth limit: the cascade has no bottom")
	}
}

// Protects that a trivial merge conflict does not kill half an hour of work.
func TestResourceConflictDoesNotFailTheRun(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(ResourceConflict, "backend", map[string]any{"path": "src/main.go"}), c)

	if s.Status == StatusFailed {
		t.Fatal("a resource conflict killed the run")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Errorf("the conflict did not wake the observer: %#v", fx)
	}
}

// ------------------------------------------------------------ human in the loop

// Protects that policy=ask is a question and not an error, and that the question
// leaves the structured reference that makes the automatic remedy possible.
func TestDeniedToolCreatesInboxWithStructuredReference(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(ToolCallDenied, "backend", map[string]any{
		"tool": "bash", "policy": "ask",
	}), c)

	if countEffects[AskHuman](fx) != 1 {
		t.Fatal("policy=ask is not an error, it is a question")
	}
	m := s.Member("backend")
	if m.State != MemberWaiting || m.Detail != "approval" {
		t.Fatalf("member = %q/%q", m.State, m.Detail)
	}
	if m.BlockedOn == nil || m.BlockedOn["inbox_id"] == nil || m.BlockedOn["tool"] != "bash" {
		t.Fatalf("incomplete blocked_ref: %#v", m.BlockedOn)
	}
	if len(s.Inbox) != 1 {
		t.Errorf("inbox = %d items", len(s.Inbox))
	}
}

func TestInboxRepliedUnblocksAndOpensTurn(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}), c)

	id := s.Inbox[0].ID
	s, fx := Decide(s, ev(InboxReplied, "", map[string]any{"inbox_id": id, "text": "go ahead"}), c)

	m := s.Member("backend")
	if m.State != MemberIdle {
		t.Errorf("the member stayed in %q after the reply", m.State)
	}
	if m.BlockedOn != nil {
		t.Error("blocked_on was not cleared: `run why` would keep reporting a resolved block")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Error("replying did not resume the work")
	}
	if !s.Inbox[0].Replied {
		t.Error("the item stayed marked as pending")
	}
}

// -------------------------------------------------------------------- budget

// Protects the ceiling that makes --budget mean something with nested spawn.
func TestBudgetWarnsAndExhaustsOverTheTree(t *testing.T) {
	c := bp()
	s := started(c) // budget 5.0, threshold 0.8 => warns at 4.0
	s, fx := Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 4.2}), c)

	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != BudgetWarning {
		t.Fatalf("4.2 of 5.0 with threshold 0.8 had to warn; effects: %#v", fx)
	}

	// The warning does not repeat: if it warned on every call, the user would
	// learn to ignore it and the warning would stop being useful.
	s, fx = Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.1}), c)
	for _, f := range fx {
		if e2, ok := f.(Emit); ok && e2.Event.Type == BudgetWarning {
			t.Error("the budget warning repeated")
		}
	}

	s, fx = Decide(s, ev(LLMResponse, "designer", map[string]any{"cost_usd": 1.0}), c)
	em2, ok := firstEffect[Emit](fx)
	if !ok || em2.Event.Type != BudgetExceeded {
		t.Fatalf("spend %.2f over budget 5.0 and it was not exhausted", s.TreeSpentUSD)
	}
	if em2.Event.Payload["tree_spent_usd"] == nil {
		t.Error("budget.exceeded without tree_spent_usd: the subtree spend is the one that matters")
	}
}

// Protects that exhausting the budget does not throw away work already paid for.
func TestBudgetExhaustedBlocksAndAsks(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(BudgetExceeded, "", map[string]any{
		"tree_spent_usd": 5.0, "budget_usd": 5.0,
	}), c)

	if s.Status != StatusBlocked {
		t.Fatalf("status = %q; exhausting the budget blocks, it does not kill", s.Status)
	}
	ah, ok := firstEffect[AskHuman](fx)
	if !ok {
		t.Fatal("nobody was asked whether they want to raise the ceiling")
	}
	if ah.OnTimeout == "" {
		t.Error("the question does not declare what to do if nobody answers")
	}
}

func TestSpendAttributedPerMember(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.5}), c)
	s, _ = Decide(s, ev(LLMResponse, "designer", map[string]any{"cost_usd": 0.25}), c)

	if got := s.Member("backend").SpentUSD; got != 0.5 {
		t.Errorf("backend spent %v, want 0.5", got)
	}
	if got := s.Member("designer").SpentUSD; got != 0.25 {
		t.Errorf("designer spent %v, want 0.25", got)
	}
	if s.SpentUSD != 0.75 || s.TreeSpentUSD != 0.75 {
		t.Errorf("run=%v tree=%v, want 0.75 in both", s.SpentUSD, s.TreeSpentUSD)
	}
}

// ---------------------------------------------------------------- quiescence

// quietCfg is a blueprint built to get stuck: the rule asks for three
// submissions and there are only two members that can submit. It is the real
// scenario (an advance rule impossible to satisfy) where the run does not fail,
// does not finish and simply goes quiet.
func quietCfg(watch bool) Config {
	c := Config{
		Stages:  []StageConfig{{Name: "solo", AdvanceWhen: "quorum:3"}},
		Members: []MemberConfig{{Name: "a"}, {Name: "b"}, {Name: "lookout", Advisory: true}},
	}
	if watch {
		c.Watchers = []Watcher{{Agent: "lookout", Pattern: "run.quiescent", Action: "activate"}}
	}
	return c.ResolveDefaults()
}

// untilQuiescent drives the run to the point where nobody has anything to do.
//
// The turns are CLOSED with agent.turn_done, and that is not ceremony. A turn is
// open from activation until it finishes, and a member with an open turn is busy
// by definition: its remaining events are still to come. Quiescence means the
// silence AFTER the work, so a sequence that left both turns open would be
// asserting that the reducer diagnoses a stuck run in the middle of two healthy
// ones -- the false positive ADR-0004 warns costs the most trust.
func untilQuiescent(t *testing.T, c Config) (State, []Effect) {
	t.Helper()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "solo", "index": 0}), c)
	s, _ = Decide(s, ev(AgentActivated, "a", nil), c)
	s, _ = Decide(s, ev(AgentActivated, "b", nil), c)
	s, _ = Decide(s, ev(StageSubmitted, "a", nil), c)
	s, _ = Decide(s, ev(AgentTurnDone, "a", nil), c)
	s, _ = Decide(s, ev(StageSubmitted, "b", nil), c)
	return Decide(s, ev(AgentTurnDone, "b", nil), c)
}

// Protects the detector of the most expensive failure mode of these systems: the
// system does not fail, does not finish, things stop happening, and the user
// finds out the next morning. An EVENT with a diagnosis is emitted, not a
// terminal state.
func TestQuiescenceIsDetectedWithDiagnosis(t *testing.T) {
	c := quietCfg(true)
	s, fx := untilQuiescent(t, c)

	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != RunQuiescent {
		t.Fatalf("run.quiescent was not emitted; effects: %#v", fx)
	}
	diag, _ := em.Event.Payload["diagnosis"].(string)
	if diag == "" {
		t.Fatal("run.quiescent with no diagnosis: 'the run is quiet' is useless to anybody")
	}
	if !strings.Contains(diag, "quorum:3") {
		t.Errorf("the diagnosis does not name the rule that is not met: %q", diag)
	}
	if s.Status != StatusRunning {
		t.Errorf("status = %q: quiescent is NOT a terminal state", s.Status)
	}
	if !s.QuiescentEmitted {
		t.Error("QuiescentEmitted was not marked: it would repeat on every event")
	}
}

// Protects: quiescence wakes the coordinator and the run does NOT fail. It is
// the difference between a system that recovers and one that only tells you that
// you lost.
func TestQuiescenceWakesTheObserverDoesNotFail(t *testing.T) {
	c := quietCfg(true)
	s, fx := untilQuiescent(t, c)

	em, _ := firstEffect[Emit](fx)
	em.Event.Seq = nextSeq()
	em.Event.ID = "eq"

	s, fx2 := Decide(s, em.Event, c)

	sp, ok := firstEffect[SpawnTurn](fx2)
	if !ok {
		t.Fatalf("the run went quiet and nobody was woken; effects: %#v", fx2)
	}
	if sp.Agent != "lookout" {
		t.Errorf("woke %q instead of the observer", sp.Agent)
	}
	if s.Status == StatusFailed {
		t.Error("quiescence killed the run instead of asking for intervention")
	}
}

func TestQuiescenceWithoutObserverDoesFail(t *testing.T) {
	c := quietCfg(false)
	s, fx := untilQuiescent(t, c)

	em, ok := firstEffect[Emit](fx)
	if !ok {
		t.Fatal("run.quiescent was not emitted")
	}
	em.Event.Seq = nextSeq()
	em.Event.ID = "eq"

	s, _ = Decide(s, em.Event, c)

	if s.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", s.Status)
	}
	if !strings.Contains(s.Result, "quorum:3") {
		t.Errorf("the result does not carry the diagnosis: %q", s.Result)
	}
}

// Protects the subtlety that made the first implementation fail: a member in
// state `submitted` is NOT runnable. It looks available (not thinking, not
// waiting) but it already submitted and has nothing to do. Counting it as
// runnable makes quiescence never get detected and the bug is invisible.
func TestSubmittedIsNotRunnable(t *testing.T) {
	if (Member{State: MemberSubmitted}).Runnable() {
		t.Fatal("submitted counts as runnable: quiescence would never be detected")
	}
	// Idle WITH a queued cause is runnable: applyTurnDone drains PendingCauses
	// into a new turn, so a turn really is coming.
	if !(Member{State: MemberIdle, PendingCauses: []string{"ev-1"}}).Runnable() {
		t.Error("idle with a queued cause does not count as runnable, so quiescence " +
			"would fire while a turn is still about to open")
	}

	// Idle with NOTHING queued is the other half of the same subtlety, and it hid
	// the bug for longer. The reducer never opens a turn just because somebody is
	// idle: every turn comes from a cause. A member whose turn ended without
	// submitting sits idle forever, and calling that "runnable" made
	// checkQuiescence believe work was still coming, so a stage whose rule needed
	// everybody went silent with no diagnosis at all.
	if (Member{State: MemberIdle}).Runnable() {
		t.Error("idle with no queued cause counts as runnable: nothing will ever " +
			"open a turn for it, so quiescence is never detected and the run hangs " +
			"silently forever")
	}

	for _, st := range []MemberState{MemberThinking, MemberTool, MemberWaiting, MemberInactive, MemberFailed} {
		if (Member{State: st}).Runnable() {
			t.Errorf("%q counts as runnable", st)
		}
	}
}

// A turn is open from activation until it finishes, and a member with an open
// turn is busy however it presents itself in between.
//
// This protects the false positive that cost both money and trust: a member that
// submits mid-turn displays MemberSubmitted, which is deliberately neither Busy
// nor Runnable, while its agent.turn_done is still on its way in the same batch.
// Folding that submit therefore found nobody busy, nobody runnable and nothing
// armed, and diagnosed a perfectly healthy run as silent forever. A watcher on
// run.quiescent then opened a paid turn to handle a problem that did not exist.
func TestAnOpenTurnCountsAsBusy(t *testing.T) {
	if !(Member{State: MemberSubmitted, TurnOpen: true}).Busy() {
		t.Error("a member that submitted mid-turn is not busy, so quiescence fires " +
			"in the middle of a healthy turn and a watcher pays for the false alarm")
	}
	if (Member{State: MemberSubmitted}).Busy() {
		t.Error("a member whose turn has finished still counts as busy, so a run " +
			"that really is stuck is never diagnosed")
	}
}

// ------------------------------------------------------------------ run why

// Protects that `run why` does not only explain but gives the exact command. The
// difference between "it is blocked" and "run `iash inbox approve inbox-1`" is
// the entire usefulness of the command.
func TestRunWhyExplainsAndRemediates(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}), c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "backend") || !strings.Contains(txt.String(), "bash") {
		t.Fatalf("why does not name the blocked member nor the cause:\n%s", txt.String())
	}
	if len(w.Fix) == 0 {
		t.Fatal("why explained the problem but did not give the command that fixes it")
	}
	if !strings.Contains(strings.Join(w.Fix, "\n"), "iash inbox approve") {
		t.Errorf("the remedy is not executable: %v", w.Fix)
	}
}

func TestRunWhyExplainsQuiescence(t *testing.T) {
	c := quietCfg(false)
	s, _ := untilQuiescent(t, c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "quorum:3") {
		t.Errorf("why does not name the advance rule that is not met:\n%s", txt.String())
	}
	if len(w.Fix) == 0 {
		t.Error("why does not suggest how to unblock a quiescent run")
	}
}

// Protects the schema contract: if a block does not bring blocked_ref, `run why`
// says so explicitly instead of showing an empty line and leaving the user
// thinking the system knows nothing.
func TestRunWhyExposesBlockWithoutReference(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentBlocked, "backend", map[string]any{"blocked_on": "weird_reason"}), c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "schema violation") {
		t.Errorf("why did not expose the block without a structured reference:\n%s", txt.String())
	}
}

// -------------------------------------------------------------- invariants

// Protects that a slow tool answering after the cancel is not an error: if it
// were, perfectly valid replays would fail.
func TestTerminalRunIgnoresLateEvents(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(RunCancelled, "", nil), c)

	s2, fx := Decide(s, ev(ToolCallCompleted, "backend", map[string]any{"tool": "bash"}), c)
	if len(fx) != 0 {
		t.Fatalf("a cancelled run produced effects: %#v", fx)
	}
	if s2.Status != StatusCancelled {
		t.Errorf("status changed to %q after terminal", s2.Status)
	}
}

// Protects the property that replay, --sim and eval depend on: the same log
// gives the same state, and the fold does not touch its input.
func TestFoldIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	c := bp()
	events := []Event{
		ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}),
		ev(AgentActivated, "backend", nil),
		ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.5}),
		ev(StageSubmitted, "backend", nil),
	}

	base := started(c)
	baseJSON, _ := json.Marshal(base)

	a, _ := Fold(base, events, c)
	b, _ := Fold(base, events, c)

	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("two folds of the same log gave different states: replay is worthless")
	}
	afterJSON, _ := json.Marshal(base)
	if string(baseJSON) != string(afterJSON) {
		t.Fatal("Fold mutated the input state")
	}
}

// This test is the replacement for the exhaustive `match` that Rust gives for
// free (see ADR-0007). If somebody adds an Effect variant and does not register
// it, this fails and tells them exactly what to do.
func TestEffectExhaustive(t *testing.T) {
	got := len(EffectVariants())
	const want = 7
	if got != want {
		t.Fatalf("registered variants = %d, expected %d.\n"+
			"If you added an Effect variant, add it to allEffectVariants "+
			"and review ALL the switches over Effect (grep 'case SpawnTurn').", got, want)
	}
	seen := map[string]bool{}
	for _, v := range EffectVariants() {
		name := fmt.Sprintf("%T", v)
		if seen[name] {
			t.Errorf("duplicate variant in the registry: %s", name)
		}
		seen[name] = true
		if v.Class() != ClassControl && v.Class() != ClassIndependent {
			t.Errorf("%s does not declare a valid class", name)
		}
	}
}

// ------------------------------------------------------------------- golden

func TestGolden(t *testing.T) {
	// seq is reset so that the golden does not depend on the order the other
	// tests ran in. Without this, `go test -run TestGolden` gives one result and
	// `go test` gives another, which is the worst class of fragile test.
	seq = 0

	c := bp()
	s := started(c)
	events := []Event{
		ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}),
		ev(AgentActivated, "backend", nil),
		ev(ToolCall, "backend", map[string]any{"tool": "bash"}),
		ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}),
		ev(LLMResponse, "designer", map[string]any{"cost_usd": 0.42}),
		ev(StageSubmitted, "designer", nil),
	}
	s, _ = Fold(s, events, c)
	w := Explain(s, c)

	out, err := json.MarshalIndent(map[string]any{"state": s, "why": w}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')

	path := filepath.Join("..", "..", "testdata", "scenarios", "blocked-on-approval.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden regenerated: " + path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Skip("golden missing; run with UPDATE_GOLDEN=1 to generate it")
	}
	if string(want) != string(out) {
		t.Errorf("the fold changed.\n"+
			"If the change is intentional: UPDATE_GOLDEN=1 go test ./internal/kernel/\n"+
			"and review the diff carefully.\n--- want ---\n%s\n--- got ---\n%s", want, out)
	}
}

// TestTurnDoneDoesNotResurrectAWaitingMember protects the guard at the top of
// applyTurnDone.
//
// The ordering it defends is not a corner case, it is the ONLY ordering this
// sequence has. A tool denied with policy=ask puts the member in MemberWaiting
// while its turn is open, so the agent.turn_done that closes that same turn
// always arrives afterwards. Clearing the state there produced a member that was
// idle and blocked at once, and every consequence of that is silent:
//
//   - `run why` reads BlockedOn and reports a block that State denies having.
//   - Runnable() reports idle members as runnable, so the member is handed new
//     turns while its approval is still unanswered -- and each new turn requests
//     the same tool, gets denied again, and files ANOTHER identical question.
//   - Quiescence sees somebody runnable and stays quiet, so the run never even
//     reports being stuck.
//
// The duplicate questions are the expensive part. The human answers one; the
// others expire into their OnTimeout "deny", which denies a tool the user
// already approved.
func TestTurnDoneDoesNotResurrectAWaitingMember(t *testing.T) {
	c := bp()
	s := started(c)

	// The turn is open, then the tool is denied: the real order.
	s, _ = Decide(s, ev(AgentActivated, "backend", map[string]any{"agent": "backend"}), c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{
		"tool": "bash", "policy": "ask",
	}), c)

	if m := s.Member("backend"); m.State != MemberWaiting {
		t.Fatalf("setup: backend is %q, expected waiting after policy=ask", m.State)
	}
	id := s.Member("backend").BlockedOn["inbox_id"]

	// The turn that was already running now finishes.
	s, fx := Decide(s, ev(AgentTurnDone, "backend", map[string]any{"agent": "backend"}), c)

	m := s.Member("backend")
	if m.State != MemberWaiting {
		t.Errorf("backend is %q after agent.turn_done, expected it to stay waiting.\n"+
			"  consequence: the member is idle and blocked simultaneously. `run why` "+
			"reports a block State says does not exist, and Runnable() offers the "+
			"member for new turns while its approval is unanswered.", m.State)
	}
	if m.BlockedOn == nil || m.BlockedOn["inbox_id"] != id {
		t.Errorf("BlockedOn is %v, expected it to still name %v.\n"+
			"  consequence: the reference that tells the user WHICH question "+
			"unblocks the run is gone, so `run why` can only say something is "+
			"wrong and the inbox has to be guessed at.", m.BlockedOn, id)
	}
	if m.Detail == "" {
		t.Error("Detail was cleared, so the block has no reason attached: `run why` " +
			"prints \"backend waits for \" with nothing after it")
	}
	if m.Runnable() {
		t.Error("a member waiting on a human reports itself runnable.\n" +
			"  consequence: it is handed a turn whose first act is to request the " +
			"same unapproved tool, which files a SECOND identical question. The " +
			"human answers one copy and the other expires into OnTimeout \"deny\", " +
			"denying a tool that was already approved.")
	}
	if countEffects[SpawnTurn](fx) != 0 {
		t.Errorf("agent.turn_done opened %d turn(s) for a member waiting on approval: "+
			"that is money spent on work that cannot proceed",
			countEffects[SpawnTurn](fx))
	}

	// And the turn must still be recorded as closed. Leaving TurnOpen set would
	// make the member look eternally busy and mask every later silence.
	if m.TurnOpen {
		t.Error("TurnOpen survived agent.turn_done.\n" +
			"  consequence: Busy() stays true forever, so quiescence can never " +
			"fire again for this run and a genuine stall goes unreported.")
	}
}

// TestStageEntryDoesNotResurrectAWaitingMember protects the sibling guard in
// applyStageEntered, and it exists because the guard in applyTurnDone was not
// enough on its own.
//
// Entering a stage wakes everybody who participates in it, which is correct and
// is the mechanism that gets work started. But a member waiting on a human is
// not asleep, and waking it has the same two costs as clearing it in
// applyTurnDone: the block is destroyed, and a turn is bought for an agent whose
// tool is still unapproved. That turn requests the same tool, gets denied again,
// and files a SECOND identical question -- so the human answers one copy while
// the other expires into OnTimeout "deny", denying a tool already approved.
//
// The cause is PARKED rather than dropped, and that half matters just as much.
// Withholding the turn without remembering why would mean the reply, when it
// finally comes, wakes a member with nothing to do: no cause, so no turn, so the
// run goes quiet and quiescence blames a blueprint that is correct. Queue, do not
// drop.
func TestStageEntryDoesNotResurrectAWaitingMember(t *testing.T) {
	c := bp()
	s := started(c)

	// backend is waiting on an approval, arrived at the only way it can be: a
	// tool denied with policy=ask during an open turn.
	s, _ = Decide(s, ev(AgentActivated, "backend", map[string]any{"agent": "backend"}), c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{
		"tool": "bash", "policy": "ask",
	}), c)
	if m := s.Member("backend"); m.State != MemberWaiting {
		t.Fatalf("setup: backend is %q, expected waiting after policy=ask", m.State)
	}
	id := s.Member("backend").BlockedOn["inbox_id"]

	// The run now enters a stage backend participates in.
	before := len(s.Inbox)
	s, fx := Decide(s, ev(StageEntered, "", map[string]any{
		"stage": c.Stages[0].Name, "index": 0,
	}), c)

	m := s.Member("backend")
	if m.State != MemberWaiting {
		t.Errorf("backend is %q after stage.entered, expected it to stay waiting.\n"+
			"  consequence: entering a stage answers the approval question on the "+
			"human's behalf. The member is idle and blocked at once, so `run why` "+
			"reports a block State denies, and the agent is handed a turn whose "+
			"first act is to request the same unapproved tool.", m.State)
	}
	if m.BlockedOn == nil || m.BlockedOn["inbox_id"] != id {
		t.Errorf("BlockedOn is %v, expected it to still name %v: the reference that "+
			"says WHICH question unblocks the run was discarded by a stage change",
			m.BlockedOn, id)
	}
	// Counted for backend specifically, not across the whole effect list. The
	// other participants SHOULD get turns -- that is what entering a stage is
	// for -- so asserting zero turns overall would forbid the correct behaviour
	// along with the bug.
	if n := spawnsFor(fx, "backend"); n != 0 {
		t.Errorf("stage.entered opened %d turn(s) for a member waiting on approval.\n"+
			"  consequence: money spent on work that cannot proceed, and the turn "+
			"files a duplicate of a question already pending. The human answers one "+
			"copy and the other times out into \"deny\", denying an approved tool.", n)
	}
	// The stage must still start for everybody else: withholding one member's
	// turn cannot become an excuse to stall the stage.
	if n := countEffects[SpawnTurn](fx); n == 0 {
		t.Error("stage.entered opened no turns at all.\n" +
			"  consequence: one member waiting on a human froze the entire stage, so " +
			"an approval question stops work that did not depend on it.")
	}
	if len(s.Inbox) != before {
		t.Errorf("the inbox grew from %d to %d items on a stage change: a second copy "+
			"of a pending question means one of them will expire unanswered",
			before, len(s.Inbox))
	}

	// The cause must be REMEMBERED, not dropped, or the answer resumes nothing.
	if len(m.PendingCauses) == 0 {
		t.Fatal("the withheld cause was dropped instead of parked.\n" +
			"  consequence: answering the question clears the block and finds no " +
			"reason to open a turn, so the member wakes with nothing to do. The run " +
			"then goes silent and quiescence reports the stage's advance rule as " +
			"unsatisfiable -- sending the user to debug a correct blueprint about " +
			"work the reducer is holding.\n" +
			"  remedy: append to PendingCauses in the MemberWaiting branch.")
	}

	// And the reply must spend exactly that parked cause.
	s, fx = Decide(s, ev(InboxReplied, "", map[string]any{
		"inbox_id": id, "text": "approved",
	}), c)
	if m := s.Member("backend"); m.State == MemberWaiting {
		t.Errorf("backend is still waiting after its question was answered: the reply " +
			"must release the block it names, or no answer can ever resume a run")
	}
	if n := countEffects[SpawnTurn](fx); n == 0 {
		t.Error("replying to the approval opened no turn.\n" +
			"  consequence: the user answered, the block cleared, and the work still " +
			"did not resume. From the outside that is indistinguishable from the " +
			"answer having been ignored, and the run sits idle having been paid for.")
	}
}

// spawnsFor counts the turns opened for one agent. Assertions need this rather
// than countEffects[SpawnTurn] whenever the correct behaviour is "this member
// gets no turn and the others do": a count over the whole list cannot tell the
// withheld turn from a stalled stage, so it would pass a reducer that froze
// everybody.
func spawnsFor(fx []Effect, agent string) int {
	n := 0
	for _, f := range fx {
		if v, ok := f.(SpawnTurn); ok && v.Agent == agent {
			n++
		}
	}
	return n
}
