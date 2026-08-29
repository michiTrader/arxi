package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/iash/internal/surface"
	"github.com/michiTrader/iash/internal/trigger"
)

// fixed is the instant every test runs at.
//
// A suite whose expected NEXT column depended on when it ran could only assert
// that something was printed, and "03:00 tomorrow" is exactly the kind of
// answer that is wrong by one day for half of every day.
var fixed = time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)

// captureStdout collects what a function prints.
//
// The help and table output ARE the interface here, so asserting on them means
// capturing them. Restored with a defer so a failure mid-test does not leave the
// rest of the suite writing into a pipe nobody reads.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, r)
		done <- b.String()
	}()
	func() {
		defer func() {
			os.Stdout = prev
			w.Close()
		}()
		fn()
	}()
	return <-done
}

func nightlyRecord() trigger.Record {
	return trigger.Record{
		Name:         "nightly-audit",
		On:           "cron:0 3 * * *",
		Then:         "run start security-team 'audit dependencies for new CVEs'",
		Budget:       5.00,
		BudgetPeriod: trigger.PeriodDay,
		OnMissed:     trigger.MissedSkip,
		Overlap:      trigger.OverlapSkip,
		Status:       trigger.StatusActive,
		CreatedAt:    "2026-08-26T12:00:00Z",
	}
}

// --- the declared parser -----------------------------------------------------

// TestTheDocumentedInvocationParses pins §20.10's exact command line. It is the
// one invocation a reader will copy, so if it stops working the document is
// wrong and this test is how we find out rather than them.
func TestTheDocumentedInvocationParses(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	vals, err := parseInvocation(c, []string{
		"nightly-audit",
		"--on", "cron:0 3 * * *",
		"--then", "run start security-team 'audit dependencies for new CVEs'",
		"--budget", "5.00", "--budget-period", "day",
		"--on-missed", "skip", "--overlap", "skip",
	})
	if err != nil {
		t.Fatalf("the documented invocation was refused: %v", err)
	}
	want := map[string]string{
		"name":          "nightly-audit",
		"on":            "cron:0 3 * * *",
		"then":          "run start security-team 'audit dependencies for new CVEs'",
		"budget":        "5.00",
		"budget-period": "day",
		"on-missed":     "skip",
		"overlap":       "skip",
	}
	for k, v := range want {
		if vals[k] != v {
			t.Errorf("%s = %q, want %q", k, vals[k], v)
		}
	}
}

// Every parameter the registry declares must be reachable. A flag advertised by
// `iash surface` and offered to agents in the tool schema, but dropped by this
// parser, is the exact drift the derived parser exists to prevent — and the
// failure is silent: the value is accepted and ignored.
func TestEveryDeclaredParameterIsAcceptedByName(t *testing.T) {
	for _, path := range [][]string{
		{"trigger", "create"}, {"trigger", "list"},
		{"trigger", "show"}, {"trigger", "pause"},
	} {
		c := surface.Lookup(path...)
		if c == nil {
			t.Fatalf("%v is not in the registry", path)
		}
		for _, pp := range c.Params {
			// Build the minimum WITHOUT this parameter, then supply it by name.
			// Including it twice is refused on purpose, so a test that did so
			// would be asserting the opposite of what it means to.
			args := requiredArgsExcept(c, pp.Name)
			v := "x"
			if len(pp.Enum) > 0 {
				v = pp.Enum[0]
			}
			if pp.Type == "number" {
				v = "1"
			}
			args = append(args, "--"+pp.Name, v)

			vals, err := parseInvocation(c, args)
			if err != nil {
				t.Errorf("%s: --%s was refused: %v", c.CLI(), pp.Name, err)
				continue
			}
			if got := vals[pp.Name]; got != v {
				t.Errorf("%s: --%s parsed as %q, want %q — a flag that is "+
					"accepted and then dropped reports success while discarding "+
					"what the user typed", c.CLI(), pp.Name, got, v)
			}
		}
	}
}

