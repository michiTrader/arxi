package exec

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestVirtualClockFiresOnlyWhenAdvanced is the property that makes timeouts
// testable at all: nothing happens on its own, so the test decides when time
// passes and the firing order is a fact rather than a race with the scheduler.
func TestVirtualClockFiresOnlyWhenAdvanced(t *testing.T) {
	v := NewVirtualClock()
	if err := v.SetTimer("stage:review", 30*60*1000); err != nil {
		t.Fatalf("SetTimer: %v", err)
	}

	// A real 30-minute timer must not have fired yet, and this assertion must
	// hold instantly.
	if fired, _ := v.Advance(0); len(fired) != 0 {
		t.Fatalf("a 30-minute timer fired after advancing 0 ms: %v. Virtual time "+
			"must move only when Advance is called, otherwise the clock races the "+
			"machine and the same test passes or fails depending on load.", fired)
	}

	fired, err := v.Advance(30 * 60 * 1000)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !reflect.DeepEqual(fired, []string{"stage:review"}) {
		t.Fatalf("got %v, want [stage:review]. A thirty-minute timeout has to be "+
			"reachable in microseconds; if it is not, nobody keeps the suite and "+
			"the timeout paths first run on a real run at 3am.", fired)
	}
}

// TestTiedTimersFireInDeterministicOrder. Go randomizes map iteration
// deliberately, so without an explicit tie-break two timers armed for the same
// instant would fire in a different order on each execution, and --sim would
// produce a different log from identical input.
//
// Repeated, because a randomness bug that shows up one run in two passes a
// single-shot test half the time.
func TestTiedTimersFireInDeterministicOrder(t *testing.T) {
	want := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < 50; i++ {
		v := NewVirtualClock()
		for _, id := range []string{"c", "e", "a", "d", "b"} {
			if err := v.SetTimer(id, 100); err != nil {
				t.Fatalf("SetTimer: %v", err)
			}
		}
		fired, err := v.Advance(100)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if !reflect.DeepEqual(fired, want) {
			t.Fatalf("iteration %d fired %v, want %v. Timers due at the same instant "+
				"must be tie-broken by id: Go randomizes map iteration, so without "+
				"the sort two --sim runs over identical input produce different "+
				"logs and `run diff` reports scheduling noise as a real change. "+
				"Remedy: keep the sort.Slice in VirtualClock.Advance.", i, fired, want)
		}
	}
}

// TestEarlierTimerFiresBeforeLaterOne: within one Advance, the timer whose
// deadline came first must be delivered first. Otherwise a stage that expired
// at t=100 could be logged after one that expired at t=900, and the causal
// chain in the log would describe an impossible history.
func TestEarlierTimerFiresBeforeLaterOne(t *testing.T) {
	v := NewVirtualClock()
	_ = v.SetTimer("late", 900)
	_ = v.SetTimer("early", 100)

	fired, _ := v.Advance(1000)
	want := []string{"early", "late"}
	if !reflect.DeepEqual(fired, want) {
		t.Fatalf("got %v, want %v: timers must be delivered in deadline order, "+
			"otherwise the log records a stage expiring before one that expired "+
			"earlier and every caused_by chain built on it is wrong.", fired, want)
	}
}

// TestReArmingATimerOverwritesIt. Re-entering a stage legitimately re-arms its
// timeout. If the second arming were rejected or ignored, the stage would keep
// the deadline from its previous entry and expire early for a reason nobody
// could reconstruct from the log.
func TestReArmingATimerOverwritesIt(t *testing.T) {
	v := NewVirtualClock()
	_ = v.SetTimer("stage:build", 100)
	if _, err := v.Advance(50); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Re-entered the stage: fresh 100 ms from NOW (t=50), so due at t=150.
	if err := v.SetTimer("stage:build", 100); err != nil {
		t.Fatalf("re-arming an existing timer failed: %v. A stage re-entry re-arms "+
			"its timeout, and rejecting that leaves the stage on its old deadline.", err)
	}

	if fired, _ := v.Advance(50); len(fired) != 0 { // t=100, old deadline
		t.Fatalf("the timer fired at the OLD deadline (%v): re-arming must replace "+
			"the deadline, otherwise a re-entered stage expires early and the log "+
			"gives no clue why.", fired)
	}
	if fired, _ := v.Advance(50); !reflect.DeepEqual(fired, []string{"stage:build"}) { // t=150
		t.Fatalf("got %v, want [stage:build] at the NEW deadline", fired)
	}
}

