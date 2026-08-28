// Package trigger parses what a trigger fires ON and computes when it fires
// NEXT.
//
// It is a separate package from the kernel because scheduling needs a calendar
// and internal/arch_test.go bans `time` in the kernel: time.Now() inside the
// reducer breaks replay and the virtual clock of --sim. That ban is not
// negotiable, so this lives outside.
//
// But the ban is only half the reason this package looks the way it does. Every
// function here that answers "when does this fire" takes `now` as a PARAMETER
// and never reads the wall clock. A cron implementation that calls time.Now()
// internally can only be tested against whenever the suite happens to run,
// which means the interesting cases — the last day of February, the minute
// before midnight, a schedule that has already been missed four times — are
// the cases that never get tested. Passing `now` in turns each of those into
// three lines.
//
// Everything here is UTC, deliberately, and that is a decision with a visible
// cost: "run at 3am" means 3am UTC, not 3am where the user lives. The
// alternative is worse. On a DST transition a local 02:30 daily trigger either
// fires twice or not at all, and the day it happens nobody is watching, which
// is the entire premise of a nightly job (§20.10). Every timestamp this package
// prints carries a trailing Z so the choice is visible at the point where it
// matters rather than documented somewhere the reader is not.
package trigger

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind is the source a trigger fires from.
//
// `on` is a prefix scheme (cron:, every:, webhook:) rather than one flag per
// source because a trigger fires from exactly ONE source. Separate --cron,
// --every and --webhook flags would make `--cron "0 3 * * *" --every 15m` a
// syntactically valid invocation whose meaning has to be invented, and whatever
// is invented will surprise somebody.
type Kind string

const (
	KindCron    Kind = "cron"    // a crontab expression
	KindEvery   Kind = "every"   // a fixed interval
	KindAt      Kind = "at"      // one instant, once
	KindWebhook Kind = "webhook" // an inbound HTTP request
	KindFile    Kind = "file"    // a path changing on disk
	KindEvent   Kind = "event"   // an event pattern from the log
)

// Spec is a parsed `--on` value.
type Spec struct {
	Kind Kind
	Raw  string // exactly what the user typed, for round-tripping into `trigger show`

	// Cron fields, set only for KindCron. Each holds the minutes/hours/... the
	// expression admits, as a set.
	minute, hour, dom, month, dow map[int]bool

	// domRestricted and dowRestricted record whether the user narrowed those
	// two fields. Cron's day-of-month/day-of-week rule needs to know, see
	// matchesDay.
	domRestricted, dowRestricted bool

	// Every is the interval for KindEvery.
	Every time.Duration

	// At is the instant for KindAt.
	At time.Time

	// Pattern is the payload for the three non-time kinds: a webhook path, a
	// file glob, an event pattern.
	Pattern string
}

// TimeBased reports whether this spec can be asked when it fires next.
//
// A webhook fires when somebody calls it and a file trigger fires when somebody
// saves a file, so their next firing is not a fact about the trigger — it is a
// fact about the outside world, and this package does not have it. The
// distinction exists because `trigger list` has a NEXT column (§20.10) and the
// alternative to admitting ignorance there is printing a number that is wrong.
func (s Spec) TimeBased() bool {
	switch s.Kind {
	case KindCron, KindEvery, KindAt:
		return true
	}
	return false
}

// ParseSpec parses an `--on` value.
//
// The colon is required even where the payload would be unambiguous. A bare
// "0 3 * * *" is recognisably cron and accepting it would be friendly exactly
// once; from then on the surface has two spellings for one thing, `trigger show`
// has to pick which to echo, and the first person to write a webhook path
// containing spaces finds out that the parser guesses.
func ParseSpec(on string) (Spec, error) {
	on = strings.TrimSpace(on)
	if on == "" {
		return Spec{}, fmt.Errorf("--on is empty: a trigger with no source " +
			"would be created, stored and never fire, and nothing would ever " +
			"report it as broken.\n" +
			"  examples: cron:0 3 * * *   every:15m   at:2026-09-01T03:00:00Z\n" +
			"            webhook:/deploy   file:./src   event:stage.failed")
	}

	scheme, payload, ok := strings.Cut(on, ":")
	if !ok {
		return Spec{}, fmt.Errorf("--on %q has no scheme.\n"+
			"  what to write: one of cron: every: at: webhook: file: event: "+
			"followed by its payload, e.g. cron:%s\n"+
			"  why the prefix is required even when the payload looks "+
			"unmistakable: without it the parser has to guess, and it will "+
			"eventually guess wrong on a payload that contains a space or a slash",
			on, on)
	}
	scheme = strings.TrimSpace(scheme)
	payload = strings.TrimSpace(payload)

	switch Kind(scheme) {
	case KindCron:
		return parseCron(on, payload)
	case KindEvery:
		return parseEvery(on, payload)
	case KindAt:
		return parseAt(on, payload)
	case KindWebhook, KindFile, KindEvent:
		if payload == "" {
			return Spec{}, fmt.Errorf("--on %q has the scheme %q and no payload.\n"+
				"  a %s trigger with nothing to match fires on everything or on "+
				"nothing, and which one it turns out to be is a property of the "+
				"implementation rather than of anything you wrote",
				on, scheme, scheme)
		}
		return Spec{Kind: Kind(scheme), Raw: on, Pattern: payload}, nil
	}

	return Spec{}, fmt.Errorf("--on %q uses the unknown scheme %q.\n"+
		"  known schemes: cron: every: at: webhook: file: event:\n"+
		"  this is refused rather than stored because a trigger whose source "+
		"nothing recognises is a row in a list that will never fire, and it "+
		"looks exactly like one that will", on, scheme)
}