// requiredArgsFor builds the minimum legal invocation of a command, positionals
// first. Derived so that a new required parameter does not silently make every
// test below construct an invalid command.
func requiredArgsFor(c *surface.Cmd) []string { return requiredArgsExcept(c, "") }

// requiredArgsExcept is the same, omitting one parameter so the caller can
// supply it themselves without tripping the given-twice check.
func requiredArgsExcept(c *surface.Cmd, skip string) []string {
	var args []string
	for _, pp := range c.Params {
		if !pp.Positional || pp.Name == skip {
			continue
		}
		args = append(args, "n")
	}
	for _, pp := range c.Params {
		if pp.Positional || !pp.Required || pp.Name == skip {
			continue
		}
		v := "x"
		switch {
		case len(pp.Enum) > 0:
			v = pp.Enum[0]
		case pp.Type == "number":
			v = "1"
		case pp.Name == "on":
			v = "cron:0 3 * * *"
		case pp.Name == "then":
			v = "run start team 'objective'"
		}
		args = append(args, "--"+pp.Name, v)
	}
	return args
}

// Defaults come from the declaration, not from this file. A default written out
// here would be a second copy, and the copy in the parser is the one that
// decides what actually happens.
func TestDefaultsAreTakenFromTheDeclaration(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	vals, err := parseInvocation(c, []string{
		"x", "--on", "cron:0 3 * * *", "--then", "run start team 'o'",
		"--budget", "5", "--budget-period", "day",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, pp := range c.Params {
		if pp.Default == "" {
			continue
		}
		if vals[pp.Name] != pp.Default {
			t.Errorf("--%s defaulted to %q, want the declared %q",
				pp.Name, vals[pp.Name], pp.Default)
		}
	}
	// And the defaults must be the safe ones §20.10 argues for, because that
	// reasoning is the whole point: catchup at 3am with nobody watching fires
	// four runs at once, and overlap gives two agents one repository.
	if vals["on-missed"] != "skip" || vals["overlap"] != "skip" {
		t.Errorf("on-missed=%q overlap=%q, want skip/skip: unattended work is "+
			"not a queue to drain", vals["on-missed"], vals["overlap"])
	}
}

func TestAllFourMandatoryFlagsAreReportedAtOnce(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	_, err := parseInvocation(c, []string{"x"})
	if err == nil {
		t.Fatal("a create with no flags was accepted")
	}
	// One at a time would make the user re-run the command four times to
	// discover four required flags.
	for _, want := range []string{"--on", "--then", "--budget", "--budget-period"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "nobody is watching") {
		t.Errorf("the error should say WHY four flags are mandatory, or it reads "+
			"as bureaucracy: %v", err)
	}
}

func TestAnEnumeratedFlagListsItsLegalValues(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	_, err := parseInvocation(c, []string{
		"x", "--on", "cron:0 3 * * *", "--then", "run start team 'o'",
		"--budget", "5", "--budget-period", "fortnight",
	})
	if err == nil {
		t.Fatal("--budget-period fortnight was accepted.\n" +
			"  a spend ceiling whose period nothing agrees on is not a ceiling")
	}
	for _, want := range []string{"day", "week", "month"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not offer %q: %v", want, err)
		}
	}
}

// A misspelled flag must be refused, and the near miss is worth more than the
// list: `--budget-preiod` silently ignored leaves the trigger with no period
// while the command reports success.
func TestAMisspelledFlagIsRefusedAndCorrected(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	_, err := parseInvocation(c, []string{
		"x", "--on", "cron:0 3 * * *", "--then", "run start team 'o'",
		"--budget", "5", "--budget-preiod", "day",
	})
	if err == nil {
		t.Fatal("--budget-preiod was accepted")
	}
	if !strings.Contains(err.Error(), "did you mean --budget-period") {
		t.Errorf("the error should guess the intended flag: %v", err)
	}
}

func TestShortFlagsReachTheDeclaredParameters(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	vals, err := parseInvocation(c, []string{
		"x", "--on", "cron:0 3 * * *", "--then", "run start team 'o'",
		"-b", "5", "--budget-period", "day",
	})
	if err != nil {
		t.Fatalf("-b was refused: %v", err)
	}
	if vals["budget"] != "5" {
		t.Errorf("-b 5 gave budget=%q; the surface assigns -b to --budget on "+
			"every command that has it", vals["budget"])
	}
}

// A value given twice is refused rather than resolved by precedence: whichever
// one loses is a value the user supplied and watched disappear.
func TestAPositionalGivenTwiceIsRefused(t *testing.T) {
	c := surface.Lookup("trigger", "show")
	_, err := parseInvocation(c, []string{"one", "--name", "two"})
	if err == nil {
		t.Fatal("a name given both positionally and by flag was accepted")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the error should say the value was given twice: %v", err)
	}
}

func TestTooManyArgumentsNamesWhatIsAccepted(t *testing.T) {
	c := surface.Lookup("trigger", "show")
	_, err := parseInvocation(c, []string{"one", "two"})
	if err == nil {
		t.Fatal("two positionals were accepted by a command that takes one")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("the error should name the parameter that is accepted: %v", err)
	}
}

// Reading commands get --json for free from WireParams. A command that
// advertises machine-readable output and then rejects the flag is worse than
// one that never offered it.
func TestReadingCommandsAcceptJSONAndMutatingOnesDoNot(t *testing.T) {
	for _, path := range [][]string{{"trigger", "list"}, {"trigger", "show"}} {
		c := surface.Lookup(path...)
		vals, err := parseInvocation(c, append(requiredArgsFor(c), "--json"))
		if err != nil {
			t.Errorf("%s refused --json: %v", c.CLI(), err)
			continue
		}
		if vals["json"] != "true" {
			t.Errorf("%s: --json parsed as %q", c.CLI(), vals["json"])
		}
	}
	// `trigger create` mutates, so it has no --json to synthesise; accepting
	// the flag there would promise output it does not produce.
	c := surface.Lookup("trigger", "create")
	if _, err := parseInvocation(c, append(requiredArgsFor(c), "--json")); err == nil {
		t.Error("trigger create accepted --json, which it does not implement")
	}
}

// Everything after `--` is data. Without it, a --then beginning with a dash is
// unpassable, and quoting does not help because the quotes are the shell's.
func TestEverythingAfterTheDoubleDashIsData(t *testing.T) {
	c := surface.Lookup("trigger", "show")
	vals, err := parseInvocation(c, []string{"--", "-weird-name"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if vals["name"] != "-weird-name" {
		t.Errorf("name = %q, want %q", vals["name"], "-weird-name")
	}
}

// A declared trigger subcommand that this file does not dispatch must be
// recognised as declared, so it gets the "declared but not implemented" answer
// rather than being called unknown.
//
// Today all four are built, so the guard is unreachable and the subprocess test
// can only confirm the fallback. This one tests the predicate itself, which is
// the part that has to still be right on the commit that declares a fifth: at
// that moment the guard becomes live, and if it is wrong the new capability is
// reported as a typo.
func TestADeclaredSubcommandIsRecognisedEvenWhenNotDispatched(t *testing.T) {
	var declared int
	for _, c := range surface.Registry {
		if len(c.Path) != 2 || c.Path[0] != "trigger" {
			continue
		}
		declared++
		// The lookup the guard performs, on the argument the user typed.
		if surface.Lookup("trigger", c.Path[1]) == nil {
			t.Errorf("trigger %s is in the registry but Lookup does not find "+
				"it, so it would be reported as not a trigger command", c.Path[1])
		}
	}
	if declared == 0 {
		t.Fatal("no trigger subcommands are declared; this test proves nothing")
	}

	// And the negative: something not declared must NOT be mistaken for a
	// capability, or every typo becomes "declared but not implemented" and the
	// user waits for a feature that was never promised.
	for _, typo := range []string{"resume", "delete", "creat"} {
		if surface.Lookup("trigger", typo) != nil {
			t.Errorf("trigger %s resolves in the registry, so a typo would be "+
				"reported as an unimplemented feature", typo)
		}
	}
}

// --- the derived columns -----------------------------------------------------

// Every way of having no next firing is a different sentence. A blank cell
// would collapse four unrelated situations, three of which need acting on.
func TestTheNextColumnExplainsEveryKindOfSilence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*trigger.Record)
		want   string
	}{
		{"a live schedule", func(r *trigger.Record) {}, "2026-08-27 03:00Z"},
		{"paused", func(r *trigger.Record) { r.Status = trigger.StatusPaused }, "(paused)"},
		{"external", func(r *trigger.Record) { r.On = "webhook:/deploy" }, "(on event)"},
		{"a one-shot already past", func(r *trigger.Record) {
			r.On = "at:2020-01-01T00:00:00Z"
		}, "unresolvable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := nightlyRecord()
			tc.mutate(&r)
			if got := nextColumn(r, fixed); got != tc.want {
				t.Errorf("nextColumn = %q, want %q\n"+
					"  a blank cell here would make %s indistinguishable from a "+
					"trigger that is working", got, tc.want, tc.name)
			}
		})
	}
}

