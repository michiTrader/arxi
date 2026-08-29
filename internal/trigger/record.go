package trigger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is whether a trigger is currently allowed to fire.
//
// Two values and no third. `pause` is the operation §20.10 chose over delete, so
// a paused trigger keeps its configuration and its history; a "deleted" status
// would be a third state meaning "present in the file, absent from the list",
// which is a row that exists and cannot be seen.
type Status string

const (
	StatusActive Status = "active"
	StatusPaused Status = "paused"
)

// Period is the window a budget ceiling applies to.
//
// These are the values the surface declares for --budget-period. They are
// re-declared here as constants rather than compared as raw strings so that a
// typo is a compile error, and TestThePeriodsMatchTheDeclaredSurface asserts the
// two agree — a third spelling of "week" living in this file is exactly the
// duplication the rest of this package exists to avoid.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
)

// Window returns how long the budget period lasts.
//
// A month is 30 days here, not a calendar month, and that is a deliberate
// approximation with a stated bound: the ceiling is a SPEND LIMIT, and 30 days
// is never longer than any real month, so a fixed window can only ever be
// stricter than a calendar one. Erring toward the tighter ceiling is the correct
// direction for the one number in this command whose job is to stop a bill.
func (p Period) Window() (time.Duration, error) {
	switch p {
	case PeriodDay:
		return 24 * time.Hour, nil
	case PeriodWeek:
		return 7 * 24 * time.Hour, nil
	case PeriodMonth:
		return 30 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("%q is not a budget period (day, week or month).\n"+
		"  a ceiling whose window nothing agrees on is not a ceiling: the number "+
		"would be enforced over whatever span the implementation happened to "+
		"assume", p)
}

// OnMissed is what to do about firings that were due while nothing was running.
type OnMissed string

const (
	MissedSkip    OnMissed = "skip"
	MissedRunOnce OnMissed = "run-once"
	MissedRunAll  OnMissed = "run-all"
)

// Overlap is what to do when the previous execution has not finished.
type Overlap string

const (
	OverlapSkip           Overlap = "skip"
	OverlapQueue          Overlap = "queue"
	OverlapParallel       Overlap = "parallel"
	OverlapCancelPrevious Overlap = "cancel-previous"
)

// Record is a stored trigger.
//
// WHAT IS STORED AND WHAT IS DERIVED is the design decision in this type, and it
// is ADR-0002's rule ("the log is the truth, snapshots are cache") applied to a
// much smaller thing.
//
// Stored: what the user wrote, and what has happened. `On`, `Then`, the budget,
// the policies, and LastFiredAt/LastStatus. Those are facts; nothing can
// recompute them.
//
// NOT stored: the next firing. It is a function of the schedule and the current
// instant, so persisting it creates a second copy of a derivable fact — and this
// one goes stale by simply existing. A NEXT column read from disk after the
// machine was asleep for four days shows an instant in the past, and it is
// indistinguishable from a trigger whose schedule is broken. NextAt is computed
// on read, every time, by Record.Next.
type Record struct {
	Name string `json:"name"`

	// On and Then are stored as the strings the user typed, not as parsed
	// structures.
	//
	// Spec's cron fields are sets built at parse time; marshalling them would
	// write a form nobody typed and, worse, would freeze the parse RULES into
	// the file. Storing the source text means a trigger written when `MON` was
	// accepted gets re-validated against today's parser on the next load, and
	// fails loudly, instead of continuing to fire on a schedule the current
	// code would refuse to create.
	On   string `json:"on"`
	Then string `json:"then"`

	Budget       float64 `json:"budget"`
	BudgetPeriod Period  `json:"budget_period"`

	OnMissed OnMissed `json:"on_missed"`
	Overlap  Overlap  `json:"overlap"`

	Status Status `json:"status"`

	// CreatedAt is RFC3339 UTC. A string and not a time.Time so that the file
	// holds exactly the characters that were written: a round-trip through
	// time.Time silently renormalises, and a file whose bytes change when it is
	// merely read cannot be diffed against what was intended.
	CreatedAt string `json:"created_at"`

	// LastFiredAt is empty until the first firing. That empty is meaningful and
	// is why this is not a zero time.Time: "never fired" and "fired at the zero
	// instant" must be distinguishable in `trigger list`, because the first is
	// normal for a new trigger and the second means something is very wrong.
	LastFiredAt string `json:"last_fired_at,omitempty"`
	LastStatus  string `json:"last_status,omitempty"`
}

// Validate re-parses everything the user wrote and checks it still holds.
//
// Called on load as well as on create, deliberately. A stored trigger is input
// from a file that may have been edited by hand, written by an older build, or
// merged from another machine. Trusting it because it is on disk is how a
// schedule the current parser would refuse keeps firing.
func (r Record) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("a trigger has no name: `trigger show` and `trigger " +
			"pause` both take a name, so an unnamed trigger can be created and " +
			"then never inspected or stopped")
	}
	if strings.ContainsAny(r.Name, "/\\") || r.Name == "." || r.Name == ".." {
		return fmt.Errorf("trigger name %q contains a path separator.\n"+
			"  names become filenames, so this one would write outside the "+
			"trigger directory — %q could name any file on the machine",
			r.Name, r.Name)
	}
	if _, err := ParseSpec(r.On); err != nil {
		return fmt.Errorf("trigger %q: %w", r.Name, err)
	}
	if _, err := ParseAction(r.Then); err != nil {
		return fmt.Errorf("trigger %q: %w", r.Name, err)
	}

	// A ceiling of zero or less is not a small ceiling, it is a trigger that can
	// never do anything, and it is stored looking active.
	if r.Budget <= 0 {
		return fmt.Errorf("trigger %q has a budget of %.2f.\n"+
			"  --budget is the spend ceiling per period and must be positive: at "+
			"zero the trigger fires on schedule, refuses to spend, and reports "+
			"failure forever while `trigger list` shows it as active", r.Name, r.Budget)
	}
	if _, err := r.BudgetPeriod.Window(); err != nil {
		return fmt.Errorf("trigger %q: %w", r.Name, err)
	}

	switch r.OnMissed {
	case MissedSkip, MissedRunOnce, MissedRunAll:
	default:
		return fmt.Errorf("trigger %q has on-missed %q, which is not one of "+
			"skip, run-once, run-all.\n"+
			"  this decides what happens after the machine was off: `run-all` "+
			"fires one execution per missed slot, all at once, and a value "+
			"nothing recognises would leave that choice to a default nobody wrote",
			r.Name, r.OnMissed)
	}
	switch r.Overlap {
	case OverlapSkip, OverlapQueue, OverlapParallel, OverlapCancelPrevious:
	default:
		return fmt.Errorf("trigger %q has overlap %q, which is not one of "+
			"skip, queue, parallel, cancel-previous.\n"+
			"  this decides what happens when the previous execution is still "+
			"running; `parallel` means two agents writing the same workspace, so "+
			"it must be chosen rather than fallen into", r.Name, r.Overlap)
	}
	switch r.Status {
	case StatusActive, StatusPaused:
	default:
		return fmt.Errorf("trigger %q has status %q, which is not active or "+
			"paused.\n  an unrecognised status is either firing when it should "+
			"not or silent when it should not, and which one it is depends on "+
			"how the scheduler happens to compare it", r.Name, r.Status)
	}
	if r.CreatedAt == "" {
		return fmt.Errorf("trigger %q has no created_at: `trigger list` sorts and "+
			"reports by it, and a missing one sorts to the front of every list", r.Name)
	}
	if _, err := time.Parse(time.RFC3339, r.CreatedAt); err != nil {
		return fmt.Errorf("trigger %q has created_at %q, which is not RFC3339: %w",
			r.Name, r.CreatedAt, err)
	}
	if r.LastFiredAt != "" {
		if _, err := time.Parse(time.RFC3339, r.LastFiredAt); err != nil {
			return fmt.Errorf("trigger %q has last_fired_at %q, which is not "+
				"RFC3339: %w", r.Name, r.LastFiredAt, err)
		}
	}
	return nil
}

