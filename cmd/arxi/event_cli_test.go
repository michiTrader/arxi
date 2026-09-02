package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// `arxi event log`, exercised as a process against real run directories.
//
// # Why a viewer needs tests at all
//
// It computes nothing. Every figure it prints is already in the log, which makes
// it look like the one command where a bug cannot matter. The opposite is true:
// every other verb in this binary summarises, and this is the one a person opens
// when they have stopped believing the summaries. A viewer that hides a field, or
// prints a value that is not in the file, is worse than no viewer -- it produces a
// confident reading of evidence the reader cannot see.
//
// So the assertions below are about DISCLOSURE rather than about arithmetic: that
// no payload key is dropped, that a filter which selects nothing says what the log
// does hold, that the exit code distinguishes "no match" from "bad flag", and that
// stdout is empty in every case where there is no table.
//
// # The fixtures are hand-written, and the shapes came out of a real log
//
// The seq numbers, the empty `scope` on executor events and the depth-0/depth-1
// split are not invented for the test. They are what `run start --sim` actually
// wrote, checked against a 21-event log before any of this was asserted: the
// reducer stamps scope, correlation_id, caused_by and depth 1; the executor stamps
// none of the four. A fixture where everything carries a cause would have let the
// footer's missing-cause note pass while being unreachable in practice.

