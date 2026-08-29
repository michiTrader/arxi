package trigger

import (
	"strings"
	"testing"
	"time"
)

// The tests for Due are about a decision made at 3am with nobody watching, so
// every case here is a specific way that decision goes wrong: firing when it
// should not, not firing when it should, or firing the wrong NUMBER of times.
//
// A fixture function rather than a package var, so one test's edit cannot
// become another test's mystery.
func active() Record {
	return Record{
		Name:         "nightly-audit",
		On:           "cron:0 3 * * *",
		Then:         "run start security-team 'audit dependencies'",
		Budget:       5.00,
		BudgetPeriod: PeriodDay,
		OnMissed:     MissedSkip,
		Overlap:      OverlapSkip,
		Status:       StatusActive,
		CreatedAt:    "2026-08-01T00:00:00Z",
	}
}

// rfc is a spelling helper for this file only. The package already has
// at(y, mo, d, h, mi); rfc exists because these tests are about records whose
// stored timestamps are RFC3339 strings, and writing the instant in the same
// notation the record uses is what makes a case readable next to its fixture.
func rfc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("bad test timestamp " + s + ": " + err.Error())
	}
	return t.UTC()
}

// TestATriggerFiresForTheFirstTime is the test that made this file necessary.
//
// Missed() returns 0 for a never-fired trigger, correctly -- a daily trigger
// created a month ago has not missed thirty runs. But a scheduler built on
// Missed alone would therefore never fire ANYTHING for the first time, and
// every trigger in the system would sit at "0 missed" forever while the
// operator watched a NEXT column tick by.
//
// Found by probing Missed with a throwaway test rather than by reading it.
func TestATriggerFiresForTheFirstTime(t *testing.T) {
	r := active() // created 2026-08-01, fires daily at 03:00, never fired

	d, err := Due(r, rfc("2026-08-02T03:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.ShouldFire {
		t.Fatalf("a never-fired trigger whose first slot has arrived did not "+
			"fire.\n  reason given: %q\n"+
			"  this is the bug that makes a scheduler do nothing at all: "+
			"Missed() reports 0 for a trigger that has never run, so due-ness "+
			"for the FIRST firing cannot come from it", d.Why)
	}
	if d.Runs != 1 {
		t.Errorf("want exactly 1 run for a first firing, got %d", d.Runs)
	}
	// The reason must name the instant, because the question asked later is
	// never "did it fire" but "did it fire at the right time".
	if !strings.Contains(d.Why, "2026-08-02T03:00:00Z") {
		t.Errorf("the reason should name the firing it acted on: %q", d.Why)
	}
}

// TestANewTriggerDoesNotFireBeforeItsFirstSlot is the other half. A trigger
// created at 10:00 must not fire immediately just because 03:00 exists in its
// past.
func TestANewTriggerDoesNotFireBeforeItsFirstSlot(t *testing.T) {
	r := active()
	r.CreatedAt = "2026-08-01T10:00:00Z"

	d, err := Due(r, rfc("2026-08-01T11:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if d.ShouldFire {
		t.Fatalf("a trigger created at 10:00 fired at 11:00 for a 03:00 "+
			"schedule.\n  reason: %q\n"+
			"  the reference point for a never-fired trigger has to be its "+
			"creation, or every daily trigger fires the moment it is made", d.Why)
	}
	if !strings.Contains(d.Why, "not due until") {
		t.Errorf("the reason should say when it WILL be due: %q", d.Why)
	}
}

// TestANewTriggerDoesNotFireForEverySlotSinceTheEpoch guards the interpretation
// that would be catastrophic rather than merely wrong: treating a cron
// schedule's unbounded past as a backlog.
func TestANewTriggerDoesNotFireForEverySlotSinceTheEpoch(t *testing.T) {
	r := active()
	r.CreatedAt = "2020-01-01T00:00:00Z" // six years before now
	r.OnMissed = MissedRunAll            // the policy that would honour a backlog

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if d.Runs > 1 {
		t.Fatalf("a never-fired trigger created six years ago asked for %d "+
			"runs.\n  reason: %q\n"+
			"  a trigger that has never fired has missed nothing; counting "+
			"from the schedule's unbounded past under --on-missed=run-all "+
			"would start thousands of paid runs at once", d.Runs, d.Why)
	}
}

func TestAPausedTriggerNeverFires(t *testing.T) {
	r := active()
	r.Status = StatusPaused
	r.LastFiredAt = "2026-08-25T03:00:00Z" // and four days are owed

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if d.ShouldFire {
		t.Fatal("a paused trigger fired.\n" +
			"  pause has to be honoured HERE, because a paused trigger's " +
			"schedule is still valid and still says 03:00 -- every other " +
			"check would conclude that it is due")
	}
	if d.Why != "paused" {
		t.Errorf("the reason should be plainly `paused`, got %q", d.Why)
	}
}

func TestAnEventTriggerHasNoDueness(t *testing.T) {
	r := active()
	r.On = "event:run.failed"

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if d.ShouldFire {
		t.Fatal("an event trigger was fired by the clock. Nobody knows when a " +
			"webhook is next called, so asking a cron parser is meaningless")
	}
	if !strings.Contains(d.Why, "external event") {
		t.Errorf("the reason should say it is event-driven: %q", d.Why)
	}
}

func TestATriggerThatAlreadyRanThisSlotDoesNotRunAgain(t *testing.T) {
	r := active()
	r.LastFiredAt = "2026-08-29T03:00:00Z"

	// Seven hours later, same day. The next slot is tomorrow.
	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if d.ShouldFire {
		t.Fatalf("a trigger fired twice in one slot.\n  reason: %q\n"+
			"  a scheduler ticks far more often than any schedule fires, so "+
			"this is the check that runs thousands of times a day and must "+
			"never be wrong", d.Why)
	}
	if !strings.Contains(d.Why, "2026-08-30T03:00:00Z") {
		t.Errorf("the reason should name the next firing: %q", d.Why)
	}
}

// TestTheOrdinaryOnTimeFiringIsNotTreatedAsABacklog is the subtle one, and the
// bug it guards against would silently stop every trigger in the system.
//
// Under --on-missed=skip, the firing due RIGHT NOW is itself one of the
// "missed" ones as far as Missed() is concerned: one slot has elapsed since the
// last run. Dropping it along with a genuine backlog would mean a trigger that
// fires once and then never again.
func TestTheOrdinaryOnTimeFiringIsNotTreatedAsABacklog(t *testing.T) {
	r := active() // --on-missed=skip
	r.LastFiredAt = "2026-08-28T03:00:00Z"

	// One day later, on time. Exactly one slot has elapsed: this one.
	d, err := Due(r, rfc("2026-08-29T03:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.ShouldFire {
		t.Fatalf("the ordinary on-time firing was skipped as a backlog.\n"+
			"  reason: %q\n"+
			"  under `skip` this is the difference between a working "+
			"scheduler and one that fires each trigger exactly once and then "+
			"goes quiet forever", d.Why)
	}
	if d.Runs != 1 {
		t.Errorf("want 1 run, got %d", d.Runs)
	}
}

func TestSkipDropsARealBacklogAndSaysHowMuch(t *testing.T) {
	r := active() // --on-missed=skip
	r.LastFiredAt = "2026-08-25T03:00:00Z"

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if d.ShouldFire {
		t.Errorf("--on-missed=skip ran a backlog: %q", d.Why)
	}
	if d.Runs != 0 {
		t.Errorf("want 0 runs under skip, got %d", d.Runs)
	}
	// The count survives the policy that discarded it. This is the sentence an
	// operator who believes their automation is healthy needs to read.
	if d.Missed != 4 {
		t.Errorf("want 4 missed firings reported, got %d", d.Missed)
	}
	if !strings.Contains(d.Why, "4") || !strings.Contains(d.Why, "skip") {
		t.Errorf("the reason must say how many were dropped and why, or a "+
			"scheduler honouring skip is indistinguishable from one that was "+
			"never down: %q", d.Why)
	}
}

func TestRunOnceCollapsesABacklogToASingleRun(t *testing.T) {
	r := active()
	r.OnMissed = MissedRunOnce
	r.LastFiredAt = "2026-08-25T03:00:00Z"

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.ShouldFire {
		t.Fatalf("run-once did not run: %q", d.Why)
	}
	if d.Runs != 1 {
		t.Errorf("run-once must be exactly 1 run, got %d -- the useful middle "+
			"is that a missed nightly audit audits TODAY, not four times", d.Runs)
	}
	if d.Missed != 4 {
		t.Errorf("want the 4 missed firings still reported, got %d", d.Missed)
	}
}

func TestRunAllOwesOneRunPerMissedFiring(t *testing.T) {
	r := active()
	r.OnMissed = MissedRunAll
	r.LastFiredAt = "2026-08-25T03:00:00Z"

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.ShouldFire || d.Runs != 4 {
		t.Fatalf("want 4 runs under run-all, got fire=%v runs=%d (%q)",
			d.ShouldFire, d.Runs, d.Why)
	}
	// §20.10 calls this the dangerous option. The count belongs in the
	// decision so the caller can say "4 runs at 5.00 each" rather than
	// discovering it one invocation at a time.
	if !strings.Contains(d.Why, "4") {
		t.Errorf("the reason should carry the number of runs: %q", d.Why)
	}
}

func TestACappedBacklogIsReportedAsAtLeast(t *testing.T) {
	r := active()
	r.On = "every:1m" // 1440 slots a day, so a week is well past the cap
	r.OnMissed = MissedSkip
	r.LastFiredAt = "2026-08-01T00:00:00Z"

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.MissedCapped {
		t.Fatalf("a month of minute firings was not reported as capped "+
			"(missed=%d)", d.Missed)
	}
	// "1000 firings" and "at least 1000 firings" are different claims, and only
	// the second is true.
	if !strings.Contains(d.Why, "at least") {
		t.Errorf("a capped count must not be stated as exact: %q", d.Why)
	}
}

// TestAnInvalidStoredTriggerIsRefusedRatherThanSkipped matters because the
// alternative is silent. A trigger whose schedule the current build would
// refuse to create, still sitting on disk, is a trigger the operator believes
// is running.
func TestAnInvalidStoredTriggerIsRefusedRatherThanSkipped(t *testing.T) {
	r := active()
	r.On = "cron:0 3 * * MON" // a spelling the parser refuses

	_, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err == nil {
		t.Fatal("a trigger the parser refuses was treated as a normal " +
			"not-due trigger.\n" +
			"  silently skipping it means the operator sees an active row " +
			"that will never fire, and nothing ever says why")
	}
}

func TestATriggerWithNoFutureIsReportedNotErrored(t *testing.T) {
	r := active()
	r.On = "at:2026-08-01T03:00:00Z" // a one-shot, already past
	r.LastFiredAt = "2026-08-01T03:00:00Z"

	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err != nil {
		t.Fatalf("a spent one-shot is the normal end of its life, not an "+
			"error: %v", err)
	}
	if d.ShouldFire {
		t.Errorf("a spent one-shot fired again: %q", d.Why)
	}
	if !strings.Contains(d.Why, "no firing left") {
		t.Errorf("the reason should say the schedule is spent: %q", d.Why)
	}
}

// TestAOneShotFiresOnceWhenItsInstantArrives is the positive case for `at:`,
// which is the schedule most easily broken by the first-firing branch.
func TestAOneShotFiresOnceWhenItsInstantArrives(t *testing.T) {
	r := active()
	r.On = "at:2026-08-29T03:00:00Z"
	r.CreatedAt = "2026-08-28T00:00:00Z"

	d, err := Due(r, rfc("2026-08-29T03:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.ShouldFire || d.Runs != 1 {
		t.Fatalf("a one-shot did not fire at its instant: fire=%v runs=%d (%q)",
			d.ShouldFire, d.Runs, d.Why)
	}
}

func TestACreatedAtThatIsNotATimeIsRefusedWithTheReason(t *testing.T) {
	r := active()
	r.CreatedAt = "yesterday"

	_, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	if err == nil {
		t.Fatal("a trigger with an unparseable created_at was scheduled anyway")
	}
	// The message has to explain the consequence, because the two ways of
	// guessing are both bad in opposite directions.
	if !strings.Contains(err.Error(), "created_at") {
		t.Errorf("the error should name the field: %v", err)
	}
}

// TestEveryDecisionCarriesAReason is a shape test over the whole surface.
//
// A Decision with an empty Why is a scheduler log line that says a trigger did
// not fire and offers nothing to act on, which is how "my automation stopped"
// becomes unanswerable.
func TestEveryDecisionCarriesAReason(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Record)
	}{
		{"paused", func(r *Record) { r.Status = StatusPaused }},
		{"event", func(r *Record) { r.On = "event:run.failed" }},
		{"not yet due", func(r *Record) { r.LastFiredAt = "2026-08-29T03:00:00Z" }},
		{"first firing", func(r *Record) {}},
		{"skipped backlog", func(r *Record) {
			r.LastFiredAt = "2026-08-20T03:00:00Z"
		}},
		{"run-all backlog", func(r *Record) {
			r.OnMissed = MissedRunAll
			r.LastFiredAt = "2026-08-20T03:00:00Z"
		}},
		{"run-once backlog", func(r *Record) {
			r.OnMissed = MissedRunOnce
			r.LastFiredAt = "2026-08-20T03:00:00Z"
		}},
		{"spent one-shot", func(r *Record) {
			r.On = "at:2026-01-01T03:00:00Z"
			r.LastFiredAt = "2026-01-01T03:00:00Z"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := active()
			c.mod(&r)
			d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
			if err != nil {
				t.Fatalf("due: %v", err)
			}
			if strings.TrimSpace(d.Why) == "" {
				t.Error("a decision with no reason. Every scheduler log line " +
					"needs to say why, including the ones that fire")
			}
		})
	}
}

// TestRunsIsZeroWheneverShouldFireIsFalse pins the invariant between the two
// fields. A caller looping `for i := 0; i < d.Runs; i++` and a caller checking
// `if d.ShouldFire` must never disagree about whether something happens.
func TestRunsIsZeroWheneverShouldFireIsFalse(t *testing.T) {
	mods := []func(*Record){
		func(r *Record) { r.Status = StatusPaused },
		func(r *Record) { r.On = "event:run.failed" },
		func(r *Record) { r.LastFiredAt = "2026-08-29T03:00:00Z" },
		func(r *Record) { r.LastFiredAt = "2026-08-20T03:00:00Z" }, // skip drops it
	}
	for i, mod := range mods {
		r := active()
		mod(&r)
		d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !d.ShouldFire && d.Runs != 0 {
			t.Errorf("case %d: ShouldFire is false but Runs is %d", i, d.Runs)
		}
		if d.ShouldFire && d.Runs < 1 {
			t.Errorf("case %d: ShouldFire is true but Runs is %d", i, d.Runs)
		}
	}
}

// TestDueReadsNoClockOfItsOwn is the property that makes every test above
// possible, checked behaviourally rather than by reading imports (the arch test
// covers the imports).
//
// The same record at the same instant must decide the same thing, no matter
// when the test runs -- including on a leap day, at 23:59, or on the machine of
// somebody in a different timezone.
func TestDueReadsNoClockOfItsOwn(t *testing.T) {
	r := active()
	r.LastFiredAt = "2026-08-28T03:00:00Z"
	now := rfc("2026-08-29T03:00:00Z")

	first, err := Due(r, now)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	// A local-time `now` for the same instant must not change the answer: a
	// scheduler on a machine in UTC+13 fires at the same moment as one in UTC.
	shifted := now.In(time.FixedZone("UTC+13", 13*3600))
	second, err := Due(r, shifted)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if first.ShouldFire != second.ShouldFire || first.Runs != second.Runs {
		t.Errorf("the same instant in another zone decided differently:\n"+
			"  UTC:    fire=%v runs=%d (%q)\n"+
			"  UTC+13: fire=%v runs=%d (%q)",
			first.ShouldFire, first.Runs, first.Why,
			second.ShouldFire, second.Runs, second.Why)
	}

	// The REASON too, and this half is what the assertion above cannot see.
	//
	// Mutation testing deleted the `now = now.UTC()` normalisation and every
	// test still passed: the decisions are all instant comparisons, which are
	// zone-independent, so ShouldFire and Runs are identical either way. What
	// changes is the timestamp printed in Why -- a scheduler on a laptop in
	// UTC+13 would log "due at 2026-08-29T16:00:00+13:00" for a firing an
	// operator configured as 03:00, and two machines' logs of the same event
	// would not be comparable by eye or by grep.
	if first.Why != second.Why {
		t.Errorf("the reason changed with the caller's timezone:\n"+
			"  UTC:    %q\n  UTC+13: %q\n"+
			"  a trigger's schedule is written in UTC, so the instant it "+
			"reports must be too", first.Why, second.Why)
	}
}

// TestALeapDayIsAnOrdinaryTestHere is the case the arch rule about the wall
// clock exists to make reachable. Waiting for 29 February is not a test
// strategy.
func TestALeapDayIsAnOrdinaryTestHere(t *testing.T) {
	r := active()
	r.On = "cron:0 3 29 2 *" // 03:00 on 29 February
	r.CreatedAt = "2024-01-01T00:00:00Z"

	d, err := Due(r, rfc("2024-02-29T03:00:00Z"))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if !d.ShouldFire {
		t.Errorf("a leap-day schedule did not fire on a leap day: %q", d.Why)
	}
}

// TestAFirstFiringOnAnAncientTriggerIsCheapToDecide is a cost test, which is
// unusual here, and it exists because the honest implementation of "which slot
// am I acting on" is a walk whose length is the gap since creation.
//
// `every:1m` created six years ago is 3.5 million steps. Measured once that is
// ~76ms -- survivable. But a scheduler asks this for every trigger on every
// tick, so a few such records turn into seconds of spin per minute, forever,
// to compute a timestamp that only appears in a log line.
func TestAFirstFiringOnAnAncientTriggerIsCheapToDecide(t *testing.T) {
	r := active()
	r.On = "every:1m"
	r.CreatedAt = "2020-01-01T00:00:00Z"

	start := time.Now()
	d, err := Due(r, rfc("2026-08-29T10:00:00Z"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	// The decision must still be one run -- a never-fired trigger has missed
	// nothing, so the bound must not turn into a backlog.
	if !d.ShouldFire || d.Runs != 1 {
		t.Fatalf("want exactly one run, got fire=%v runs=%d (%q)",
			d.ShouldFire, d.Runs, d.Why)
	}
	// Generous, because this asserts a bound and not a benchmark: the
	// unbounded version was 76ms and the bounded one is microseconds, so
	// anything in this range distinguishes them without being flaky on a
	// loaded machine.
	if elapsed > 20*time.Millisecond {
		t.Errorf("deciding one trigger took %v, which is a per-tick cost for "+
			"every trigger in the store", elapsed)
	}
}
