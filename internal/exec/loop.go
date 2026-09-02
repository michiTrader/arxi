package exec

import (
	"context"
	"fmt"

	"github.com/michiTrader/arxi/internal/kernel"
)

// Timekeeper is how the run loop obtains the passage of time.
//
// This interface is the ONE place where a real run and a simulation differ.
// Everything else in the loop is identical, and that is the property that makes
// `--sim` worth trusting: if the two shared no code, a simulation would be
// predicting its own behaviour rather than the run's, and would agree with `run`
// only by coincidence.
//
// It is a separate seam from Clock because the two answer different questions.
// Clock is the WRITE side, driven by the reducer's effects: arm this deadline,
// cancel that one. Timekeeper is the READ side, driven by the loop: has anything
// fired, how long until something does, make time pass. Merging them would force
// every implementation to supply both, and the Fake executor used by unit tests
// has no business knowing how to sleep.
//
// Advance is the method that separates the two worlds. A simulation JUMPS to the
// next deadline, so a thirty-minute stage timeout is exercised in microseconds; a
// real run WAITS for it. Both then read the fired ids through the same Due, and
// the reducer cannot tell which one it is talking to.
type Timekeeper interface {
	// Due drains the timers that have fired and not yet been delivered. It
	// drains rather than peeks because the loop turns each id into an event: if
	// reading did not consume, the next iteration would emit the same timeout
	// again and the log would claim one stage expired twice.
	Due() []string

	// NextDeadlineMs reports how far time must move for the next timer to fire,
	// and whether any timer is armed at all. The bool is what tells the loop the
	// difference between "nothing to do yet" and "nothing to do ever".
	NextDeadlineMs() (int64, bool)

	// Advance makes byMs milliseconds pass. A simulation returns immediately, a
	// real run blocks. It takes a context so that a cancelled run stops waiting
	// on a deadline whose outcome nobody will read.
	Advance(ctx context.Context, byMs int64) error
}

// LoopLog is the storage the loop needs, on top of what the Runner needs.
//
// Read is the addition, and ADR-0002 is the reason it is needed. The loop does
// not track the run by remembering what it just did; it re-reads the log and
// folds it. Effects executed by the Runner append their results, so the next read
// picks them up and the fold turns them into the next decision. That is what
// makes the loop, `replay` and a resumed run the same fold over the same bytes,
// rather than three procedures somebody has to keep in agreement.
type LoopLog interface {
	Log
	Read(fromSeq, toSeq int64) ([]kernel.Event, error)
	Head() int64
}

// Loop drives a run to a standstill by folding the log and executing whatever
// the reducer decides.
//
// It holds no state about the run beyond a cursor. The State is always DERIVED
// from the events read so far, never accumulated from what the executor
// reported. Those two agree when everything works, and when they disagree the
// log is right by definition.
type Loop struct {
	Runner *Runner
	Log    LoopLog
	Time   Timekeeper
	Config kernel.Config

	// Cursor is the seq of the last event whose effects were already executed.
	// The loop resumes at Cursor+1. Zero means the beginning of the log.
	//
	// This has to be given rather than inferred, and both ways of inferring it are
	// wrong in a way that costs either money or the whole run:
	//
	//   - Head() assumes every event in the log was already acted upon. That is
	//     false for a run that has only just started: run.started sits in the log,
	//     its stage.entered was never executed, so the loop finds nothing unread,
	//     declares the run idle, and it dies of silence before anybody was asked to
	//     work. It is false again for any pass that stopped mid-fold, because
	//     executing one event appends several: those appends sit above the last
	//     event the loop actually decided and would be skipped forever.
	//   - Zero assumes nothing was acted upon. That is false on resume: every turn
	//     is spawned a second time, so a provider is paid twice for work whose
	//     result is already in the log.
	//
	// It is a cache in exactly the sense ADR-0002 means: the log stays the truth,
	// and this only records how far a previous pass got. Losing it costs
	// duplicated effects, never correctness of the state.
	Cursor int64

	// MaxSteps bounds how many events the loop will fold in one call.
	//
	// This guards against a reducer bug, not against a long run. If a derived
	// event ever causes an event that causes the first one again, the loop would
	// fold forever, and because each fold is cheap it would do so silently at
	// full CPU while the log grew until the disk filled. Neither ceiling that
	// exists can catch it: --max-turns sees no turns opened and the budget sees
	// no money spent. This is the only bound that does.
	//
	// Zero means DefaultMaxSteps. It is deliberately high: reaching it is a bug
	// report, not a limit users should tune.
	MaxSteps int
}