// logAt writes a run whose log has the shapes this viewer has to survive.
//
// runAt already supplies run.started at seq 1 and stage.entered at seq 2, so the
// extras start at 3. The events here are deliberately mixed: two with no cause and
// no scope (the executor's shape), one with both (the reducer's), and one carrying
// a 64-character sha so elision is exercised by a value the system really writes
// rather than by a string invented to be long.
func logAt(t *testing.T, dir, id string) {
	t.Helper()

	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"agent.activated","source":"runtime","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e4","seq":4,"type":"llm.response","source":"agent","actor":"backend","payload":{"agent":"backend","tokens_in":1200,"tokens_out":340,"cost_usd":0.000015}}
{"id":"e5","seq":5,"type":"stage.submitted","source":"agent","actor":"backend","payload":{"agent":"backend","stage":"execute","blueprint_sha":"456be043e0c173310d2fca009fa592e6eac1794d961c4e0fd757f2158e74027d"}}
{"id":"e6","seq":6,"type":"run.result","scope":"run:`+id+`","source":"runtime","correlation_id":"c1","caused_by":["e5"],"depth":1,"payload":{"summary":"all stages completed","result_from":"last_submit"}}
`)
}

func TestEventLogPrintsEverySequenceInLogOrder(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "log", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	// Every seq, and in the order the file holds them. A viewer that sorted by
	// anything else -- type, actor, a timestamp that is 1970 for most of a
	// simulated log -- would rearrange the one thing the log is authoritative
	// about, which is what happened before what.
	want := []string{"run.started", "stage.entered", "agent.activated",
		"llm.response", "stage.submitted", "run.result"}
	at := -1
	for _, typ := range want {
		i := strings.Index(out, typ)
		if i < 0 {
			t.Fatalf("%s is missing from the table:\n%s\n"+
				"  consequence: this is the command a reader opens when they have "+
				"stopped trusting the summaries. An event it declines to show is "+
				"evidence that exists on disk and cannot be found.", typ, out)
		}
		if i < at {
			t.Errorf("%s appears out of log order:\n%s\n"+
				"  consequence: the log's only authoritative claim is what happened "+
				"before what. Re-ordering it inverts causes and effects.", typ, out)
		}
		at = i
	}

	if !strings.Contains(out, "6 events, seq 1..6") {
		t.Errorf("the header does not report what it counted:\n%s\n"+
			"  consequence: a reader cannot tell a short log from a truncated "+
			"reading of a long one.", out)
	}
	if !strings.Contains(out, "succeeded") {
		t.Errorf("the header omits the folded status:\n%s", out)
	}
}

// TestEventLogPrintsDepthZeroAsANumberAndNotADash is the defect this verb was
// caught in the first time it met a real log.
//
// dashIfZero is right for every other optional column in this binary, and wrong
// here: it turned all seventeen executor events into `-` while the footer directly
// underneath announced that the causal chain "is broken at depth 0" -- a note about
// a value that appeared nowhere in the table above it. 0 is not a missing depth, it
// is the depth of a root cause, and the column exists precisely so a cascade that
// flattens to 0 halfway down the log is visible.
func TestEventLogPrintsDepthZeroAsANumberAndNotADash(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, _, _ := arxiStreams(t, dir, "event", "log", id)

	var depths []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// A row is SEQ TYPE SOURCE ACTOR DEPTH PAYLOAD..., and the payload cell
		// holds spaces of its own, so the columns are read from the LEFT. Field 3
		// is never empty: an actorless event prints a dash there.
		if len(f) < 5 || !isDigits(f[0]) || !strings.Contains(f[1], ".") {
			continue
		}
		depths = append(depths, f[4])
	}
	if len(depths) == 0 {
		t.Fatalf("no rows were parsed out of:\n%s", out)
	}

	sawZero := false
	for _, d := range depths {
		if d == "0" {
			sawZero = true
		}
	}
	if !sawZero {
		t.Errorf("no row shows depth 0:\n%s\n"+
			"  consequence: the executor stamps depth 0 on everything it writes, so "+
			"a viewer that renders 0 as a dash blanks the DEPTH column for the "+
			"majority of a real log -- while the footer explains a zero the reader "+
			"cannot see.", out)
	}
}

// isDigits reports whether s is a non-empty run of decimal digits.
//
// Used to tell a table row from a footer line, which is the only reliable
// difference between them: both are indented text containing dots.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func TestEventLogTypeFilterMatchesAWholeSegmentAndSaysHowManyItSkipped(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "log", id, "--type", "stage.*")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "stage.entered") || !strings.Contains(out, "stage.submitted") {
		t.Errorf("stage.* did not select both stage events:\n%s", out)
	}
	if strings.Contains(out, "llm.response") || strings.Contains(out, "run.result") {
		t.Errorf("stage.* selected events outside the segment:\n%s", out)
	}

	// "2 of 6" and not "2": a filtered view that reports only what it kept reads
	// as the whole log, and the reader then concludes the run did far less than it
	// did. The denominator is what makes the filter visible in its own output.
	if !strings.Contains(out, "2 of 6 events") {
		t.Errorf("the header does not say what the filter left out:\n%s\n"+
			"  consequence: a filtered table that reports only its own row count "+
			"is indistinguishable from a complete log, so a reader takes two "+
			"events as everything that happened.", out)
	}
}

// TestEventLogSinceSeqIsInclusive pins the boundary the usage line promises.
//
// Inclusive is the choice that makes `--since-seq $(($LAST))` wrong by one and
// `--since-seq $LAST` right, and either convention is defensible -- which is
// exactly why it needs a test rather than a preference. A tailing script that
// re-reads from the last seq it saw is the intended use, and an off-by-one there
// either duplicates one event per poll or silently skips one.
func TestEventLogSinceSeqIsInclusive(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "log", id, "--since-seq", "5")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "seq 5..6") {
		t.Errorf("--since-seq 5 did not start AT 5:\n%s\n"+
			"  consequence: a tailer that resumes from the last seq it saw either "+
			"repeats an event every poll or drops one, and both look like the log "+
			"is lying.", out)
	}
	if strings.Contains(out, "llm.response") {
		t.Errorf("--since-seq 5 kept seq 4:\n%s", out)
	}

	// 0 means "no bound", not "before the first event", because a script computing
	// a bound off an EMPTY log passes 0 and must get the whole thing.
	zero, _, code := arxiStreams(t, dir, "event", "log", id, "--since-seq", "0")
	if code != 0 {
		t.Fatalf("--since-seq 0 exit %d, want 0", code)
	}
	if !strings.Contains(zero, "6 events, seq 1..6") {
		t.Errorf("--since-seq 0 is not the unbounded view:\n%s\n"+
			"  consequence: the first poll of a tailer starts from 0, so treating "+
			"it as a filter hides the beginning of every run.", zero)
	}
}

// TestEventLogAFilterThatFindsNothingLeavesStdoutEmptyAndExitsZero is the pair of
// facts a pipeline depends on.
//
// Nothing found is an ANSWER. Exiting non-zero for it makes `arxi event log r1
// --type tool.* && deploy` refuse to deploy a run that simply used no tools, and
// writing the explanation to stdout puts prose into the file the next stage reads
// as events. The diagnosis still has to be somewhere, which is stderr's job, and it
// has to name the types the log DOES hold -- because the overwhelmingly likely
// cause is a typo in the pattern, and a reader who cannot see the real spellings
// tries the same typo again.
func TestEventLogAFilterThatFindsNothingLeavesStdoutEmptyAndExitsZero(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "log", id, "--type", "stage.submited")
	if code != 0 {
		t.Fatalf("exit %d, want 0 -- a filter that matches nothing is an answer\n"+
			"stderr:\n%s", code, errb)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout is not empty:\n%s\n"+
			"  consequence: `arxi event log r1 --type tool.* > events.txt` writes an "+
			"explanation into a file the next stage parses as events.", out)
	}
	for _, typ := range []string{"stage.submitted", "llm.response", "run.result"} {
		if !strings.Contains(errb, typ) {
			t.Errorf("stderr does not name %s among the types present:\n%s\n"+
				"  consequence: the likeliest cause is a misspelt pattern, and a "+
				"reader who is not shown the real spellings retypes the typo.",
				typ, errb)
		}
	}
}

