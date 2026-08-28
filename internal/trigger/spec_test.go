package trigger

import (
	"strings"
	"testing"
	"time"
)

// at is a terse UTC constructor. Every expected value in this file is an
// absolute instant rather than an offset from time.Now(), which is what makes
// the interesting cases (leap day, the minute before midnight, a schedule
// already missed) writable at all.
func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func mustSpec(t *testing.T, on string) Spec {
	t.Helper()
	s, err := ParseSpec(on)
	if err != nil {
		t.Fatalf("ParseSpec(%q) failed and the rest of this test depends on it: %v", on, err)
	}
	return s
}

func mustNext(t *testing.T, on string, now time.Time) time.Time {
	t.Helper()
	n, err := mustSpec(t, on).Next(now)
	if err != nil {
		t.Fatalf("Next for %q at %s failed: %v", on, now.Format(time.RFC3339), err)
	}
	return n
}

// TestTheNightlyAuditFiresWhenTheDocumentSaysItDoes pins the one invocation
// that appears in §20.10, because that is the example a reader trusts. If the
// document and the code disagree, the document is the thing people act on and
// the code is the thing that runs, and the gap shows up as a job at the wrong
// hour with nobody watching.
func TestTheNightlyAuditFiresWhenTheDocumentSaysItDoes(t *testing.T) {
	// The document was written on 2026-08-26 and printed next: 2026-08-27 03:00Z.
	now := at(2026, time.August, 26, 12, 0)
	got := mustNext(t, "cron:0 3 * * *", now)
	want := at(2026, time.August, 27, 3, 0)
	if !got.Equal(want) {
		t.Fatalf("cron:0 3 * * * asked at %s fired next at %s, and docs/design/20-use-cases.md\n"+
			"§20.10 prints %s. One of the two is wrong and users act on the document",
			now.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextIsStrictlyAfterNow is the anti-storm test.
//
// The scheduler computes the next firing FROM the instant a trigger just fired
// at. If Next answered "at or after", it would return that same instant, the
// trigger would fire again, and each individual firing would look correct while
// the budget drained in a loop.
func TestNextIsStrictlyAfterNow(t *testing.T) {
	// Exactly on a firing instant.
	now := at(2026, time.August, 27, 3, 0)
	got := mustNext(t, "cron:0 3 * * *", now)
	if !got.After(now) {
		t.Fatalf("asked at %s (itself a firing instant), cron:0 3 * * * returned %s.\n"+
			"An answer at-or-equal to now makes the scheduler re-fire the trigger it "+
			"just ran: an unattended loop where every single firing looks right",
			now.Format(time.RFC3339), got.Format(time.RFC3339))
	}
	if want := at(2026, time.August, 28, 3, 0); !got.Equal(want) {
		t.Fatalf("expected the next day %s, got %s", want.Format(time.RFC3339), got.Format(time.RFC3339))
	}
}

// TestSecondsWithinTheMinuteDoNotSkipADay protects the Truncate in Next.
//
// Searching from now+1m instead of from the next whole minute means every
// candidate carries the current seconds, so no candidate ever lands on :00 and a
// `0 3 * * *` schedule silently skips to the following day. The bug only appears
// when the command is invoked at a non-zero second, which is almost always, and
// never in a test that builds `now` from a whole minute.
func TestSecondsWithinTheMinuteDoNotSkipADay(t *testing.T) {
	now := time.Date(2026, time.August, 27, 2, 59, 30, 0, time.UTC)
	got := mustNext(t, "cron:0 3 * * *", now)
	want := at(2026, time.August, 27, 3, 0)
	if !got.Equal(want) {
		t.Fatalf("asked at %s (30 seconds into the minute), the next 03:00 came out as %s\n"+
			"instead of %s: a whole day skipped because the search started from a "+
			"non-zero second. Fix by truncating to the minute before stepping",
			now.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestTheSearchCrossesMonthAndYearBoundaries. A next-firing computed by adding
// to fields rather than to an instant breaks at the edges, and the edges are
// December 31st and the end of a short month.
func TestTheSearchCrossesMonthAndYearBoundaries(t *testing.T) {
	cases := []struct {
		name string
		on   string
		now  time.Time
		want time.Time
	}{
		{"across midnight", "cron:0 3 * * *", at(2026, time.August, 27, 23, 59), at(2026, time.August, 28, 3, 0)},
		{"across a month", "cron:0 3 1 * *", at(2026, time.August, 27, 3, 0), at(2026, time.September, 1, 3, 0)},
		{"across a year", "cron:0 3 1 1 *", at(2026, time.August, 27, 3, 0), at(2027, time.January, 1, 3, 0)},
		{"end of a 31-day month", "cron:0 3 31 * *", at(2026, time.August, 31, 4, 0), at(2026, time.October, 31, 3, 0)},
		{"leap day, four years out", "cron:0 3 29 2 *", at(2026, time.March, 1, 0, 0), at(2028, time.February, 29, 3, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustNext(t, c.on, c.now)
			if !got.Equal(c.want) {
				t.Fatalf("%s asked at %s gave %s, want %s.\n"+
					"A next-firing that is wrong at a boundary is wrong on the one "+
					"night a month or year that nobody is watching",
					c.on, c.now.Format(time.RFC3339), got.Format(time.RFC3339),
					c.want.Format(time.RFC3339))
			}
		})
	}
}

// TestSeptember31stIsRefusedRatherThanSearchedForever protects the horizon.
//
// `0 3 30 2 *` is February 30th: five valid fields, and an instant that does not
// exist. Without a horizon the search runs until the process is killed; with it,
// the user is told at create time. The alternative is a trigger sitting in
// `trigger list` marked active that has never once fired.
// An impossible date is refused when the schedule is PARSED, not when somebody
// happens to ask for its next firing.
//
// This test used to check Next(), and passing there was not good enough:
// `trigger create` validates a record, validation has no clock on purpose, so
// the trigger was stored. February 30th never arrives, so the only symptom was
// a row in `trigger list` marked active that never once ran. Impossibility is a
// property of the expression, so it is decided where the expression is read.
func TestAnImpossibleDateIsRefusedAtParseTime(t *testing.T) {
	cases := []string{
		"cron:0 3 30 2 *",   // February 30th
		"cron:0 3 31 4 *",   // April 31st
		"cron:0 3 31 2,4 *", // 31st of two short months
	}
	for _, on := range cases {
		t.Run(on, func(t *testing.T) {
			_, err := ParseSpec(on)
			if err == nil {
				t.Fatalf("ParseSpec(%q) accepted a date the calendar never "+
					"reaches. Stored, it sits in `trigger list` marked active "+
					"and never runs", on)
			}
			if !strings.Contains(err.Error(), "does not exist in any year") {
				t.Fatalf("the error should say the date cannot occur, so the "+
					"reader knows this is a calendar problem and not a syntax "+
					"one. Got: %v", err)
			}
		})
	}
}

// The mirror of the test above: 29 February HAPPENS, just not every year, and
// the impossibility check must not mistake "rare" for "never".
func TestTheLeapDayIsNotMistakenForAnImpossibleDate(t *testing.T) {
	for _, on := range []string{"cron:0 3 29 2 *", "cron:0 3 30 2 1", "cron:0 3 31 4 5"} {
		if _, err := ParseSpec(on); err != nil {
			t.Errorf("ParseSpec(%q) was refused: %v\n"+
				"  29 February is a real date, and when day-of-week is also "+
				"restricted cron's rule is EITHER matching, so an impossible "+
				"day-of-month still fires on the weekday", on, err)
		}
	}
}

// The horizon was four years, justified as "the shortest span guaranteed to
// contain a 29 February". 2100 is not a leap year, so asked in 2096 the next
// leap day is 2104 — eight years out — and a legal schedule was reported as one
// that never fires. The failing case only appears in the 2090s, which is why a
// suite that asks about dates near today stayed green.
func TestALeapDayResolvesAcrossASkippedCentury(t *testing.T) {
	got := mustNext(t, "cron:0 3 29 2 *", at(2096, time.March, 1, 0, 0))
	want := at(2104, time.February, 29, 3, 0)
	if !got.Equal(want) {
		t.Errorf("next leap-day firing after 2096-03-01 = %v, want %v\n"+
			"  1900, 2100 and 2200 are not leap years: the gap between 2096 and "+
			"2104 is eight years, so a horizon shorter than that refuses a "+
			"schedule the parser already accepted", got, want)
	}
}

// TestALeapDayScheduleStillResolves is the other half of the horizon decision.
// A cap short enough to be comfortable (one year, three years) would report the
// perfectly legal `0 3 29 2 *` as impossible.
func TestALeapDayScheduleStillResolves(t *testing.T) {
	got := mustNext(t, "cron:0 3 29 2 *", at(2025, time.March, 1, 0, 0))
	want := at(2028, time.February, 29, 3, 0)
	if !got.Equal(want) {
		t.Fatalf("cron:0 3 29 2 * asked in March 2025 gave %s, want %s. A horizon "+
			"shorter than four years reports a legal leap-day schedule as impossible",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestBothDayFieldsRestrictedMeansEither pins cron's real rule.
//
// `0 3 1 * 1` is "the 1st AND every Monday", not "the 1st when it is a Monday".
// This is counter-intuitive and it is what every crontab does, so a pasted line
// keeps its meaning. Implementing the intuitive rule instead would make a
// trigger fire FEWER times here than the user has watched it fire elsewhere,
// with nothing to point at.
func TestBothDayFieldsRestrictedMeansEither(t *testing.T) {
	s := mustSpec(t, "cron:0 3 1 * 1") // 1st of the month, or any Monday
	if !s.AmbiguousDayRule() {
		t.Fatal("cron:0 3 1 * 1 restricts both day fields, so AmbiguousDayRule must " +
			"report true: `trigger create` warns on it, and a trigger firing more " +
			"often than intended is a budget event")
	}

	// 2026-09-01 is a Tuesday: matches by day-of-month only.
	if got := mustNext(t, "cron:0 3 1 * 1", at(2026, time.August, 31, 12, 0)); !got.Equal(at(2026, time.September, 1, 3, 0)) {
		t.Fatalf("the 1st of September 2026 is a Tuesday and must still match by "+
			"day-of-month under the OR rule; got %s", got.Format(time.RFC3339))
	}
	// 2026-09-07 is a Monday: matches by day-of-week only.
	if got := mustNext(t, "cron:0 3 1 * 1", at(2026, time.September, 1, 3, 0)); !got.Equal(at(2026, time.September, 7, 3, 0)) {
		t.Fatalf("the next Monday must match by day-of-week under the OR rule, "+
			"giving 2026-09-07; got %s.\nAn AND here would jump to the next 1st "+
			"that happens to be a Monday and fire far less often than the same "+
			"line does in any system crontab", got.Format(time.RFC3339))
	}
}

// TestOneRestrictedDayFieldIsPlainAnd. The OR rule applies only when BOTH are
// narrowed. With `* ` in day-of-week, `0 3 1 * *` must be the 1st and nothing
// else — an unconditional OR would make every day match through the wildcard.
func TestOneRestrictedDayFieldIsPlainAnd(t *testing.T) {
	got := mustNext(t, "cron:0 3 1 * *", at(2026, time.September, 2, 0, 0))
	want := at(2026, time.October, 1, 3, 0)
	if !got.Equal(want) {
		t.Fatalf("cron:0 3 1 * * gave %s, want %s. Applying the day OR rule when only "+
			"one field is restricted lets the wildcard match every day, turning a "+
			"monthly job into a daily one",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestStepsAndListsAndRanges covers the field syntax that actually appears in
// crontabs. Each is here because dropping support for it silently changes a
// schedule: an unparsed `*/15` that fell back to `*` would fire 15x too often.
func TestStepsAndListsAndRanges(t *testing.T) {
	cases := []struct {
		on   string
		now  time.Time
		want time.Time
	}{
		{"cron:*/15 * * * *", at(2026, time.August, 27, 3, 1), at(2026, time.August, 27, 3, 15)},
		{"cron:0,30 * * * *", at(2026, time.August, 27, 3, 5), at(2026, time.August, 27, 3, 30)},
		{"cron:0 9-17 * * *", at(2026, time.August, 27, 3, 0), at(2026, time.August, 27, 9, 0)},
		{"cron:0 0 * * 1-5", at(2026, time.August, 29, 1, 0), at(2026, time.August, 31, 0, 0)}, // Sat -> Mon
		{"cron:5/10 * * * *", at(2026, time.August, 27, 3, 0), at(2026, time.August, 27, 3, 5)},
		{"cron:5/10 * * * *", at(2026, time.August, 27, 3, 5), at(2026, time.August, 27, 3, 15)},
	}
	for _, c := range cases {
		t.Run(c.on, func(t *testing.T) {
			got := mustNext(t, c.on, c.now)
			if !got.Equal(c.want) {
				t.Fatalf("%s asked at %s gave %s, want %s. A field form that parses "+
					"but is not honoured changes how often the job runs, and the "+
					"bill is where you find out",
					c.on, c.now.Format(time.RFC3339), got.Format(time.RFC3339),
					c.want.Format(time.RFC3339))
			}
		})
	}
}

// TestAMalformedScheduleIsRefusedAtCreateTime. Every one of these is a value a
// user plausibly types, and every one of them, if accepted, produces a trigger
// that is stored, listed as active, and either never fires or fires at an hour
// nobody chose.
func TestAMalformedScheduleIsRefusedAtCreateTime(t *testing.T) {
	bad := []struct {
		on  string
		why string
	}{
		{"", "no source at all"},
		{"0 3 * * *", "cron expression with no scheme prefix"},
		{"cron:0 3 * *", "four fields instead of five"},
		{"cron:0 3 * * * *", "six fields: a seconds dialect would shift every field"},
		{"cron:0 3 * * MON", "a day name, which dialects number differently"},
		{"cron:99 3 * * *", "minute 99"},
		{"cron:0 3 0 * *", "day-of-month 0, which does not exist"},
		{"cron:0 3 * 13 *", "month 13"},
		{"cron:0 3 * * 7", "day-of-week 7 in a 0-6 field"},
		{"cron:*/0 * * * *", "a zero step, which would divide by nothing or loop"},
		{"cron:0,,3 * * * *", "an empty list element"},
		{"cron:30-10 * * * *", "a backwards range that dialects disagree about"},
		{"every:", "an interval with no duration"},
		{"every:30s", "below the one-minute floor the scheduler can honour"},
		{"every:banana", "not a duration"},
		{"at:tomorrow", "not an RFC3339 instant"},
		{"at:2026-09-01T03:00:00", "an instant with no zone: a different moment per machine"},
		{"webhook:", "a webhook with no path"},
		{"file:", "a file trigger with no path"},
		{"event:", "an event trigger with no pattern"},
		{"tomorrow:3am", "an unknown scheme"},
	}
	for _, c := range bad {
		t.Run(c.on, func(t *testing.T) {
			s, err := ParseSpec(c.on)
			if err == nil {
				t.Fatalf("--on %q was accepted (%s). Stored, this becomes a row in "+
					"`trigger list` that looks active and is not, and it is "+
					"indistinguishable from a working one", c.on, c.why)
			}
			if s.Kind != "" {
				t.Fatalf("a rejected spec must come back zeroed, or a caller that "+
					"ignores the error stores half a trigger; got Kind=%q", s.Kind)
			}
			// A refusal that does not say what to write is a refusal the user
			// answers by trying variations until something sticks.
			if len(err.Error()) < 40 {
				t.Fatalf("the error for %q is too terse to act on: %q", c.on, err)
			}
		})
	}
}

// TestABackwardsRangeSaysWhyRatherThanJustFailing.
//
// Found by mutation: removing the `lo > hi` check leaves `30-10` still
// rejected, because the empty set trips the "admits no values" fallback. The
// invocation is refused either way, so a test that only asks "did it error"
// cannot tell the two apart — and they are not the same. "admits no values"
// sends the reader looking for a typo in their numbers; the real answer is that
// some cron dialects read a backwards range as a wrap-around and the request is
// ambiguous, not empty. A test that accepts any error at all lets the useful
// message be deleted with the suite still green.
func TestABackwardsRangeSaysWhyRatherThanJustFailing(t *testing.T) {
	_, err := ParseSpec("cron:30-10 * * * *")
	if err == nil {
		t.Fatal("cron:30-10 * * * * was accepted; dialects disagree on whether a " +
			"backwards range wraps around or is empty")
	}
	if !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("the refusal for a backwards range must name the wrap-around "+
			"ambiguity and suggest writing two comma-separated ranges. Falling "+
			"through to the generic \"admits no values\" message points the "+
			"reader at their numbers instead of at the real problem. Got: %v", err)
	}
}

// TestAOneShotFiresAtExactlyTheInstantGiven.
//
// Found by mutation: adding an hour to the returned instant left every test
// passing, because the `at:` cases only ever checked that a PAST instant is
// refused and never what a future one resolves to. `at:` is the one kind whose
// answer is written out in full by the user, so it is the one where a silent
// offset is least excusable and was least covered.
func TestAOneShotFiresAtExactlyTheInstantGiven(t *testing.T) {
	got := mustNext(t, "at:2026-09-01T03:00:00Z", at(2026, time.August, 27, 0, 0))
	want := at(2026, time.September, 1, 3, 0)
	if !got.Equal(want) {
		t.Fatalf("at:2026-09-01T03:00:00Z resolved to %s, not %s. The user wrote the "+
			"instant out in full; any offset applied to it is the tool "+
			"overriding the only unambiguous input this package accepts",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestAPastOneShotIsRefused. `at:` in the past either fires at once or never,
// and which one it is would be a property of the scheduler rather than of what
// the user wrote down.
func TestAPastOneShotIsRefused(t *testing.T) {
	s := mustSpec(t, "at:2020-01-01T03:00:00Z")
	if _, err := s.Next(at(2026, time.August, 27, 0, 0)); err == nil {
		t.Fatal("at:2020-01-01T03:00:00Z resolved to a firing instant when asked in " +
			"2026. A one-shot whose moment has passed must be refused: firing it " +
			"immediately runs an unattended job at an hour nobody chose, and " +
			"skipping it silently stores a trigger that will never run")
	}
}

// TestAnExternalTriggerRefusesToGuessItsNextFiring.
//
// `trigger list` has a NEXT column. For a webhook the honest content of that
// cell is nothing, because the next firing depends on somebody making a request.
// Returning a plausible-looking instant would put a fabricated fact in a table
// people use to decide whether automation is working.
func TestAnExternalTriggerRefusesToGuessItsNextFiring(t *testing.T) {
	for _, on := range []string{"webhook:/deploy", "file:./src", "event:stage.failed"} {
		s := mustSpec(t, on)
		if s.TimeBased() {
			t.Fatalf("%s reports TimeBased: its firing depends on the outside world, "+
				"and callers use this to decide whether to print a NEXT value", on)
		}
		if _, err := s.Next(at(2026, time.August, 27, 0, 0)); err == nil {
			t.Fatalf("%s returned a next-firing instant. Any value here is invented, "+
				"and it lands in the NEXT column of `trigger list` looking like a fact", on)
		}
		if s.Pattern == "" {
			t.Fatalf("%s parsed with an empty Pattern, so the payload was dropped: "+
				"the trigger would match everything or nothing", on)
		}
	}
}

// TestTheRawSpecSurvivesParsing. `trigger show` echoes what the user wrote.
// Reconstructing it from the parsed fields would print a normalised form the
// user never typed, so the string they search their notes for is not the string
// the tool shows them.
func TestTheRawSpecSurvivesParsing(t *testing.T) {
	for _, on := range []string{
		"cron:0 3 * * *", "every:15m", "at:2026-09-01T03:00:00Z", "webhook:/deploy",
	} {
		if got := mustSpec(t, on).Raw; got != on {
			t.Fatalf("ParseSpec(%q).Raw is %q. `trigger show` echoes Raw, and a "+
				"normalised echo is not the line the user can find in their shell "+
				"history", on, got)
		}
	}
}

// TestEveryIsMeasuredFromNow, not from a fixed epoch. `every:15m` created at
// 03:07 means 03:22, not 03:15: the user asked for an interval, and snapping to
// a boundary would make the first gap shorter than the one they asked for.
func TestEveryIsMeasuredFromNow(t *testing.T) {
	now := at(2026, time.August, 27, 3, 7)
	got := mustNext(t, "every:15m", now)
	if want := at(2026, time.August, 27, 3, 22); !got.Equal(want) {
		t.Fatalf("every:15m at %s gave %s, want %s: an interval is measured from "+
			"when it was armed, and snapping to a boundary makes the first "+
			"interval shorter than the one that was requested",
			now.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextIsAlwaysUTC. Everything printed carries a Z (§20.10), and a Next that
// returned the caller's zone would make the trailing Z a lie in the one place
// that matters: the NEXT column of `trigger list`.
func TestNextIsAlwaysUTC(t *testing.T) {
	// A caller in a zone eight hours off UTC.
	zone := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, zone)
	got := mustNext(t, "cron:0 3 * * *", now)
	if got.Location() != time.UTC {
		t.Fatalf("Next returned an instant in %v. Every timestamp this system prints "+
			"carries a Z, so an answer in the caller's zone makes the Z wrong "+
			"rather than making the time local", got.Location())
	}
	// 12:00+08:00 is 04:00Z, so the next 03:00Z is the following day.
	if want := at(2026, time.August, 28, 3, 0); !got.Equal(want) {
		t.Fatalf("got %s, want %s: the caller's zone must be converted, not ignored",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextDoesNotReadTheWallClock is the testability guarantee stated in the
// package comment, enforced rather than promised. Called twice with the same
// `now`, Next must give the same answer; a time.Now() inside would make the
// interesting cases untestable and `every:` non-deterministic.
func TestNextDoesNotReadTheWallClock(t *testing.T) {
	now := at(2026, time.August, 27, 3, 7)
	for _, on := range []string{"cron:0 3 * * *", "every:15m", "at:2026-09-01T03:00:00Z"} {
		a := mustNext(t, on, now)
		time.Sleep(2 * time.Millisecond)
		b := mustNext(t, on, now)
		if !a.Equal(b) {
			t.Fatalf("%s answered %s and then %s for the same `now`. Something here "+
				"reads the wall clock, which makes the leap-day and "+
				"already-missed cases impossible to test and `every:` "+
				"irreproducible", on, a.Format(time.RFC3339), b.Format(time.RFC3339))
		}
	}
}
