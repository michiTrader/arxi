package exec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/michiTrader/iash/internal/kernel"
)

// memLog is an in-memory Log. It assigns Seq, because that is the property of
// the real log the runner depends on: the reducer emits events with Seq 0 and
// only the single writer numbers them.
type memLog struct {
	mu        sync.Mutex
	events    []kernel.Event
	head      int64
	snapshots map[int64]kernel.State

	failAppend   error
	failFold     error
	failSnapshot error

	// appendOrder records the Type of each appended event so a test can assert
	// on append ORDER, which is the property that keeps --sim diffable.
	appendOrder []string
}

func newMemLog() *memLog {
	return &memLog{snapshots: map[int64]kernel.State{}}
}

func (m *memLog) Append(events []kernel.Event) ([]kernel.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAppend != nil {
		return nil, m.failAppend
	}
	out := make([]kernel.Event, 0, len(events))
	for _, e := range events {
		m.head++
		e.Seq = m.head
		m.events = append(m.events, e)
		m.appendOrder = append(m.appendOrder, string(e.Type))
		out = append(out, e)
	}
	return out, nil
}

func (m *memLog) Fold(c kernel.Config, untilSeq int64) (kernel.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failFold != nil {
		return kernel.State{}, m.failFold
	}
	var upto []kernel.Event
	for _, e := range m.events {
		if untilSeq <= 0 || e.Seq <= untilSeq {
			upto = append(upto, e)
		}
	}
	st, _ := kernel.Fold(kernel.State{}, upto, c)
	return st, nil
}

func (m *memLog) WriteSnapshot(st kernel.State, atSeq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSnapshot != nil {
		return m.failSnapshot
	}
	m.snapshots[atSeq] = st
	return nil
}

func (m *memLog) order() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.appendOrder...)
}

func newRunner() (*Runner, *memLog, *Fake, *VirtualClock) {
	log := newMemLog()
	fake := NewFake()
	clock := NewVirtualClock()
	return &Runner{Log: log, Clock: clock, Executor: fake, Config: kernel.Config{}}, log, fake, clock
}

func emit(t kernel.EventType) kernel.Effect {
	return kernel.Emit{Event: kernel.Event{ID: string(t), Type: t}}
}

