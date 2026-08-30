package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/trigger"
)

// ---- fakes -----------------------------------------------------------------

// fakeStore is a trigger store in memory.
//
// saveErr exists because the case it enables -- a firing that starts and cannot
// be recorded -- is the one with the worst consequence (the same run repeating
// every tick, forever) and is nearly impossible to arrange against a real
// directory without chmod games that behave differently as root.
type fakeStore struct {
	recs     []trigger.Record
	listErr  error
	saveErr  error
	saved    []trigger.Record
	saveCall int
}

func (f *fakeStore) List() ([]trigger.Record, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]trigger.Record, len(f.recs))
	copy(out, f.recs)
	return out, nil
}

func (f *fakeStore) Save(r trigger.Record) error {
	f.saveCall++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, r)
	for i := range f.recs {
		if f.recs[i].Name == r.Name {
			f.recs[i] = r // so a second Tick sees the updated LastFiredAt
			return nil
		}
	}
	f.recs = append(f.recs, r)
	return nil
}

// fakeExec is one execution whose lifetime the test controls exactly.
type fakeExec struct {
	done      chan struct{}
	cancels   int
	cancelled bool
}

func newExec() *fakeExec { return &fakeExec{done: make(chan struct{})} }

func (e *fakeExec) Done() <-chan struct{} { return e.done }

func (e *fakeExec) Cancel() {
	e.cancels++
	// Deliberately does NOT finish the execution. Cancel is a request, and the
	// scheduler must keep counting the work until it actually stops -- a fake
	// that closed done here would hide exactly that bug.
	e.cancelled = true
}

func (e *fakeExec) finish() { close(e.done) }

// fakeRunner records what it was asked to start.
type fakeRunner struct {
	started []trigger.Action
	recs    []trigger.Record
	execs   []*fakeExec
	err     error

	// attempts counts every call, including the ones that fail. started counts
	// only the successes, and the DIFFERENCE is what distinguishes "gave up
	// after a failure" from "retried the rest of the backlog immediately".
	attempts int

	failFrom int  // fail on the Nth Start onwards; 0 means never
	nilExec  bool // return (nil, nil): a runner that lies about succeeding
}

func (r *fakeRunner) Start(rec trigger.Record, a trigger.Action) (Execution, error) {
	r.attempts++
	n := len(r.started) + 1
	if r.err != nil && (r.failFrom == 0 || n >= r.failFrom) {
		return nil, r.err
	}
	r.started = append(r.started, a)
	r.recs = append(r.recs, rec)
	if r.nilExec {
		return nil, nil
	}
	e := newExec()
	r.execs = append(r.execs, e)
	return e, nil
}

// ---- fixtures --------------------------------------------------------------

// nightly is the §20.10 record: a daily 03:00 audit, created 2026-08-01.
func nightly() trigger.Record {
	return trigger.Record{
		Name:         "nightly-audit",
		On:           "cron:0 3 * * *",
		Then:         "run start security-team 'audit dependencies'",
		Budget:       5,
		BudgetPeriod: trigger.PeriodDay,
		OnMissed:     trigger.MissedSkip,
		Overlap:      trigger.OverlapSkip,
		Status:       trigger.StatusActive,
		CreatedAt:    "2026-08-01T00:00:00Z",
	}
}

func rfc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// harness builds a scheduler over the given records and collects its reports.
func harness(t *testing.T, recs ...trigger.Record) (*Scheduler, *fakeStore, *fakeRunner, *[]Report) {
	t.Helper()
	st := &fakeStore{recs: recs}
	rn := &fakeRunner{}
	var reports []Report
	s, err := New(st, rn, func(r Report) { reports = append(reports, r) })
	if err != nil {
		t.Fatal(err)
	}
	return s, st, rn, &reports
}

// ---- construction ----------------------------------------------------------