// minEvery is the floor on `every:`.
//
// One minute, because the scheduler wakes on a minute boundary to evaluate cron,
// and a trigger asking to fire every thirty seconds would fire every sixty while
// reporting the thirty it was given. There is also a budget argument: `every:30s`
// is 2880 runs a day, and the ceiling declared on this command is per DAY
// (§20.10), so the interval that looks like a small number is the one that
// exhausts the budget before breakfast.
const minEvery = time.Minute

func parseEvery(raw, payload string) (Spec, error) {
	d, err := time.ParseDuration(payload)
	if err != nil {
		return Spec{}, fmt.Errorf("--on %q: %q is not a duration.\n"+
			"  what to write: a number and a unit, e.g. every:15m, every:2h, every:24h",
			raw, payload)
	}
	if d < minEvery {
		return Spec{}, fmt.Errorf("--on %q asks to fire every %s, and the floor is "+
			"%s.\n"+
			"  why: the scheduler evaluates on the minute, so a shorter interval "+
			"would fire every minute while the trigger claims %s — and the "+
			"budget on this command is per day, which %s would spend in an hour",
			raw, d, minEvery, d, d)
	}
	return Spec{Kind: KindEvery, Raw: raw, Every: d}, nil
}

func parseAt(raw, payload string) (Spec, error) {
	t, err := time.Parse(time.RFC3339, payload)
	if err != nil {
		return Spec{}, fmt.Errorf("--on %q: %q is not an RFC3339 instant.\n"+
			"  what to write: at:2026-09-01T03:00:00Z\n"+
			"  the Z is not optional: an instant with no zone is a different "+
			"instant on every machine that reads it, and the whole point of "+
			"scheduling something is that it happens once", raw, payload)
	}
	return Spec{Kind: KindAt, Raw: raw, At: t.UTC()}, nil
}

// cronField describes one column, for error messages and range checking.
type cronField struct {
	name     string
	min, max int
}

var cronFields = []cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

