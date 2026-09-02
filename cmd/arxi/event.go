package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi event log <run>` -- the log itself, which every other verb is a reading of.
//
// # Why this one, and why it is only a read
//
// ADR-0002 says the log is the source of truth and state.snapshot.json is a cache
// nothing reads. Every command built so far has honoured that by folding the log
// and printing a conclusion: `run show` prints a status, `run result` prints a
// deliverable, `run why` prints a diagnosis. When one of those conclusions is
// wrong, there has been no way to see the evidence it was drawn from except
// `cat runs/<id>/events.ndjson`, which is 21 lines of one-line JSON with the keys
// in alphabetical order. This is the verb that makes the source of truth readable
// by a person, so a disagreement between two commands can be settled by looking.
//
// It writes nothing and takes no lock, for the reason `run list` documents at
// length: the runs a reader most wants to inspect are the ones currently
// executing, and a viewer that refused those with "someone else is writing" would
// be useless exactly when it is needed.
//
// # What a real log turned out to look like
//
// MEASURED on a 21-event --sim run produced by this binary, before this file was
// written, because three of the decisions below are only defensible against real
// bytes:
//
//	 1 run.started      {"actor":"feature-team","blueprint_sha":"456be043…","budget_usd":5,"simulated":true}
//	 2 stage.entered    {"simulated":true,"stage":"plan"}
//	 3 agent.activated  {"agent":"backend","simulated":true}
//	...
//	21 run.result       {"result_from":"last_submit","summary":"all stages completed"}
//
// What those bytes settled:
//
//   - THERE IS NO USABLE TIMESTAMP. run.started carries "ts":"" and every other
//     event carries "ts":"1970-01-01T00:00:00Z", because --sim drives a virtual
//     clock. A TS column would therefore be twenty identical 1970s and one blank,
//     six characters of noise per row on the widest thing in the table. So there
//     is no column, and the footer reports the situation it measured on THIS log
//     rather than asserting a general rule -- a live run has real timestamps and
//     the footer says so instead. --json carries ts verbatim either way.
//
//   - SCOPE IS EMPTY ON EVERY EXECUTOR EVENT. agent.activated, llm.response,
//     stage.submitted and agent.turn_done all have "scope":"", and only the
//     reducer's own events carry "scope":"run:<id>". So this command selects
//     events by DIRECTORY and never by scope. A version that filtered on
//     scope == "run:"+id would have silently dropped the majority of a real log
//     while looking like it worked, which is the worst available failure: fewer
//     rows, no error.
//
//   - THE CAUSAL CHAIN IS BROKEN THROUGH THE EXECUTOR. Reducer events carry
//     correlation_id, caused_by and depth 1; executor events carry none of the
//     three and depth 0. That is a real gap in the producers rather than in this
//     reader, and it is why DEPTH is a column here: seeing a cascade flatten to
//     zero halfway down the log is how somebody will find it.
//
// # No key is hidden
//
// The payload is printed whole, keys sorted, one `key=value` per key. Individual
// VALUES are elided at a width, and when that happens the footer says so and
// points at --json; keys never are. A log viewer that quietly drops a field is
// worse than no log viewer, because it produces confident readings of evidence
// the reader cannot see -- and blueprint_sha, the one long value in a real log,
// is precisely the kind of thing somebody would go looking for after being told
// two runs were identical.
//
// # Quote the pattern
//
// The usage line says to quote --type, and that is a finding rather than
// pedantry: `--type stage.*` was run from a directory that happened to hold a
// file matching it, and the shell handed this command the FILENAME. The pattern
// then selected nothing, the no-match message dutifully listed the eight types
// present, and none of it mentioned the word the user actually typed. Nothing in
// a process can detect that substitution -- by the time main sees argv the
// original text is gone -- so the only available fix is to tell the reader,
// where they are already looking, before it happens.
const eventLogUsage = "usage: arxi event log <run> [--type PATTERN] [--since-seq N] [--json]\n" +
	"  --type       stage.entered (exact), stage.* (one trailing segment) or * (all)\n" +
	"               quote it -- --type 'stage.*' -- or the shell may expand it\n" +
	"  --since-seq  inclusive: --since-seq 12 starts AT seq 12, not after it\n" +
	"  short: -r run · -J json\n" +
	"  see what exists: arxi run list\n"

// eventLogView is the whole answer, resolved once.
//
// Projected up front for the reason runListRow and resultView are: the human and
// the JSON renderer must not be able to disagree about what was selected. A
// version where each counted the matches itself is one edit away from a header
// that says 7 events above 9 rows.
type eventLogView struct {
	id        string
	dir       string
	st        kernel.State
	simulated bool

	rows  []kernel.Event // what matched, in log order
	total int            // how many the log holds, before filtering

	typePattern string
	sinceSeq    int64
	hasSince    bool

	headSeq  int64    // the highest seq in the log, filtered or not
	allTypes []string // every type present in the log, sorted, for a typo'd --type

	// tsVirtual and tsMissing are measured on the rows shown, not assumed. See
	// the file comment: what the footer says about time depends on the log.
	tsVirtual int
	tsMissing int
}

// cmdEvent routes the group and refuses nothing itself.
//
// A bare `arxi event` goes to notImplemented rather than growing a usage string
// here: that function's incomplete-path branch already lists the subcommands,
// computed from the surface, so a hand-written list here would be a second copy
// able to omit the next verb wired. The group is now complete, so the fallthrough
// below answers only for a misspelling -- and it answers it with the real list.
func cmdEvent(args []string) {
	if len(args) == 0 {
		notImplemented([]string{"event"})
		return
	}
	switch args[0] {
	case "log":
		cmdEventLog(args[1:])
		return
	case "emit":
		cmdEventEmit(args[1:])
		return
	case "trace":
		cmdEventTrace(args[1:])
		return
	}
	notImplemented(append([]string{"event"}, args...))
}

func cmdEventLog(args []string) {
	c := surface.Lookup("event", "log")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event log: %v\n\n%s", err, eventLogUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		// A blank id is not hypothetical: `arxi event log "$RUN"` with RUN unset
		// satisfies parseInvocation, and without this check resolveRunDir would
		// report the runs directory itself as a run holding no event log.
		fmt.Fprint(os.Stderr, "arxi event log: which run?\n\n"+eventLogUsage)
		os.Exit(2)
	}

	pattern := strings.TrimSpace(vals["type"])
	if err := checkTypePattern(pattern); err != nil {
		fmt.Fprintf(os.Stderr, "arxi event log: %v\n\n%s", err, eventLogUsage)
		os.Exit(2)
	}

	sinceSeq, hasSince, err := parseSinceSeq(vals["since-seq"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event log: %v\n\n%s", err, eventLogUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// One read for the header and the rows both. foldRunDirEvents hands back the
	// events it folded precisely so a viewer cannot print one moment's status
	// above another moment's events -- a log being appended to right now would
	// otherwise give a status at seq 18 over rows ending at seq 20.
	st, _, simulated, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event log: %v\n", err)
		os.Exit(1)
	}

	id := st.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	v := eventLogView{
		id: id, dir: dir, st: st, simulated: simulated,
		total: len(events), typePattern: pattern,
		sinceSeq: sinceSeq, hasSince: hasSince,
		allTypes: distinctTypes(events),
	}
	for _, e := range events {
		if e.Seq > v.headSeq {
			v.headSeq = e.Seq
		}
		if pattern != "" && !kernel.MatchEventType(pattern, string(e.Type)) {
			continue
		}
		if hasSince && e.Seq < sinceSeq {
			continue
		}
		v.rows = append(v.rows, e)
		switch e.Ts {
		case "":
			v.tsMissing++
		case virtualClockTs:
			v.tsVirtual++
		}
	}

	if vals["json"] == "true" {
		emitJSON(eventLogPayload(v))
		return
	}
	printEventLog(v)
}

// virtualClockTs is what --sim stamps: the zero instant, formatted.
//
// Named rather than compared inline because the footer's claim about time hangs
// on it, and a reader of that footer should be able to find the one string it
// recognises. Measured off a real --sim log, not derived from time.Time{}.
const virtualClockTs = "1970-01-01T00:00:00Z"

// eventPayloadValueWidth is where a single payload VALUE is elided.
//
// 40 rather than something rounder, measured: the widest real value in a --sim
// log is blueprint_sha at 64 hex characters, and the next widest is a prompt.
// Everything else in the measured log fits well inside 40, so this elides the two
// values a reader can go and get with --json and leaves every other row whole.
const eventPayloadValueWidth = 40

// checkTypePattern refuses the globs kernel.MatchEventType cannot honour.
//
// It is the CLI's copy of internal/blueprint's (*validator).pattern, and the
// duplication is the point: a blueprint's watcher pattern and a `--type` are the
// same language, so the same two mistakes have to get the same two answers. What
// is NOT duplicated is the matching rule itself -- both call one exported
// function, so no amount of drift here can make `--type stage.*` select something
// a watcher would not.
//
// An unknown but well-formed type is deliberately NOT refused. `event emit`
// declares a free-form type, so `custom.deploy.finished` is legal in a log this
// binary can write, and a viewer with a hardcoded list of the 32 constants would
// refuse to look for the events a user emitted themselves. A type that matches
// nothing is reported by the no-matches path with the types actually present,
// which is a better answer than a rejection: it is measured against this log
// rather than against the catalogue.
func checkTypePattern(p string) error {
	if p == "" {
		return nil
	}
	if i := strings.IndexByte(p, '*'); i >= 0 && i != len(p)-1 {
		return fmt.Errorf("--type %q; only a single trailing wildcard is supported "+
			"(stage.*), because a pattern nobody can read at a glance selects events "+
			"nobody expected", p)
	}
	if strings.HasSuffix(p, "*") && p != "*" && !strings.HasSuffix(p, ".*") {
		return fmt.Errorf("--type %q; the wildcard covers a whole segment, so write "+
			"it as %q", p, strings.TrimSuffix(p, "*")+".*")
	}
	return nil
}

// parseSinceSeq reads --since-seq, and says which way the bound points.
//
// Inclusive, matching the parameter's own description ("from this seq"): 12 means
// starting AT 12. The alternative reading is defensible and this one is written
// down in the usage line, because the two differ by exactly one event and the
// event a reader is looking for is very often the one they named.
//
// Zero is accepted and means the same as absent, since seq starts at 1 -- a
// script computing `--since-seq $((last))` from an empty log passes 0 and should
// get the whole log rather than an error about a flag it filled in correctly.
// Negative is refused: there is no seq below 1, so it is a bug in the caller and
// silently widening it would hide the bug.
func parseSinceSeq(raw string) (int64, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("--since-seq %q is not a number; it is a "+
			"sequence number within one run, as printed in the SEQ column", raw)
	}
	if n < 0 {
		return 0, false, fmt.Errorf("--since-seq %d is below the first seq; "+
			"a run's log starts at seq 1", n)
	}
	return n, n > 0, nil
}

// distinctTypes lists the types this log actually holds, sorted.
//
// Computed from the events rather than from kernel's constants, because that is
// what makes it useful to somebody who mistyped a type: the answer to
// `--type stage.submited` is not the catalogue of everything that could ever
// happen, it is the eight things that happened here.
func distinctTypes(events []kernel.Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range events {
		if t := string(e.Type); !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// printEventLog writes the header, the table, and then what the table left out.
//
// The order matters and was chosen against the failure mode this command exists
// to prevent. A reader arrives here because some other command said something they
// doubt; the first line has to tell them they are looking at the right run, and
// the last lines have to tell them what is NOT on screen. Notes in the middle
// would be read as data.
func printEventLog(v eventLogView) {
	if len(v.rows) == 0 {
		printNoEventsMatched(v)
		return
	}

	sim := ""
	if v.simulated {
		sim = "  [simulated]"
	}
	shown := fmt.Sprintf("%d event%s", len(v.rows), plural(len(v.rows)))
	if len(v.rows) != v.total {
		shown = fmt.Sprintf("%d of %d events", len(v.rows), v.total)
	}
	fmt.Printf("run %s  %s, seq %d..%d  %s%s\n\n",
		v.id, shown, v.rows[0].Seq, v.rows[len(v.rows)-1].Seq, v.st.Status, sim)

	// Widths from the rows actually shown, never fixed. `run list` shipped a
	// fixed id column that elided every real id, and the cost there was an id
	// nobody could paste; here it would be a type nobody could pass to --type.
	seqW, typeW, actorW := len("SEQ"), len("TYPE"), len("ACTOR")
	for _, e := range v.rows {
		if n := len(strconv.FormatInt(e.Seq, 10)); n > seqW {
			seqW = n
		}
		if n := len(e.Type); n > typeW {
			typeW = n
		}
		if n := len(e.Actor); n > actorW {
			actorW = n
		}
	}

	fmt.Printf("%*s  %-*s  %-7s  %-*s  %5s  %s\n",
		seqW, "SEQ", typeW, "TYPE", "SOURCE", actorW, "ACTOR", "DEPTH", "PAYLOAD")

	// DEPTH prints the number even when it is 0, unlike the dash every other
	// optional column gets. Found by running this: dashIfZero turned all
	// seventeen executor events into `-`, and the footer directly underneath said
	// the chain "is broken at depth 0" -- a note about a value that appeared
	// nowhere in the table above it. 0 is not a missing depth, it is the depth of
	// a root cause, and the column exists so a cascade that flattens to 0
	// halfway down the log is visible.
	elided := false
	for _, e := range v.rows {
		cell, cut := formatPayloadCell(e.Payload)
		elided = elided || cut
		fmt.Printf("%*d  %-*s  %-7s  %-*s  %5d  %s\n",
			seqW, e.Seq, typeW, string(e.Type), dashIfEmpty(string(e.Source)),
			actorW, dashIfEmpty(e.Actor), e.Depth, cell)
	}

	printEventLogFooter(v, elided)
}

// printEventLogFooter reports only what it measured.
//
// Every line here is conditional on a count taken off the rows above, because a
// note that is always printed is a note that is always skipped -- and the one
// worth reading is the one about time. On a --sim log it says the clock is
// virtual, on a live log it says nothing at all, and it never claims either
// without having counted.
func printEventLogFooter(v eventLogView, elided bool) {
	var notes []string

	if v.tsMissing > 0 || v.tsVirtual > 0 {
		switch {
		// Nothing here is timed at all. Said separately from the partial case
		// because the partial case's sentence -- "the ones that have a ts are in
		// --json" -- is a false promise when there are none, and pointing a reader
		// at --json for a field no event carries is how they conclude this view is
		// hiding something. Found by a hand-written fixture where no event had a
		// ts, which is also what a log written by an older build looks like.
		case v.tsVirtual == 0 && v.tsMissing == len(v.rows):
			notes = append(notes, fmt.Sprintf(
				"not one of these %d events carries a timestamp, so there is no\n"+
					"    time column and --json will not supply one either",
				len(v.rows)))
		case v.tsVirtual == 0:
			notes = append(notes, fmt.Sprintf(
				"%d of these events carry no timestamp at all, so there is no time\n"+
					"    column; the ones that have a ts are in --json",
				v.tsMissing))
		case v.tsMissing == 0 && v.tsVirtual == len(v.rows):
			notes = append(notes, "every timestamp here is "+virtualClockTs+
				": a simulated run\n    drives a virtual clock, so the log has an order and no times")
		default:
			// "1 are empty" is what the first hand run printed on a real 21-event
			// log, since exactly one event (run.started) has no ts. The count is
			// almost always 1, so the ungrammatical branch was the common one.
			notes = append(notes, fmt.Sprintf(
				"%d of %d timestamps are %s and %d %s empty, so\n"+
					"    there is no time column; --json carries ts verbatim",
				v.tsVirtual, len(v.rows), virtualClockTs, v.tsMissing,
				isAre(v.tsMissing)))
		}
	}

	if elided {
		notes = append(notes, fmt.Sprintf(
			"a payload value longer than %d characters is shown elided with …;\n"+
				"    --json has every value whole", eventPayloadValueWidth))
	}

	// Said once, and only when the log holds more than one root. Below that it is
	// the shape of every healthy log -- run.started records no cause because
	// nothing caused it -- and a note on every rendering is a note nobody reads.
	// Above it, the count is the number of separate causal threads, which is worth
	// knowing and is also how a producer that stopped attributing shows up.
	if flat, deep := causalDepthSplit(v.rows); flat > 1 && deep > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d of these events record no cause, so the log holds %d causal threads:\n"+
				"    a root is the run's own start, a timer firing, or something fed in\n"+
				"    from outside the fold. See how one of them runs: arxi event trace",
			flat, flat))
	}

	if len(notes) > 0 {
		fmt.Println()
		for _, n := range notes {
			fmt.Printf("  %s\n", n)
		}
	}

	fmt.Printf("\n  event ids, causes and timestamps: arxi event log %s --json\n", v.id)
	fmt.Printf("  what the fold makes of this: arxi run show %s\n", v.id)
}

// isAre agrees the verb with a count.
//
// A sibling of inbox.go's plural, kept next to its only caller rather than added
// there: the footer's sentence needs the verb and not the noun's suffix, and
// "1 events are empty" and "1 event are empty" are wrong in different ways.
func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// causalDepthSplit counts the roots of a log and the events caused by something.
//
// Split rather than a bare "any missing?", because an event with no cause is not
// evidence of a broken chain: it is a root, and every log has at least one. A log
// that is ALL roots is a single run.started, or a stretch of nothing but timer
// ticks -- both legitimate, and warning about either would train the reader to
// ignore the note. What the callers do with the split is print it only above one
// root, where the count says how many threads the log holds.
func causalDepthSplit(rows []kernel.Event) (flat, deep int) {
	for _, e := range rows {
		if len(e.CausedBy) == 0 && e.CorrelationID == "" {
			flat++
		} else {
			deep++
		}
	}
	return flat, deep
}

// printNoEventsMatched: exit 0, stdout EMPTY, one diagnosis on stderr.
//
// Zero matches is not a failure. `arxi event log r1 --type agent.failed` finding
// nothing is the good news, and a script that greps for failures must not have to
// treat a clean run as an error -- so the exit code stays 0.
//
// stdout stays empty for the reason `run result` keeps it empty: `event log r1
// --type x > slice.ndjson` on a filter that matched nothing has to leave the file
// empty, because the next stage of the pipeline reads whatever is in it as
// content. A header saying "0 events" would be parsed as an event by anything
// naive enough to split on lines.
//
// What goes to stderr is the filter and the types that ARE here. Measured need:
// `--type stage.submited` and `--type stage.submitted` differ by one letter and
// print identically without this, so the user concludes the run never submitted.
func printNoEventsMatched(v eventLogView) {
	switch {
	case v.total == 0:
		fmt.Fprintf(os.Stderr, "run %s has an empty log.\n"+
			"  a run directory with no events is a run that never started; the\n"+
			"  directory is %s\n", v.id, v.dir)
		return

	case v.hasSince && v.sinceSeq > v.headSeq:
		// Separated from a plain no-match because the answer is different: nothing
		// is misspelled, the reader is simply past the end. Printing the head seq
		// turns "why is this empty" into "poll again", which is usually what a
		// caller doing --since-seq is for.
		fmt.Fprintf(os.Stderr, "run %s: nothing at or after seq %d -- the log "+
			"ends at seq %d.\n", v.id, v.sinceSeq, v.headSeq)
		if v.typePattern != "" {
			fmt.Fprintf(os.Stderr, "  --type %s was not reached, so it is not why "+
				"this is empty.\n", v.typePattern)
		}
		return
	}

	var filters []string
	if v.typePattern != "" {
		filters = append(filters, "--type "+v.typePattern)
	}
	if v.hasSince {
		filters = append(filters, fmt.Sprintf("--since-seq %d", v.sinceSeq))
	}
	fmt.Fprintf(os.Stderr, "run %s: no event matches %s (the log holds %d).\n",
		v.id, strings.Join(filters, " "), v.total)
	fmt.Fprintf(os.Stderr, "  types in this log: %s\n", strings.Join(v.allTypes, ", "))
	fmt.Fprintf(os.Stderr, "  exit 0, because a filter that finds nothing is an "+
		"answer and not a failure.\n")
}

