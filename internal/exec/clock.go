package exec

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// VirtualClock is time under our control, used by `--sim` and by every test
// that involves a timeout.
//
// It exists because of a property of the design: SetTimer carries a RELATIVE
// offset in milliseconds, never an absolute instant (see kernel.SetTimer). That
// makes a stage with a thirty-minute timeout testable in microseconds, and the
// alternative is not "slower tests", it is untested timeouts: nobody keeps a
// suite that takes half an hour, so the timeout paths would simply never run
// until a real run hit them at 3am.
//
// Time does not advance by itself here. It advances only when Advance is
// called, which is what makes the ordering of fired timers a fact of the test
// rather than a race against the machine's scheduler.
type VirtualClock struct {
	mu     sync.Mutex
	nowMs  int64
	timers map[string]int64

	// fired holds the ids that Advance released and that nobody has collected
	// yet. It is a queue and not a callback because a callback would run
	// reducer code from inside the clock, and a clock that can trigger folds is
	// a clock that can recurse into itself.
	fired []string
}

// NewVirtualClock returns a clock at t=0.
//
// Zero and not time.Now(): a simulated log whose timestamps are real wall-clock
// instants is indistinguishable at a glance from a real log, and the two must
// never be confusable. Starting at zero also makes every expected value in a
// test an absolute number instead of an offset from an unknown base.
func NewVirtualClock() *VirtualClock {
	return &VirtualClock{timers: map[string]int64{}}
}

// SetTimer arms a timer to fire afterMs milliseconds from the current virtual
// instant.
//
// Re-arming an existing id overwrites it instead of failing. That is the
// behaviour the reducer needs: re-entering a stage legitimately re-arms its
// timeout, and if the second arming were rejected the stage would still be
// holding the deadline from its previous entry, expiring early for reasons
// nobody could reconstruct from the log.
func (v *VirtualClock) SetTimer(id string, afterMs int64) error {
	if id == "" {
		return fmt.Errorf("timer id is empty: an unnamed timer cannot be cancelled " +
			"or matched to a stage, so it would fire with no way to trace why")
	}
	// A non-positive offset is refused rather than fired immediately. Firing it
	// would make the timer land inside the same step that armed it, so the
	// reducer would see a stage expire before it was ever entered; refusing
	// points at the real bug, which is a timeout computed from stale values.
	if afterMs <= 0 {
		return fmt.Errorf("timer %s has a non-positive offset (%d ms): it would fire "+
			"inside the step that armed it, making a stage expire before it was "+
			"entered; the offset is being computed from a stale deadline", id, afterMs)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.timers[id] = v.nowMs + afterMs
	return nil
}

// CancelTimer disarms a timer.
//
// Cancelling an unknown id succeeds. The reducer cancels defensively (a stage
// that advanced before its timeout never had to arm one), and turning that into
// an error would force every call site to first ask whether the timer exists,
// which is a race in itself and makes correct reducer code look broken.
func (v *VirtualClock) CancelTimer(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.timers, id)

	// A cancelled timer is also removed from the pending fired queue. Without
	// this, a timer that fired in the same step in which it was cancelled would
	// still be delivered, and the run would process a timeout for a stage that
	// had already advanced: the exact double-outcome race that made
	// CancelTimer a control effect in the first place.
	kept := v.fired[:0]
	for _, f := range v.fired {
		if f != id {
			kept = append(kept, f)
		}
	}
	v.fired = kept
	return nil
}

// Advance moves virtual time forward and returns the ids of the timers that
// fired, in firing order.
//
// Ties are broken by id, not left to map iteration order. Go randomizes map
// iteration deliberately, so without this two timers armed for the same instant
// would fire in a different order on each execution: --sim would produce a
// different log each time from identical input, which is the one thing --sim
// must not do.
func (v *VirtualClock) Advance(byMs int64) ([]string, error) {
	if byMs < 0 {
		return nil, fmt.Errorf("cannot advance the clock by %d ms: time moving "+
			"backwards would let an already-fired timer fire again", byMs)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	v.nowMs += byMs

	type due struct {
		id string
		at int64
	}
	var ready []due
	for id, at := range v.timers {
		if at <= v.nowMs {
			ready = append(ready, due{id, at})
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].at != ready[j].at {
			return ready[i].at < ready[j].at
		}
		return ready[i].id < ready[j].id
	})

	out := make([]string, 0, len(ready))
	for _, d := range ready {
		delete(v.timers, d.id)
		out = append(out, d.id)
		v.fired = append(v.fired, d.id)
	}
	return out, nil
}

// NextDeadlineMs returns how far the clock must advance for the next timer to
// fire, and whether any timer is armed at all.
//
// It returns a DELTA and not an absolute instant because that is what Advance
// consumes, and converting between the two at the call site is exactly the kind
// of arithmetic that produces an off-by-one which only shows up as a timeout
// that never fires.
//
// A timer that is already past due yields 1 rather than 0, and that floor is
// load-bearing. The run loop advances by this amount to make progress; a 0
// would let it call Advance(0) forever, each call reporting success and
// changing nothing, so a run would hang while looking busy. One millisecond
// always crosses the deadline of a timer that is already behind.
func (v *VirtualClock) NextDeadlineMs() (int64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.timers) == 0 {
		return 0, false
	}
	first := true
	var earliest int64
	for _, at := range v.timers {
		if first || at < earliest {
			earliest, first = at, false
		}
	}
	delta := earliest - v.nowMs
	if delta < 1 {
		delta = 1
	}
	return delta, true
}

// NowMs is the current virtual instant.
func (v *VirtualClock) NowMs() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.nowMs
}