// TestSimulatedTurnDrivesTheFullLifecycle protects what makes --sim predictive
// rather than merely quiet.
//
// The Fake used to answer a turn with llm.response + agent.turn_done and nothing
// else, so a simulated member went from idle straight back to idle. Three
// behaviours silently could not occur in any simulation:
//
//   - State.Turns never advanced, so --max-turns was unreachable in --sim;
//   - m.Busy() was never true, so applyTurnDone never coalesced and each queued
//     cause opened its own paid turn — the exact 5x the design exists to avoid;
//   - a steer arriving mid-turn took the not-busy branch instead of queueing.
//
// A simulation that skips the turn lifecycle is worse than no simulation: it
// predicts a run that cannot happen, and it does so confidently.
//
// The ORDER is asserted, not just the presence. agent.activated must precede
// llm.response (it is what marks the member busy) and llm.response must precede
// agent.turn_done (the reducer charges the budget on it, so a turn_done seen
// first would let a run look finished and under budget while the money was
// already spent).
func TestSimulatedTurnDrivesTheFullLifecycle(t *testing.T) {
	r, _, _, _ := newRunner()

	res, err := r.Run(context.Background(), []kernel.Effect{
		kernel.SpawnTurn{Agent: "backend"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got []kernel.EventType
	for _, e := range res.Events {
		got = append(got, e.Type)
	}
	want := []kernel.EventType{
		kernel.AgentActivated, kernel.LLMResponse, kernel.AgentTurnDone,
	}
	if len(got) != len(want) {
		t.Fatalf("a simulated turn produced %v, want %v.\n"+
			"  consequence: without agent.activated the member never becomes busy, so "+
			"in --sim State.Turns never advances (--max-turns is unreachable), "+
			"coalescing never engages (every queued cause opens its own paid turn), "+
			"and a mid-turn steer does not queue.\n"+
			"  remedy: Fake.SpawnTurn emits agent.activated before llm.response.",
			got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d is %q, want %q (full order %v).\n"+
				"  consequence: agent.activated after llm.response leaves the member idle "+
				"while it is billed; agent.turn_done before llm.response lets a run look "+
				"finished and under budget with the money already spent.",
				i, got[i], want[i], got)
		}
	}

	if res.Events[0].Actor != "backend" {
		t.Errorf("agent.activated names actor %q, want backend; the reducer looks the "+
			"member up by Actor, so a blank one silently updates nobody",
			res.Events[0].Actor)
	}
}

// TestEveryVariantIsDispatched is the counterpart of kernel's
// TestEffectExhaustive: kernel guarantees no variant is forgotten in the
// registry, and this guarantees no registered variant is forgotten by the
// runner.
//
// Without it, adding an eighth Effect variant and wiring it nowhere here would
// produce a run that looks perfectly healthy and silently does not do what the
// reducer decided, which is unfindable from the log because the log has no
// entry for something that never happened.
func TestEveryVariantIsDispatched(t *testing.T) {
	for _, variant := range kernel.EffectVariants() {
		r, _, _, _ := newRunner()

		var err error
		switch variant.Class() {
		case kernel.ClassControl:
			var res Result
			err = r.runControl(context.Background(), variant, &res)
		case kernel.ClassIndependent:
			_, err = r.dispatch(context.Background(), variant)
		default:
			t.Fatalf("effect %T has class %d, which exec knows nothing about: the "+
				"runner splits work into a sequential control prefix and a parallel "+
				"tail, and a third class has no defined placement. Either map it "+
				"onto one of the two existing classes or extend Runner.Run with an "+
				"explicit rule for it.", variant, variant.Class())
		}

		if err != nil && errIsUnhandled(err) {
			t.Errorf("effect %T is registered in kernel.EffectVariants but the "+
				"runner does not dispatch it, so the reducer's decision to perform "+
				"it would be dropped with no trace in the log and the run would "+
				"look healthy while doing less than it decided. Remedy: add a case "+
				"for %T in exec.runControl (control) or exec.dispatch plus a method "+
				"on the Executor interface (independent). Error was: %v",
				variant, variant, err)
		}
	}
}

func errIsUnhandled(err error) bool {
	msg := err.Error()
	for _, needle := range []string{"unhandled control effect", "unhandled independent effect"} {
		if len(msg) >= len(needle) && contains(msg, needle) {
			return true
		}
	}
	return false
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestControlEffectsRunInExactListOrder protects the semantic order of Emits.
// kernel emits stage.advanced before stage.entered on purpose; if the runner
// reordered or parallelized them, a reader of the log would see a stage being
// entered before the previous one advanced, and every causal tool built on
// caused_by would describe a history that never happened.
func TestControlEffectsRunInExactListOrder(t *testing.T) {
	r, log, _, _ := newRunner()

	fx := []kernel.Effect{
		emit(kernel.StageAdvanced),
		emit(kernel.StageEntered),
		emit(kernel.AgentActivated),
	}
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("Run returned an unexpected error: %v", err)
	}

	want := []string{"stage.advanced", "stage.entered", "agent.activated"}
	if got := log.order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("control effects were appended as %v, want %v. Control effects "+
			"must run one at a time in exact list order: the order of Emits among "+
			"themselves is semantic, so reordering them writes a log describing a "+
			"history that never happened. Remedy: keep the sequential loop over "+
			"fx[:split] in Runner.Run and do not sort or batch it.", got, want)
	}
}

// TestSeqIsAssignedByTheLogNotTheReducer pins the invariant from ADR-0002 and
// event.go: the reducer emits Seq 0 and only the single writer numbers events.
// If the runner ever assigned Seq itself, two writers would be numbering the
// same log and a CAS on seq would become meaningless.
func TestSeqIsAssignedByTheLogNotTheReducer(t *testing.T) {
	r, _, _, _ := newRunner()

	fx := []kernel.Effect{emit(kernel.StageEntered), emit(kernel.StageAdvanced)}
	res, err := r.Run(context.Background(), fx)
	if err != nil {
		t.Fatalf("Run returned an unexpected error: %v", err)
	}

	if len(res.Events) != 2 {
		t.Fatalf("got %d appended events, want 2", len(res.Events))
	}
	for i, e := range res.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d came back with Seq %d, want %d. The runner must "+
				"return the events as the LOG numbered them, never the Seq 0 the "+
				"reducer produced: callers use these seq values for CAS "+
				"(if_seq), and a zero or invented seq makes every conditional "+
				"write either always fail or always pass. Remedy: append "+
				"res.Events from the slice Log.Append returned, not from the "+
				"input effects.", i, e.Seq, i+1)
		}
	}
}