// formatPayloadCell renders a payload as sorted key=value pairs.
//
// Sorted so two runs of the same command produce the same bytes -- Go's map order
// is randomised, and an unsorted cell would make `diff` between two logs report
// every line as changed. It happens to match the order the NDJSON is written in,
// which is a convenience and not the reason.
//
// The bool is whether any VALUE was elided, so the footer can say so once instead
// of the reader wondering what the … swallowed. Keys are never elided and no key
// is dropped: see the file comment.
func formatPayloadCell(m map[string]any) (string, bool) {
	if len(m) == 0 {
		return "-", false
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	elided := false
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		s, cut := formatPayloadValue(m[k])
		elided = elided || cut
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, " "), elided
}

// formatPayloadValue is the one-line spelling of a JSON value.
//
// Numbers go through 'f' with -1 precision rather than %v, because encoding/json
// decodes every number as float64 and %v then prints budget_usd 5 as "5" but a
// cost of 0.000015 as "1.5e-05". A dollar figure in exponent notation in the
// middle of a log line is the kind of thing a reader misreads as 1.5.
//
// Elision happens BEFORE quoting, so a cut string still ends in a quote and the …
// is inside it -- `prompt="add rate limiting to the p…"` rather than a line with
// an unbalanced quote, which looks like the renderer broke.
func formatPayloadValue(v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case bool:
		s = strconv.FormatBool(x)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		s = "null"
	default:
		b, err := json.Marshal(x)
		if err != nil {
			s = fmt.Sprint(x)
		} else {
			s = string(b)
		}
	}

	elided := false
	if len(s) > eventPayloadValueWidth {
		s = s[:eventPayloadValueWidth-1] + "…"
		elided = true
	}
	// Quoted only when it would otherwise be ambiguous. A cell is space-separated
	// key=value pairs, so a value containing a space has to be delimited or the
	// reader cannot tell where the next key begins.
	if strings.ContainsAny(s, " \t\n\"") {
		s = strconv.Quote(s)
	}
	return s, elided
}

