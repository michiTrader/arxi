// Package exec runs the effects that the reducer described.
//
// The split of responsibility with internal/kernel is the whole point of the
// architecture: kernel decides and returns []Effect without touching the world,
// exec touches the world and returns events. That is what makes `run`, `--sim`
// and `replay` three configurations of one code path instead of three
// implementations that drift apart:
//
//	run     = kernel.Fold + real Executor    + real Log
//	--sim   = kernel.Fold + Fake Executor    + real Log + VirtualClock
//	replay  = kernel.Fold + no Executor at all
//
// If this package ever starts making decisions (choosing which agent to wake,
// deciding a stage advanced), that equivalence dies: `--sim` would stop
// predicting `run`, and the simulation would become a lie that is worse than
// having no simulation.
package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/michiTrader/arxi/internal/kernel"
)

// Log is the slice of the event log that the runner needs, declared here rather
// than imported from the storage package.
//
// The direction of this dependency is deliberate: exec declares what it needs
// and storage happens to satisfy it. The alternative (exec imports the concrete
// store) makes it impossible to test the runner without touching a filesystem,
// and would let storage details leak into the run loop.
//
// The method set matches internal/logstore's contract exactly, so the concrete
// store satisfies this interface with no adapter. That is a coordination
// constraint, not a coincidence: if the two drift, the compiler says so at the
// wiring site instead of at runtime.
type Log interface {
	// Append writes events and returns them with Seq assigned. The reducer
	// leaves Seq at 0 because it is not the single writer (see event.go); the
	// runner does not assign it either, for the same reason. Only the log does.
	Append(events []kernel.Event) ([]kernel.Event, error)

	// Fold rebuilds the state up to untilSeq. The runner needs this to honour
	// Snapshot, see the comment in runSnapshot.
	Fold(c kernel.Config, untilSeq int64) (kernel.State, error)

	// WriteSnapshot stores a materialized state. Its failure is not fatal, see
	// runSnapshot.
	WriteSnapshot(st kernel.State, atSeq int64) error
}

// Clock is time, injected instead of taken from the package `time`.
//
// SetTimer carries a relative offset in milliseconds and not an absolute
// instant precisely so that this interface can be implemented by a virtual
// clock: a test of a stage with a 30-minute timeout has to run in
// microseconds, and it has to exercise the same reducer path that the real run
// exercises. A `time.Sleep` hidden inside the runner would make that
// impossible and would push everyone to test timeouts by not testing them.
type Clock interface {
	// SetTimer arms a timer that fires afterMs milliseconds from now.
	SetTimer(id string, afterMs int64) error
	// CancelTimer disarms a timer. Cancelling a timer that does not exist is
	// NOT an error: the reducer legitimately cancels defensively (a stage that
	// advanced before its timeout), and turning that into a failure would make
	// correct reducer code look broken.
	CancelTimer(id string) error
}

// Executor performs the effects that cost money or block on a human.
//
// One method per variant, rather than a single Do(Effect), and that choice is
// the same argument as ADR-0007: with three methods, an implementation that
// forgets one does not compile, and adding a fourth independent variant breaks
// every implementation loudly at build time. A single Do(Effect) with a type
// switch inside moves that check to runtime, where it shows up as an effect
// that silently does nothing.
//
// Every method may return events AND an error at the same time, and that is
// not sloppiness:
//
//   - A DOMAIN failure (the tool exited non-zero, the provider refused the
//     prompt) must come back as an event, because it happened and the log is
//     the truth. Returning only an error would erase it from history.
//   - A returned error means the failure could not be turned into a fact:
//     the transport broke, the context was cancelled. That is a different and
//     worse thing, and the runner surfaces it instead of swallowing it.
type Executor interface {
	SpawnTurn(ctx context.Context, e kernel.SpawnTurn) ([]kernel.Event, error)
	CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error)
	AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error)
}