// TestIndependentEffectsRunInParallel proves parallelism instead of assuming
// it, and it does so with a barrier rather than with timing.
//
// A timing assertion ("it took less than X") is flaky on a loaded machine and
// would eventually be deleted. A barrier cannot be satisfied unless the effects
// genuinely overlap: if the runner serialized them, the first would wait
// forever for siblings that have not started, and the test fails by timeout
// with a clear reason.
//
// This matters because serializing independent effects removes the entire
// reason the tool exists: three agents that know nothing about each other would
// take three times as long and cost the same.
func TestIndependentEffectsRunInParallel(t *testing.T) {
	const n = 3
	arrived := make(chan struct{}, n)
	release := make(chan struct{})

	r, _, _, _ := newRunner()
	r.Executor = &barrierExecutor{arrived: arrived, release: release}

	fx := []kernel.Effect{
		kernel.SpawnTurn{Agent: "a"},
		kernel.SpawnTurn{Agent: "b"},
		kernel.SpawnTurn{Agent: "c"},
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), fx)
		done <- err
	}()

	// Wait for all three to be inside the executor at the same time.
	for i := 0; i < n; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d independent effects had started after 2s: they "+
				"are being executed sequentially. Independent effects exist to run "+
				"concurrently; serializing them makes three unrelated agent turns "+
				"take three times as long for the same cost, which removes the "+
				"reason this tool exists. Remedy: keep the WaitGroup fan-out over "+
				"fx[split:] in Runner.runIndependent.", i, n)
		}
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish after releasing the barrier")
	}
}

type barrierExecutor struct {
	arrived chan struct{}
	release chan struct{}
}

func (b *barrierExecutor) SpawnTurn(ctx context.Context, e kernel.SpawnTurn) ([]kernel.Event, error) {
	b.arrived <- struct{}{}
	<-b.release
	return []kernel.Event{{ID: e.Agent, Type: kernel.AgentTurnDone, Actor: e.Agent}}, nil
}

func (b *barrierExecutor) CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error) {
	return nil, nil
}

func (b *barrierExecutor) AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}

// TestIndependentResultsAppendInListOrder is the test that keeps --sim useful.
//
// The effects finish in reverse order here (the last one returns first). If the
// runner appended in completion order, the log would depend on which provider
// answered first, so two runs over identical input would produce different
// logs and `run diff` could not tell a real change from scheduling noise.
func TestIndependentResultsAppendInListOrder(t *testing.T) {
	r, log, _, _ := newRunner()
	r.Executor = &reverseOrderExecutor{}

	fx := []kernel.Effect{
		kernel.SpawnTurn{Agent: "first"},
		kernel.SpawnTurn{Agent: "second"},
		kernel.SpawnTurn{Agent: "third"},
	}
	res, err := r.Run(context.Background(), fx)
	if err != nil {
		t.Fatalf("Run returned an unexpected error: %v", err)
	}

	var got []string
	for _, e := range res.Events {
		got = append(got, e.Actor)
	}
	want := []string{"first", "second", "third"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("independent results were appended as %v, want %v. Results must "+
			"be appended in LIST order and never in completion order: otherwise "+
			"the log depends on which provider answered first, two --sim runs "+
			"over identical input produce different logs, and `run diff` cannot "+
			"distinguish a real change from scheduling noise. Remedy: keep the "+
			"indexed outcomes slice in runIndependent and append while iterating "+
			"it in order.", got, want)
	}
	_ = log
}

// reverseOrderExecutor makes the LAST effect finish FIRST, so a completion-order
// bug cannot pass by luck.
type reverseOrderExecutor struct {
	mu   sync.Mutex
	seen int
}

func (x *reverseOrderExecutor) SpawnTurn(ctx context.Context, e kernel.SpawnTurn) ([]kernel.Event, error) {
	// A sleep whose length is inverse to arrival order. Sleeping is acceptable
	// here because the assertion is about ORDER, not duration: the test passes
	// or fails on the appended sequence, so a slow machine only makes it
	// slower, never flaky.
	x.mu.Lock()
	x.seen++
	n := x.seen
	x.mu.Unlock()

	time.Sleep(time.Duration(20*(4-n)) * time.Millisecond)
	return []kernel.Event{{ID: e.Agent, Type: kernel.AgentTurnDone, Actor: e.Agent}}, nil
}