// DefaultMaxSteps is the fold ceiling when Loop.MaxSteps is unset.
const DefaultMaxSteps = 100_000

// ErrStepLimit means the loop folded MaxSteps events without the run reaching a
// standstill. It is always a bug in the reducer or the blueprint and never
// normal operation, so it is a distinct error the caller can recognise.
var ErrStepLimit = fmt.Errorf("run loop exceeded its step limit")

// Outcome is what one Run of the loop produced.
type Outcome struct {
	// State is folded from every event the loop read. It is the answer to "what
	// happened", and it is derived, not accumulated.
	State kernel.State

	// Steps counts the events folded. Reported because it is the cheapest signal
	// that a blueprint is spinning: a run doing useful work folds tens of
	// events, one that loops folds thousands.
	Steps int

	// Errs holds every effect-level failure, in order. The loop does NOT stop on
	// these: an agent turn that failed is a fact the reducer may legitimately
	// react to (a watcher on agent.failed, a stage that advances anyway), and
	// stopping would prevent the recovery the blueprint declared.
	Errs []error

	// StoppedBy names the condition that ended the loop, for `run show` and for
	// tests. See the stop constants.
	StoppedBy string

	// Cursor is how far this pass got: the seq of the last event whose effects
	// were executed. Handing it to the Cursor field of a later Loop continues the
	// run without re-executing anything, which is what lets an interrupted run be
	// resumed without paying a provider twice. See Loop.Cursor.
	Cursor int64

	// SnapshotSkipped counts the snapshots that could not be written.
	//
	// It is carried up from the Runner rather than discarded, and the reason is
	// the same one that makes a skipped snapshot non-fatal in the first place. A
	// snapshot is a cache (ADR-0002), so failing to write one must not stop the
	// run -- but "must not stop the run" is not the same as "nobody needs to
	// know". The Runner counts these deliberately; swallowing the count here
	// means a read-only disk or a corrupt tail degrades every run silently, and
	// `run show` and `run why` just get slower and slower with nothing anywhere
	// to point at. An optimization that has stopped working is invisible exactly
	// because it was optional.
	SnapshotSkipped int
}

// The reasons a loop stops. They are distinct because they call for different
// responses: Terminal is a finished run, Idle is a run waiting on the outside
// world, and Cancelled is the caller giving up.
const (
	StopTerminal  = "terminal"  // the run reached a terminal status
	StopIdle      = "idle"      // no unread events and no armed timer
	StopCancelled = "cancelled" // the context was cancelled
)

