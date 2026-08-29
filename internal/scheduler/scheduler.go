// Package scheduler is the half of triggers that owns a clock.
//
// internal/trigger answers two questions purely: Due says whether a slot has
// arrived and how many runs are owed, Admit says what to do about that given
// what is already running. Neither can see a clock, a disk or a process. This
// package is what hands them those answers and acts on the result, and it is a
// separate package for the reason the arch test enforces: internal/trigger may
// not import os, so the loop cannot live there even if it wanted to.
//
// # Tick takes `now` as a parameter
//
// That is the whole testing strategy. Everything interesting about a scheduler
// happens on a timescale nobody wants in a test suite -- a nightly audit that
// was down for four days, a trigger that fires once a month, an execution that
// outlives three of its own slots. Tick(now) makes all of those a function
// call, so the only thing that ever needs a real clock is Run, which is twenty
// lines and does nothing but call Tick.
//
// # The tick interval is a latency knob, not a correctness one
//
// This surprised me and it is worth writing down, because it is what makes the
// loop simple. Dueness is derived from LastFiredAt (ADR-0002), so a slot that
// is not acted on stays due; ticking late does not LOSE a firing, it delays it,
// and Missed reports the gap either way. A scheduler that oversleeps by an hour
// runs the trigger an hour late and says so. One that oversleeps by a week
// reports six missed firings and applies --on-missed to them.
//
// So there is no drift correction, no catch-up loop, no attempt to align ticks
// to slot boundaries. Those exist in schedulers whose queue lives in memory,
// where a missed tick is a lost job. Here the store is the queue.
//
// # One bad trigger does not stop the others
//
// Every per-record failure is collected and reported, and the loop continues.
// A single hand-edited file with a broken cron expression must not stop the
// other eleven triggers from firing -- that turns a typo in one schedule into a
// silent outage of all automation, which is exactly the failure this system
// exists to prevent.
package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/michiTrader/iash/internal/trigger"
)

// Store is the part of trigstore this package needs.
//
// Declared here as an interface rather than taking *trigstore.Store, so a test
// can supply a store that fails on Save -- which is the case that matters most
// and is nearly impossible to arrange with a real directory. A scheduler that
// starts a run and then cannot record it will start the same run again on the
// next tick, forever, and that behaviour deserves a test rather than a comment.
type Store interface {
	List() ([]trigger.Record, error)
	Save(trigger.Record) error
}

// Runner starts the work a trigger names. One method, because starting is the
// only thing the scheduler asks for; cancelling belongs to the Execution that
// was started, which is the thing that knows what it is.
type Runner interface {
	// Start begins one execution and returns without waiting for it.
	//
	// It is handed the whole record, not just the action, because what a
	// trigger is allowed to spend (Budget, BudgetPeriod) travels with it and a
	// runner that only saw the command line would have to be told separately.
	Start(r trigger.Record, a trigger.Action) (Execution, error)
}

// Execution is one in-flight piece of work.
type Execution interface {
	// Done is closed when the execution finishes, however it finishes.
	//
	// A channel and not a Finished() bool, because the scheduler must be able
	// to check without blocking (it polls on every tick) AND a future Run
	// could select on it to wake early. A bool would allow only the first.
	Done() <-chan struct{}

	// Cancel stops the execution. It must be safe to call more than once and
	// safe to call on something that has already finished: the scheduler
	// cancels from a snapshot of what was running, and the race between
	// "decided to cancel" and "finished on its own" is unavoidable and normal.
	Cancel()
}

// Scheduler fires triggers. It is not safe for concurrent use; Run owns it.
type Scheduler struct {
	store  Store
	runner Runner

	// inflight is what is currently running, per trigger name.
	//
	// This is the ONLY state the scheduler keeps in memory, and it is
	// deliberately the only thing that cannot be recovered from disk: a
	// restart loses track of executions started by the previous process. That
	// is honest rather than ideal -- those processes died with their parent --
	// and the alternative, writing pids to a file, invents a second source of
	// truth that goes stale the moment a machine is power-cycled.
	inflight map[string][]Execution

	// Now is the reporting hook. nil is fine and means silence.
	//
	// A func rather than an io.Writer or a *log.Logger, so tests can assert on
	// structured facts instead of parsing sentences, and so the CLI can print
	// whatever shape it likes without this package having an opinion.
	observe func(Report)
}

