package trigger

import (
	"strings"
	"testing"
)

// with returns the nightly-audit record under a given overlap policy.
//
// It reuses active() from due_test.go deliberately: the whole point of Admit is
// that it is the same record Due saw, asked a different question.
func with(o Overlap) Record {
	r := active()
	r.Overlap = o
	return r
}

// fires is the decision Due returns for an ordinary on-time firing. Written out
// rather than obtained by calling Due, so a change in Due's wording cannot make
// these tests fail for a reason that has nothing to do with overlap.
func fires() Decision {
	return Decision{ShouldFire: true, Runs: 1, Missed: 1, Why: "due at 2026-08-02T03:00:00Z"}
}

func TestAnUndueTriggerIsNotAnOverlapQuestion(t *testing.T) {
	// The ordering that matters: if the switch ran first, `parallel` would
	// start executions for slots that have not arrived yet -- a trigger that
	// fires continuously the moment anything is running.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		d := Decision{ShouldFire: false, Why: "not due until 2026-08-03T03:00:00Z"}
		for _, running := range []int{0, 1, 5} {
			a, err := Admit(with(o), d, running)
			if err != nil {
				t.Fatalf("overlap %s, running %d: %v", o, running, err)
			}
			if a.Start != 0 || a.Cancel || a.Consume {
				t.Errorf("overlap %s with %d running acted on an undue trigger: %+v",
					o, running, a)
			}
			if a.Why != d.Why {
				t.Errorf("overlap %s: reason was rewritten to %q, losing the "+
					"schedule's own answer %q", o, a.Why, d.Why)
			}
		}
	}
}

func TestEveryPolicyAgreesWhenNothingIsRunning(t *testing.T) {
	// This is the ordinary case, and the reason the four switch arms only
	// describe what they differ on.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		a, err := Admit(with(o), fires(), 0)
		if err != nil {
			t.Fatalf("overlap %s: %v", o, err)
		}
		if a.Start != 1 {
			t.Errorf("overlap %s did not start the ordinary firing: Start=%d", o, a.Start)
		}
		if a.Cancel {
			t.Errorf("overlap %s cancelled something with nothing in flight", o)
		}
		if a.Consume {
			t.Errorf("overlap %s consumed a slot it was starting", o)
		}
	}
}