// "never" and not "-": a trigger created a month ago that has never fired is
// the exact situation this column exists to reveal.
func TestTheLastColumnDistinguishesNeverFromFailed(t *testing.T) {
	r := nightlyRecord()
	if got := lastColumn(r); got != "never" {
		t.Errorf("lastColumn on a new trigger = %q, want %q", got, "never")
	}
	r.LastFiredAt = "2026-08-25T03:00:00Z"
	if got := lastColumn(r); got != "fired" {
		t.Errorf("lastColumn after a firing = %q, want %q", got, "fired")
	}
	r.LastStatus = "failed"
	if got := lastColumn(r); got != "failed" {
		t.Errorf("lastColumn = %q, want the recorded status %q", got, "failed")
	}
}

// The NEXT column is UTC. A trigger reporting a local time fires at a different
// printed hour after a DST change while the schedule has not moved.
func TestTheNextColumnIsUTC(t *testing.T) {
	r := nightlyRecord()
	// Ask from a zone eleven hours off, which is where a naive implementation
	// would leak the caller's location into the output.
	zone := time.FixedZone("nowhere", 11*3600)
	got := nextColumn(r, fixed.In(zone))
	if !strings.HasSuffix(got, "Z") {
		t.Errorf("nextColumn = %q, want a UTC instant ending in Z", got)
	}
	if got != "2026-08-27 03:00Z" {
		t.Errorf("nextColumn = %q, want 2026-08-27 03:00Z regardless of the "+
			"caller's zone", got)
	}
}