func (x *reverseOrderExecutor) CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error) {
	return nil, nil
}

func (x *reverseOrderExecutor) AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}

// TestUnorderedListIsRefused makes sure the runner does not paper over a
// reducer bug. Re-sorting silently would leave an Emit in the parallel group,
// and the resulting run would finish in two different ways depending on
// scheduling luck: the hardest failure mode to ever reproduce.
func TestUnorderedListIsRefused(t *testing.T) {
	r, log, _, _ := newRunner()

	fx := []kernel.Effect{
		kernel.CallTool{Agent: "a", Tool: "read"},
		emit(kernel.StageAdvanced),
	}
	_, err := r.Run(context.Background(), fx)
	if !errors.Is(err, ErrUnorderedEffects) {
		t.Fatalf("Run accepted a list with a control effect after an independent "+
			"one (err=%v), want ErrUnorderedEffects. The runner must refuse such a "+
			"list rather than re-sort it: re-sorting hides a bug in "+
			"kernel.orderEffects whose symptom is an Emit racing inside the "+
			"parallel group, so the run ends differently depending on scheduling "+
			"luck. Remedy: keep the second loop in controlPrefixLen.", err)
	}
	if n := len(log.order()); n != 0 {
		t.Fatalf("%d events were appended from a list that was refused, want 0. A "+
			"refused list must have no side effects at all, otherwise the log "+
			"holds half of a step that was never executed and the fold produces a "+
			"state no code ever decided.", n)
	}
}

// TestControlFailureAbortsBeforeSpending: if an Emit cannot be written, the
// independent tail must not run. The tail was decided assuming the control
// effects took place, so spawning turns afterwards means paying a provider to
// act on a state that never existed.
func TestControlFailureAbortsBeforeSpending(t *testing.T) {
	r, log, fake, _ := newRunner()
	log.failAppend = errors.New("disk full")

	fx := []kernel.Effect{
		emit(kernel.StageEntered),
		kernel.SpawnTurn{Agent: "worker"},
	}
	if _, err := r.Run(context.Background(), fx); err == nil {
		t.Fatal("Run succeeded even though the control Emit could not be appended; " +
			"a control failure must abort the step")
	}

	if n := len(fake.Calls); n != 0 {
		t.Fatalf("the executor was called %d times after a control effect failed, "+
			"want 0. The independent tail was decided assuming the control effects "+
			"happened, so running it after a failed Emit means paying a provider "+
			"to act on a state that never existed. Remedy: keep the early return "+
			"inside the fx[:split] loop in Runner.Run.", n)
	}
}

// TestFailingEffectDoesNotDiscardSiblings: by the time one turn errors, the
// others have already spent real money and the tool call has already touched
// the filesystem. Dropping their events would leave the log describing a world
// that does not exist, which is the one thing the log may never do.
func TestFailingEffectDoesNotDiscardSiblings(t *testing.T) {
	r, _, fake, _ := newRunner()
	fake.BreakTools["broken"] = errors.New("connection reset")

	fx := []kernel.Effect{
		kernel.SpawnTurn{Agent: "a"},
		kernel.CallTool{Agent: "a", Tool: "broken"},
		kernel.CallTool{Agent: "a", Tool: "ok"},
	}
	res, err := r.Run(context.Background(), fx)
	if err != nil {
		t.Fatalf("Run returned a step-level error for a per-effect failure: %v. A "+
			"broken independent effect is reported in Result.Errs, not by failing "+
			"the whole step, because its siblings succeeded and their events are "+
			"real.", err)
	}

	if len(res.Errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1 (the broken tool). Errs is a slice "+
			"so a partial failure does not hide the other failures: with two of "+
			"five effects broken the operator needs both reasons, not the first "+
			"one.", len(res.Errs))
	}

	// The turn produced agent.activated + llm.response + agent.turn_done, the
	// working tool one tool.call_completed. The broken one produced nothing, by
	// design.
	if len(res.Events) != 4 {
		var got []string
		for _, e := range res.Events {
			got = append(got, string(e.Type))
		}
		t.Fatalf("got %d events (%v), want 4: the successful siblings' events must "+
			"survive a sibling's failure. They already spent money and touched the "+
			"filesystem, so discarding them leaves the log describing a world "+
			"that does not exist. Remedy: in runIndependent, append events "+
			"unconditionally before recording the error.", len(res.Events), got)
	}
}