// Spec parses this record's schedule.
func (r Record) Spec() (Spec, error) { return ParseSpec(r.On) }

// Action parses this record's action.
func (r Record) Action() (Action, error) { return ParseAction(r.Then) }

// Next computes the next firing, and reports whether there is one to show.
//
// Three ways there is nothing to print in the NEXT column, and they are
// different enough that collapsing them would be a lie:
//
//   - the trigger is PAUSED. Its schedule still says 03:00, but it will not
//     fire, and printing the instant it would have fired at is the single most
//     misleading thing this table could do: an operator scanning for "is my
//     automation running" reads a future timestamp as yes.
//   - it fires on an EXTERNAL event. Nobody knows when a webhook is next called.
//   - the schedule has NO future firing (a past `at:`, an impossible date). The
//     error is returned rather than swallowed, because that is a trigger that
//     will never run again and the operator needs to be told.
func (r Record) Next(now time.Time) (time.Time, bool, error) {
	if r.Status == StatusPaused {
		return time.Time{}, false, nil
	}
	s, err := r.Spec()
	if err != nil {
		return time.Time{}, false, err
	}
	if !s.TimeBased() {
		return time.Time{}, false, nil
	}
	t, err := s.Next(now)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// Missed counts the firings that were due between the last one and now.
//
// This is what --on-missed is about, and the count is what makes the decision
// concrete: `run-all` after a four-day outage on a daily schedule means FOUR
// runs starting simultaneously, at four times the budget, unattended. §20.10
// picked `skip` as the default for exactly this, and a caller that has the
// number can say so instead of discovering it.
//
// The cap is not a performance guard. A trigger firing every minute that was
// down for a week is 10,080 missed slots, and the honest report is "more than
// the cap", because the difference between 10,080 and 20,160 changes no decision
// anybody would make — while counting them all would make `trigger list` walk a
// year of minutes per row.
const missedCap = 1000

func (r Record) Missed(now time.Time) (n int, capped bool, err error) {
	if r.LastFiredAt == "" {
		// Never fired: nothing was missed. A trigger created yesterday has not
		// "missed" every slot since its schedule began, and counting from
		// CreatedAt would make a new daily trigger created a month ago report 30
		// missed runs the moment it is loaded.
		return 0, false, nil
	}
	last, err := time.Parse(time.RFC3339, r.LastFiredAt)
	if err != nil {
		return 0, false, fmt.Errorf("trigger %q: last_fired_at %q is not RFC3339: %w",
			r.Name, r.LastFiredAt, err)
	}
	s, err := r.Spec()
	if err != nil {
		return 0, false, err
	}
	if !s.TimeBased() {
		return 0, false, nil
	}

	t := last.UTC()
	now = now.UTC()
	for n < missedCap {
		next, err := s.Next(t)
		if err != nil {
			// A schedule with no further firing has missed nothing more. This is
			// not an error here: `at:` in the past is a one-shot that already
			// happened, which is the normal end of its life.
			return n, false, nil
		}
		if next.After(now) {
			return n, false, nil
		}
		n++
		t = next
	}
	return n, true, nil
}

// Encode renders the on-disk form: indented JSON with a trailing newline.
//
// Indented because these files are meant to be read and diffed by people. A
// trigger is configuration — the whole reason §20.10 chose `pause` over delete
// is that its configuration has value — and single-line JSON makes every
// one-field change a diff of the entire record.
//
// This is a named method and NOT MarshalJSON, which was the first attempt and
// does not work: encoding/json COMPACTS whatever a MarshalJSON implementation
// returns, so json.MarshalIndent inside it is discarded with no error. The
// indentation simply was not there, and the only reason it was noticed is that
// TestTheStoredFormIsDiffable asserted on the bytes rather than trusting the
// call. Writing it as its own method also makes the store's single line of
// intent explicit, instead of hiding formatting inside an interface the caller
// does not know it is invoking.
//
// The trailing newline is for the same audience: a file without one makes
// `cat`, `tail` and diff output run into the next line of the terminal.
func (r Record) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode trigger %q: %w", r.Name, err)
	}
	return append(b, '\n'), nil
}

// SortRecords orders records by name.
//
// By name and not by creation time, because `trigger list` is something an
// operator reads repeatedly looking for one row. A list whose order changes as
// triggers are added means the row you are watching moves, and creation order is
// not a property anybody remembers. Deterministic beats chronological for
// anything read by eye.
func SortRecords(rs []Record) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
}