// ErrUnorderedEffects means the effect list arrived with a control effect after
// an independent one.
//
// The runner refuses to execute such a list instead of quietly re-sorting it.
// Re-sorting would hide a bug in the reducer, and this particular bug is the
// expensive kind: if the list is [CallTool, Emit] and the runner split it at
// the first independent effect, the Emit would run in the parallel group and
// race the tool call that was supposed to observe it. The run would then finish
// in two different ways depending on scheduling luck, which is the failure mode
// that is hardest to ever reproduce.
var ErrUnorderedEffects = errors.New("effects are not ordered: control after independent")

// Result is what one step of the runner produced.
type Result struct {
	// Events are the events appended to the log, with Seq assigned, in the
	// order they were appended.
	Events []kernel.Event

	// Errs holds every error raised by an effect. It is a slice and not a
	// single error because a partial failure must not hide the other failures:
	// if two of five turns broke, the operator needs both reasons, not the
	// first one.
	Errs []error

	// SnapshotSkipped counts snapshots that could not be written. It is
	// reported rather than raised, see runSnapshot.
	SnapshotSkipped int
}

// Err collapses Errs into one error, or nil if there were none. Callers that
// only want to know "did anything break" use this; callers that want to report
// every cause read Errs.
func (r Result) Err() error {
	if len(r.Errs) == 0 {
		return nil
	}
	return errors.Join(r.Errs...)
}

// Runner executes effect lists. It holds the Config because Snapshot needs to
// re-fold the log, see runSnapshot.
type Runner struct {
	Log      Log
	Clock    Clock
	Executor Executor
	Config   kernel.Config

	// Now supplies the timestamp stamped onto events that arrive without one.
	//
	// Injected rather than read from the package `time` for the same reason
	// Clock is injected, and with a sharper consequence: a wall-clock timestamp
	// inside a --sim run makes two simulations of identical input differ on
	// every line, which is precisely what `run diff` cannot tolerate. The sim
	// wiring passes a virtual-clock-backed function; nil falls back to leaving
	// Ts empty rather than reaching for wall time behind the caller's back.
	Now func() string

	// idSeq mints ids for events that arrive without one. See stamp.
	idSeq int64
}

// SeedIDs tells the runner how far the log already goes, so the ids it mints
// cannot collide with ids already written.
//
// This matters on resume. Without it the counter restarts at zero and the
// second half of a resumed run re-issues the ids the first half already used,
// so caused_by stops identifying one event and starts identifying two. The
// causal graph `run why` walks would then contain forks that never happened.
//
// Seeding from the log tip is sufficient, not merely heuristic: every minted id
// is attached to an event that is then appended and consumes exactly one
// sequence number, so the count of ids ever minted is at most the tip. Starting
// above the tip therefore starts above every id in the log.
func (r *Runner) SeedIDs(fromSeq int64) {
	if fromSeq > r.idSeq {
		r.idSeq = fromSeq
	}
}

// stamp fills in the identity fields that the reducer deliberately leaves
// empty, and it is the missing half of a contract that was documented only
// partially.
//
// event.go names the owner of Seq (the log, not the reducer) but names no owner
// for ID and Ts, and kernel.derived() sets neither. The reducer is right not to:
// it has no clock and no randomness, by design. But nobody downstream was
// filling them either, and the result was not a cosmetic blank field. derived()
// writes CausedBy: []string{cause.ID}, so an unstamped derived event becomes the
// cause of the next one with an empty id, and CorrelationID inherits the same
// blank. Every causal chain collapsed to "" at its first derived event — and
// walking that chain backwards is the entire reason `run why` exists.
//
// A non-empty ID or Ts is never overwritten. Events arriving from the Executor
// already carry ids that mean something (the Fake derives them from the effect,
// a real provider from its own response), and replacing those would throw away
// the more specific identifier for a generic one.
//
// Stamping happens only on the runner's own goroutine: control effects are
// sequential by ADR-0003, and the results of independent effects are appended
// in the sequential tail of runIndependent, never inside the worker goroutines.
// That is what makes the unguarded counter safe, and it is also why it is
// deterministic: the mint order follows list order, not completion order.
func (r *Runner) stamp(events []kernel.Event) []kernel.Event {
	for i := range events {
		if events[i].ID == "" {
			r.idSeq++
			events[i].ID = fmt.Sprintf("ev-%06d", r.idSeq)
		}
		if events[i].Ts == "" && r.Now != nil {
			events[i].Ts = r.Now()
		}
	}
	return events
}