// TestEventLogSinceSeqPastTheHeadNamesTheHead separates two no-match cases that
// look identical from the outside.
//
// "no event matches" for a bound of 99 on a six-event log sends the reader looking
// for a filter problem they do not have. The log simply is not that long yet, and
// the only useful sentence names the seq it does end at.
func TestEventLogSinceSeqPastTheHeadNamesTheHead(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "log", id, "--since-seq", "99")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errb)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout is not empty:\n%s", out)
	}
	if !strings.Contains(errb, "seq 6") {
		t.Errorf("stderr does not say where the log actually ends:\n%s\n"+
			"  consequence: a reader polling ahead of the writer is told their "+
			"filter matched nothing, and goes hunting for a pattern bug in a run "+
			"that has simply not got there yet.", errb)
	}
}

// TestEventLogRefusesAPatternItCannotHonour keeps the CLI's answer identical to the
// blueprint validator's.
//
// The pattern language is exact, `*`, or ONE trailing segment. `stage*` and
// `st*ge.entered` are the two shapes internal/blueprint/load.go already refuses in
// a watcher, and a viewer that quietly matched nothing for them would teach the
// user a dialect the rest of the system does not speak -- then the same pattern in
// a blueprint would be rejected, with no way to tell which spelling belonged where.
//
// Exit 2, not the exit 0 a no-match gets: the request itself is malformed, so there
// is no log reading to report.
func TestEventLogRefusesAPatternItCannotHonour(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	for _, tc := range []struct{ pattern, wants string }{
		{"stage*", `"stage.*"`},
		{"st*ge.entered", "trailing"},
	} {
		out, errb, code := arxiStreams(t, dir, "event", "log", id, "--type", tc.pattern)
		if code != 2 {
			t.Errorf("--type %s exit %d, want 2\nstderr:\n%s", tc.pattern, code, errb)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("--type %s wrote to stdout:\n%s", tc.pattern, out)
		}
		if !strings.Contains(errb, tc.wants) {
			t.Errorf("--type %s does not say what to write instead (want %q):\n%s\n"+
				"  consequence: refusing a pattern without naming the supported "+
				"spelling leaves the user guessing at a language three parts of "+
				"this system agree about.", tc.pattern, tc.wants, errb)
		}
	}

	// A well-formed type nobody has ever emitted is NOT refused. `event emit`
	// accepts free-form custom.* types, so an unknown-but-legal type is a filter
	// that will match later, not a mistake.
	_, _, code := arxiStreams(t, dir, "event", "log", id, "--type", "custom.deploy.finished")
	if code != 0 {
		t.Errorf("a well-formed unknown type exited %d, want 0\n"+
			"  consequence: custom.* types are emitted by agents, so refusing an "+
			"unseen one makes the filter unusable for the events it exists for.", code)
	}
}

// TestEventLogElidesValuesAndNeverHidesAKey is the disclosure rule.
//
// A long value has to be cut, because a 64-character sha pushes everything after it
// off a terminal. Which key was present must survive that cut: a reader comparing
// two runs needs to know blueprint_sha is recorded even when they have to go to
// --json for the digits. Dropping the key produces the failure this whole file
// exists to prevent -- a confident reading of a log with a field silently missing.
func TestEventLogElidesValuesAndNeverHidesAKey(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, _, _ := arxiStreams(t, dir, "event", "log", id)

	if !strings.Contains(out, "blueprint_sha=") {
		t.Errorf("the long key is not shown at all:\n%s\n"+
			"  consequence: a reader told two runs used the same blueprint cannot "+
			"see that the log records which one.", out)
	}
	if strings.Contains(out, "456be043e0c173310d2fca009fa592e6eac1794d961c4e0fd757f2158e74027d") {
		t.Errorf("a 64-character value was printed whole:\n%s\n"+
			"  consequence: one sha per row pushes the rest of the payload past the "+
			"width of a terminal, so the keys a reader came for scroll away.", out)
	}
	if !strings.Contains(out, "--json") {
		t.Errorf("nothing points at where the elided digits are:\n%s", out)
	}

	// A small cost is not scientific notation. 0.000015 through %g is "1.5e-05",
	// which a reader scanning a money column does not read as fifteen millionths
	// of a dollar -- they read it as noise, or as 1.5.
	if !strings.Contains(out, "0.000015") {
		t.Errorf("a sub-cent cost is not printed as a decimal:\n%s\n"+
			"  consequence: 1.5e-05 in a cost field is either skipped as noise or "+
			"misread by five orders of magnitude.", out)
	}
}