// Pending returns the armed timer ids, sorted. Sorted for the same reason
// Advance sorts: an assertion over this must not depend on map iteration.
func (v *VirtualClock) Pending() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, 0, len(v.timers))
	for id := range v.timers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// TakeFired drains and returns the timers that fired since the last drain.
//
// Draining rather than peeking, because the caller turns each fired id into an
// event. If reading did not consume, a caller that advanced twice before
// reading would emit the first batch of timeouts a second time, and the log
// would claim a stage expired twice.
func (v *VirtualClock) TakeFired() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := v.fired
	v.fired = nil
	return out
}

// RealClock is the Clock used by a real `run`.
//
// Note what it does NOT do: it does not sleep, and it does not own a goroutine
// per timer. Timers are wall-clock deadlines recorded here, and the run loop
// asks which ones are due. That keeps the real clock and the virtual clock the
// same shape, which is what makes the loop in --sim and the loop in run the
// same code. A RealClock built on time.AfterFunc would need callbacks that the
// virtual clock deliberately does not have, and the two paths would diverge.
type RealClock struct {
	mu     sync.Mutex
	timers map[string]time.Time
	fired  []string

	// Now is injectable so that a test can drive RealClock itself without
	// waiting. Nil means time.Now.
	Now func() time.Time
}

// NewRealClock returns a clock backed by wall time.
func NewRealClock() *RealClock {
	return &RealClock{timers: map[string]time.Time{}}
}

func (r *RealClock) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetTimer arms a wall-clock deadline. Same validation as VirtualClock, for the
// same reasons: the two must accept and reject exactly the same inputs, or a
// blueprint that simulates cleanly could still fail on a real run.
func (r *RealClock) SetTimer(id string, afterMs int64) error {
	if id == "" {
		return fmt.Errorf("timer id is empty: an unnamed timer cannot be cancelled " +
			"or matched to a stage, so it would fire with no way to trace why")
	}
	if afterMs <= 0 {
		return fmt.Errorf("timer %s has a non-positive offset (%d ms): it would fire "+
			"inside the step that armed it, making a stage expire before it was "+
			"entered; the offset is being computed from a stale deadline", id, afterMs)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.timers[id] = r.now().Add(time.Duration(afterMs) * time.Millisecond)
	return nil
}

// CancelTimer disarms a timer, tolerating unknown ids for the same reason as
// VirtualClock.CancelTimer.
func (r *RealClock) CancelTimer(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.timers, id)

	kept := r.fired[:0]
	for _, f := range r.fired {
		if f != id {
			kept = append(kept, f)
		}
	}
	r.fired = kept
	return nil
}

// Due collects the timers whose deadline has passed, sorted by deadline then
// id. Sorted for the determinism reason given in VirtualClock.Advance: even on
// a real run, two timers expiring in the same poll must land in the log in a
// reproducible order, otherwise replaying the same scenario gives a different
// causal chain.
func (r *RealClock) Due() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	type due struct {
		id string
		at time.Time
	}
	var ready []due
	for id, at := range r.timers {
		if !at.After(now) {
			ready = append(ready, due{id, at})
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if !ready[i].at.Equal(ready[j].at) {
			return ready[i].at.Before(ready[j].at)
		}
		return ready[i].id < ready[j].id
	})

	out := make([]string, 0, len(ready))
	for _, d := range ready {
		delete(r.timers, d.id)
		out = append(out, d.id)
		r.fired = append(r.fired, d.id)
	}
	return out
}

// NextDeadlineMs returns how long until the next timer fires, and whether any
// timer is armed at all. Same contract as VirtualClock.NextDeadlineMs,
// deliberately: the run loop calls this through one interface and must not be
// able to tell which clock answered.
//
// The floor of 1 ms is the same load-bearing detail as in the virtual clock. A
// timer already past due yields 1 rather than 0, because the loop advances by
// this amount in order to make progress, and a 0 would let it wait zero
// milliseconds forever — reporting success and changing nothing while the run
// hangs looking busy.
func (r *RealClock) NextDeadlineMs() (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.timers) == 0 {
		return 0, false
	}
	now := r.now()
	first := true
	var earliest time.Time
	for _, at := range r.timers {
		if first || at.Before(earliest) {
			earliest, first = at, false
		}
	}
	delta := earliest.Sub(now).Milliseconds()
	if delta < 1 {
		delta = 1
	}
	return delta, true
}

// Sleep waits for byMs milliseconds, or until the context is cancelled.
//
// This is the ONLY blocking call in the run path, and concentrating it here is
// what keeps everything else testable without a sleep anywhere: the reducer is
// pure, the runner never waits, and the loop waits only through this method.
//
// It selects on ctx.Done rather than sleeping outright because a cancelled run
// has to stop now. A bare time.Sleep would make `run cancel` wait out the
// remainder of a thirty-minute stage timeout before noticing, and the user would
// reasonably conclude that cancel does not work.
//
// A cancelled wait returns nil, not the context error, and that distinction is
// deliberate: the wait genuinely ended, and the loop's own ctx.Err() check at the
// top of its next iteration is what turns cancellation into StopCancelled.
// Returning an error here would surface an ordinary cancellation as a failure of
// the clock.
func (r *RealClock) Sleep(ctx context.Context, byMs int64) error {
	if byMs <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(byMs) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

// TakeFired drains the fired queue, same contract as VirtualClock.TakeFired.
func (r *RealClock) TakeFired() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.fired
	r.fired = nil
	return out
}

// Pending returns the armed timer ids, sorted.
func (r *RealClock) Pending() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.timers))
	for id := range r.timers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