// TestDomainFailureBecomesAnEventAndTransportFailureDoesNot pins the
// distinction that keeps the log honest: a tool that ran and refused is a fact,
// a call that never landed is not.
func TestDomainFailureBecomesAnEventAndTransportFailureDoesNot(t *testing.T) {
	r, _, fake, _ := newRunner()
	fake.FailTools["refuses"] = "exit status 1"
	fake.BreakTools["unreachable"] = errors.New("no route to host")

	res, err := r.Run(context.Background(), []kernel.Effect{
		kernel.CallTool{Agent: "a", Tool: "refuses"},
	})
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if len(res.Events) != 1 || len(res.Errs) != 0 {
		t.Fatalf("a DOMAIN failure produced %d events and %d errors, want 1 and 0. "+
			"A tool that ran and refused is something that HAPPENED, so it belongs "+
			"in the log as tool.call_completed with ok=false; reporting it only as "+
			"a Go error erases it from history and `run why` can no longer explain "+
			"why the agent stopped.", len(res.Events), len(res.Errs))
	}

	r2, _, _, _ := newRunner()
	r2.Executor = fake
	res2, err := r2.Run(context.Background(), []kernel.Effect{
		kernel.CallTool{Agent: "a", Tool: "unreachable"},
	})
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if len(res2.Events) != 0 || len(res2.Errs) != 1 {
		t.Fatalf("a TRANSPORT failure produced %d events and %d errors, want 0 and "+
			"1. When the call never landed nothing can be said about what "+
			"happened, so writing an event would put a guess into the log, and "+
			"the log is the one place that may not contain guesses.",
			len(res2.Events), len(res2.Errs))
	}
}

// TestPanicInAnEffectDoesNotLoseSiblings: a panicking provider client must not
// take down the run, and above all must not take down the events its siblings
// already produced.
func TestPanicInAnEffectDoesNotLoseSiblings(t *testing.T) {
	r, _, _, _ := newRunner()
	r.Executor = &panicExecutor{}

	res, err := r.Run(context.Background(), []kernel.Effect{
		kernel.SpawnTurn{Agent: "panics"},
		kernel.CallTool{Agent: "a", Tool: "fine"},
	})
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if len(res.Errs) != 1 {
		t.Fatalf("got %d errors, want 1: the panic must be converted into an error, "+
			"not propagated, otherwise a bug in one provider client aborts the "+
			"whole process and the sibling events already produced are never "+
			"appended. Remedy: keep the recover() in runIndependent's goroutine.",
			len(res.Errs))
	}
	if len(res.Events) != 1 {
		t.Fatalf("got %d events, want 1: the sibling that succeeded must still have "+
			"its event appended after another effect panicked.", len(res.Events))
	}
}

type panicExecutor struct{}

func (panicExecutor) SpawnTurn(ctx context.Context, e kernel.SpawnTurn) ([]kernel.Event, error) {
	panic("provider client dereferenced nil")
}

func (panicExecutor) CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error) {
	return []kernel.Event{{ID: "t1", Type: kernel.ToolCallCompleted, Actor: e.Agent}}, nil
}

func (panicExecutor) AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}

// TestSnapshotIsRefoldedFromTheLog protects ADR-0002 the practical way: the
// snapshot must reflect the state AFTER the control Emits that preceded it,
// because a cache that disagrees with the log is a wrong answer served fast.
func TestSnapshotIsRefoldedFromTheLog(t *testing.T) {
	r, log, _, _ := newRunner()

	fx := []kernel.Effect{
		kernel.Emit{Event: kernel.Event{
			ID: "e1", Type: kernel.RunStarted,
			Payload: map[string]any{"run_id": "r1", "actor": "me", "budget_usd": 1.0},
		}},
		kernel.Snapshot{AtSeq: 0}, // 0 means "everything appended so far"
	}
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	st, ok := log.snapshots[0]
	if !ok {
		t.Fatal("no snapshot was written")
	}
	if st.RunID != "r1" {
		t.Fatalf("the snapshot has RunID %q, want %q. The snapshot must be re-folded "+
			"from the log AFTER the preceding control Emits, not taken from a "+
			"state captured before them: a snapshot that disagrees with the log "+
			"is read in preference to the log, so it serves a wrong answer fast. "+
			"Remedy: keep the Log.Fold call inside runSnapshot.", st.RunID, "r1")
	}
}

