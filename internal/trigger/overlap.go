package trigger

// The overlap policy: what to do when a firing comes due and the previous one
// has not finished.
//
// This is the second half of the scheduler's decision and it lives here, beside
// Due, for the reason Due lives here at all: it is a function of the record, the
// dueness, and a count. No clock, no store, no subprocess. `cancel-previous` is
// the policy most likely to be got wrong and it is the one that costs the most
// when it is — it kills work that has already been paid for — so it wants to be
// a table entry, not something exercised by starting goroutines and hoping.
//
// Due deliberately does not do this. Due answers "is this slot due" from the
// record alone; whether anything is currently RUNNING is knowledge only the
// process holding the subprocesses has. Folding the two together would mean Due
// could not be tested without inventing a fleet of fake executions, and would
// mean `trigger list` — which wants dueness and has no runner — could not call
// it.
//
// # There is no queue data structure
//
// `overlap: queue` was the policy that looked like it needed one. It does not,
// and seeing why is what makes the rest of this file small.
//
// Dueness is DERIVED from LastFiredAt on every tick (ADR-0002: nothing is
// stored that can be computed). So a firing that is not started, and whose
// LastFiredAt is therefore not advanced, is still due on the next tick. The
// store IS the queue, durably, across restarts, with no second copy of the
// truth to reconcile. An in-memory queue would add one, and would lose it on
// the first crash while the record on disk still said the slot was unattended.
//
// That is also what makes `skip` and `queue` genuinely different rather than
// two spellings of "do not start it now". Under `queue` the slot stays
// unattended and comes back. Under `skip` it must be FORGOTTEN, which means
// something has to record that it was consciously passed over — hence
// Admission.Consume below, and hence this being a three-outcome decision rather
// than a boolean.

import "fmt"

// Admission is what the scheduler should do about a firing, once both the
// schedule and the overlap policy have had their say.
//
// Three separate fields rather than one enum, because they are independently
// true: `cancel-previous` both cancels AND starts, and a policy that only
// cancelled would be a policy that silently stops a trigger from ever
// completing anything.
type Admission struct {
	// Start is how many executions to begin now. 0 means none.
	Start int

	// Cancel means stop the in-flight execution first. Only ever set when
	// Start > 0: cancelling in order to start nothing is not a policy anybody
	// asked for, and it is the shape a bug would take.
	Cancel bool

	// Consume means "advance LastFiredAt even though nothing ran".
	//
	// This is how a slot is deliberately forgotten. Without it, `skip` and
	// `queue` would behave identically — both decline to start — and the
	// skipped firing would come back on the next tick, forever, which is the
	// definition of not having skipped it.
	//
	// The scheduler records LastStatus alongside, so the reason survives into
	// `trigger list`. A slot dropped with no trace is a slot the operator will
	// assume ran.
	Consume bool

	// Why is always set, for firing and for declining alike, and it is the
	// field that makes an unattended system answerable. See Decision.Why.
	Why string
}

// Admit combines a schedule decision with the overlap policy.
//
// running is how many executions of THIS trigger are currently in flight.
// Negative is a programming error in the caller rather than a state of the
// world, and is refused rather than clamped: a count that went below zero means
// the scheduler has lost track of its own subprocesses, and every answer
// derived from it afterwards would be fiction.
func Admit(r Record, d Decision, running int) (Admission, error) {
	if running < 0 {
		return Admission{}, fmt.Errorf("trigger %q: %d executions in flight, "+
			"which is not a number of things that can exist.\n"+
			"  this is a bookkeeping bug in the scheduler, not a bad trigger: a "+
			"count below zero means a finished execution was released twice, so "+
			"the overlap policy is now being applied to a fleet that does not "+
			"exist", r.Name, running)
	}

	// Not due is not an overlap question. Answering it here would let the
	// overlap policy override the schedule, and `parallel` would start runs
	// for slots that had not arrived.
	if !d.ShouldFire {
		return Admission{Why: d.Why}, nil
	}

	// Due with nothing running is the ordinary case, and every policy agrees
	// about it. Checking this before the switch means the four policies only
	// have to describe the case they actually differ on, and a new policy added
	// later cannot get the ordinary path wrong.
	if running == 0 {
		return Admission{Start: d.Runs, Why: d.Why}, nil
	}

	switch r.Overlap {
	case OverlapParallel:
		// The user has said concurrent executions are fine. Note this is the
		// one policy under which a backlog and a slow run compound: run-all
		// with parallel can start several while several are already going.
		// That is what was asked for, and the budget is the thing that stops
		// it, which is why --budget is required at create time.
		return Admission{Start: d.Runs, Why: fmt.Sprintf(
			"%s; starting alongside %s already running (overlap: parallel)",
			d.Why, plural(running, "execution"))}, nil

	case OverlapCancelPrevious:
		// Deliberately destructive, and the message says so. The in-flight
		// execution has already spent budget; the operator chose this because
		// a fresher answer is worth more than a finished stale one (a
		// dependency audit, a status summary). Naming the cost in Why means
		// nobody discovers the trade-off by noticing runs that never end.
		return Admission{Start: d.Runs, Cancel: true, Why: fmt.Sprintf(
			"%s; cancelling %s in flight to start fresh (overlap: cancel-previous)",
			d.Why, plural(running, "execution"))}, nil

	case OverlapQueue:
		// Nothing is recorded, which is the entire mechanism: the slot stays
		// unattended, so the next tick finds it due again and it runs once the
		// previous execution has finished.
		//
		// One interaction is worth knowing and is not obvious: this composes
		// with on-missed. If the running execution outlasts several slots, the
		// queued firing is a BACKLOG by the time it can start, and on-missed
		// decides what happens to it -- under the default `skip` it is then
		// dropped as a backlog rather than run late. That is consistent (a
		// user who said "skip missed work" gets missed work skipped) but it
		// means queue+skip is not a promise that the firing will eventually
		// happen. queue+run-once is.
		return Admission{Why: fmt.Sprintf(
			"%s; deferred while %s in flight (overlap: queue), and still due "+
				"on the next tick", d.Why, plural(running, "execution"))}, nil

	case OverlapSkip:
		// Consume, not silence. The slot is marked attended so it does not
		// come back, and the reason goes to LastStatus so `trigger list` shows
		// a trigger that is working as configured rather than one that appears
		// to have run.
		return Admission{Consume: true, Why: fmt.Sprintf(
			"%s; dropped because %s still in flight (overlap: skip)",
			d.Why, plural(running, "execution"))}, nil
	}

	// Unreachable through Validate, which refuses an unknown overlap on both
	// create and load. Kept because the alternative to erroring here is a
	// switch that falls through to the zero Admission -- a trigger that
	// silently never fires and never explains why, which is the single hardest
	// failure to diagnose in a scheduler.
	return Admission{}, fmt.Errorf("trigger %q has overlap %q, which no policy "+
		"handles.\n"+
		"  this is a gap in the scheduler, not in the trigger: Validate accepted "+
		"this value, so a policy was added to the vocabulary without being "+
		"taught to Admit", r.Name, r.Overlap)
}

// plural renders a count with its noun, because "1 executions still in flight"
// in a log an operator reads at 3am is a small tax on trust.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