// --- the JSON projection -----------------------------------------------------

// `next` must be RFC3339 or absent, never the table's human string. "(paused)"
// in a field named next is a parse failure for whoever assumed a timestamp, and
// those who do not crash will compare it as a string and treat a paused trigger
// as scheduled.
func TestJSONNextIsATimestampOrAbsent(t *testing.T) {
	live := showPayload(nightlyRecord(), fixed)
	if live.Next != "2026-08-27T03:00:00Z" {
		t.Errorf("next = %q, want an RFC3339 instant", live.Next)
	}
	if live.NextAbsent != "" {
		t.Errorf("next_absent = %q on a live schedule", live.NextAbsent)
	}
	if _, err := time.Parse(time.RFC3339, live.Next); err != nil {
		t.Errorf("next %q does not parse as RFC3339: %v", live.Next, err)
	}

	paused := nightlyRecord()
	paused.Status = trigger.StatusPaused
	p := showPayload(paused, fixed)
	if p.Next != "" {
		t.Errorf("next = %q on a paused trigger, want it absent so a machine "+
			"must handle the absence rather than misread a sentence", p.Next)
	}
	if p.NextAbsent != "paused" {
		t.Errorf("next_absent = %q, want %q", p.NextAbsent, "paused")
	}

	// The third absence, and the one that mattered most to test: a schedule
	// that cannot be resolved at all. Left out of the first version of this
	// test, and a mutation deleting the branch survived — which is the whole
	// point of the split field. `next` absent and `next_absent` absent too
	// says "there is no next firing and I will not tell you why", and a
	// consumer reading that has exactly the same information as one reading
	// "-": none.
	broken := nightlyRecord()
	broken.On = "at:2020-01-01T00:00:00Z" // a one-shot that already happened
	b := showPayload(broken, fixed)
	if b.Next != "" {
		t.Errorf("next = %q on an unresolvable schedule, want it absent", b.Next)
	}
	if b.NextAbsent != "unresolvable" {
		t.Errorf("next_absent = %q, want %q — a machine cannot distinguish a "+
			"broken schedule from a paused one without it",
			b.NextAbsent, "unresolvable")
	}

	// external, for completeness: all four of nextColumn's outcomes have a
	// JSON counterpart, and a projection that covered three of them would be
	// the same defect one branch along.
	ext := nightlyRecord()
	ext.On = "webhook:/deploy"
	e := showPayload(ext, fixed)
	if e.Next != "" || e.NextAbsent != "external" {
		t.Errorf("event-driven trigger: next=%q next_absent=%q, want %q",
			e.Next, e.NextAbsent, "external")
	}
}