// TestSnapshotFailureDoesNotFailTheRun: a snapshot is a cache (ADR-0002). A run
// that could not write one is still entirely correct, only slower to inspect.
// Aborting would invert the ADR and make an optimization mandatory.
func TestSnapshotFailureDoesNotFailTheRun(t *testing.T) {
	for name, setup := range map[string]func(*memLog){
		"fold fails":     func(m *memLog) { m.failFold = errors.New("corrupt log tail") },
		"snapshot fails": func(m *memLog) { m.failSnapshot = errors.New("read-only fs") },
	} {
		t.Run(name, func(t *testing.T) {
			r, log, _, _ := newRunner()
			setup(log)

			res, err := r.Run(context.Background(), []kernel.Effect{kernel.Snapshot{AtSeq: 1}})
			if err != nil {
				t.Fatalf("Run failed because a SNAPSHOT could not be written: %v. The "+
					"snapshot is a cache and the log is the truth (ADR-0002): the run "+
					"is still correct, only slower to inspect. Failing here inverts "+
					"the ADR and turns an optimization into a requirement, so a "+
					"read-only disk would kill runs that are otherwise fine. Remedy: "+
					"keep runSnapshot returning nothing and counting into "+
					"SnapshotSkipped.", err)
			}
			if res.SnapshotSkipped != 1 {
				t.Fatalf("SnapshotSkipped is %d, want 1. A skipped snapshot must be "+
					"REPORTED even though it is not fatal, otherwise a permanently "+
					"failing cache is invisible and `run show` stays mysteriously "+
					"slow forever with nothing to point at.", res.SnapshotSkipped)
			}
		})
	}
}

// TestCancelledContextStopsSpending: a cancelled run must stop paying, not
// finish paying for turns whose results will be thrown away.
func TestCancelledContextStopsSpending(t *testing.T) {
	r, _, fake, _ := newRunner()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := r.Run(ctx, []kernel.Effect{kernel.SpawnTurn{Agent: "a"}})
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if len(res.Errs) != 1 {
		t.Fatalf("got %d errors, want 1: a spawn on a cancelled context must fail "+
			"instead of silently succeeding.", len(res.Errs))
	}
	if len(res.Events) != 0 {
		t.Fatalf("got %d events, want 0: a turn refused for cancellation never "+
			"happened, so it must leave nothing in the log.", len(res.Events))
	}
	// The call is still RECORDED: the fake records before checking, so a test
	// can tell "was refused" from "was never attempted".
	_ = fake
}

// TestEmptyListIsANoOp. The reducer legitimately returns no effects for events
// that only change state, and that must not be an error: if it were, every
// caller would need a length check before every step and the one that forgets
// it turns a normal fold into a failed run.
func TestEmptyListIsANoOp(t *testing.T) {
	r, log, fake, _ := newRunner()

	res, err := r.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run(nil) failed: %v. An event that only changes state produces no "+
			"effects, which is normal; erroring here would force a length check at "+
			"every call site.", err)
	}
	if len(res.Events) != 0 || len(res.Errs) != 0 || len(log.order()) != 0 || len(fake.Calls) != 0 {
		t.Fatal("Run(nil) had side effects, want none")
	}
}

// TestRunnerIsRaceFree exercises the parallel path under -race. It is here
// because the concurrency in runIndependent is normal operation, not an edge
// case: every step with more than one independent effect hits it.
func TestRunnerIsRaceFree(t *testing.T) {
	r, _, _, _ := newRunner()

	var fx []kernel.Effect
	for i := 0; i < 16; i++ {
		fx = append(fx, kernel.SpawnTurn{Agent: fmt.Sprintf("agent-%02d", i)})
		fx = append(fx, kernel.CallTool{Agent: fmt.Sprintf("agent-%02d", i), Tool: "read"})
	}

	res, err := r.Run(context.Background(), fx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("unexpected effect errors: %v", err)
	}
	// 16 turns * 3 events (activated, llm.response, turn_done) + 16 tool calls
	// * 1 event.
	if want := 64; len(res.Events) != want {
		t.Fatalf("got %d events, want %d", len(res.Events), want)
	}
}