func TestASchedulerWithoutARunnerIsRefused(t *testing.T) {
	// The failure this prevents is the quiet one: such a scheduler decides
	// correctly, records every firing as done, runs nothing, and because it
	// records them the missed count stays at zero and the trigger list looks
	// perfectly healthy.
	_, err := New(&fakeStore{}, nil, nil)
	if err == nil {
		t.Fatal("a scheduler with no runner was accepted")
	}
	if !strings.Contains(err.Error(), "without anything running") &&
		!strings.Contains(err.Error(), "marked") {
		t.Errorf("error does not explain the silent failure: %v", err)
	}
}

func TestASchedulerWithoutAStoreIsRefused(t *testing.T) {
	if _, err := New(nil, &fakeRunner{}, nil); err == nil {
		t.Fatal("a scheduler with no store was accepted")
	}
}

func TestAnAbsentObserverIsNotAFailure(t *testing.T) {
	// nil observe is the normal case for anything that does not want logs, and
	// it must not panic on the very first tick.
	s, err := New(&fakeStore{recs: []trigger.Record{nightly()}}, &fakeRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
}

// ---- the ordinary path -----------------------------------------------------

func TestADueTriggerStartsItsAction(t *testing.T) {
	s, st, rn, reports := harness(t, nightly())

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("started %d executions, want 1", len(rn.started))
	}
	if got := rn.started[0].CLI(); !strings.HasPrefix(got, "run start") {
		t.Errorf("ran %q, which is not the trigger's --then", got)
	}
	if len(st.saved) != 1 {
		t.Fatalf("recorded %d firings, want 1", len(st.saved))
	}
	if st.saved[0].LastFiredAt == "" {
		t.Error("the firing was not recorded, so it will run again next tick")
	}
	if st.saved[0].LastStatus != "started" {
		t.Errorf("LastStatus = %q", st.saved[0].LastStatus)
	}
	if len(*reports) != 1 || (*reports)[0].Started != 1 {
		t.Errorf("reports = %+v", *reports)
	}
}

func TestTheRunnerIsToldWhatTheTriggerMaySpend(t *testing.T) {
	// The budget travels with the record. A runner handed only a command line
	// would have to be told the ceiling separately, and the one thing a
	// trigger must not do is spend without one.
	s, _, rn, _ := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.recs) != 1 {
		t.Fatalf("started %d", len(rn.recs))
	}
	if rn.recs[0].Budget != 5 || rn.recs[0].BudgetPeriod != trigger.PeriodDay {
		t.Errorf("runner got budget %v/%v", rn.recs[0].Budget, rn.recs[0].BudgetPeriod)
	}
}

func TestATriggerThatIsNotDueDoesNothing(t *testing.T) {
	r := nightly()
	r.LastFiredAt = "2026-08-02T03:00:00Z"
	s, st, rn, reports := harness(t, r)

	if err := s.Tick(rfc("2026-08-02T09:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 0 {
		t.Errorf("started %d executions for a trigger that is not due", len(rn.started))
	}
	if st.saveCall != 0 {
		t.Error("rewrote a record it had no reason to touch")
	}
	// But it still SAYS so: this is the report an operator asking "why has
	// this not run" needs to see.
	if len(*reports) != 1 || strings.TrimSpace((*reports)[0].Why) == "" {
		t.Errorf("no reason reported for declining: %+v", *reports)
	}
}

func TestFiringIsNotRepeatedOnTheNextTick(t *testing.T) {
	// The end-to-end version of "the store is the queue": tick twice within
	// the same slot and the second must do nothing.
	s, _, rn, _ := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-02T03:00:30Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("fired %d times inside one slot", len(rn.started))
	}
}

func TestTheNextSlotFiresAgain(t *testing.T) {
	// The complement of the test above: proof that recording a firing does not
	// permanently silence the trigger.
	s, _, rn, _ := harness(t, nightly())
	for _, at := range []string{
		"2026-08-02T03:00:00Z",
		"2026-08-03T03:00:00Z",
		"2026-08-04T03:00:00Z",
	} {
		if err := s.Tick(rfc(at)); err != nil {
			t.Fatal(err)
		}
		// Let each run finish, so overlap: skip does not drop the next.
		for _, e := range rn.execs {
			select {
			case <-e.Done():
			default:
				e.finish()
			}
		}
	}
	if len(rn.started) != 3 {
		t.Fatalf("fired %d times across three days, want 3", len(rn.started))
	}
}