func TestJSONListIsAnObjectNotABareArray(t *testing.T) {
	b, err := json.Marshal(listPayload([]trigger.Record{nightlyRecord()}, fixed))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the list payload is not a JSON object: %v\n"+
			"  a bare array means adding a count later changes the type of the "+
			"whole document and breaks every parser at once", err)
	}
	if _, ok := doc["triggers"]; !ok {
		t.Errorf("the payload has no `triggers` key: %s", b)
	}
}

// The stored record round-trips through the JSON output unchanged: a projection
// that renames or drops a field makes the machine-readable form a second schema
// to keep in step with the file.
func TestTheJSONRecordMatchesTheStoredRecord(t *testing.T) {
	want := nightlyRecord()
	b, err := json.Marshal(showPayload(want, fixed))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Record trigger.Record `json:"record"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Record != want {
		t.Errorf("the record changed through the JSON projection:\n got %+v\nwant %+v",
			got.Record, want)
	}
}

// The counter-intuitive rule is reported where the schedule is displayed. Read
// as AND, `0 3 1 * 1` looks like a monthly job; cron fires it on the 1st AND
// every Monday, which is four extra runs a month nobody authorised.
func TestTheAmbiguousDayRuleIsAnnounced(t *testing.T) {
	r := nightlyRecord()
	r.On = "cron:0 3 1 * 1"
	got := showPayload(r, fixed)
	if got.Note == "" {
		t.Fatal("a schedule with both day fields restricted carried no note")
	}
	if !strings.Contains(got.Note, "either") {
		t.Errorf("the note should say EITHER matching fires: %q", got.Note)
	}

	plain := showPayload(nightlyRecord(), fixed)
	if plain.Note != "" {
		t.Errorf("an unambiguous schedule carried a note: %q\n"+
			"  a warning on every trigger is a warning nobody reads", plain.Note)
	}
}

// --- help --------------------------------------------------------------------

// Help is what a user reads INSTEAD of the source, so it is built from the
// declaration and cannot describe a flag the parser does not accept.
func TestHelpIsBuiltFromTheDeclaration(t *testing.T) {
	c := surface.Lookup("trigger", "create")
	out := captureStdout(t, func() { printDeclaredHelp(c) })
	for _, pp := range c.Params {
		if !strings.Contains(out, pp.Name) {
			t.Errorf("help does not mention the declared parameter %q", pp.Name)
		}
		for _, v := range pp.Enum {
			if !strings.Contains(out, v) {
				t.Errorf("help does not list %q, a legal value of --%s", v, pp.Name)
			}
		}
	}
	// The names a machine client uses are printed too, because the same
	// capability is one thing with several projections and a user reading help
	// is often the person wiring the tool up.
	if !strings.Contains(out, c.Name()) || !strings.Contains(out, c.ProtocolType()) {
		t.Errorf("help omits the tool or protocol name: %s", out)
	}
}
