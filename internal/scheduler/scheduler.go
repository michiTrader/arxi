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

	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/trigger"
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

type CoordinationView struct {
	Revision    uint64
	Occurrences map[job.OccurrenceID]job.Occurrence
	Jobs        map[job.JobID]job.Job
}

type Admission struct {
	Occurrence job.Occurrence
	Window     job.LedgerWindow
	Ceiling    job.Amount
	Reserved   job.Amount
}

type Cancellation struct {
	JobID  job.JobID
	Actor  string
	Reason string
}

// Coordinator is the durable scheduling seam. It deliberately mirrors only the
// cross-job operations this package needs; concrete journal layout and locking
// remain adapter concerns.
type Coordinator interface {
	View() CoordinationView
	RecordOccurrence(uint64, job.Occurrence) (job.Occurrence, uint64, error)
	Admit(uint64, Admission) (job.Occurrence, uint64, error)
	Cancel(uint64, Cancellation) (uint64, error)
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

// DurableRunner receives the canonical identity accepted before launch. Runner
// remains the compatibility shape for non-durable callers; production wiring
// implements this extension and never chooses an identity itself.
type DurableRunner interface {
	StartSlot(r trigger.Record, slot Slot, a trigger.Action) (Execution, error)
}

// Slot is the durable identity already accepted for one nominal firing.
type Slot struct {
	NominalAt    time.Time
	OccurrenceID job.OccurrenceID
	JobID        job.JobID
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

// ExecutionError is implemented by executions that retain their terminal wait
// error. Reap reports it instead of silently treating infrastructure loss as a
// successful completion.
type ExecutionError interface {
	Err() error
}

// Scheduler fires triggers. It is not safe for concurrent use; Run owns it.
type Scheduler struct {
	store       Store
	runner      Runner
	coordinator Coordinator

	// inflight is what is currently running, per trigger name.
	//
	// This is the ONLY state the scheduler keeps in memory, and it is
	// deliberately the only thing that cannot be recovered from disk: a
	// restart loses track of executions started by the previous process. That
	// is honest rather than ideal -- those processes died with their parent --
	// and the alternative, writing pids to a file, invents a second source of
	// truth that goes stale the moment a machine is power-cycled.
	// executions is only a wake/cancel convenience after durable policy accepts.
	// Missing entries after restart do not change overlap decisions.
	inflight   map[string][]Execution
	executions map[job.JobID]Execution

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
	return NewDurable(store, runner, nil, observe)
}

// NewDurable installs cross-process occurrence, overlap, and budget truth.
func NewDurable(store Store, runner Runner, coordinator Coordinator, observe func(Report)) (*Scheduler, error) {
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
		store:       store,
		runner:      runner,
		coordinator: coordinator,
		inflight:    map[string][]Execution{},
		executions:  map[job.JobID]Execution{},
		observe:     observe,
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
	if s.coordinator != nil {
		s.tickDurable(r, now, d, report)
		return
	}

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
		if err := s.recordFiring(r, now, report.Started, a, d); err != nil {
			// Reported and not retried. The consequence is knowable and worth
			// stating: the slot stays due, so the next tick fires it again.
			// That is a duplicate run, which is the failure mode chosen above.
			report.Err = errors.Join(report.Err, err)
		}
	}
	s.report(report)
}

func (s *Scheduler) tickDurable(r trigger.Record, now time.Time, d trigger.Decision, report Report) {
	revision := s.coordinator.View().Revision
	for _, slot := range d.SkippedSlots {
		occurrence := occurrenceFor(r, slot, job.OccurrenceSkipped, d.Why)
		_, next, err := s.coordinator.RecordOccurrence(revision, occurrence)
		if err != nil {
			report.Err = errors.Join(report.Err, fmt.Errorf("record skipped occurrence %s: %w", occurrence.ID, err))
			break
		}
		revision = next
		report.Consume = true
	}
	for _, nominal := range d.Slots {
		started, cancel, consume, next, err := s.admitSlot(r, nominal, revision)
		revision = next
		report.Cancel = report.Cancel || cancel
		report.Consume = report.Consume || consume
		if err != nil {
			report.Err = errors.Join(report.Err, err)
			break
		}
		report.Started += started
	}
	if report.Started > 0 || report.Consume {
		a := trigger.Admission{Cancel: report.Cancel, Consume: report.Consume, Why: d.Why}
		if err := s.recordFiring(r, now, report.Started, a, d); err != nil {
			report.Err = errors.Join(report.Err, err)
		}
	}
	report.Why = d.Why
	s.report(report)
}

func (s *Scheduler) admitSlot(r trigger.Record, nominal time.Time, revision uint64) (int, bool, bool, uint64, error) {
	view := s.coordinator.View()
	revision = view.Revision
	occurrenceID := job.OccurrenceIdentity(job.TriggerID(r.Identity()), nominal)
	if existing, ok := view.Occurrences[occurrenceID]; ok {
		switch existing.State {
		case job.OccurrenceAdmitted:
			started, err := s.startAccepted(r, nominal, existing)
			return started, false, true, revision, err
		case job.OccurrenceSkipped:
			return 0, false, true, revision, nil
		}
	}
	active := activeJobs(view, job.TriggerID(r.Identity()))
	if len(active) > 0 {
		switch r.Overlap {
		case trigger.OverlapSkip:
			o := occurrenceFor(r, nominal, job.OccurrenceSkipped, "overlap policy skipped an active trigger")
			_, next, err := s.coordinator.RecordOccurrence(revision, o)
			return 0, false, err == nil, next, err
		case trigger.OverlapQueue:
			o := occurrenceFor(r, nominal, job.OccurrencePending, "")
			_, next, err := s.coordinator.RecordOccurrence(revision, o)
			return 0, false, false, next, err
		case trigger.OverlapCancelPrevious:
			for _, id := range active {
				next, err := s.coordinator.Cancel(revision, Cancellation{JobID: id, Actor: "scheduler", Reason: "trigger overlap replacement"})
				if err != nil {
					return 0, true, false, revision, fmt.Errorf("record cancellation for %s before replacement: %w", id, err)
				}
				revision = next
				if ex := s.execution(id); ex != nil {
					ex.Cancel()
				}
			}
		}
	}
	o := occurrenceFor(r, nominal, job.OccurrenceAdmitted, "")
	o.JobID = job.ScheduledJobIdentity(o.ID)
	o.ReservationID = "reservation-" + string(o.ID)
	amount, err := amountFromUSD(r.Budget)
	if err != nil {
		return 0, false, false, revision, err
	}
	window, err := ledgerWindow(r, nominal)
	if err != nil {
		return 0, false, false, revision, err
	}
	accepted, next, err := s.coordinator.Admit(revision, Admission{Occurrence: o, Window: window, Ceiling: amount, Reserved: amount})
	if err != nil {
		return 0, false, false, next, fmt.Errorf("admit occurrence %s: %w", o.ID, err)
	}
	started, err := s.startAccepted(r, nominal, accepted)
	if err != nil {
		return 0, false, false, next, err
	}
	return started, len(active) > 0 && r.Overlap == trigger.OverlapCancelPrevious, true, next, nil
}

func (s *Scheduler) startAccepted(r trigger.Record, nominal time.Time, occurrence job.Occurrence) (int, error) {
	if _, ok := s.executions[occurrence.JobID]; ok {
		return 0, nil
	}
	action, err := r.Action()
	if err != nil {
		return 0, err
	}
	durable, ok := s.runner.(DurableRunner)
	if !ok {
		return 0, errors.New("durable scheduler runner does not accept canonical slot identity")
	}
	ex, err := durable.StartSlot(r, Slot{NominalAt: nominal, OccurrenceID: occurrence.ID, JobID: occurrence.JobID}, action)
	if err != nil {
		return 0, fmt.Errorf("publish admitted job %s: %w", occurrence.JobID, err)
	}
	if ex == nil {
		return 0, errors.New("durable runner returned no execution")
	}
	s.inflight[r.Name] = append(s.inflight[r.Name], ex)
	s.executions[occurrence.JobID] = ex
	return 1, nil
}

func occurrenceFor(r trigger.Record, nominal time.Time, state job.OccurrenceState, reason string) job.Occurrence {
	triggerID := job.TriggerID(r.Identity())
	return job.Occurrence{ID: job.OccurrenceIdentity(triggerID, nominal), TriggerID: triggerID, NominalAt: nominal.UTC(), State: state, SkipReason: reason}
}

func activeJobs(view CoordinationView, triggerID job.TriggerID) []job.JobID {
	seen := map[job.JobID]bool{}
	var ids []job.JobID
	for _, occurrence := range view.Occurrences {
		if occurrence.TriggerID != triggerID || occurrence.State != job.OccurrenceAdmitted || occurrence.JobID == "" {
			continue
		}
		stored, ok := view.Jobs[occurrence.JobID]
		if !ok || job.JobTerminal(stored.State) || seen[occurrence.JobID] {
			continue
		}
		seen[occurrence.JobID] = true
		ids = append(ids, occurrence.JobID)
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] < ids[k] })
	return ids
}