func TestParallelStartsAlongsideWhatIsAlreadyRunning(t *testing.T) {
	a, err := Admit(with(OverlapParallel), fires(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Start != 1 || a.Cancel || a.Consume {
		t.Fatalf("parallel should start and nothing else: %+v", a)
	}
	if !strings.Contains(a.Why, "2 executions") {
		t.Errorf("reason does not say what it is running alongside: %q", a.Why)
	}
}

func TestCancelPreviousBothCancelsAndStarts(t *testing.T) {
	// A policy that only cancelled would be a policy that stops the trigger
	// from ever completing anything.
	a, err := Admit(with(OverlapCancelPrevious), fires(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Cancel {
		t.Error("cancel-previous did not cancel")
	}
	if a.Start != 1 {
		t.Errorf("cancel-previous cancelled without starting a replacement: Start=%d", a.Start)
	}
	if a.Consume {
		t.Error("cancel-previous consumed the slot it was starting")
	}
}

func TestCancelIsNeverSetWithoutStarting(t *testing.T) {
	// The invariant the Admission doc claims. Cancelling in order to start
	// nothing destroys paid-for work for no gain, and is the shape a bug would
	// take rather than a policy anybody asked for.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		for _, d := range []Decision{
			fires(),
			{ShouldFire: true, Runs: 4, Missed: 4, Why: "backlog"},
			{ShouldFire: false, Why: "not due"},
		} {
			for _, running := range []int{0, 1, 3} {
				a, err := Admit(with(o), d, running)
				if err != nil {
					t.Fatal(err)
				}
				if a.Cancel && a.Start == 0 {
					t.Errorf("overlap %s, %d running, %q: cancelled without starting",
						o, running, d.Why)
				}
			}
		}
	}
}

func TestQueueLeavesTheSlotUnattendedSoItComesBack(t *testing.T) {
	// The mechanism: NOT consuming is what queues it. If this ever sets
	// Consume, `queue` silently becomes `skip`.
	a, err := Admit(with(OverlapQueue), fires(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Start != 0 || a.Cancel {
		t.Fatalf("queue should start nothing and cancel nothing: %+v", a)
	}
	if a.Consume {
		t.Fatal("queue consumed the slot, which is what `skip` does: the deferred " +
			"firing will never come back")
	}
	if !strings.Contains(a.Why, "next tick") {
		t.Errorf("reason does not say the firing is still coming: %q", a.Why)
	}
}

func TestSkipConsumesTheSlotSoItDoesNotComeBack(t *testing.T) {
	// The inverse, and the pair is the whole distinction between the two
	// policies. Without Consume they are two spellings of "not now".
	a, err := Admit(with(OverlapSkip), fires(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Start != 0 || a.Cancel {
		t.Fatalf("skip should start nothing and cancel nothing: %+v", a)
	}
	if !a.Consume {
		t.Fatal("skip did not consume the slot, so the dropped firing is still due " +
			"on the next tick, forever: that is `queue`, not `skip`")
	}
}

func TestSkipAndQueueDifferOnlyInWhetherTheSlotReturns(t *testing.T) {
	// Stated as its own test because it is the property most likely to be
	// broken by a later "simplification" that notices both start nothing.
	skip, err := Admit(with(OverlapSkip), fires(), 1)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := Admit(with(OverlapQueue), fires(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if skip.Start != queue.Start || skip.Cancel != queue.Cancel {
		t.Fatalf("skip and queue should differ ONLY in Consume: %+v vs %+v", skip, queue)
	}
	if skip.Consume == queue.Consume {
		t.Fatalf("skip and queue are indistinguishable (Consume=%v both), which "+
			"means one of the two policies does not exist", skip.Consume)
	}
}

func TestABacklogIsCarriedThroughToTheStartCount(t *testing.T) {
	// Admit must not silently collapse run-all's backlog to one execution:
	// on-missed already decided how many are owed, and re-deciding here would
	// make --on-missed=run-all a lie whenever anything was in flight.
	d := Decision{ShouldFire: true, Runs: 4, Missed: 4, Why: "4 missed firings, running all"}

	a, err := Admit(with(OverlapParallel), d, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Start != 4 {
		t.Errorf("parallel dropped the backlog: Start=%d, want 4", a.Start)
	}

	a, err = Admit(with(OverlapCancelPrevious), d, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Start != 4 {
		t.Errorf("cancel-previous dropped the backlog: Start=%d, want 4", a.Start)
	}

	// And with nothing running, every policy owes all four.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		a, err := Admit(with(o), d, 0)
		if err != nil {
			t.Fatal(err)
		}
		if a.Start != 4 {
			t.Errorf("overlap %s owed 4 runs but starts %d", o, a.Start)
		}
	}
}

func TestANegativeRunningCountIsRefusedRatherThanClamped(t *testing.T) {
	// Clamping to zero would start the firing and carry on with a scheduler
	// that has demonstrably lost track of its own subprocesses; every
	// subsequent overlap answer would be fiction.
	a, err := Admit(with(OverlapSkip), fires(), -1)
	if err == nil {
		t.Fatal("a negative in-flight count was accepted")
	}
	if a != (Admission{}) {
		t.Errorf("an error came back with an actionable admission %+v: a caller "+
			"that logs the error and carries on would act on it", a)
	}
	if !strings.Contains(err.Error(), "nightly-audit") {
		t.Errorf("error does not name the trigger: %v", err)
	}
	if !strings.Contains(err.Error(), "scheduler") {
		t.Errorf("error blames the trigger rather than the scheduler bookkeeping: %v", err)
	}
}

func TestAnUnknownOverlapIsRefusedRatherThanSilentlyNeverFiring(t *testing.T) {
	// Unreachable via Validate; the point is that the failure mode if a policy
	// is ever added to the vocabulary without being taught to Admit is a loud
	// error, not a trigger that quietly stops firing.
	r := with("eventually")
	a, err := Admit(r, fires(), 1)
	if err == nil {
		t.Fatal("an unknown overlap policy was accepted, and the trigger would " +
			"silently never fire")
	}
	// Found by mutation testing: returning Admission{Start: 1} beside this
	// error survived the whole suite, because every test here only asserted
	// err != nil. A scheduler that logs a failure for one trigger and carries
	// on to the next -- which is the correct thing for it to do -- would have
	// started a run off the back of a policy nothing understands.
	if a != (Admission{}) {
		t.Errorf("an error came back with an actionable admission %+v: a caller "+
			"that logs the error and carries on would act on it", a)
	}
	if !strings.Contains(err.Error(), "eventually") {
		t.Errorf("error does not name the unhandled value: %v", err)
	}
	if !strings.Contains(err.Error(), "gap in the scheduler") {
		t.Errorf("error does not point at the real cause: %v", err)
	}
}

func TestEveryAdmissionCarriesAReason(t *testing.T) {
	// Same property Due has, for the same reason: an unattended system that
	// declines to act has to be able to say why, or the operator's only
	// evidence is an empty run list.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		for _, running := range []int{0, 1, 3} {
			for _, d := range []Decision{
				fires(),
				{ShouldFire: true, Runs: 3, Missed: 3, Why: "3 missed firings"},
				{ShouldFire: false, Why: "paused"},
			} {
				a, err := Admit(with(o), d, running)
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(a.Why) == "" {
					t.Errorf("overlap %s, %d running, %q: no reason given", o, running, d.Why)
				}
			}
		}
	}
}

func TestTheDecisionsReasonIsNeverThrownAway(t *testing.T) {
	// The overlap policy adds to the schedule's answer; it does not replace it.
	// "dropped because 1 execution still in flight" without "due at 03:00" is
	// half an explanation, and the missing half is the one that says WHICH
	// firing was dropped.
	d := fires()
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		for _, running := range []int{0, 2} {
			a, err := Admit(with(o), d, running)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(a.Why, d.Why) {
				t.Errorf("overlap %s, %d running: %q does not contain the "+
					"schedule's reason %q", o, running, a.Why, d.Why)
			}
		}
	}
}

func TestOneExecutionIsNotReportedAsExecutions(t *testing.T) {
	// Small, and deliberately tested. An operator reading "1 executions still
	// in flight" at 3am trusts the rest of the output slightly less.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		a, err := Admit(with(o), fires(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(a.Why, "1 executions") {
			t.Errorf("overlap %s: %q", o, a.Why)
		}
	}
	a, err := Admit(with(OverlapParallel), fires(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Why, "2 executions") {
		t.Errorf("plural form is wrong for 2: %q", a.Why)
	}
}

func TestAdmitReadsNoClockOfItsOwn(t *testing.T) {
	// It takes no time.Time at all, which is the strongest form of this
	// property, but the record it reads contains timestamps and a future
	// version could reach for time.Now() to compare against them. Two calls
	// with identical inputs must be identical answers.
	for _, o := range []Overlap{OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious} {
		first, err := Admit(with(o), fires(), 1)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Admit(with(o), fires(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Errorf("overlap %s is not a function of its inputs: %+v then %+v",
				o, first, second)
		}
	}
}