func TestALateTickStillFires(t *testing.T) {
	// The tick interval is a latency knob, not a correctness one. A scheduler
	// that wakes at 03:47 for a 03:00 slot runs it late rather than losing it.
	s, _, rn, reports := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:47:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("a late tick lost the firing: started %d", len(rn.started))
	}
	if !strings.Contains((*reports)[0].Why, "2026-08-02T03:00") {
		t.Errorf("reason does not name the slot it is running late: %q", (*reports)[0].Why)
	}
}

func TestAPausedTriggerIsNeverStarted(t *testing.T) {
	r := nightly()
	r.Status = trigger.StatusPaused
	s, st, rn, _ := harness(t, r)
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 0 {
		t.Error("a paused trigger fired")
	}
	if st.saveCall != 0 {
		t.Error("a paused trigger's record was rewritten")
	}
}

// ---- overlap, end to end ---------------------------------------------------

func TestSkipDropsTheSlotSoItDoesNotReturn(t *testing.T) {
	s, st, rn, _ := harness(t, nightly())

	// Day one starts and does not finish.
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	// Day two arrives with day one still running.
	if err := s.Tick(rfc("2026-08-03T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("overlap: skip started a second execution: %d", len(rn.started))
	}
	if len(st.saved) != 2 {
		t.Fatalf("the skipped slot was not consumed, so it is still due: %d saves",
			len(st.saved))
	}
	last := st.saved[1]
	if !strings.HasPrefix(last.LastStatus, "skipped") {
		t.Errorf("LastStatus = %q: an operator would read this timestamp as a "+
			"successful run", last.LastStatus)
	}
}

func TestQueueLeavesTheSlotDueSoItRunsWhenTheWayIsClear(t *testing.T) {
	r := nightly()
	r.Overlap = trigger.OverlapQueue
	r.OnMissed = trigger.MissedRunOnce // so the deferred slot is not later dropped
	s, st, rn, _ := harness(t, r)

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	// Day two, still busy: deferred, and NOT recorded.
	if err := s.Tick(rfc("2026-08-03T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("queue started while busy: %d", len(rn.started))
	}
	if len(st.saved) != 1 {
		t.Fatalf("queue consumed the slot, which is what skip does: %d saves",
			len(st.saved))
	}

	// The first execution finishes; the deferred firing must now run.
	rn.execs[0].finish()
	if err := s.Tick(rfc("2026-08-03T04:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 2 {
		t.Fatalf("the queued firing never ran: %d started", len(rn.started))
	}
}

func TestParallelStartsAlongsideRunningWork(t *testing.T) {
	r := nightly()
	r.Overlap = trigger.OverlapParallel
	s, _, rn, _ := harness(t, r)

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-03T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 2 {
		t.Fatalf("parallel did not start alongside: %d", len(rn.started))
	}
	if got := s.Running()["nightly-audit"]; got != 2 {
		t.Errorf("in flight = %d, want 2", got)
	}
}

func TestCancelPreviousStopsTheOldRunAndStartsANewOne(t *testing.T) {
	r := nightly()
	r.Overlap = trigger.OverlapCancelPrevious
	s, st, rn, _ := harness(t, r)

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-03T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if rn.execs[0].cancels != 1 {
		t.Errorf("the previous execution was cancelled %d times", rn.execs[0].cancels)
	}
	if len(rn.started) != 2 {
		t.Fatalf("cancelled without starting a replacement: %d started", len(rn.started))
	}
	if !strings.Contains(st.saved[1].LastStatus, "cancelling") {
		t.Errorf("LastStatus does not record that work was destroyed: %q",
			st.saved[1].LastStatus)
	}
}

func TestCancelledWorkStaysCountedUntilItActuallyStops(t *testing.T) {
	// The property cancelAll's comment claims, and the one that stops
	// cancel-previous from starting an unbounded number of replacements for a
	// process that ignores cancellation.
	r := nightly()
	r.Overlap = trigger.OverlapCancelPrevious
	s, _, _, _ := harness(t, r)

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-03T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	// The cancelled one never finishes. It must still be counted.
	if got := s.Running()["nightly-audit"]; got != 2 {
		t.Fatalf("in flight = %d, want 2: cancelled-but-alive work was forgotten "+
			"while it is still spending", got)
	}
}

func TestFinishedWorkIsForgottenBeforeTheNextDecision(t *testing.T) {
	// reap runs first. If it ran after the decision, this skip-policy trigger
	// would drop day two because of an execution that had already finished.
	s, _, rn, _ := harness(t, nightly())

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	rn.execs[0].finish()
	if err := s.Tick(rfc("2026-08-03T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 2 {
		t.Fatalf("day two was dropped because of a run that had already "+
			"finished: %d started", len(rn.started))
	}
	if len(s.Running()) != 1 {
		t.Errorf("Running() = %v", s.Running())
	}
}

// ---- on-missed, end to end -------------------------------------------------

func TestRunAllOwesOneExecutionPerMissedFiring(t *testing.T) {
	r := nightly()
	r.OnMissed = trigger.MissedRunAll
	r.Overlap = trigger.OverlapParallel
	r.LastFiredAt = "2026-08-02T03:00:00Z"
	s, _, rn, reports := harness(t, r)

	// Four days later: 03:00 on the 3rd, 4th, 5th and 6th were all missed.
	if err := s.Tick(rfc("2026-08-06T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 4 {
		t.Fatalf("run-all started %d executions after a four-day gap, want 4",
			len(rn.started))
	}
	if (*reports)[0].Missed != 4 {
		t.Errorf("reported %d missed", (*reports)[0].Missed)
	}
}

func TestTheMissedCountIsReportedEvenWhenTheFiringsAreDropped(t *testing.T) {
	// "skipped 4 nightly audits" is the most important sentence this system can
	// say to somebody who believes their automation is healthy.
	r := nightly()
	r.OnMissed = trigger.MissedSkip
	r.LastFiredAt = "2026-08-02T03:00:00Z"
	s, _, rn, reports := harness(t, r)

	if err := s.Tick(rfc("2026-08-06T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 0 {
		t.Errorf("skip ran %d of the backlog", len(rn.started))
	}
	if (*reports)[0].Missed != 4 {
		t.Fatalf("the outage was not reported: Missed = %d", (*reports)[0].Missed)
	}
}

// ---- failures --------------------------------------------------------------

func TestAnUnreadableStoreStopsTheTickRatherThanFiringBlind(t *testing.T) {
	st := &fakeStore{listErr: errors.New("disk on fire")}
	s, err := New(st, &fakeRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Tick(rfc("2026-08-02T03:00:00Z"))
	if err == nil {
		t.Fatal("a tick over an unreadable store reported success")
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("the cause was swallowed: %v", err)
	}
}

func TestOneBrokenTriggerDoesNotStopTheOthers(t *testing.T) {
	// A typo in one hand-edited file must not become a silent outage of all
	// automation, which is the failure this whole system exists to prevent.
	broken := nightly()
	broken.Name = "broken"
	broken.CreatedAt = "not a timestamp"

	ok := nightly()
	s, st, rn, reports := harness(t, broken, ok)

	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatalf("one bad record stopped the whole tick: %v", err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("the healthy trigger did not fire: %d started", len(rn.started))
	}
	var sawErr bool
	for _, rep := range *reports {
		if rep.Trigger == "broken" && rep.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("the broken trigger failed silently")
	}
	// Found by mutation testing: dropping the `return` after reporting a Due
	// failure survived the whole suite, because this test only checked that
	// SOMETHING was started and that an error was reported. It never checked
	// that the broken trigger was not itself acted on -- and a record whose
	// dueness could not be decided must not then be started off the zero
	// Decision that comes back with the error.
	for _, rec := range rn.recs {
		if rec.Name == "broken" {
			t.Error("a trigger whose dueness could not be decided was started " +
				"anyway, off the zero Decision returned with the error")
		}
	}
	for _, rec := range st.saved {
		if rec.Name == "broken" {
			t.Error("a trigger that could not be decided had its firing recorded, " +
				"so the failure is now invisible on the next tick")
		}
	}
}

// TestATriggerWithAnUnknownOverlapIsRefusedBeforeItIsAdmitted records a
// finding rather than a behaviour, and the finding is that Admit's error
// CANNOT BE REACHED through Tick.
//
// I wrote this test to kill a surviving mutation -- deleting the `return`
// after an Admit failure -- and it would not die. Probing the two layers
// separately explained why, and the answer was not what the test assumed:
//
//	Due err   = trigger "nightly-audit" has overlap "eventually", which is
//	            not one of skip, queue, parallel, cancel-previous
//	Admit err = <nil>
//
// Due calls Validate first, so a bad policy is refused one layer earlier and
// tickOne returns on THAT error. And Admit only consults the policy when work
// is already running; with an idle trigger it returns before the switch. Its
// other error, a negative in-flight count, is unreachable by construction
// because the argument is a len().
//
// So the mutation is equivalent, and the `return` it deletes is defence in
// depth rather than a live branch. That is worth keeping -- Due's validation
// is not Admit's to rely on, and the day someone reorders those two calls the
// branch becomes load-bearing -- but it is NOT worth a test that contorts the
// scheduler into reaching it, which is what I tried first and threw away. A
// test that fakes its way to an unreachable line documents the fake, not the
// code.
//
// What is asserted instead is the layer that really does the refusing.
func TestATriggerWithAnUnknownOverlapIsRefusedBeforeItIsAdmitted(t *testing.T) {
	r := nightly()
	r.Overlap = "eventually"

	st := &fakeStore{recs: []trigger.Record{r}}
	rn := &fakeRunner{}
	var reports []Report
	s, err := New(st, rn, func(rp Report) { reports = append(reports, rp) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 0 {
		t.Errorf("started %d executions under a policy nothing understands",
			len(rn.started))
	}
	if st.saveCall != 0 {
		t.Error("recorded a firing that never happened, hiding the failure")
	}
	if len(reports) != 1 || reports[0].Err == nil {
		t.Fatalf("the undecidable trigger was not reported: %+v", reports)
	}
	if !strings.Contains(reports[0].Err.Error(), "eventually") {
		t.Errorf("the report does not name the bad value: %v", reports[0].Err)
	}
}

func TestARunThatCannotBeRecordedIsReportedWithItsConsequence(t *testing.T) {
	// The worst outcome in the file: the run happened, the record did not
	// move, so the next tick runs it again. It must say so.
	st := &fakeStore{recs: []trigger.Record{nightly()}, saveErr: errors.New("read-only fs")}
	rn := &fakeRunner{}
	var reports []Report
	s, err := New(st, rn, func(r Report) { reports = append(reports, r) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Fatalf("started %d", len(rn.started))
	}
	if reports[0].Err == nil {
		t.Fatal("a firing that could not be recorded was reported as clean")
	}
	if !strings.Contains(reports[0].Err.Error(), "fire again") {
		t.Errorf("the consequence is not stated: %v", reports[0].Err)
	}
}

func TestAFiringThatCannotStartLeavesTheSlotDue(t *testing.T) {
	// Nothing ran, so nothing may be recorded: recording here would consume a
	// slot that never executed and the firing would be lost silently.
	st := &fakeStore{recs: []trigger.Record{nightly()}}
	rn := &fakeRunner{err: errors.New("fork failed")}
	var reports []Report
	s, err := New(st, rn, func(r Report) { reports = append(reports, r) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if st.saveCall != 0 {
		t.Fatal("a firing that never started was recorded, so the slot is now " +
			"consumed and the run is lost")
	}
	if reports[0].Err == nil {
		t.Error("the failure to start was not reported")
	}

	// And the proof that it stays due: the next tick tries again.
	rn.err = nil
	if err := s.Tick(rfc("2026-08-02T03:01:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 1 {
		t.Errorf("the slot did not stay due after a failed start: %d", len(rn.started))
	}
}

func TestAPartiallyStartedBacklogRecordsWhatDidStart(t *testing.T) {
	// Two of four started, then fork failed. Bailing out entirely would leave
	// the slot due and start the two successful ones AGAIN on the next tick.
	r := nightly()
	r.OnMissed = trigger.MissedRunAll
	r.Overlap = trigger.OverlapParallel
	r.LastFiredAt = "2026-08-02T03:00:00Z"

	st := &fakeStore{recs: []trigger.Record{r}}
	rn := &fakeRunner{err: errors.New("fork failed"), failFrom: 3}
	var reports []Report
	s, err := New(st, rn, func(rp Report) { reports = append(reports, rp) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-06T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 2 {
		t.Fatalf("started %d before failing, want 2", len(rn.started))
	}
	// Found by mutation testing: `continue` instead of `break` survived,
	// because the fake fails from the third Start onwards and so both spellings
	// end with two started. They are not the same, though. `continue` retries
	// a failing runner once per remaining owed run -- four owed becomes two
	// more doomed forks in the same tick, forty owed becomes thirty-eight --
	// and the thing that has just failed is usually the machine (out of
	// memory, out of process handles), so retrying it immediately in a tight
	// loop is the worst available response.
	//
	// Asserting the ATTEMPTS rather than the successes is what tells the two
	// apart: break makes exactly three, continue makes four.
	if rn.attempts != 3 {
		t.Errorf("the runner was called %d times for a backlog of 4 that failed "+
			"on the third: want 3, so a failing runner is not retried once per "+
			"remaining owed run within the same tick", rn.attempts)
	}
	if st.saveCall != 1 {
		t.Fatalf("the two that did start were not recorded, so they will run "+
			"again next tick: %d saves", st.saveCall)
	}
	if reports[0].Started != 2 || reports[0].Err == nil {
		t.Errorf("report does not say both what ran and what failed: %+v", reports[0])
	}
}

func TestARunnerThatReturnsNoExecutionIsRefused(t *testing.T) {
	// (nil, nil) would otherwise be booked as in-flight work that never
	// finishes, and every later overlap decision would be made against a
	// phantom that blocks the trigger forever.
	st := &fakeStore{recs: []trigger.Record{nightly()}}
	rn := &fakeRunner{nilExec: true}
	var reports []Report
	s, err := New(st, rn, func(r Report) { reports = append(reports, r) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if reports[0].Err == nil {
		t.Fatal("a runner that returned no execution was accepted")
	}
	if got := s.Running()["nightly-audit"]; got != 0 {
		t.Errorf("a phantom execution was booked: %d in flight", got)
	}
}

// ---- bookkeeping -----------------------------------------------------------

// TestRunningIsACopyAndNotTheLiveMap cannot fail today, and is kept anyway.
//
// Mutation testing could not kill it, and the honest reason is that the
// property is currently guaranteed by the TYPES rather than by the code:
// inflight is map[string][]Execution and Running returns map[string]int, so
// there is no way to alias one to the other even deliberately. My attempt at a
// mutation was an equivalent mutant -- it allocated a different fresh map --
// which is a defect in the mutation, not evidence about the test.
//
// It stays because the thing it protects is a refactor, not this code: the
// moment inflight becomes a map[string]int (which is the obvious
// simplification once somebody notices Running is the only reader), returning
// it directly compiles, passes every other test in this file, and hands
// callers a live handle on the scheduler's only piece of state.
func TestRunningIsACopyAndNotTheLiveMap(t *testing.T) {
	s, _, _, _ := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	got := s.Running()
	got["nightly-audit"] = 99
	got["invented"] = 5
	if s.Running()["nightly-audit"] != 1 || len(s.Running()) != 1 {
		t.Errorf("the caller mutated the scheduler's only state: %v", s.Running())
	}
}

func TestFinishedTriggersLeaveNoEntryBehind(t *testing.T) {
	// A long-lived scheduler must not accumulate one map entry per trigger
	// that has ever run.
	s, _, rn, _ := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	rn.execs[0].finish()
	if err := s.Tick(rfc("2026-08-02T03:30:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(s.Names()) != 0 {
		t.Errorf("Names() = %v after everything finished", s.Names())
	}
}

func TestTheRecordedInstantIsWhenItActuallyRan(t *testing.T) {
	// The wall clock, not the slot. "When did this run" is a fact an operator
	// can check; "which slot did it belong to" is a derivation they cannot.
	s, st, _, _ := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:47:12Z")); err != nil {
		t.Fatal(err)
	}
	if st.saved[0].LastFiredAt != "2026-08-02T03:47:12Z" {
		t.Errorf("LastFiredAt = %q, want the instant it ran", st.saved[0].LastFiredAt)
	}
}

func TestTheRecordedFiringIsStillAValidTrigger(t *testing.T) {
	// Save validates, so a scheduler writing a malformed LastFiredAt would
	// fail on every tick. Asserted directly because the fake store does not
	// validate and would hide it.
	s, st, _, _ := harness(t, nightly())
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := st.saved[0].Validate(); err != nil {
		t.Fatalf("the scheduler wrote a record the store would refuse: %v", err)
	}
}

func TestTickIsNotAffectedByTheZoneItIsHanded(t *testing.T) {
	// Every answer must come from the instant, not from how it was spelled.
	utc := rfc("2026-08-02T03:00:00Z")
	east := utc.In(time.FixedZone("UTC+13", 13*3600))

	a, sa, ra, _ := harness(t, nightly())
	if err := a.Tick(utc); err != nil {
		t.Fatal(err)
	}
	b, sb, rb, _ := harness(t, nightly())
	if err := b.Tick(east); err != nil {
		t.Fatal(err)
	}
	if len(ra.started) != len(rb.started) {
		t.Errorf("same instant, different zones: %d vs %d started",
			len(ra.started), len(rb.started))
	}

	// The STORED STRING, not just the decision. This is what the mutation
	// testing found: with `now = now.UTC()` removed the tick still fires
	// correctly -- the comparisons are zone-independent -- but it writes
	// "2026-08-02T16:00:00+13:00" into the trigger file.
	//
	// That is a real difference and not a cosmetic one, because LastFiredAt is
	// TEXT that outlives the process which wrote it. Two machines in different
	// regions sharing a triggers directory would write the same instant two
	// ways; `trigger list` would show a LAST column whose meaning depended on
	// which host last fired it, and anybody diffing the files would see
	// spurious changes. Unlike the identical-looking line in trigger.Due --
	// which is genuinely redundant, since every instant Due prints comes from
	// s.Next() -- this one is load-bearing, and now has a test that says so.
	if sa.saved[0].LastFiredAt != sb.saved[0].LastFiredAt {
		t.Errorf("the same instant was recorded two ways: %q vs %q",
			sa.saved[0].LastFiredAt, sb.saved[0].LastFiredAt)
	}
	if !strings.HasSuffix(sb.saved[0].LastFiredAt, "Z") {
		t.Errorf("LastFiredAt %q carries the host's zone into the stored file",
			sb.saved[0].LastFiredAt)
	}
}

func TestAnEventTriggerIsNeverFiredByTheClock(t *testing.T) {
	r := nightly()
	r.On = "event:deploy.finished"
	s, st, rn, _ := harness(t, r)
	if err := s.Tick(rfc("2026-08-02T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(rn.started) != 0 {
		t.Error("the scheduler fired an event-driven trigger from the clock")
	}
	if st.saveCall != 0 {
		t.Error("an event trigger's record was rewritten by a clock tick")
	}
}