func parseCron(raw, payload string) (Spec, error) {
	f := strings.Fields(payload)
	if len(f) != 5 {
		return Spec{}, fmt.Errorf("--on %q has %d cron fields and needs 5.\n"+
			"  order: minute hour day-of-month month day-of-week\n"+
			"  e.g. cron:0 3 * * *  is 03:00 UTC every day\n"+
			"  five and not six: this parser has no seconds column. A crontab "+
			"line pasted from a system crontab works; one pasted from a "+
			"six-field dialect would silently shift every field by one, so it "+
			"is refused here instead", raw, len(f))
	}

	s := Spec{Kind: KindCron, Raw: raw}
	sets := make([]map[int]bool, 5)
	for i, spec := range f {
		set, err := parseCronField(spec, cronFields[i])
		if err != nil {
			return Spec{}, fmt.Errorf("--on %q, %s field: %w", raw, cronFields[i].name, err)
		}
		sets[i] = set
	}
	s.minute, s.hour, s.dom, s.month, s.dow = sets[0], sets[1], sets[2], sets[3], sets[4]
	s.domRestricted = f[2] != "*"
	s.dowRestricted = f[4] != "*"

	if err := s.checkTheCalendarAllowsIt(raw); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// checkTheCalendarAllowsIt refuses a date that cannot occur, at parse time.
//
// Next() already reports this by exhausting its four-year search, but that
// answer arrives too late to be the one that matters. `trigger create` validates
// a record, and validation has no clock on purpose — a schedule whose legality
// depended on when it was submitted would be a different thing on Tuesday. So
// without this check `cron:0 3 30 2 *` is STORED, and February 30th never comes:
// the trigger sits in `trigger list` marked active, and the only evidence that
// anything is wrong is a NEXT column that has to be read and understood. The
// README already claimed this was refused at create time; it was not.
//
// Impossibility here is a property of the expression, not of the moment, which
// is why it can be decided with no `now` at all: if every allowed month is
// shorter than every allowed day-of-month, no year helps.
//
// It applies only when day-of-week is unrestricted. With both day fields
// restricted, cron's rule is EITHER matching, so `0 3 30 2 MON`-shaped
// expressions still fire on Mondays in February and are perfectly valid.
func (s Spec) checkTheCalendarAllowsIt(raw string) error {
	if !s.domRestricted || s.dowRestricted {
		return nil
	}
	// Longest each month can be in any year. February is 29 and not 28: `0 3
	// 29 2 *` is a real leap-day schedule and must survive.
	longest := map[int]int{1: 31, 2: 29, 3: 31, 4: 30, 5: 31, 6: 30,
		7: 31, 8: 31, 9: 30, 10: 31, 11: 30, 12: 31}

	for m := 1; m <= 12; m++ {
		if !s.month[m] {
			continue
		}
		for d := 1; d <= longest[m]; d++ {
			if s.dom[d] {
				return nil // one possible date is enough
			}
		}
	}
	return fmt.Errorf("--on %q names a date that does not exist in any year.\n"+
		"  e.g. cron:0 3 30 2 * is February 30th, and cron:0 3 31 4 * is April "+
		"31st: both have five valid fields and describe an instant the calendar "+
		"never reaches.\n"+
		"  refused here rather than stored: a stored one would sit in `trigger "+
		"list` marked active and never once run.\n"+
		"  note 29 February IS accepted — it happens, just not every year", raw)
}

// parseCronField expands one column into the set of values it admits.
//
// Supported: * , - and /. NOT supported: names (JAN, MON), @daily, and the
// nonstandard L/W/# extensions. Names are refused with the number to use rather
// than accepted, because "MON" is 1 in most implementations and there is at
// least one where the week starts on Monday=0; a schedule that runs on the wrong
// day of the week for a month before anyone notices is the failure this avoids.
func parseCronField(spec string, f cronField) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%q has an empty element: a stray comma makes "+
				"the field mean either \"and also nothing\" or \"and also "+
				"everything\" depending on the implementation", spec)
		}

		body, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(strings.TrimSpace(stepStr))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%q has the step %q, which must be a "+
					"positive whole number (e.g. */15)", part, stepStr)
			}
			step = n
		}

		lo, hi := f.min, f.max
		switch {
		case body == "*":
			// the full range, already assigned
		case strings.Contains(body, "-"):
			a, b, _ := strings.Cut(body, "-")
			var err error
			if lo, err = cronNumber(a, f); err != nil {
				return nil, err
			}
			if hi, err = cronNumber(b, f); err != nil {
				return nil, err
			}
			if lo > hi {
				return nil, fmt.Errorf("the range %q runs backwards. Some cron "+
					"dialects read this as a wrap-around (%d..%d then %d..%d) "+
					"and others as empty, so it is refused rather than guessed; "+
					"write the two ranges separated by a comma if a wrap is "+
					"what you meant", body, lo, f.max, f.min, hi)
			}
		default:
			n, err := cronNumber(body, f)
			if err != nil {
				return nil, err
			}
			lo, hi = n, n
			if hasStep {
				// `5/10` means 5,15,25,... in Vixie cron. Accepted, because a
				// crontab line using it must keep its meaning here.
				hi = f.max
			}
		}

		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%q admits no values, so this schedule can never "+
			"fire", spec)
	}
	return out, nil
}

func cronNumber(s string, f cronField) (int, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		// The name case is worth its own message: it is the mistake a user
		// makes by pasting a crontab that a different cron accepted.
		if isAlpha(s) {
			return 0, fmt.Errorf("%q is a name, and this parser takes numbers "+
				"only.\n  the %s field is %d-%d; write the number, because "+
				"dialects disagree on whether the week starts at Sunday=0 or "+
				"Monday=0 and a wrong day of the week goes unnoticed for weeks",
				s, f.name, f.min, f.max)
		}
		return 0, fmt.Errorf("%q is not a number (the %s field is %d-%d)",
			s, f.name, f.min, f.max)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%d is outside the %s field's range %d-%d",
			n, f.name, f.min, f.max)
	}
	return n, nil
}

func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// cronHorizon caps the search for the next firing.
//
// The cap exists so the search TERMINATES; deciding whether an expression is
// possible at all is checkTheCalendarAllowsIt's job, at parse time, with no
// clock. Those two were conflated in the first version of this file and the
// horizon was set to four years — "the shortest span guaranteed to contain a 29
// February" — which is wrong twice over. It is wrong as arithmetic: 2100 is not
// a leap year, so asked in March 2096 the next 29 February is 2104, EIGHT years
// out, and `cron:0 3 29 2 *` — a legal schedule, already accepted by the parser
// — was reported as one that "never fires". And it was wrong as design, because
// a horizon that doubles as the impossibility test has to be tight to be
// useful, which is exactly the pressure that made it too tight.
//
// Nine years now: comfortably past the widest real gap between leap days (the
// eight years spanning a skipped century) with room that costs nothing, since
// matchesDay skips whole days and a hopeless search is refused before it starts.
//
// A future reader tempted to shrink this: the failing case appears in the 2090s,
// so a test suite that only ever asks about dates near today stays green while
// the bug waits. TestALeapDayResolvesAcrossASkippedCentury pins it.
const cronHorizon = 9 * 366 * 24 * time.Hour

