package trigger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/surface"
)

// nightly is the record form of the §20.10 invocation. Every test that needs a
// valid trigger starts from this and breaks one thing, so a test's subject is
// the one line it changes.
func nightly() Record {
	return Record{
		Name:         "nightly-audit",
		On:           "cron:0 3 * * *",
		Then:         "run start security-team 'audit dependencies for new CVEs'",
		Budget:       5.00,
		BudgetPeriod: PeriodDay,
		OnMissed:     MissedSkip,
		Overlap:      OverlapSkip,
		Status:       StatusActive,
		CreatedAt:    "2026-08-26T12:00:00Z",
	}
}

func TestTheDocumentedTriggerIsValid(t *testing.T) {
	if err := nightly().Validate(); err != nil {
		t.Fatalf("the trigger printed in docs/design/20-use-cases.md §20.10 does not "+
			"validate: %v\nUsers will paste that invocation", err)
	}
}

// TestTheNextFiringIsNotStored is the ADR-0002 argument at small scale.
//
// A persisted next-firing is a second copy of a derivable fact, and this one
// rots by existing: after the machine is asleep for four days, a NEXT read from
// disk is an instant in the past, which is indistinguishable from a trigger
// whose schedule is broken. The JSON must not carry it.
func TestTheNextFiringIsNotStored(t *testing.T) {
	b, err := nightly().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"next", "next_at", "next_fire"} {
		if strings.Contains(string(b), `"`+forbidden+`"`) {
			t.Fatalf("the stored form contains %q. The next firing is a function of "+
				"the schedule and the current instant: stored, it is a stale "+
				"timestamp after any downtime, and a past NEXT in `trigger list` "+
				"looks exactly like a broken schedule.\ngot: %s", forbidden, b)
		}
	}

	// And it must be computable.
	next, ok, err := nightly().Next(at(2026, time.August, 26, 12, 0))
	if err != nil || !ok {
		t.Fatalf("Next on a valid active trigger returned ok=%v err=%v", ok, err)
	}
	if want := at(2026, time.August, 27, 3, 0); !next.Equal(want) {
		t.Fatalf("Next gave %s, want %s (the instant §20.10 prints)",
			next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestAPausedTriggerShowsNoNextFiring.
//
// The most misleading thing this table could print. An operator scanning
// `trigger list` for "is my automation running" reads a future timestamp as yes;
// a paused trigger's schedule still says 03:00 and it will not fire.
func TestAPausedTriggerShowsNoNextFiring(t *testing.T) {
	r := nightly()
	r.Status = StatusPaused
	_, ok, err := r.Next(at(2026, time.August, 26, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a paused trigger reported a next firing. Its schedule still names " +
			"an instant, but it will not fire, and an operator scanning the NEXT " +
			"column for \"is automation running\" reads a future timestamp as yes")
	}
}

// TestAnExternalTriggerShowsNoNextFiring. Same column, different reason: nobody
// knows when a webhook is next called, and the two must be distinguishable from
// a schedule that is broken.
func TestAnExternalTriggerShowsNoNextFiring(t *testing.T) {
	r := nightly()
	r.On = "webhook:/deploy"
	_, ok, err := r.Next(at(2026, time.August, 26, 12, 0))
	if err != nil {
		t.Fatalf("an external trigger must not be an ERROR, only absent from the "+
			"NEXT column: it is a normal, working trigger. Got %v", err)
	}
	if ok {
		t.Fatal("a webhook trigger reported a next firing; any value there is invented")
	}
}

// TestAScheduleWithNoFutureIsReportedRatherThanBlank.
//
// The third empty NEXT, and the one that must NOT be silent: a one-shot whose
// instant has passed will never run again, and blanking the cell would make it
// look like the webhook case. The caller has to be able to tell "nothing to
// show" from "this will never fire".
func TestAScheduleWithNoFutureIsReportedRatherThanBlank(t *testing.T) {
	r := nightly()
	r.On = "at:2020-01-01T03:00:00Z"
	_, ok, err := r.Next(at(2026, time.August, 26, 12, 0))
	if err == nil {
		t.Fatal("a past one-shot returned no error. It will never fire again, and " +
			"reporting that as a blank NEXT makes it indistinguishable from a " +
			"webhook waiting for a call")
	}
	if ok {
		t.Fatal("ok must be false when there is no firing")
	}
}

// TestStoredTextIsRevalidatedOnLoad.
//
// The reason On and Then are stored as the user's text and not as parsed
// structures. A file may be hand-edited, written by an older build, or merged
// from another machine; Validate re-parses so a schedule today's parser would
// refuse cannot keep firing because it is already on disk.
func TestStoredTextIsRevalidatedOnLoad(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Record)
		why    string
	}{
		{"a schedule an older parser accepted", func(r *Record) { r.On = "cron:0 3 * * MON" },
			"day names are refused now; dialects disagree on Sunday=0 vs Monday=0"},
		{"a schedule with no scheme", func(r *Record) { r.On = "0 3 * * *" },
			"a prefix-less spec would have to be guessed"},
		{"an action that is not a command", func(r *Record) { r.Then = "notify:ops" },
			"`notify` never existed; the stale prefix is in older revisions of this repo"},
		{"an action withheld from non-humans", func(r *Record) { r.Then = "inbox approve i1" },
			"a stored trigger approving inbox items is the ask-policy bypass, hidden in a file"},
		{"a name that is a path", func(r *Record) { r.Name = "../../etc/passwd" },
			"names become filenames"},
		{"a zero budget", func(r *Record) { r.Budget = 0 },
			"fires forever, spends nothing, reports failure, looks active"},
		{"a negative budget", func(r *Record) { r.Budget = -1 }, "same, less plausibly typed"},
		{"an unknown period", func(r *Record) { r.BudgetPeriod = "fortnight" },
			"a ceiling whose window nothing agrees on is not a ceiling"},
		{"an unknown on-missed", func(r *Record) { r.OnMissed = "catchup" },
			"the choice would fall to a default nobody wrote"},
		{"an unknown overlap", func(r *Record) { r.Overlap = "yes" },
			"`parallel` means two agents in one workspace; it must be chosen"},
		{"an unknown status", func(r *Record) { r.Status = "disabled" },
			"firing or silent depending on how the scheduler compares strings"},
		{"no created_at", func(r *Record) { r.CreatedAt = "" },
			"`trigger list` reports by it and a missing one sorts to the front"},
		{"a malformed created_at", func(r *Record) { r.CreatedAt = "yesterday" }, "not parseable"},
		{"a malformed last_fired_at", func(r *Record) { r.LastFiredAt = "recently" },
			"Missed() parses it to count missed slots"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := nightly()
			c.mutate(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a record with %s (%s).\nTrusting a "+
					"record because it is already on disk is how a configuration "+
					"the current code would refuse to create keeps running",
					c.name, c.why)
			}
			if len(err.Error()) < 40 {
				t.Fatalf("the refusal is too terse to act on: %q", err)
			}
		})
	}
}