// TestEmittedEventsGetAnIdBecauseCausalityDependsOnIt closes a gap that was
// found by the author of internal/logstore while implementing against the same
// ADRs, and confirmed here by reading kernel.derived().
//
// kernel.derived() sets CausedBy and CorrelationID but leaves ID and Ts empty,
// which is correct for the reducer: it has no clock and no randomness, on
// purpose. event.go documents who owns Seq (the log) and names no owner for ID
// or Ts, so neither half filled them and the events reached the log anonymous.
//
// The consequence is not a blank field. `run why` answers "why did this happen"
// by walking caused_by backwards, and caused_by holds event IDs. An event with
// no id cannot be the target of a link, so the chain terminates at the first
// derived event: the question the tool exists to answer becomes unanswerable
// for every event the reducer itself produced.
func TestEmittedEventsGetAnIdBecauseCausalityDependsOnIt(t *testing.T) {
	r, log, _, _ := newRunner()
	r.Now = func() string { return "2026-01-01T00:00:00Z" }

	// An event as the reducer hands it over: typed and caused, but anonymous.
	fx := []kernel.Effect{kernel.Emit{Event: kernel.Event{
		Type:     kernel.StageEntered,
		CausedBy: []string{"root-1"},
	}}}

	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.events) != 1 {
		t.Fatalf("expected 1 event in the log, got %d", len(log.events))
	}

	got := log.events[0]
	if got.ID == "" {
		t.Fatalf("event %s reached the log with an empty ID.\n"+
			"Consequence: caused_by holds event IDs, so nothing can ever reference "+
			"this event and `run why` cannot walk a chain through it. Every event "+
			"the reducer derives is affected, which is most of the log.\n"+
			"Remedy: Runner.stamp must assign an ID to any event arriving without "+
			"one, and both Append call sites must route through it.", got.Type)
	}
	if got.Ts == "" {
		t.Fatalf("event %s reached the log with an empty Ts.\n"+
			"Consequence: the log cannot be read chronologically and no duration "+
			"can be computed from it, so a run's cost over time is unreportable.\n"+
			"Remedy: Runner.stamp must set Ts from the injected Now when the event "+
			"arrives without one.", got.Type)
	}
}