// Run folds the log forward, executing effects, until the run reaches a
// standstill.
//
// THE SHAPE OF THE LOOP, and why it is a fold rather than a work queue:
//
//	read the events after the cursor
//	  none? deliver due timers; still none? advance to the next deadline;
//	        no deadline either? the run is idle, stop
//	  for each event: state, effects = Decide(state, event, config)
//	                  the runner executes them, which appends MORE events
//	  repeat
//
// The Runner's appends are what feed the next read, so this function contains no
// queue and has nothing to keep in sync. An Emit becomes an event that is folded
// exactly like an event from a provider, which is why a derived stage.entered
// drives the run identically whether the reducer produced it or a human typed
// `arxi run steer`.
//
// The cursor advances only after an event is folded, and the state is carried
// across iterations rather than re-folded from seq 1 each time. Re-folding would
// be equally correct and quadratic; carrying it is safe because events are read
// in seq order and folded exactly once, which is the same sequence kernel.Fold
// performs.
func (l *Loop) Run(ctx context.Context) (Outcome, error) {
	var out Outcome

	limit := l.MaxSteps
	if limit <= 0 {
		limit = DefaultMaxSteps
	}

	// The state is folded from the log that already exists, not assumed empty.
	// That is what makes resume work: a run interrupted mid-stage comes back with
	// its members, its spend and its armed stage exactly as the log describes
	// them, because the log is the only thing that describes them.
	// It is folded up to the CURSOR and not to the log tip, and the two differ in
	// the case that matters. Executing one event appends several, so a pass that
	// stopped mid-fold left events above its cursor that were never decided.
	// Folding to the tip would absorb those into the starting state, and the loop
	// would then never read them: the effects they call for — the turns, the
	// timers, the stage advance — are lost while the state claims they happened.
	// Folding to the cursor leaves them unread, which is exactly what they are.
	//
	// Fold's untilSeq treats 0 as "the whole log", so a zero cursor cannot be
	// passed through: it would fold everything and mean the precise opposite of
	// the nothing-consumed-yet that it denotes here. The empty state is the honest
	// starting point, and it is what makes a fresh run correct, because run.started
	// is then read and decided rather than silently absorbed.
	cursor := l.Cursor
	if cursor > 0 {
		state, err := l.Log.Fold(l.Config, cursor)
		if err != nil {
			return out, fmt.Errorf("run loop: fold the log up to seq %d: %w", cursor, err)
		}
		out.State = state
	}
	out.Cursor = cursor

	// The Runner mints ids for events that arrive without one. Seeding it past
	// the log tip is what stops a resumed run from re-issuing ids the first half
	// already used, which would make caused_by identify two events instead of
	// one and put forks in the causal graph that never happened.
	//
	// The seed is the TIP and not the cursor. Those differ precisely when a pass
	// stopped mid-fold, and the tip is the safe one: ids were already minted for
	// events written above the cursor, so seeding from the cursor would hand the
	// next event an id that is already in the log.
	l.Runner.SeedIDs(l.Log.Head())

	for {
		if err := ctx.Err(); err != nil {
			out.StoppedBy = StopCancelled
			return out, nil
		}
		if out.State.Status.Terminal() {
			out.StoppedBy = StopTerminal
			return out, nil
		}

		events, err := l.Log.Read(cursor+1, 0)
		if err != nil {
			return out, fmt.Errorf("run loop: read the log after seq %d: %w", cursor, err)
		}

		if len(events) == 0 {
			// Nothing new to fold. Before concluding the run is idle, give the
			// clock a chance: a stage waiting on its timeout has no unread
			// events and is very much not finished.
			moved, err := l.tick(ctx)
			if err != nil {
				return out, err
			}
			if moved {
				continue
			}
			out.StoppedBy = StopIdle
			return out, nil
		}

		for _, e := range events {
			if out.Steps >= limit {
				return out, fmt.Errorf("%w (%d events): the reducer keeps deriving "+
					"events without the run reaching a standstill, which is a cycle in "+
					"the causal graph rather than a long run. Neither --max-turns nor "+
					"the budget can catch it, because it opens no turns and spends no "+
					"money. Look at the last events of run %q for a pair that cause "+
					"each other",
					ErrStepLimit, out.Steps, out.State.RunID)
			}

			var fx []kernel.Effect
			out.State, fx = kernel.Decide(out.State, e, l.Config)
			cursor = e.Seq
			out.Steps++

			res, err := l.Runner.Run(ctx, fx)
			out.Errs = append(out.Errs, res.Errs...)
			out.SnapshotSkipped += res.SnapshotSkipped
			if err != nil {
				// A step-level error is not an effect that failed, it is a step
				// that could not be carried out: the log refused a write, or the
				// effect list arrived unordered. Continuing would fold the next
				// event against a world the log does not describe.
				return out, fmt.Errorf("run loop: execute the effects of %s (seq %d): %w",
					e.Type, e.Seq, err)
			}

			// The cursor advances only once the effects of this event have been
			// carried out, so it never claims more progress than was made. The
			// failure above returns with the cursor still on the previous event,
			// which makes a resumed run re-execute this one.
			//
			// That direction is the deliberate one. A cursor that lags re-executes
			// an event's effects; a cursor that leads skips them. Re-spawning a turn
			// costs money, and skipping the turn a stage waits on costs the whole
			// run: it waits forever for a result nobody will produce.
			out.Cursor = cursor
		}
	}
}