// TestEventLogJSONIsTheFileShapeAndNotAReformattingOfTheTable is what makes the
// human view safe to elide from.
//
// Every note the table prints points at --json for what it left out, so --json has
// to carry the fields the table has no column for: id, ts, scope, correlation_id
// and caused_by. Marshalling kernel.Event rather than a view struct is what
// guarantees it -- the wire shape is the file's shape, so a consumer can diff one
// against the other.
func TestEventLogJSONIsTheFileShapeAndNotAReformattingOfTheTable(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "log", id, "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errb)
	}

	var got struct {
		Run    string `json:"run"`
		Count  int    `json:"count"`
		Total  int    `json:"total"`
		Status string `json:"status"`
		Events []struct {
			Seq           int64    `json:"seq"`
			ID            string   `json:"id"`
			Type          string   `json:"type"`
			CorrelationID string   `json:"correlation_id"`
			CausedBy      []string `json:"caused_by"`
			Payload       map[string]any
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, out)
	}

	if got.Run != id || got.Count != 6 || got.Total != 6 || got.Status != "succeeded" {
		t.Errorf("header fields wrong: %+v", got)
	}
	if len(got.Events) != 6 {
		t.Fatalf("got %d events, want 6", len(got.Events))
	}

	last := got.Events[5]
	if last.ID != "e6" || last.CorrelationID != "c1" || len(last.CausedBy) != 1 {
		t.Errorf("the causal fields the table has no column for are missing: %+v\n"+
			"  consequence: the table's own footer sends a reader here for ids and "+
			"causes. If they are not in the JSON either, the elision was a "+
			"deletion.", last)
	}
	if last.Payload["summary"] != "all stages completed" {
		t.Errorf("payloads are not carried whole: %+v", last.Payload)
	}
	if sha, _ := got.Events[4].Payload["blueprint_sha"].(string); len(sha) != 64 {
		t.Errorf("the value the table elided is not whole here: %q\n"+
			"  consequence: --json is the answer to every elision note, so an "+
			"elided value in it leaves the digits unreachable.", sha)
	}
}

// TestEventLogJSONNoMatchIsAnEmptyArrayAndNotNull is one character of difference
// that decides whether a consumer crashes.
//
// A nil slice marshals to `null`, and `for ev in data["events"]` over null raises in
// every language likely to be on the other end of this pipe. The shape must not
// depend on whether the filter happened to match.
func TestEventLogJSONNoMatchIsAnEmptyArrayAndNotNull(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, _, code := arxiStreams(t, dir, "event", "log", id, "--type", "tool.*", "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(out, `"events": []`) {
		t.Errorf("an empty result is not an empty array:\n%s\n"+
			"  consequence: `null` here makes every consumer that iterates events "+
			"fail on a run that merely used no tools.", out)
	}
	if !strings.Contains(out, `"total": 6`) {
		t.Errorf("--json drops the unfiltered total:\n%s\n"+
			"  consequence: a machine reading count 0 with no denominator cannot "+
			"tell an empty log from an unmatched filter.", out)
	}
}

// TestEventLogAsksWhichRunRatherThanReadingTheRunsDirectory covers the id that is
// blank because a shell variable was unset.
//
// `arxi event log "$RUN"` with RUN empty satisfies the parser, and without the
// explicit check resolveRunDir names ./runs itself as a directory holding no event
// log -- a message about a path the user never typed.
func TestEventLogAsksWhichRunRatherThanReadingTheRunsDirectory(t *testing.T) {
	dir := t.TempDir()
	logAt(t, dir, "rmthws2dz-93381f43")

	out, errb, code := arxiStreams(t, dir, "event", "log", "   ")
	if code != 2 {
		t.Errorf("exit %d, want 2\nstderr:\n%s", code, errb)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout is not empty:\n%s", out)
	}
	if !strings.Contains(errb, "which run?") {
		t.Errorf("stderr does not ask for the missing id:\n%s\n"+
			"  consequence: an unset variable is reported as a problem with the "+
			"runs directory, which is not what the user got wrong.", errb)
	}
}