// TestCancellingAnUnknownTimerSucceeds. The reducer cancels defensively: a
// stage that advanced before its timeout never armed one. Erroring would force
// every call site into a check-then-act race and make correct reducer code look
// broken.
func TestCancellingAnUnknownTimerSucceeds(t *testing.T) {
	for name, c := range map[string]Clock{
		"virtual": NewVirtualClock(),
		"real":    NewRealClock(),
	} {
		if err := c.CancelTimer("never-armed"); err != nil {
			t.Errorf("%s clock: cancelling an unknown timer failed: %v. The reducer "+
				"cancels defensively, so this must be a no-op; erroring forces a "+
				"check-then-act race at every call site.", name, err)
		}
	}
}

// TestCancelRemovesAnAlreadyFiredTimer is the one that protects the reason
// CancelTimer is a CONTROL effect. If a timer that fired in the same step in
// which it was cancelled were still delivered, the run would process a timeout
// for a stage that had already advanced: the double-outcome race ADR-0003 was
// written to prevent.
func TestCancelRemovesAnAlreadyFiredTimer(t *testing.T) {
	v := NewVirtualClock()
	_ = v.SetTimer("stage:review", 100)
	if fired, _ := v.Advance(100); len(fired) != 1 {
		t.Fatalf("setup: expected the timer to fire, got %v", fired)
	}

	// The stage advanced in this same step, so the reducer cancels.
	if err := v.CancelTimer("stage:review"); err != nil {
		t.Fatalf("CancelTimer: %v", err)
	}

	if got := v.TakeFired(); len(got) != 0 {
		t.Fatalf("TakeFired still returned %v after the timer was cancelled. A "+
			"timer cancelled in the same step in which it fired must NOT be "+
			"delivered: otherwise the run handles a timeout for a stage that "+
			"already advanced, and the run ends in one of two different ways "+
			"depending on luck. Remedy: keep the fired-queue filtering in "+
			"CancelTimer.", got)
	}
}

// TestTakeFiredDrains. If reading did not consume, a caller that advanced twice
// before reading would emit the first batch of timeouts a second time, and the
// log would claim a stage expired twice.
func TestTakeFiredDrains(t *testing.T) {
	v := NewVirtualClock()
	_ = v.SetTimer("t1", 100)
	_, _ = v.Advance(100)

	first := v.TakeFired()
	if len(first) != 1 {
		t.Fatalf("first drain got %v, want 1 timer", first)
	}
	if second := v.TakeFired(); len(second) != 0 {
		t.Fatalf("second drain returned %v, want nothing. TakeFired must CONSUME: "+
			"if reading only peeked, a caller that advanced twice before reading "+
			"would emit the same timeout twice and the log would claim a stage "+
			"expired twice.", second)
	}
}

// TestNonPositiveOffsetIsRefused. Firing such a timer would make it land inside
// the step that armed it, so the reducer would see a stage expire before it was
// entered. Refusing points at the real bug, which is a deadline computed from
// stale values.
func TestNonPositiveOffsetIsRefused(t *testing.T) {
	for name, c := range map[string]Clock{
		"virtual": NewVirtualClock(),
		"real":    NewRealClock(),
	} {
		for _, offset := range []int64{0, -1, -5000} {
			if err := c.SetTimer("t", offset); err == nil {
				t.Errorf("%s clock accepted an offset of %d ms. It would fire inside "+
					"the step that armed it, making a stage expire before it was "+
					"entered; the real bug is a deadline computed from stale values, "+
					"and accepting the timer hides it.", name, offset)
			}
		}
		if err := c.SetTimer("", 100); err == nil {
			t.Errorf("%s clock accepted an empty timer id. An unnamed timer cannot be "+
				"cancelled or matched to a stage, so it fires with no way to trace "+
				"why.", name)
		}
	}
}