// Report is one thing the scheduler did, or declined to do, to one trigger.
//
// Emitted for DECLINED firings too, which is the point. A scheduler that only
// reports what it started is a scheduler whose logs are empty in exactly the
// situation an operator is trying to debug: the trigger that is not running.
type Report struct {
	Trigger string
	At      time.Time
	Started int
	Cancel  bool
	Consume bool
	Missed  int
	Why     string
	Err     error
}

// New builds a scheduler.
func New(store Store, runner Runner, observe func(Report)) (*Scheduler, error) {
	if store == nil {
		return nil, errors.New("scheduler: no store, so there would be nothing to schedule")
	}
	if runner == nil {
		// Refused rather than defaulted to a no-op. A scheduler with no runner
		// ticks, decides correctly, records every firing as done and executes
		// nothing -- and because it records them, the missed-firing report
		// stays at zero and `trigger list` looks perfectly healthy.
		return nil, errors.New("scheduler: no runner, so triggers would be marked " +
			"as fired without anything running")
	}
	return &Scheduler{
		store:    store,
		runner:   runner,
		inflight: map[string][]Execution{},
		observe:  observe,
	}, nil
}

// Tick makes one pass over every trigger.
//
// The returned error is a listing failure -- something that stopped the tick
// from happening at all. Failures affecting individual triggers are reported
// through observe and do not stop the pass, because they must not.
func (s *Scheduler) Tick(now time.Time) error {
	now = now.UTC()

	// Reaping first, before anything is decided. If it ran afterwards, every
	// overlap decision on this tick would be made against a count that
	// includes executions which have already finished -- `skip` would drop a
	// firing because of a run that ended an hour ago.
	s.reap()

	records, err := s.store.List()
	if err != nil {
		return fmt.Errorf("scheduler: cannot read triggers, so nothing can be "+
			"scheduled this tick: %w", err)
	}

	for _, r := range records {
		s.tickOne(r, now)
	}
	return nil
}

// tickOne is the whole decision-and-act sequence for a single trigger.
func (s *Scheduler) tickOne(r trigger.Record, now time.Time) {
	report := Report{Trigger: r.Name, At: now}

	d, err := trigger.Due(r, now)
	if err != nil {
		report.Err = err
		report.Why = "could not decide whether this is due"
		s.report(report)
		return
	}
	report.Missed = d.Missed

	a, err := trigger.Admit(r, d, len(s.inflight[r.Name]))
	if err != nil {
		report.Err = err
		report.Why = "could not decide whether to start it"
		s.report(report)
		return
	}
	report.Why = a.Why
	report.Cancel = a.Cancel
	report.Consume = a.Consume

	// Nothing to do and nothing to record. Reported anyway: "not due until
	// 03:00" is the answer to the question an operator is actually asking.
	if a.Start == 0 && !a.Consume {
		s.report(report)
		return
	}

	if a.Cancel {
		s.cancelAll(r.Name)
	}

	// Started BEFORE the record is updated, deliberately, and the ordering is
	// a choice between two bad outcomes on a crash:
	//
	//   start then record -- a crash between them re-runs the firing.
	//   record then start -- a crash between them SKIPS it silently.
	//
	// A duplicate run is visible, costs money once, and shows up in the run
	// list. A skipped one is invisible and is indistinguishable from a
	// trigger that is working. The visible failure is the better one.
	for i := 0; i < a.Start; i++ {
		if err := s.start(r); err != nil {
			report.Err = err
			// Whatever did start is still running and still counted, so the
			// firing is recorded below for those. Bailing out entirely would
			// leave the slot due and start the successful ones again.
			break
		}
		report.Started++
	}

	// A firing is recorded when something started OR when the slot was
	// consciously consumed. If neither happened -- the first Start failed --
	// the slot stays due, which is correct: nothing ran.
	if report.Started > 0 || a.Consume {
		if err := s.recordFiring(r, now, report.Started, a); err != nil {
			// Reported and not retried. The consequence is knowable and worth
			// stating: the slot stays due, so the next tick fires it again.
			// That is a duplicate run, which is the failure mode chosen above.
			report.Err = errors.Join(report.Err, err)
		}
	}
	s.report(report)
}