// TestEventGroupNeverCallsADeclaredSubcommandUnknown is the reason cmdEvent routes
// through notImplemented instead of printing its own message.
//
// A group that grows a dispatcher is exactly where "unknown command" starts being
// printed for capabilities `arxi surface` publishes -- it happened to trigger and to
// eval before they were fixed, and the answer is the worst one available: it sends a
// user hunting for a typo they never made in a command that genuinely exists.
//
// The verbs are taken from the registry rather than listed here. This test used to
// name the group's unwired verb and assert the not-implemented sentence, which was
// a list of one that went stale the moment `event trace` was wired: it then asserted
// the opposite of what the binary does. The registry cannot go stale that way, and
// the assertion below holds whether a verb is wired or not -- what it forbids is the
// group disowning a verb the surface publishes.
func TestEventGroupNeverCallsADeclaredSubcommandUnknown(t *testing.T) {
	dir := t.TempDir()

	var subs []string
	for _, p := range surfaceRegistryPaths() {
		if len(p) == 2 && p[0] == "event" {
			subs = append(subs, p[1])
		}
	}
	if len(subs) == 0 {
		t.Fatal("the registry declares no event subcommands, so the loop below " +
			"asserts nothing -- the filter is wrong, not the surface")
	}

	for _, sub := range subs {
		r := arxi(t, dir, "event", sub)
		for _, bad := range []string{"not an event command", "unknown command"} {
			if strings.Contains(strings.ToLower(r.out), bad) {
				t.Errorf("arxi event %s is declared but the group answers %q:\n%s\n"+
					"  consequence: `arxi surface` lists it, so calling it unknown makes "+
					"a reader distrust the surface or reimplement what is coming.",
					sub, bad, r.out)
			}
		}
	}

	// The bare group names the verbs it accepts, computed from the registry, so a
	// verb wired later needs no edit here or there -- including here, which is why
	// this checks the same registry-derived list rather than a copy of it.
	bare := arxi(t, dir, "event")
	for _, sub := range subs {
		if !strings.Contains(bare.out, sub) {
			t.Errorf("bare `arxi event` does not list %s:\n%s", sub, bare.out)
		}
	}

	// A word that really is wrong gets the other sentence: the group is fine, the
	// verb is not.
	bogus := arxi(t, dir, "event", "frobnicate")
	if !strings.Contains(bogus.out, "frobnicate") ||
		!strings.Contains(bogus.out, "not an event command") {
		t.Errorf("a wrong verb is not blamed on the wrong word:\n%s", bogus.out)
	}
}

// TestEventLogFooterAgreesWithItselfAboutOne is the other defect the first hand run
// printed.
//
// "1 are empty" -- and it is the COMMON case, not an edge: exactly one event in a
// real log, run.started, is written before the clock exists, so the ungrammatical
// branch is the one nearly every reader sees. A footer whose whole job is to be
// trusted about what the table left out cannot be visibly careless in the same
// sentence.
func TestEventLogFooterAgreesWithItselfAboutOne(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	logAt(t, dir, id)

	out, _, _ := arxiStreams(t, dir, "event", "log", id)

	if strings.Contains(out, "1 are ") || strings.Contains(out, " 1 events") {
		t.Errorf("the footer disagrees with itself about one:\n%s\n"+
			"  consequence: this footer exists to be believed about what the table "+
			"omitted, and it is reporting a count in the same breath as getting "+
			"that count's grammar wrong.", out)
	}
	// Whatever it says about causes, it must be pinned to a number it counted: the
	// note fired unconditionally at first, announcing a broken chain on logs where
	// nothing had a cause to break.
	if strings.Contains(out, "carry no cause") &&
		!strings.Contains(out, "5 of these events carry no cause") {
		t.Errorf("the missing-cause note is not pinned to the rows shown:\n%s\n"+
			"  consequence: a note that fires regardless of the log tells a reader "+
			"their run is damaged when it is merely simple.", out)
	}

	// No event in this fixture has a ts, and the note must not then offer --json as
	// the place to find them. Pointing a reader at a view for a field nothing
	// carries is how they conclude the table is withholding something.
	if strings.Contains(out, "the ones that have a ts are in --json") {
		t.Errorf("the timestamp note promises timestamps that do not exist:\n%s\n"+
			"  consequence: a reader who runs --json on this advice finds the same "+
			"absence and stops believing the rest of the footer.", out)
	}
}