// TestClockCannotGoBackwards: time moving backwards would let an already-fired
// timer fire again.
func TestClockCannotGoBackwards(t *testing.T) {
	v := NewVirtualClock()
	if _, err := v.Advance(-1); err == nil {
		t.Fatal("Advance(-1) succeeded. Moving virtual time backwards would let an " +
			"already-fired timer fire a second time, so the same stage would time " +
			"out twice from one arming.")
	}
}

// TestBothClocksSatisfyTheSameContract. The two must accept and reject exactly
// the same inputs, otherwise a blueprint that simulates cleanly can still fail
// on a real run, and --sim stops being a safe place to find that out.
func TestBothClocksSatisfyTheSameContract(t *testing.T) {
	// Compile-time proof that both implement Clock, so the run loop is one code
	// path in --sim and in run.
	var _ Clock = NewVirtualClock()
	var _ Clock = NewRealClock()

	// RealClock is driven through its injectable Now, so this stays fast: a
	// clock test that actually waits is a clock test that gets skipped.
	base := time.Unix(0, 0)
	now := base
	r := NewRealClock()
	r.Now = func() time.Time { return now }

	if err := r.SetTimer("stage:build", 30*60*1000); err != nil {
		t.Fatalf("SetTimer: %v", err)
	}
	if due := r.Due(); len(due) != 0 {
		t.Fatalf("RealClock fired %v before its deadline", due)
	}

	now = base.Add(30 * time.Minute)
	if due := r.Due(); !reflect.DeepEqual(due, []string{"stage:build"}) {
		t.Fatalf("RealClock.Due got %v, want [stage:build]. Now is injectable so the "+
			"real clock can be driven without waiting; a clock test that actually "+
			"waits half an hour is a clock test that gets deleted.", due)
	}
	if due := r.Due(); len(due) != 0 {
		t.Fatalf("RealClock.Due returned %v a second time: a collected deadline must "+
			"be removed, otherwise every poll re-reports the same timeout and the "+
			"log fills with duplicate expiries.", due)
	}
}

// TestRealClockTiesAreDeterministicToo. Even on a real run, two timers expiring
// in the same poll must land in the log in a reproducible order, otherwise
// replaying the same scenario yields a different causal chain.
func TestRealClockTiesAreDeterministicToo(t *testing.T) {
	base := time.Unix(0, 0)
	want := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < 50; i++ {
		now := base
		r := NewRealClock()
		r.Now = func() time.Time { return now }
		for _, id := range []string{"d", "a", "e", "b", "c"} {
			_ = r.SetTimer(id, 100)
		}
		now = base.Add(200 * time.Millisecond)
		if due := r.Due(); !reflect.DeepEqual(due, want) {
			t.Fatalf("iteration %d: RealClock.Due got %v, want %v. Ties must be broken "+
				"by id here as well; a real run whose timeout order depends on map "+
				"iteration cannot be replayed into the same causal chain.", i, due, want)
		}
	}
}

// TestClocksAreRaceFree. Both clocks are touched from the runner's parallel
// path, so concurrent access is normal operation and not an edge case.
func TestClocksAreRaceFree(t *testing.T) {
	v := NewVirtualClock()
	r := NewRealClock()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			_ = v.SetTimer(id, 100)
			_ = r.SetTimer(id, 100)
			_, _ = v.Advance(1)
			_ = r.Due()
			_ = v.Pending()
			_ = r.Pending()
			_ = v.CancelTimer(id)
			_ = r.CancelTimer(id)
			_ = v.TakeFired()
			_ = r.TakeFired()
			_ = v.NowMs()
		}(i)
	}
	wg.Wait()
}