func amountFromUSD(value float64) (job.Amount, error) {
	if value <= 0 {
		return job.Amount{}, errors.New("trigger budget must be positive")
	}
	text := fmt.Sprintf("%.9f", value)
	var whole uint64
	var scale uint8
	seenDot := false
	for _, ch := range text {
		if ch == '.' {
			seenDot = true
			continue
		}
		whole = whole*10 + uint64(ch-'0')
		if seenDot {
			scale++
		}
	}
	return job.NewAmount(whole, scale), nil
}

func ledgerWindow(r trigger.Record, nominal time.Time) (job.LedgerWindow, error) {
	nominal = nominal.UTC()
	var start time.Time
	var period job.PeriodKind
	switch r.BudgetPeriod {
	case trigger.PeriodDay:
		period, start = job.PeriodDay, time.Date(nominal.Year(), nominal.Month(), nominal.Day(), 0, 0, 0, 0, time.UTC)
	case trigger.PeriodWeek:
		period = job.PeriodWeek
		days := (int(nominal.Weekday()) + 6) % 7
		day := nominal.AddDate(0, 0, -days)
		start = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	case trigger.PeriodMonth:
		period, start = job.PeriodMonth, time.Date(nominal.Year(), nominal.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return job.LedgerWindow{}, fmt.Errorf("unknown budget period %q", r.BudgetPeriod)
	}
	return job.LedgerWindow{TriggerID: job.TriggerID(r.Identity()), Period: period, StartsAt: start}, nil
}

func (s *Scheduler) execution(id job.JobID) Execution { return s.executions[id] }

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
func (s *Scheduler) recordFiring(r trigger.Record, now time.Time, started int, a trigger.Admission, d trigger.Decision) error {
	// LastFiredAt remains the wall-clock observation shown to operators. The
	// separate schedule cursor advances by nominal slot so a late tick cannot make
	// an interval schedule drift and durable occurrence identity remains exact.
	if r.ID == "" {
		r.ID = r.Identity()
	}
	r.LastFiredAt = now.Format(time.RFC3339)
	consumed := append(append([]time.Time(nil), d.Slots...), d.SkippedSlots...)
	if len(consumed) > 0 {
		latest := consumed[0]
		for _, slot := range consumed[1:] {
			if slot.After(latest) {
				latest = slot
			}
		}
		r.LastScheduledAt = latest.UTC().Format(time.RFC3339Nano)
	}
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
				for id, registered := range s.executions {
					if registered == ex {
						delete(s.executions, id)
					}
				}
				if failed, ok := ex.(ExecutionError); ok && failed.Err() != nil {
					s.report(Report{Trigger: name, Err: fmt.Errorf("execution ended: %w", failed.Err())})
				}
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