// Run executes one effect list under the rule from ADR-0003: the control
// prefix sequentially and in exact list order, then the independent tail in
// parallel.
//
// The list is expected to arrive already ordered, because kernel.Decide sorts
// it with orderEffects before returning. Run verifies that expectation instead
// of assuming it; see ErrUnorderedEffects for what assuming it would cost.
func (r *Runner) Run(ctx context.Context, fx []kernel.Effect) (Result, error) {
	var res Result

	split, err := controlPrefixLen(fx)
	if err != nil {
		return res, err
	}

	// Control effects, one at a time. Not batched, not overlapped: an Emit is
	// classified as control precisely because the effects after it must be
	// able to observe the event it wrote, and "observe" means "already in the
	// log". Batching consecutive Emits would in fact be safe, but the saving is
	// one write per step and the cost is a reader having to prove that the
	// batching boundary is always in a safe place. Not worth it.
	for _, e := range fx[:split] {
		if err := r.runControl(ctx, e, &res); err != nil {
			// A control effect that fails aborts the step, and the independent
			// tail never runs. That is intentional: the tail was decided under
			// the assumption that the control effects took place, so spawning
			// turns after a failed Emit means paying a provider to act on a
			// state that never existed.
			return res, err
		}
	}

	r.runIndependent(ctx, fx[split:], &res)
	return res, nil
}

// controlPrefixLen returns the length of the leading run of control effects,
// and fails if any control effect appears after an independent one.
func controlPrefixLen(fx []kernel.Effect) (int, error) {
	split := 0
	for split < len(fx) && fx[split].Class() == kernel.ClassControl {
		split++
	}
	for i := split; i < len(fx); i++ {
		if fx[i].Class() == kernel.ClassControl {
			return 0, fmt.Errorf("%w: effect %d of %d is control but sits after an "+
				"independent one; kernel.Decide must return orderEffects(fx), and "+
				"running this list as-is would put an Emit in the parallel group",
				ErrUnorderedEffects, i, len(fx))
		}
	}
	return split, nil
}

// runControl performs a single control effect.
//
// The default branch is the exhaustiveness guard that Go does not give us. If
// someone adds a control variant to kernel and forgets this switch, the effect
// would otherwise be silently dropped: the run would look healthy and simply
// not do what the reducer decided. See TestEveryVariantIsDispatched.
func (r *Runner) runControl(ctx context.Context, e kernel.Effect, res *Result) error {
	switch v := e.(type) {
	case kernel.Emit:
		written, err := r.Log.Append(r.stamp([]kernel.Event{v.Event}))
		if err != nil {
			return fmt.Errorf("append emitted %s: %w", v.Event.Type, err)
		}
		res.Events = append(res.Events, written...)
		return nil

	case kernel.SetTimer:
		if err := r.Clock.SetTimer(v.ID, v.FiresAtMs); err != nil {
			return fmt.Errorf("set timer %s: %w", v.ID, err)
		}
		return nil

	case kernel.CancelTimer:
		if err := r.Clock.CancelTimer(v.ID); err != nil {
			return fmt.Errorf("cancel timer %s: %w", v.ID, err)
		}
		return nil

	case kernel.Snapshot:
		r.runSnapshot(v, res)
		return nil

	default:
		return fmt.Errorf("unhandled control effect %T: it was added to "+
			"kernel.Effect but not to exec.runControl, so the reducer's decision "+
			"would be dropped without a trace; add a case here", e)
	}
}