// TestNeverFiredIsNotTheZeroInstant. `trigger list` has a LAST column, and "has
// not fired yet" is normal for a new trigger while "fired at the zero instant"
// means something is badly wrong. Collapsing them into a zero time.Time makes
// the two indistinguishable.
func TestNeverFiredIsNotTheZeroInstant(t *testing.T) {
	r := nightly()
	if r.LastFiredAt != "" {
		t.Fatal("a fresh record must have an empty LastFiredAt")
	}
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Fatalf("the stored form wrote a zero instant for a trigger that has never "+
			"fired: %s\nThat is a LAST column reading like a real firing at an "+
			"impossible date", b)
	}
	if strings.Contains(string(b), `"last_fired_at"`) {
		t.Fatalf("last_fired_at is present and empty. omitempty keeps \"never fired\" "+
			"absent rather than blank, so the two cannot be confused by anything "+
			"reading the file: %s", b)
	}
}

// TestMissedCountsWhatOnMissedDecidesAbout.
//
// The number is what makes --on-missed concrete: `run-all` after a four-day
// outage on a daily schedule is four runs starting at once, at four times the
// budget, unattended. §20.10 defaults to skip for exactly this, and a caller
// holding the count can say so instead of the user discovering it.
func TestMissedCountsWhatOnMissedDecidesAbout(t *testing.T) {
	r := nightly()
	r.LastFiredAt = "2026-08-23T03:00:00Z"

	// Four days later: the 24th, 25th, 26th and 27th were due.
	n, capped, err := r.Missed(at(2026, time.August, 27, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if capped {
		t.Fatal("four missed firings should not hit the cap")
	}
	if n != 4 {
		t.Fatalf("Missed counted %d firings between 2026-08-23T03:00Z and "+
			"2026-08-27T12:00Z on a daily 03:00 schedule, want 4 (the 24th "+
			"through the 27th). This number is the difference between `run-all` "+
			"meaning \"one run\" and \"four simultaneous runs at 3am\"", n)
	}
}

// TestANewTriggerHasMissedNothing.
//
// Counting from CreatedAt would make a daily trigger created a month ago report
// 30 missed runs the moment it loads, and with --on-missed=run-all that is 30
// runs. "Never fired" is not "missed everything".
func TestANewTriggerHasMissedNothing(t *testing.T) {
	r := nightly() // CreatedAt a month before `now`, LastFiredAt empty
	n, _, err := r.Missed(at(2026, time.September, 26, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a trigger that has never fired reported %d missed firings. "+
			"Counting from creation makes an old, never-fired daily trigger "+
			"report a month of missed runs, and with --on-missed=run-all that "+
			"is a month of runs at once", n)
	}
}

// TestAVeryLongOutageIsReportedAsCappedRatherThanCounted. A minutely trigger
// down for a week is 10,080 slots. The distinction between that and 20,160
// changes no decision, and counting them all makes `trigger list` walk a year of
// minutes per row.
func TestAVeryLongOutageIsReportedAsCappedRatherThanCounted(t *testing.T) {
	r := nightly()
	r.On = "every:1m"
	r.LastFiredAt = "2026-08-01T00:00:00Z"

	n, capped, err := r.Missed(at(2026, time.August, 27, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !capped {
		t.Fatalf("a minutely trigger down for 26 days reported %d missed firings "+
			"and capped=false. The count must be bounded, and the caller must "+
			"know it was bounded so it can say \"more than %d\" instead of "+
			"printing a number it walked 37,000 iterations to get", n, missedCap)
	}
	if n != missedCap {
		t.Fatalf("a capped count came back as %d, want the cap %d, so the caller "+
			"can render it as \"%d+\"", n, missedCap, missedCap)
	}
}

// TestThePeriodsMatchTheDeclaredSurface. The Period constants are a second
// spelling of --budget-period's enum, which is the duplication this package
// spends most of its comments avoiding. It is worth having for compile-time
// safety, and only if the two provably agree.
func TestThePeriodsMatchTheDeclaredSurface(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	if c == nil {
		t.Fatal("trigger create is not in the registry; the surface lost the command")
	}
	var declared []string
	for _, pp := range c.Params {
		if pp.Name == "budget-period" {
			declared = pp.Enum
		}
	}
	if len(declared) == 0 {
		t.Fatal("budget-period declares no enum. Only Enum is machine-readable: " +
			"without it the protocol validator accepts any period at all")
	}

	known := map[string]bool{}
	for _, p := range []Period{PeriodDay, PeriodWeek, PeriodMonth} {
		if _, err := p.Window(); err != nil {
			t.Fatalf("Period %q has no window: %v", p, err)
		}
		known[string(p)] = true
	}
	for _, d := range declared {
		if !known[d] {
			t.Fatalf("the surface declares budget-period %q and this package has no "+
				"constant with a window for it. The CLI would accept a period the "+
				"budget cannot be enforced over", d)
		}
	}
	if len(known) != len(declared) {
		t.Fatalf("this package knows %d periods and the surface declares %d (%v). "+
			"A period with a window here and no declaration there is unreachable; "+
			"the reverse is unenforceable", len(known), len(declared), declared)
	}
}

// TestEachPeriodIsItsOwnWindow.
//
// Found by mutation: making `week` return the month's 30 days broke nothing,
// because the only window any test asserted on was the month's. Three periods
// that all compile to the same duration is not a compile error and not a
// runtime error — it is a --budget-period flag that accepts three values and
// enforces one, which is the same defect as the missing enum, one layer down.
//
// Bounds rather than exact equality on the month, because 30 days is a stated
// approximation. A 30-day month is never longer than a real one, so a fixed
// window can only be stricter than a calendar one, and for the one number whose
// job is to stop a bill, erring tight is the correct direction.
func TestEachPeriodIsItsOwnWindow(t *testing.T) {
	day, err := PeriodDay.Window()
	if err != nil {
		t.Fatal(err)
	}
	week, err := PeriodWeek.Window()
	if err != nil {
		t.Fatal(err)
	}
	month, err := PeriodMonth.Window()
	if err != nil {
		t.Fatal(err)
	}

	if day != 24*time.Hour {
		t.Fatalf("the day window is %s, not 24h", day)
	}
	if week != 7*24*time.Hour {
		t.Fatalf("the week window is %s, not 7 days. A period that resolves to the "+
			"wrong span means --budget-period accepts three values and enforces a "+
			"different number of them: a weekly 5.00 ceiling spread over a month "+
			"is a monthly ceiling of 5.00, and the user asked for four times that", week)
	}
	if !(day < week && week < month) {
		t.Fatalf("the windows are not strictly increasing (day=%s week=%s month=%s). "+
			"Two periods with the same window make one of the three values a "+
			"synonym nobody declared", day, week, month)
	}

	if month > 31*24*time.Hour {
		t.Fatalf("the month window is %s, longer than the longest real month. A "+
			"budget window wider than the calendar period lets the ceiling be "+
			"exceeded within a single month", month)
	}
	if month < 28*24*time.Hour {
		t.Fatalf("the month window is %s, shorter than the shortest real month, so "+
			"a monthly ceiling would reset twice in February", month)
	}
}

// TestTheStoredFormIsDiffable. `pause` exists instead of delete because a
// trigger's configuration has value; single-line JSON makes every change to any
// field show up as one line containing the whole record.
//
// This test earned itself immediately: the first implementation put
// MarshalIndent inside a MarshalJSON method, and encoding/json COMPACTS the
// output of MarshalJSON without complaining. The call looked right, returned no
// error, and produced one line. Asserting on the bytes is what caught it; a test
// that only checked "does it marshal" would have passed.
func TestTheStoredFormIsDiffable(t *testing.T) {
	b, err := nightly().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.TrimRight(string(b), "\n"), "\n") {
		t.Fatalf("the stored form is a single line: %s\nThese files are "+
			"configuration meant to be read and diffed; one line means every "+
			"one-field change is a diff of the entire record.\nNote that "+
			"MarshalIndent inside a MarshalJSON method does NOT work: "+
			"encoding/json compacts whatever MarshalJSON returns", b)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatal("the stored form has no trailing newline, so `cat` and `tail` run " +
			"it into the next line of the terminal")
	}

	// And it must round-trip, or a load-modify-store cycle corrupts it.
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("the stored form does not unmarshal: %v", err)
	}
	if back != nightly() {
		t.Fatalf("a record did not survive a round-trip.\n got: %+v\nwant: %+v\n"+
			"Every `trigger pause` is a load-modify-store, so a lossy round-trip "+
			"silently rewrites fields the user never touched", back, nightly())
	}
}

// TestListOrderIsStable. `trigger list` is read repeatedly by eye looking for
// one row. Ordering by creation would move the row being watched whenever a
// trigger is added, and nobody remembers creation order.
func TestListOrderIsStable(t *testing.T) {
	rs := []Record{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}}
	SortRecords(rs)
	if rs[0].Name != "alpha" || rs[1].Name != "mid" || rs[2].Name != "zeta" {
		t.Fatalf("SortRecords gave %q, %q, %q; want alphabetical so a row an "+
			"operator is watching does not move when a trigger is added",
			rs[0].Name, rs[1].Name, rs[2].Name)
	}
}
