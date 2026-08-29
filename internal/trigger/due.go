package trigger

import (
	"fmt"
	"time"
)

// This file answers one question: given a stored trigger and an instant, should
// it fire right now, and how many times?
//
// It is separate from record.go because Next and Missed answer questions an
// operator asks ("when will this run", "what did I lose while the laptop was
// shut") and Due answers the question the SCHEDULER asks, which is not the same
// question and has different edge cases. Keeping it here, in the pure package,
// is what makes 3am on a leap day a test rather than a wait.
//
// The scheduler that calls this owns a clock, a store and a subprocess. None of
// those appear below. Every answer is a function of the record and the `now` it
// is handed, so the interesting cases -- a trigger due for the first time, a
// four-day outage, a schedule that has run out of future -- are ordinary table
// entries.

// Decision is what the scheduler should do about one trigger at one instant.
//
// Runs is separate from ShouldFire because the two are genuinely different
// facts, and collapsing them into an int with 0 meaning "no" would lose the
// reason. A trigger can be not-firing because it is paused, because it is
// event-driven, because nothing is due yet, or because firings WERE missed and
// the policy says to drop them -- and the last of those is the only one an
// operator needs to be told about. A bare 0 cannot say which happened.
type Decision struct {
	// ShouldFire is whether to invoke the action at all.
	ShouldFire bool

	// Runs is how many invocations are owed. It is 1 in the ordinary case and
	// greater only under --on-missed=run-all after an outage.
	//
	// It is not a count of "times the schedule elapsed": under `skip` and
	// `run-once` the schedule may have elapsed forty times and this is still
	// 1 or 0. It is what the POLICY says to actually do.
	Runs int

	// Missed is how many scheduled firings elapsed unattended, whatever the
	// policy then decided to do about them.
	//
	// Reported even when the policy drops them, and that is the point: "skipped
	// 4 nightly audits" is the single most important sentence this system can
	// say to somebody who thinks their automation is healthy. A scheduler that
	// silently honoured `skip` would be indistinguishable from one that was
	// never down.
	Missed int

	// MissedCapped says the real number is at least Missed, not exactly Missed.
	MissedCapped bool

	// Why is a short reason, always set, in the operator's vocabulary rather
	// than the code's.
	//
	// Always set INCLUDING when firing, because the log line "fired
	// nightly-audit" is worth less than "fired nightly-audit (due at
	// 03:00Z)" when the question later is whether it fired at the right time.
	Why string
}