// Next returns the first firing strictly after `now`, in UTC.
//
// Strictly after, not at-or-after. Called with the instant a trigger just fired
// at, an at-or-after answer would return that same instant, and the scheduler
// would fire the trigger again immediately, forever, each firing looking
// individually correct.
func (s Spec) Next(now time.Time) (time.Time, error) {
	now = now.UTC()
	switch s.Kind {
	case KindAt:
		if !s.At.After(now) {
			return time.Time{}, fmt.Errorf("at:%s is in the past (now is %s).\n"+
				"  a one-shot whose instant has gone either fires immediately or "+
				"never, and neither is what was written down; pick a future "+
				"instant, or use cron: if it was meant to repeat",
				s.At.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		return s.At, nil

	case KindEvery:
		return now.Add(s.Every), nil

	case KindCron:
		// Minute-by-minute from the next whole minute. Truncating first matters:
		// starting from now+1m at 03:00:30 would begin the search at 03:01:30
		// and no candidate would ever land on :00, so a `0 3 * * *` schedule
		// would skip a day whenever it was asked at a non-zero second.
		t := now.Truncate(time.Minute).Add(time.Minute)
		deadline := now.Add(cronHorizon)
		for !t.After(deadline) {
			if s.matches(t) {
				return t, nil
			}
			// Whole days are skipped when the day itself cannot match. Without
			// this, `0 3 29 2 *` walks 1440 candidates per day across four
			// years — two million iterations for one answer, on a command a
			// user is waiting on.
			if !s.matchesDay(t) {
				t = t.Truncate(24 * time.Hour).Add(24 * time.Hour)
				continue
			}
			t = t.Add(time.Minute)
		}
		return time.Time{}, fmt.Errorf("%s never fires: no matching instant "+
			"exists within nine years of %s.\n"+
			"  dates that cannot occur at all (cron:0 3 30 2 * is February "+
			"30th) are refused when the schedule is parsed, so reaching this "+
			"message means the expression is possible in principle but not "+
			"inside the search window — please report it, because the window is "+
			"meant to be wider than the longest real gap between firings "+
			"(eight years, when a leap day falls across a skipped century).",
			s.Raw, now.UTC().Format(time.RFC3339))
	}

	return time.Time{}, fmt.Errorf("%s fires on an external event, so its next "+
		"firing is not a fact about the trigger.\n"+
		"  ask Spec.TimeBased() before calling Next: a webhook fires when "+
		"somebody calls it, and printing a guess in the NEXT column would be "+
		"worse than printing nothing", s.Raw)
}

// matchesDay is the day half of the cron match, split out so that Next can skip
// a whole day at a time.
//
// The day-of-month / day-of-week rule is cron's genuine oddity: when BOTH are
// restricted the expression matches when EITHER matches, not both. So
// `0 3 1 * 1` is "the 1st of the month AND every Monday", which fires far more
// often than reading it left to right suggests.
//
// This implements the standard rule rather than the intuitive one, and that is a
// deliberate choice against intuition. Users paste crontab lines. A line that
// means one thing in /etc/crontab and something narrower here would fire fewer
// times than the user has already observed it firing elsewhere, with no error to
// point at. Being surprising in the same way as every other cron is better than
// being surprising in a new way.
func (s Spec) matchesDay(t time.Time) bool {
	dom := s.dom[t.Day()]
	dow := s.dow[int(t.Weekday())]
	if s.domRestricted && s.dowRestricted {
		return dom || dow
	}
	return dom && dow
}

func (s Spec) matches(t time.Time) bool {
	return s.minute[t.Minute()] &&
		s.hour[t.Hour()] &&
		s.month[int(t.Month())] &&
		s.matchesDay(t)
}

// AmbiguousDayRule reports whether this expression restricts day-of-month and
// day-of-week at once, which is where the OR rule above surprises people.
//
// It is exported so `trigger create` can say so at create time. A trigger that
// fires more often than intended is a budget event, and the moment to mention it
// is while the person is still looking at the terminal — not in a doc comment
// they will read after the bill.
func (s Spec) AmbiguousDayRule() bool {
	return s.Kind == KindCron && s.domRestricted && s.dowRestricted
}