// eventLogPayload is the machine reading: the events as they are in the log.
//
// Each event is marshalled as kernel.Event, so the wire shape and the file's own
// shape are the same shape by construction. A hand-built map here would be a
// second schema for the one type the whole system agrees on, and it would drop
// whatever field was added to Event next.
//
// Under a named key rather than as a bare array, which is trigger.go's
// convention throughout: an array cannot grow a sibling field, so the day this
// needs to report a truncated log there is nowhere to put it.
//
// The filter is echoed back because a consumer that got 0 events needs to know
// whether that is a quiet run or its own pattern. total beside count answers it
// without a second call.
func eventLogPayload(v eventLogView) map[string]any {
	events := v.rows
	if events == nil {
		// json.Marshal renders a nil slice as null, and a consumer doing
		// `for e in .events` on null gets an error rather than zero iterations.
		events = []kernel.Event{}
	}
	out := map[string]any{
		"run":       v.id,
		"dir":       v.dir,
		"status":    string(v.st.Status),
		"simulated": v.simulated,
		"count":     len(v.rows),
		"total":     v.total,
		"events":    events,
	}
	if v.typePattern != "" {
		out["type"] = v.typePattern
	}
	if v.hasSince {
		out["since_seq"] = v.sinceSeq
	}
	return out
}