// TestAnUnstampedEventPoisonsItsDescendants is the reason the gap above is
// severe rather than cosmetic, and it is the part that a "field is empty" check
// does not express.
//
// kernel.derived() builds a child as CausedBy: []string{cause.ID} and inherits
// CorrelationID from the cause. So an unstamped parent does not merely lack an
// id: it hands "" down as the identity of the cause, and the child inherits a
// blank correlation too. The damage therefore spreads along the causal graph
// instead of staying on one event, and it spreads silently, because a chain of
// events all claiming to be caused by "" is structurally valid JSON that folds
// without complaint.
//
// The test asserts the fix is upstream of derivation: the parent must already
// have an id by the time anything is derived from it.
func TestAnUnstampedEventPoisonsItsDescendants(t *testing.T) {
	r, log, _, _ := newRunner()
	r.Now = func() string { return "2026-01-01T00:00:00Z" }

	parent := kernel.Event{Type: kernel.StageEntered, Scope: "run:r1"}
	if _, err := r.Run(context.Background(), []kernel.Effect{kernel.Emit{Event: parent}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stamped := log.events[0]

	// Derive from the event AS THE LOG HOLDS IT, which is how a real step works:
	// the reducer folds the log and derives from what it read there.
	child := kernel.Event{
		Type:          kernel.StageAdvanced,
		CausedBy:      []string{stamped.ID},
		CorrelationID: stamped.ID,
	}
	if _, err := r.Run(context.Background(), []kernel.Effect{kernel.Emit{Event: child}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := log.events[1]
	if len(got.CausedBy) != 1 || got.CausedBy[0] == "" {
		t.Fatalf("derived event %s has caused_by %v.\n"+
			"Consequence: an empty cause id means the causal graph collapses to a "+
			"single anonymous node that every event claims as its parent. The log "+
			"stays valid and folds cleanly, so nothing reports the damage, and "+
			"`run why` returns a chain that is plausible and wrong.\n"+
			"Remedy: stamp events on the way IN to the log, so anything derived "+
			"from them later reads a real id.", got.Type, got.CausedBy)
	}
	if got.CausedBy[0] != stamped.ID {
		t.Fatalf("derived event points at %q but its parent is %q: the chain does "+
			"not lead back to the event that caused it, so `run why` walks to the "+
			"wrong ancestor", got.CausedBy[0], stamped.ID)
	}
}

// TestExecutorSuppliedIdsAreNotOverwritten protects the other direction of the
// same fix.
//
// The Fake derives its ids from the effect's content, and a real provider
// returns ids that identify the call on the provider's side. Both are more
// specific than anything the runner could mint, and the provider's is the only
// handle support has when reconciling a bill. Stamping unconditionally would
// discard exactly the identifier worth keeping.
func TestExecutorSuppliedIdsAreNotOverwritten(t *testing.T) {
	r, log, _, _ := newRunner()
	r.Now = func() string { return "2026-01-01T00:00:00Z" }

	fx := []kernel.Effect{kernel.SpawnTurn{Agent: "writer"}}
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.events) == 0 {
		t.Fatal("the turn produced no events")
	}
	for _, e := range log.events {
		if !contains(e.ID, "sim-writer") {
			t.Fatalf("event %s has id %q, want the Fake's own id preserved.\n"+
				"Consequence: overwriting an executor-supplied id throws away the "+
				"identifier that ties the event to the provider call it records, "+
				"which is what a cost dispute is reconciled against.\n"+
				"Remedy: stamp only when ID is empty.", e.Type, e.ID)
		}
	}
}

// TestMintedIdsAreUniqueAcrossAStep guards the boring failure that would make
// the fix worse than the bug: ids that exist but repeat.
//
// A duplicate id is harder to detect than a missing one, because every
// consumer accepts it. caused_by would then resolve to two different events and
// `run why` would report a causal graph containing a fork that never happened.
func TestMintedIdsAreUniqueAcrossAStep(t *testing.T) {
	r, log, _, _ := newRunner()
	r.Now = func() string { return "2026-01-01T00:00:00Z" }

	var fx []kernel.Effect
	for i := 0; i < 5; i++ {
		fx = append(fx, kernel.Emit{Event: kernel.Event{Type: kernel.StageEntered}})
	}
	// Two steps, because the counter must survive across Run calls: a run is
	// many steps and ids must not restart at each one.
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := map[string]int{}
	for _, e := range log.events {
		seen[e.ID]++
		if seen[e.ID] > 1 {
			t.Fatalf("id %q was assigned to %d events.\n"+
				"Consequence: caused_by resolves to more than one event, so the "+
				"causal graph gains branches that never existed. A duplicate id is "+
				"worse than a missing one because every reader accepts it.\n"+
				"Remedy: the id counter must be monotonic for the life of the "+
				"runner, not per step.", e.ID, seen[e.ID])
		}
	}
	if len(seen) != 10 {
		t.Fatalf("got %d distinct ids over 10 events, want 10", len(seen))
	}
}

// TestSeedIDsPreventsCollisionsOnResume covers the case where the run did not
// start from an empty log.
//
// On resume the runner is new but the log is not. A counter starting at zero
// re-issues ids the earlier half already wrote, so caused_by stops identifying
// one event and starts identifying two — and the collision is between the two
// halves of the same run, which is where causal questions are actually asked.
func TestSeedIDsPreventsCollisionsOnResume(t *testing.T) {
	r, log, _, _ := newRunner()
	r.Now = func() string { return "2026-01-01T00:00:00Z" }

	fx := []kernel.Effect{kernel.Emit{Event: kernel.Event{Type: kernel.StageEntered}}}
	for i := 0; i < 3; i++ {
		if _, err := r.Run(context.Background(), fx); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	before := map[string]bool{}
	for _, e := range log.events {
		before[e.ID] = true
	}
	tip := log.events[len(log.events)-1].Seq

	// A fresh runner over the same log: this is resume.
	resumed := &Runner{Log: log, Clock: NewVirtualClock(), Executor: NewFake(),
		Now: func() string { return "2026-01-01T00:00:01Z" }}
	resumed.SeedIDs(tip)

	if _, err := resumed.Run(context.Background(), fx); err != nil {
		t.Fatalf("Run after resume: %v", err)
	}
	got := log.events[len(log.events)-1]
	if before[got.ID] {
		t.Fatalf("the resumed runner re-issued id %q, which the log already holds.\n"+
			"Consequence: two distinct events share an id inside one run, so any "+
			"caused_by pointing at it is ambiguous exactly where causality is "+
			"most often questioned — across a crash and a resume.\n"+
			"Remedy: call SeedIDs with the log tip before the first step of a "+
			"resumed run.", got.ID)
	}
}