// tick delivers fired timers as events, advancing time if nothing has fired yet.
//
// It reports whether it made progress. False means no timer is armed at all,
// which — together with no unread events — is the definition of an idle run.
//
// Timers become timer.tick events and NOT stage.timeout events, even though a
// stage timeout is what they almost always mean. The translation belongs to the
// reducer (kernel.applyTimerTick): a tick is a fact about the clock, and deciding
// what it means for the run is a decision. If this function emitted stage.timeout
// directly, `run`, `--sim` and resume would each carry a copy of that mapping,
// and `replay` — which appends nothing — could not agree with any of them.
func (l *Loop) tick(ctx context.Context) (bool, error) {
	if fired := l.Time.Due(); len(fired) > 0 {
		return true, l.appendTicks(fired)
	}

	delta, armed := l.Time.NextDeadlineMs()
	if !armed {
		return false, nil
	}
	if err := l.Time.Advance(ctx, delta); err != nil {
		return false, fmt.Errorf("run loop: advance time by %d ms: %w", delta, err)
	}

	// Advancing does not deliver anything by itself: Due is what drains. If
	// nothing fired even after advancing to its own deadline, reporting progress
	// would spin the loop forever, so the honest answer is that nothing moved.
	// This is also the path a cancelled Sleep takes, and returning false lets
	// the caller reach its ctx.Err() check instead of looping on a dead clock.
	fired := l.Time.Due()
	if len(fired) == 0 {
		return false, nil
	}
	return true, l.appendTicks(fired)
}

// appendTicks writes one timer.tick per fired id.
//
// Source is SourceRuntime because the clock IS part of the runtime, and that
// classification has a consequence the reducer relies on: runtime events do not
// re-trigger watchers (see the e.Source check in kernel.Decide). A tick marked
// SourceAgent would let a watcher on `*` react to the mere passage of time,
// which is an infinite loop with a credit card attached.
//
// Seq is left 0 and ID empty: the log assigns the sequence number (ADR-0002) and
// the Runner mints the id. Setting either here would put two writers in charge of
// one field.
//
// A tick is a ROOT of its causal thread: no caused_by, and depth 0, which is why
// this is the one caller of stamp that has no effect to attribute. The tick's
// SetTimer is long gone -- it ran in an earlier step, and the clock that delivers
// the id does not remember who armed it. Reconstructing the link would mean a
// timer-to-cause map in the Runner, and that map is not in the log: a resumed run
// would rebuild it empty and write uncaused ticks where a fresh run wrote caused
// ones, so `run`, `--sim`, resume and replay would stop being one fold over the
// same bytes. A tick being a root is also true to what it is: time passing is not
// an event's consequence. See the note on kernel.SetTimer.
func (l *Loop) appendTicks(fired []string) error {
	events := make([]kernel.Event, 0, len(fired))
	for _, id := range fired {
		events = append(events, kernel.Event{
			Type:    kernel.TimerTick,
			Scope:   "run:" + l.Config.Blueprint,
			Source:  kernel.SourceRuntime,
			Payload: map[string]any{"timer_id": id},
		})
	}
	if _, err := l.Log.Append(l.Runner.stamp(events)); err != nil {
		return fmt.Errorf("run loop: append %d timer tick(s): %w", len(events), err)
	}
	return nil
}

// VirtualTime adapts VirtualClock to Timekeeper: time jumps.
//
// This is the simulation half. Advance moves the clock instantly, so a stage with
// a thirty-minute timeout is exercised in microseconds. That is not a
// convenience: nobody keeps a suite that takes half an hour, so without it the
// timeout paths would never run until a real run hit them at 3am.
type VirtualTime struct{ C *VirtualClock }

func (v VirtualTime) Due() []string { return v.C.TakeFired() }

func (v VirtualTime) NextDeadlineMs() (int64, bool) { return v.C.NextDeadlineMs() }

func (v VirtualTime) Advance(_ context.Context, byMs int64) error {
	// The ids Advance returns are discarded here on purpose: VirtualClock queues
	// them internally and Due drains that queue. Delivering from two places is
	// how one timer gets reported twice.
	_, err := v.C.Advance(byMs)
	return err
}

// RealTime adapts RealClock to Timekeeper: time is waited for.
//
// This is the production half, and the only object in the run path that blocks.
// Concentrating the wait in one adapter is what lets the reducer, the runner and
// the loop all be tested without a sleep anywhere.
type RealTime struct{ C *RealClock }

func (r RealTime) Due() []string {
	// Both calls are needed and they do different things: Due moves newly
	// expired deadlines into the fired queue, TakeFired hands that queue over.
	r.C.Due()
	return r.C.TakeFired()
}

func (r RealTime) NextDeadlineMs() (int64, bool) { return r.C.NextDeadlineMs() }

func (r RealTime) Advance(ctx context.Context, byMs int64) error {
	return r.C.Sleep(ctx, byMs)
}