// start launches one execution and books it as in-flight.
func (s *Scheduler) start(r trigger.Record) error {
	a, err := r.Action()
	if err != nil {
		// Validate already parsed this on load, so reaching here means the
		// action vocabulary changed under a stored trigger. Worth saying so
		// rather than reporting a generic parse failure.
		return fmt.Errorf("trigger %q: --then no longer parses, so it cannot be "+
			"run: %w", r.Name, err)
	}
	ex, err := s.runner.Start(r, a)
	if err != nil {
		return fmt.Errorf("trigger %q: could not start %q: %w", r.Name, a.CLI(), err)
	}
	if ex == nil {
		// A Runner that returns (nil, nil) would otherwise be booked as an
		// in-flight execution that never finishes, and every subsequent
		// overlap decision for this trigger would be made against a phantom.
		return fmt.Errorf("trigger %q: the runner reported success but returned "+
			"no execution, so there is nothing to wait on or cancel", r.Name)
	}
	s.inflight[r.Name] = append(s.inflight[r.Name], ex)
	return nil
}

// recordFiring writes back what happened, so the next tick does not repeat it.
func (s *Scheduler) recordFiring(r trigger.Record, now time.Time, started int, a trigger.Admission) error {
	// The WALL CLOCK instant, not the slot the firing belongs to.
	//
	// Both work for scheduling -- Missed counts slots strictly after this
	// value, and `now` is at or after the slot by definition of being due -- so
	// the tie is broken by what the LAST column should mean to a human. "When
	// did this actually run" is a fact; "which slot did it nominally belong
	// to" is a derivation the operator cannot check against anything.
	r.LastFiredAt = now.Format(time.RFC3339)
	r.LastStatus = firingStatus(started, a)

	if err := s.store.Save(r); err != nil {
		return fmt.Errorf("trigger %q fired but could not be recorded, so it "+
			"will fire again on the next tick: %w", r.Name, err)
	}
	return nil
}

// firingStatus is the one line `trigger list` shows under LAST.
func firingStatus(started int, a trigger.Admission) string {
	switch {
	case started == 0:
		// Consumed without running: the ONE case where LastFiredAt advances
		// though nothing happened. It has to say so, or the operator reads a
		// recent timestamp as evidence of a successful run.
		return "skipped: " + a.Why
	case started == 1 && !a.Cancel:
		return "started"
	case a.Cancel:
		return fmt.Sprintf("started %d after cancelling the previous", started)
	default:
		return fmt.Sprintf("started %d", started)
	}
}

// cancelAll stops everything in flight for one trigger.
func (s *Scheduler) cancelAll(name string) {
	for _, ex := range s.inflight[name] {
		ex.Cancel()
	}
	// Not removed from inflight here. Cancel is a request, not a completion,
	// and an execution that ignores it is still running and still spending.
	// reap removes things when they are actually done, and letting cancelled
	// work stay counted until then is what stops `cancel-previous` from
	// starting an unbounded number of replacements for a process that will not
	// die.
}

// reap forgets executions that have finished.
func (s *Scheduler) reap() {
	for name, exs := range s.inflight {
		live := exs[:0] // reuses the backing array; exs is not read again after
		for _, ex := range exs {
			select {
			case <-ex.Done():
				// finished, drop it
			default:
				live = append(live, ex)
			}
		}
		if len(live) == 0 {
			// Deleted rather than left as an empty slice, so a long-lived
			// scheduler does not accumulate one map entry per trigger that has
			// ever run.
			delete(s.inflight, name)
			continue
		}
		s.inflight[name] = live
	}
}

// report hands a Report to the observer, if there is one.
func (s *Scheduler) report(r Report) {
	if s.observe != nil {
		s.observe(r)
	}
}

// Running reports how many executions are in flight per trigger.
//
// Exported for tests and for whatever `trigger list` eventually shows, and it
// returns a copy: handing out the live map would let a caller mutate the
// scheduler's only piece of state.
func (s *Scheduler) Running() map[string]int {
	out := make(map[string]int, len(s.inflight))
	for name, exs := range s.inflight {
		if len(exs) > 0 {
			out[name] = len(exs)
		}
	}
	return out
}

// Names returns the triggers with work in flight, sorted, for stable output.
func (s *Scheduler) Names() []string {
	out := make([]string, 0, len(s.inflight))
	for name := range s.inflight {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