// runSnapshot writes a snapshot, and never fails the step when it cannot.
//
// Two decisions live here, and both come straight out of ADR-0002 (the log is
// the truth, snapshots are a cache):
//
// First, the runner does NOT snapshot a state it was handed. By the time a
// Snapshot effect runs, the control Emits before it have already changed the
// state; writing the pre-Emit state would produce a cache that disagrees with
// the log. And because a disagreeing cache is read in preference to the log,
// that would be a wrong answer served fast. So the state is re-folded from the
// log, which is by definition consistent with it.
//
// Second, a snapshot that cannot be written is not a failed run. The run is
// still entirely correct, it is only slower to inspect. Aborting because a
// cache write failed would invert ADR-0002 and make an optimization mandatory.
func (r *Runner) runSnapshot(v kernel.Snapshot, res *Result) {
	st, err := r.Log.Fold(r.Config, v.AtSeq)
	if err != nil {
		res.SnapshotSkipped++
		return
	}
	if err := r.Log.WriteSnapshot(st, v.AtSeq); err != nil {
		res.SnapshotSkipped++
	}
}

// runIndependent executes the tail concurrently and then appends the results in
// LIST order, not in completion order.
//
// This is the decision that keeps --sim worth having. Replay reads the log, so
// whatever order landed there is the truth and replay stays faithful either
// way. But if the append order followed whichever provider answered first, then
// two --sim runs over identical input would produce different logs, and the
// simulation could no longer be used to predict or to diff. Indexing the
// results costs one slice and buys determinism.
//
// The other decision is that a failing effect does not discard its siblings.
// By the time one SpawnTurn errors, the other two have already spent real money
// and the tool call has already touched the filesystem. Dropping their events
// would leave the log describing a world that does not exist, which is the one
// thing the log is not allowed to do.
//
// A failing SpawnTurn does, however, owe the log one event of its own. See
// turnFailure: the reducer records a commissioned turn on the member, and only
// the log can close it.
func (r *Runner) runIndependent(ctx context.Context, fx []kernel.Effect, res *Result) {
	if len(fx) == 0 {
		return
	}

	type outcome struct {
		events []kernel.Event
		err    error
	}
	outcomes := make([]outcome, len(fx))

	var wg sync.WaitGroup
	for i, e := range fx {
		wg.Add(1)
		go func(i int, e kernel.Effect) {
			defer wg.Done()
			// A panicking provider client must not take the whole run down and,
			// worse, must not take down the events its siblings already
			// produced. Converting the panic into an error keeps the partial
			// results appendable.
			defer func() {
				if p := recover(); p != nil {
					outcomes[i].err = fmt.Errorf("effect %T panicked: %v", e, p)
				}
			}()
			outcomes[i].events, outcomes[i].err = r.dispatch(ctx, e)
		}(i, e)
	}
	wg.Wait()

	for i := range outcomes {
		// Events first, unconditionally: an effect that reports both a domain
		// event and an error produced both, and the event is the part that
		// belongs in history.
		if len(outcomes[i].events) > 0 {
			ev := attribute(fx[i], outcomes[i].events)
			written, err := r.Log.Append(r.stamp(ev))
			if err != nil {
				res.Errs = append(res.Errs, fmt.Errorf("append result of %T: %w", fx[i], err))
			} else {
				res.Events = append(res.Events, written...)
			}
		}
		if outcomes[i].err != nil {
			if ev := turnFailure(fx[i], outcomes[i].events, outcomes[i].err); ev != nil {
				written, err := r.Log.Append(r.stamp(attribute(fx[i], []kernel.Event{*ev})))
				if err != nil {
					res.Errs = append(res.Errs, fmt.Errorf("append the failure of %T: %w", fx[i], err))
				} else {
					res.Events = append(res.Events, written...)
				}
			}
			res.Errs = append(res.Errs, outcomes[i].err)
		}
	}
}