// Due decides whether a trigger fires at now.
//
// The order of the checks below is the design, and each one is a different
// question that must be asked before the next is meaningful:
//
//  1. PAUSED. Asked first because a paused trigger's schedule is still valid
//     and still says 03:00 -- so every later check would happily conclude "due".
//     Pause is the operation §20.10 chose over delete precisely so that the
//     configuration survives, which means the scheduler is the thing that has to
//     honour it.
//  2. PARSEABLE. A trigger that no longer parses must not be silently skipped.
//     It is returned as an error so the caller reports it: a schedule the
//     current build would refuse to create, still sitting on disk, is a trigger
//     the operator believes is running.
//  3. TIME-BASED. An event trigger has no due-ness in the clock. Nobody knows
//     when a webhook is next called, and asking a cron parser is meaningless.
//  4. FIRST FIRING. Handled explicitly, because Missed deliberately returns 0
//     for a never-fired trigger and it is right to: a daily trigger created a
//     month ago has not "missed" thirty runs. But that means due-ness for the
//     very first firing cannot come from Missed, and a scheduler built only on
//     Missed would never fire anything for the first time. That is the bug this
//     function exists to make impossible, and it was found by probing Missed
//     rather than by reading it.
//  5. MISSED, then the policy.
func Due(r Record, now time.Time) (Decision, error) {
	if err := r.Validate(); err != nil {
		// Validated here rather than trusted, for the same reason Record.Validate
		// runs on load: a record can come from a hand-edited file or an older
		// build. A scheduler that fires an invalid trigger is worse than one
		// that refuses, because it acts on a definition nobody can reproduce.
		return Decision{}, err
	}

	if r.Status == StatusPaused {
		return Decision{Why: "paused"}, nil
	}

	s, err := r.Spec()
	if err != nil {
		return Decision{}, err
	}
	if !s.TimeBased() {
		return Decision{Why: "fires on an external event, not a schedule"}, nil
	}

	now = now.UTC()

	// The first firing.
	//
	// The reference point is CreatedAt, and it has to be: without a last
	// firing, the only other candidate is "the beginning of the schedule",
	// which for `cron:0 3 * * *` is unbounded in the past. A trigger created at
	// 10:00 is due at the next 03:00, not at every 03:00 since the epoch.
	if r.LastFiredAt == "" {
		created, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			return Decision{}, fmt.Errorf(
				"trigger %q: created_at %q is not RFC3339: %w.\n"+
					"  the scheduler needs it to know when a never-fired "+
					"trigger became due, and guessing would either fire "+
					"immediately or never", r.Name, r.CreatedAt, err)
		}
		first, err := s.Next(created.UTC())
		if err != nil {
			// A one-shot `at:` that was already in the past when the process
			// started. Not an error: the trigger simply has no firing left,
			// and saying so is more useful than a parse failure.
			return Decision{Why: "no firing left in this schedule"}, nil
		}
		if first.After(now) {
			return Decision{Why: "not due until " + first.Format(time.RFC3339)}, nil
		}
		return Decision{ShouldFire: true, Runs: 1,
			Why: "first firing, due at " + first.Format(time.RFC3339)}, nil
	}

	missed, capped, err := r.Missed(now)
	if err != nil {
		return Decision{}, err
	}
	if missed == 0 {
		next, ok, err := r.Next(now)
		switch {
		case err != nil:
			// A schedule with no future is reported through Why rather than as
			// an error here: the trigger has already done its job. `at:` in the
			// past is the normal end of a one-shot's life.
			return Decision{Why: "no firing left in this schedule"}, nil
		case !ok:
			return Decision{Why: "no scheduled firing"}, nil
		default:
			return Decision{Why: "not due until " + next.Format(time.RFC3339)}, nil
		}
	}

	// Something was due. What to do about it is the policy's decision, and the
	// count is reported either way.
	d := Decision{Missed: missed, MissedCapped: capped}

	switch r.OnMissed {
	case MissedRunAll:
		// Every owed firing. §20.10 is explicit that this is the dangerous
		// option -- four days of a daily schedule is four simultaneous runs at
		// four times the budget, unattended -- which is why it is not the
		// default and why Runs carries the number instead of the caller
		// discovering it one invocation at a time.
		d.ShouldFire = true
		d.Runs = missed
		d.Why = fmt.Sprintf("%s owed firings, running all of them", countOf(missed, capped))

	case MissedRunOnce:
		// One run, whatever was owed. The useful middle: a nightly audit that
		// was missed for four days should audit TODAY, not four times.
		d.ShouldFire = true
		d.Runs = 1
		if missed == 1 {
			d.Why = "due"
		} else {
			d.Why = fmt.Sprintf("%s owed firings, collapsing to one run",
				countOf(missed, capped))
		}

	case MissedSkip:
		// The default, and the only safe one for unattended scheduled work.
		//
		// A subtlety worth stating: skip does NOT mean "never fire again". The
		// firing due right now is one of the missed ones, and dropping it along
		// with the outage would mean a trigger that stopped forever after a
		// single missed night. So `missed == 1` -- the ordinary on-time case,
		// where the one elapsed slot IS this firing -- fires, and only a
		// genuine backlog is dropped.
		if missed == 1 {
			d.ShouldFire = true
			d.Runs = 1
			d.Why = "due"
			return d, nil
		}
		d.ShouldFire = false
		d.Runs = 0
		d.Why = fmt.Sprintf("%s firings were missed and --on-missed=skip, "+
			"so none of them will be run", countOf(missed, capped))

	default:
		// Unreachable while Validate rejects unknown policies, and it stays
		// because the alternative is a silent no-fire: a policy this switch
		// does not know would fall through as "not due", and the trigger would
		// simply stop with nothing said.
		return Decision{}, fmt.Errorf("trigger %q: unknown --on-missed policy %q",
			r.Name, r.OnMissed)
	}

	return d, nil
}

// countOf renders a possibly-capped count honestly.
//
// "1000 firings" and "at least 1000 firings" are different claims, and the
// second is the true one when the count hit the cap. The distinction matters
// here more than in `trigger list`, because this string explains an action that
// was or was not taken.
func countOf(n int, capped bool) string {
	if capped {
		return fmt.Sprintf("at least %d", n)
	}
	return fmt.Sprintf("%d", n)
}