// turnFailure is the agent.failed event a commissioned turn owes the log when it
// could not be delivered, or nil when the effect owes nothing.
//
// It exists because the reducer now records a commissioned turn on the member
// (kernel.Member.TurnOpen, set by spawnFor) and clears it on agent.turn_done. A
// SpawnTurn that returns an error without one leaves that marker set for the rest
// of the run, so Busy() stays true, quiescence can never fire again and a genuine
// stall later on goes unreported. Result.Errs cannot close it: Errs belongs to one
// step and dies with the process, while the state is rebuilt by folding the log,
// so anything the state must survive has to BE in the log. agent.failed was
// already declared, catalogued (spec/events.md) and reduced to MemberFailed
// (internal/kernel/decide.go) with nothing emitting it; this is the emitter
// docs/design/10-execution.md said it was waiting for.
//
// The condition is "no agent.turn_done among the events it did produce", not "no
// events at all". A turn that wrote agent.activated and then died is the same
// disease -- a member the reducer believes is working, with a turn that can never
// close -- and a refusal that legitimately returns llm.response{ok:false} plus
// turn_done has already closed its own turn and must NOT be marked failed, since
// MemberFailed takes the member out of every later stage.
//
// Only SpawnTurn. A CallTool or AskHuman that fails strands nothing in the state:
// the member is left where it was, and the reducer's own timers and blocks still
// apply. Inventing an event for those would be writing a guess into the log,
// which is the rule this stays inside rather than an exception to it -- the fact
// recorded here is that a turn this run commissioned did not happen, not any
// claim about what the agent did.
func turnFailure(fx kernel.Effect, produced []kernel.Event, cause error) *kernel.Event {
	st, ok := fx.(kernel.SpawnTurn)
	if !ok {
		return nil
	}
	for _, e := range produced {
		if e.Type == kernel.AgentTurnDone {
			return nil
		}
	}
	return &kernel.Event{
		Type:   kernel.AgentFailed,
		Source: kernel.SourceRuntime,
		Actor:  st.Agent,
		// The payload key is `error`, which is what the catalogue promises and what
		// the reducer reads into Member.Detail -- so this text is what `run show`
		// and `run why` put in front of the operator. The provider's message is
		// carried verbatim: "503 from the provider" is the whole diagnosis, and a
		// summary of it would send somebody to the wrong page.
		Payload: map[string]any{"agent": st.Agent, "error": cause.Error()},
	}
}

// attribute copies the effect's provenance onto the events it produced.
//
// Here and not inside the Executor, because otherwise every implementation of
// the interface has to remember: the Fake, the live provider one, and whichever
// comes next. Copying three fields is not a decision an executor should get to
// make differently, and the one that forgot would produce a log that traces
// correctly under --sim and not in production -- the worst place for the
// difference to live.
//
// Here and not inside stamp for the opposite reason: stamp fills identity, is
// called with a bare []kernel.Event from three places, and one of them
// (Loop.appendTicks) has no effect at all. Provenance needs the effect, and this
// is the last point where it is still in hand.
//
// All the events of one effect get the SAME cause, flat -- not activated causing
// llm.response causing turn_done. One effect is one causal step. Chaining them
// would read prettier in `event trace` and would triple the depth every turn
// adds, and since wakeWatchers stops at MaxDepth, cascades a blueprint asked for
// would start dying two generations early for a reason nothing prints.
func attribute(fx kernel.Effect, events []kernel.Event) []kernel.Event {
	cause := fx.Provenance()
	for i := range events {
		cause.Apply(&events[i])
	}
	return events
}

// dispatch routes one independent effect to the Executor.
//
// Same reasoning as runControl's default branch: a new independent variant that
// nobody wired here must fail loudly, because the alternative is an agent turn
// that the reducer decided and that never happens.
func (r *Runner) dispatch(ctx context.Context, e kernel.Effect) ([]kernel.Event, error) {
	switch v := e.(type) {
	case kernel.SpawnTurn:
		return r.Executor.SpawnTurn(ctx, v)
	case kernel.CallTool:
		return r.Executor.CallTool(ctx, v)
	case kernel.AskHuman:
		return r.Executor.AskHuman(ctx, v)
	default:
		return nil, fmt.Errorf("unhandled independent effect %T: it was added to "+
			"kernel.Effect but not to exec.dispatch, so the reducer's decision "+
			"would never reach the executor; add a case here and a method to "+
			"the Executor interface", e)
	}
}
