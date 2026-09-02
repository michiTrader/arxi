package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What `arxi event trace` promises, pinned against logs written by hand.
//
// Every fixture here is hand-written, and not because it is convenient. The
// causal fields this verb reads -- caused_by, correlation_id, depth -- are
// written in exactly one place, kernel.derived, and only for events the reducer
// derives. A real log, simulated or not, therefore holds one shape: a handful of
// derived events with a cause and everything else with none. The shapes that
// break a chain walker are the ones a real run cannot produce on demand:
//
//   - a diamond, where two causes are recorded for one event
//   - a cycle, which appending in order cannot write
//   - a caused_by naming an id that is not in the file
//   - an event with no id at all
//   - two events carrying the same id
//   - a stamped depth that disagrees with the tree
//   - an id that is a bare number, which is also how a seq is spelled
//
// A test suite that only fed this verb real logs would exercise the one branch
// that needs it least and leave every diagnosis unproven. So the fixtures are
// files, the same choice runAt documents: a log is a file, and anything that can
// be in a file will one day be read from one.

// chainAt writes a run whose log holds a chain with a branch off the root.
//
// The branch is the point. ev-other is caused by the same event as the subject's
// parent, so it is a sibling: on the correlation group, off the chain. A walker
// that starts at the root and prints everything below it would show it, which is
// how a tree grows into a copy of the log.
func chainAt(t *testing.T, dir, id string) {
	t.Helper()

	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"ev-root","seq":3,"type":"stage.entered","source":"runtime","depth":0,"payload":{"stage":"execute","index":0}}
{"id":"ev-other","seq":4,"type":"agent.activated","source":"runtime","actor":"docs","correlation_id":"ev-root","caused_by":["ev-root"],"depth":1,"payload":{"agent":"docs"}}
{"id":"ev-mid","seq":5,"type":"agent.activated","source":"runtime","actor":"backend","correlation_id":"ev-root","caused_by":["ev-root"],"depth":1,"payload":{"agent":"backend"}}
{"id":"ev-subj","seq":6,"type":"llm.response","source":"agent","actor":"backend","correlation_id":"ev-root","caused_by":["ev-mid"],"depth":2,"payload":{"tokens_out":340}}
{"id":"ev-kid-a","seq":7,"type":"tool.call","source":"agent","actor":"backend","correlation_id":"ev-root","caused_by":["ev-subj"],"depth":3,"payload":{"tool":"bash"}}
{"id":"ev-kid-b","seq":8,"type":"stage.submitted","source":"agent","actor":"backend","correlation_id":"ev-root","caused_by":["ev-subj"],"depth":3,"payload":{"stage":"execute"}}
{"id":"ev-grand","seq":9,"type":"stage.advanced","source":"runtime","correlation_id":"ev-root","caused_by":["ev-kid-b"],"depth":4,"payload":{"to":"review"}}
`)
}

// traceTable returns the printed rows, between the column header and the blank
// line that ends the table.
//
// Sliced by position rather than recognised by pattern. The footer's notes begin
// with a count -- "2 causes above ev-subj, 3 events below it" -- so any rule
// loose enough to read a row's SEQ off the front of a line reads that note as a
// row, and an assertion about the tree would then be answered by the prose
// underneath it.
func traceTable(t *testing.T, out string) []string {
	t.Helper()

	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "SEQ") && strings.Contains(l, "PAYLOAD") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("the output has no column header, so it printed no table:\n%s\n"+
			"  consequence: every assertion below about the tree would pass on an "+
			"empty list of rows.", out)
	}

	var rows []string
	for _, l := range lines[start:] {
		if strings.TrimSpace(l) == "" {
			break
		}
		rows = append(rows, l)
	}
	if len(rows) == 0 {
		t.Fatalf("the table is empty:\n%s", out)
	}
	return rows
}

// traceIDs returns the id of every printed row, in print order.
//
// The box-drawing glyphs are dropped rather than kept, because they belong to the
// layout and the id is what caused_by names. Fields() sees them as separate words
// -- the prefix is "│  └─ " -- so skipping them is a loop and not a trim.
func traceIDs(t *testing.T, out string) []string {
	t.Helper()

	var ids []string
	for _, row := range traceTable(t, out) {
		f := strings.Fields(row)
		if len(f) > 0 && f[0] == ">" {
			f = f[1:]
		}
		if len(f) > 1 {
			f = f[1:] // the SEQ cell
		}
		for len(f) > 0 && (f[0] == "├─" || f[0] == "└─" || f[0] == "│") {
			f = f[1:]
		}
		if len(f) == 0 {
			t.Fatalf("a printed row carries no id: %q", row)
		}
		ids = append(ids, f[0])
	}
	return ids
}

// markedIDs returns the ids of the rows carrying the subject marker.
//
// A list rather than a single id, because "> is on exactly one row" is the thing
// worth asserting: the marker is the only thing telling the reader which of six
// events they asked about.
func markedIDs(t *testing.T, out string) []string {
	t.Helper()

	ids := traceIDs(t, out)
	var marked []string
	for i, row := range traceTable(t, out) {
		if strings.HasPrefix(row, "> ") {
			marked = append(marked, ids[i])
		}
	}
	return marked
}

// traceJSON runs the verb with --json and decodes the object.
//
// The flag is added here rather than by the caller, and that is a correction: it
// used to be the caller's job, the doc comment said otherwise, and four call sites
// left it off. The failure was a decode error against a printed table -- a
// confusing way to be told the flag is missing, and one that says nothing about
// the assertion underneath. A helper whose name promises JSON should ask for it.
func traceJSON(t *testing.T, dir string, args ...string) map[string]any {
	t.Helper()

	argv := append([]string{"event", "trace"}, args...)
	argv = append(argv, "--json")

	out, errb, code := arxiStreams(t, dir, argv...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("--json did not produce one JSON object: %v\n%s\n"+
			"  consequence: --json exists so another program can read this. Output "+
			"it cannot parse is worse than no flag, because the failure surfaces in "+
			"the caller.", err, out)
	}
	return m
}

// num reads a number out of a decoded JSON object.
func num(t *testing.T, m map[string]any, key string) int {
	t.Helper()

	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%q is %v (%T) in the JSON, want a number", key, m[key], m[key])
	}
	return int(f)
}

// strList reads an array of strings out of a decoded JSON object.
//
// A missing key and an empty array are different answers, and this tells them
// apart. The payload builder passes the three defect slices through emptyIfNil
// precisely so a caller sees [] instead of null; a helper that returned nil for
// both would make that decision untestable.
func strList(t *testing.T, m map[string]any, key string) []string {
	t.Helper()

	raw, ok := m[key]
	if !ok {
		t.Fatalf("the JSON has no %q key: %v", key, m)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("%q is %v (%T) in the JSON, want an array", key, raw, raw)
	}
	out := make([]string, 0, len(arr))
	for i, v := range arr {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%q[%d] is %v (%T), want a string", key, i, v, v)
		}
		out = append(out, s)
	}
	return out
}

// traceChain returns the chain rows out of a decoded trace object.
func traceChain(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()

	arr, ok := m["chain"].([]any)
	if !ok {
		t.Fatalf("the JSON has no chain array: %v", m)
	}
	var rows []map[string]any
	for i, v := range arr {
		row, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("chain[%d] is %v (%T), want an object", i, v, v)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestEventTracePrintsTheChainRootFirstWithOneRowMarked(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	chainAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "ev-subj")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	// Root first, then down. The order is the claim the verb makes in its own
	// usage line, and it is the reason to read a tree instead of the log: the log
	// is in append order, which puts a cause and its effect side by side only
	// when nothing else happened in between.
	want := []string{"ev-root", "ev-mid", "ev-subj", "ev-kid-a", "ev-kid-b", "ev-grand"}
	got := traceIDs(t, r.out)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the chain printed %v, want %v\n%s\n"+
			"  consequence: this verb answers \"what led to this\". Rows in the "+
			"wrong order, or a row missing, is a different answer to that question.",
			got, want, r.out)
	}

	if marked := markedIDs(t, r.out); len(marked) != 1 || marked[0] != "ev-subj" {
		t.Errorf("the subject marker is on %v, want exactly [ev-subj]\n%s\n"+
			"  consequence: the marker is the only thing on screen saying which of "+
			"these six events was asked about. On two rows it says nothing; on the "+
			"wrong row it misdirects.", marked, r.out)
	}

	for _, want := range []string{
		"6 of 9 events on the chain of ev-subj",
		"> marks ev-subj, the event asked about",
		"2 causes above ev-subj, 3 events below it",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the output does not say %q:\n%s\n"+
				"  consequence: the header and footer are what make the tree "+
				"readable without counting rows by hand.", want, r.out)
		}
	}
}

// A sibling is off the chain and the footer is the only place it exists.
//
// Both halves matter and they pull against each other. Printing ev-other would
// make the tree a copy of the log; saying nothing about it would make a two-row
// tree look like the whole story of a turn that produced four events. The
// correlation id is the one thing in the file that knows the sibling is there.
func TestEventTraceLeavesASiblingOffTheChainAndSaysItIsThere(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	chainAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "ev-subj")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	for _, gone := range traceIDs(t, r.out) {
		if gone == "ev-other" {
			t.Errorf("ev-other is on the chain of ev-subj:\n%s\n"+
				"  consequence: it is caused by ev-root, the same event that caused "+
				"the subject's parent -- a sibling, not an ancestor. Walking down "+
				"from a root without restriction prints the whole log and buries "+
				"the event asked about.", r.out)
		}
	}

	// 7 and not 6: the group includes ev-root, whose id IS the correlation id and
	// which therefore does not carry it. Counting only the carriers would report a
	// group of 6 for 7 events that belong together.
	const note = "7 events share the correlation id ev-root"
	if !strings.Contains(r.out, note) {
		t.Errorf("the footer does not say %q:\n%s\n"+
			"  consequence: the sibling this tree deliberately does not print is "+
			"then invisible, and the reader has no reason to look for it.", note, r.out)
	}
	if !strings.Contains(r.out, "6 are on this chain and the rest are siblings") {
		t.Errorf("the footer does not say how many of the group are on the chain:\n%s\n"+
			"  consequence: \"7 share the id\" without \"6 are here\" leaves the "+
			"reader to subtract, and the whole point of the note is the difference.",
			r.out)
	}
}

// diamondAt writes a log holding two shapes the reducer cannot produce together.
//
// ev-join records two causes, which kernel.derived never writes -- it takes ONE
// cause -- and ev-late records a depth that disagrees with where the tree puts
// it. Both are in one fixture because the interesting assertion is that only one
// of them is reported.
func diamondAt(t *testing.T, dir, id string) {
	t.Helper()

	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"ev-a","seq":3,"type":"stage.entered","source":"runtime","depth":0,"payload":{"stage":"execute","index":0}}
{"id":"ev-b","seq":4,"type":"agent.activated","source":"runtime","actor":"backend","caused_by":["ev-a"],"depth":1,"payload":{"agent":"backend"}}
{"id":"ev-c","seq":5,"type":"llm.response","source":"agent","actor":"backend","caused_by":["ev-b"],"depth":2,"payload":{"tokens_out":12}}
{"id":"ev-late","seq":6,"type":"tool.call","source":"agent","actor":"backend","caused_by":["ev-c"],"depth":9,"payload":{"tool":"bash"}}
{"id":"ev-join","seq":7,"type":"stage.submitted","source":"agent","actor":"backend","caused_by":["ev-a","ev-late"],"depth":1,"payload":{"stage":"execute"}}
`)
}

func TestEventTraceCountsADiamondOnceAndPrintsItTwice(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	diamondAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "ev-join")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	// Six rows, five events: ev-join is reached down the long path and again on
	// the direct edge from ev-a. Printed both times, because a reader following
	// the tree from ev-a must not find the branch ending in nothing -- and counted
	// once, because a header claiming 6 of 7 would claim a chain the log cannot
	// hold.
	if rows := len(traceTable(t, r.out)); rows != 6 {
		t.Errorf("the table has %d rows, want 6:\n%s\n"+
			"  consequence: ev-join is caused by two events here. Reaching it "+
			"twice and printing it once leaves one branch of the tree stopping "+
			"at nothing.", rows, r.out)
	}
	if !strings.Contains(r.out, "5 of 7 events on the chain of ev-join") {
		t.Errorf("the header does not say 5 of 7:\n%s\n"+
			"  consequence: six rows for five events. Counting rows would report a "+
			"chain longer than the log, which is the one number a reader cannot "+
			"check without counting the file.", r.out)
	}
	if !strings.Contains(r.out, "(shown above)") {
		t.Errorf("the repeated row is not marked:\n%s\n"+
			"  consequence: the same event twice, with its payload twice, reads as "+
			"two events -- exactly the double-counting the header avoids.", r.out)
	}
}

// A depth stamped from one of two causes is not a disagreement to report.
//
// kernel.derived takes ONE cause and sets depth to that cause's depth plus one.
// An event with two causes therefore agrees with one parent and disagrees with
// the other by however far apart they sit, and reporting that is reporting the
// design. A footer note that fires on correct logs is one the reader learns to
// skip -- and it is the note that has to be trusted when a depth really is wrong.
func TestEventTraceReportsAWrongDepthButNotADiamondsDepth(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	diamondAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "ev-join")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	// ev-late has one cause and sits three deep while recording nine, which is
	// either a bad stamp or a rebuilt tree that is wrong. Either way the reader
	// needs to know, because depth is what MaxDepth brakes on.
	if !strings.Contains(r.out, "ev-late records 9, sits at 3") {
		t.Errorf("the footer does not report ev-late's depth:\n%s\n"+
			"  consequence: depth is the figure MaxDepth stops a cascade on. A "+
			"stamp that disagrees with the tree means one of the two is wrong, and "+
			"nothing else in the output compares them.", r.out)
	}
	// ev-join sits at 4 and records 1, taken from ev-a. Correct by construction.
	if strings.Contains(r.out, "ev-join records") {
		t.Errorf("the footer reports ev-join's depth as a disagreement:\n%s\n"+
			"  consequence: every event with two causes would be reported, so the "+
			"note would fire on logs with nothing wrong and stop being read.", r.out)
	}
	if !strings.Contains(r.out, "depth disagrees with this tree for 1 event:") {
		t.Errorf("the count in the depth note is not 1:\n%s\n"+
			"  consequence: the count is how a reader decides whether one stamp is "+
			"off or the tree is being rebuilt wrong.", r.out)
	}
}

// A cycle terminates the walk, and the footer says the tree is not the file.
//
// Two events naming each other cannot be appended in order, so a log holding one
// was written by something other than this binary -- or truncated and stitched.
// The walk stopping is not a nicety: the failure of a chain walker on a cyclic
// graph is not a wrong answer, it is no answer, and this test proves termination
// by the only means available, which is returning at all.
func TestEventTraceTerminatesOnACycleAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"ev-x","seq":3,"type":"agent.activated","source":"runtime","actor":"backend","caused_by":["ev-y"],"depth":0,"payload":{"agent":"backend"}}
{"id":"ev-y","seq":4,"type":"llm.response","source":"agent","actor":"backend","caused_by":["ev-x"],"depth":1,"payload":{"tokens_out":7}}
`)

	r := arxi(t, dir, "event", "trace", "ev-y")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s\n"+
			"  a cyclic log is a log to diagnose, not an argument to refuse: "+
			"refusing leaves the user with the file and no tool.", r.code, r.out)
	}

	if !strings.Contains(r.out, "(shown above, and it is above itself)") {
		t.Errorf("the second reach of ev-x is not marked as a cycle:\n%s\n"+
			"  consequence: a diamond and a cycle both arrive at a row already "+
			"printed. Marked the same way, an impossible log reads as an ordinary "+
			"one.", r.out)
	}
	if !strings.Contains(r.out, "this log records a cycle at ev-y, ev-x") {
		t.Errorf("the footer does not name the cycle:\n%s\n"+
			"  consequence: the walk stops at the repeat, so the tree above is not "+
			"a faithful picture of the file. Without the note it looks like one.",
			r.out)
	}
	// The pointer spells the run as a directory. A run started with --dir sits
	// outside runs/ and resolveRunDir cannot find it from the id, so the id form
	// would print a command that fails for exactly the user who needs it most.
	if want := "arxi event log " + filepath.Join("runs", id); !strings.Contains(r.out, want) {
		t.Errorf("the cycle note does not point at %q:\n%s\n"+
			"  consequence: the advice for an unfaithful tree is to read the raw "+
			"log, and a pointer that does not resolve is not advice.", want, r.out)
	}
}

// danglingAt writes a log whose only cause-carrying event names an id that is
// not in the file.
//
// ev-000042 is spelled the way the reducer mints ids, so it is the id of an event
// that plausibly existed -- in another run, or in a line that was lost. That is
// the case worth diagnosing; a cause naming "nonsense" would be the same code
// path with a less honest fixture.
func danglingAt(t *testing.T, dir, id string) {
	t.Helper()

	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"ev-orphan","seq":3,"type":"tool.call","source":"agent","actor":"backend","caused_by":["ev-000042"],"depth":1,"payload":{"tool":"bash"}}
`)
}

// A cause that names nothing is still a cause, and the footer must not deny it.
//
// The subject records one. Nothing in the log carries the id it names, so the walk
// finds no ancestor and the chain is the subject alone -- the same arithmetic as
// an event with no cause at all, reached for the opposite reason. Both halves of
// this test are wording an earlier draft got wrong, found by reading the rendered
// footer against this fixture rather than by reading the code: it said the event
// "records no cause" two lines above the note listing the cause it records, and
// it said "1 of the 3 events here do carry one".
func TestEventTraceDoesNotSayNoCauseWhenTheCauseIsDangling(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	danglingAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "ev-orphan")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	if !strings.Contains(r.out, "the chain is this one event: nothing in this log records ev-orphan as a") {
		t.Errorf("the footer does not explain the one-event chain for a dangling cause:\n%s\n"+
			"  consequence: a chain of one is the answer a reader is least able to "+
			"act on, so it is the one that has to say why it is one.", r.out)
	}
	if strings.Contains(r.out, "records no cause") {
		t.Errorf("the footer says ev-orphan records no cause, and the note below it lists one:\n%s\n"+
			"  consequence: a footer that contradicts itself reads as broken rather "+
			"than as precise, and nothing tells the reader which sentence to trust.",
			r.out)
	}
	if !strings.Contains(r.out, "caused_by names 1 id no event here carries: ev-000042") {
		t.Errorf("the footer does not name the id that resolves to nothing:\n%s\n"+
			"  consequence: this is the difference between a chain that ends and a "+
			"log that is missing a line. The id is the only thing the reader can "+
			"search another file for.", r.out)
	}

	// The tail counts the events that do carry a cause, and here that is one: the
	// subject. A count of 1 in front of a plural verb is the disagreement isAre
	// exists to prevent, and this fixture is the shortest log that reaches it.
	if !strings.Contains(r.out, "a cause is recorded on 1 of the 3 events in this log") {
		t.Errorf("the footer does not count the events carrying a cause:\n%s\n"+
			"  consequence: without it, a one-event chain in a log where causes are "+
			"written looks the same as one in a log where they never are -- and only "+
			"the first is a gap worth chasing.", r.out)
	}
	if strings.Contains(r.out, "do carry one") {
		t.Errorf("the footer still uses the phrasing that disagrees with a count of 1:\n%s\n"+
			"  consequence: \"1 of the 3 events here do carry one\". The sentence was "+
			"rewritten so the number never sits in front of a verb, which is the only "+
			"fix that holds for every count.", r.out)
	}
}

// --json keeps "names a cause" and "has one" apart, which the tree cannot.
//
// A row's `parents` is the edge list: the causes this log can resolve. The id the
// subject actually names stays in `event.caused_by`. A consumer reading only
// parents sees a root; one reading only caused_by sees a child; the pair is what
// says the line is missing. Collapsing them either way is what makes an automated
// reader of this output draw the wrong conclusion silently.
func TestEventTraceJSONKeepsADanglingCauseOutOfTheEdges(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	danglingAt(t, dir, id)

	m := traceJSON(t, dir, "ev-orphan")

	if got := strList(t, m, "dangling"); len(got) != 1 || got[0] != "ev-000042" {
		t.Errorf("dangling is %v, want [ev-000042]\n"+
			"  consequence: this is the machine-readable half of the footer note. A "+
			"consumer checking whether a log is whole has nothing else to check.", got)
	}

	rows := traceChain(t, m)
	if len(rows) != 1 {
		t.Fatalf("the chain has %d rows, want 1: %v", len(rows), rows)
	}
	if parents := strList(t, rows[0], "parents"); len(parents) != 0 {
		t.Errorf("parents is %v, want empty\n"+
			"  consequence: parents is the edge list, and an id that resolves to "+
			"nothing is not an edge. A consumer rebuilding the tree from it would "+
			"add a node that does not exist.", parents)
	}
	ev, ok := rows[0]["event"].(map[string]any)
	if !ok {
		t.Fatalf("chain[0] has no event object: %v", rows[0])
	}
	if cb := strList(t, ev, "caused_by"); len(cb) != 1 || cb[0] != "ev-000042" {
		t.Errorf("event.caused_by is %v, want [ev-000042]\n"+
			"  consequence: the row carries the whole event so the log's own record "+
			"survives this view. Dropping the unresolvable id here would leave no "+
			"way to tell a missing line from an event that never had a cause.", cb)
	}

	// Two of the three: run.started and stage.entered, which this fixture writes
	// without a cause. The subject is the one event here carrying one, and that is
	// what makes the count worth publishing -- a consumer seeing 0 knows the chain
	// is empty for a reason that has nothing to do with this log's contents.
	if n := num(t, m, "events_without_cause"); n != 2 {
		t.Errorf("events_without_cause is %d, want 2\n"+
			"  consequence: an event with no cause is a root, and every log has one. A "+
			"consumer cannot tell a root from a chain broken by a producer that stopped "+
			"attributing without this figure.", n)
	}

	// Empty, though ev-orphan records depth 1 and prints at the top of the tree.
	// A root's expected depth is seeded from its own stamp precisely because there
	// is nothing above it to check against: this tree was rebuilt from a fragment,
	// and the fragment's first event is not evidence of anything.
	if got := strList(t, m, "depth_mismatch"); len(got) != 0 {
		t.Errorf("depth_mismatch is %v, want empty\n"+
			"  consequence: every chain that does not start at depth 0 -- which is "+
			"every chain traced from a partial log -- would report its own root as a "+
			"defect, and the note would fire on logs with nothing wrong.", got)
	}

	if got, _ := m["matched_by"].(string); got != "id" {
		t.Errorf("matched_by is %q, want \"id\"\n"+
			"  consequence: the argument is read as an id and as a seq. A consumer "+
			"that passed one and got the other back has to know which happened "+
			"before it can follow caused_by.", got)
	}
}

// messyAt writes a log holding two defects that cannot shadow each other.
//
// Two events carry the id ev-dup, and a third carries none. Both are hand-written
// shapes -- exec.stamp mints ev-000001, ev-000002, ... so neither can occur -- and
// they are in one fixture because they are counted by separate passes: an event
// with no id is left out of the index and out of the reverse index, so it cannot
// appear on a chain, while a duplicate id is in the index once and decides which
// of two events every caused_by referring to it resolves to.
//
// The twins carry different types on purpose. That is the only thing in a printed
// row that says which of the two was traced.
func messyAt(t *testing.T, dir, id string) {
	t.Helper()

	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"ev-dup","seq":3,"type":"agent.activated","source":"runtime","actor":"backend","depth":0,"payload":{"agent":"backend"}}
{"id":"ev-dup","seq":4,"type":"llm.response","source":"agent","actor":"backend","depth":0,"payload":{"tokens_out":5}}
{"seq":5,"type":"stage.submitted","source":"agent","actor":"backend","caused_by":["ev-dup"],"depth":1,"payload":{"stage":"execute"}}
{"id":"ev-kid","seq":6,"type":"tool.call","source":"agent","actor":"backend","caused_by":["ev-dup"],"depth":1,"payload":{"tool":"bash"}}
`)
}

func TestEventTraceTracesTheFirstOfTwoEventsSharingAnIDAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	messyAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "ev-dup")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	rows := traceTable(t, r.out)
	if !strings.Contains(rows[0], "agent.activated") {
		t.Errorf("the traced row is %q, want the seq 3 twin (agent.activated)\n%s\n"+
			"  consequence: the index takes the first event carrying an id, and that "+
			"is also the one every caused_by resolves to. Tracing the second would "+
			"print a subject that nothing in the tree below is actually attached to.",
			rows[0], r.out)
	}
	for _, row := range rows {
		if strings.Contains(row, "llm.response") {
			t.Errorf("the seq 4 twin is on the chain:\n%s\n"+
				"  consequence: it is a second event with the same id, not a second "+
				"place in the chain. Printing both would double every count in the "+
				"header.", r.out)
		}
	}

	if !strings.Contains(r.out, "2 events in this log carry the id ev-dup. The first is the one traced,") {
		t.Errorf("the footer does not report the shared id:\n%s\n"+
			"  consequence: the reader is looking at one of two events and has no way "+
			"to know it. The ambiguity is in the file, so it cannot be resolved here "+
			"-- only reported.", r.out)
	}
	// One of the six, and it is not on the chain: an event with no id is left out
	// of both indexes, so nothing can name it as a cause and it can cause nothing.
	// The count is the only trace of it in this view.
	if !strings.Contains(r.out, "no id on 1 of the 6 events in this log") {
		t.Errorf("the footer does not count the event with no id:\n%s\n"+
			"  consequence: it is silently absent from the tree. A reader comparing "+
			"the chain against the raw log would find a line this view never "+
			"mentions.", r.out)
	}
	if !strings.Contains(r.out, "2 of 6 events on the chain of ev-dup") {
		t.Errorf("the header does not say 2 of 6:\n%s\n"+
			"  consequence: the total counts every line in the log, including the one "+
			"with no id and the duplicate twin. A total that quietly skipped them "+
			"would not match the file the reader can open.", r.out)
	}

	m := traceJSON(t, dir, "ev-dup")
	if n := num(t, m, "duplicate_ids"); n != 2 {
		t.Errorf("duplicate_ids is %d, want 2\n"+
			"  consequence: the key exists so a consumer can tell that the subject it "+
			"asked for is not uniquely named. Without it the JSON looks definite.", n)
	}
	if n := num(t, m, "blank_ids"); n != 1 {
		t.Errorf("blank_ids is %d, want 1\n"+
			"  consequence: a consumer diffing this chain against the log would find "+
			"an event it cannot account for.", n)
	}
}

// An event with no id is refused as a subject, and pointed away from.
//
// Reachable only through a seq, since caused_by names ids and so does the argument
// when it is read as one. Nothing in the log can reference this event, so there is
// no chain below it -- and printing a one-row tree would read as "nothing caused
// this", a claim about the run rather than about the log's shape. The refusal
// names the cause it does record, which is the nearest question that has an answer.
func TestEventTraceRefusesAnEventWithNoIDAndOffersItsCause(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	messyAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "event", "trace", "5")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s\n"+
			"  a subject that cannot be traced is a refusal, not a result: exit 0 "+
			"would tell a script the chain was printed.", code, out, errb)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the refusal wrote to stdout:\n%s\n"+
			"  consequence: stdout is where the tree goes. A refusal there is "+
			"indistinguishable from output for anything reading a pipe.", out)
	}
	if !strings.Contains(errb, "seq 5 of run ") || !strings.Contains(errb, "is a stage.submitted with no id.") {
		t.Errorf("the refusal does not say which line has no id:\n%s\n"+
			"  consequence: the argument was read as a seq, so the seq is the only "+
			"handle the user has on that line. Without it they cannot find it in the "+
			"file.", errb)
	}
	// The cause is offered run-qualified, because a bare id is what the ambiguity
	// refusal exists for: this advice must not need a second correction.
	if want := "arxi event trace " + filepath.Join("runs", id, "ev-dup"); !strings.Contains(errb, want) {
		t.Errorf("the refusal does not offer %q:\n%s\n"+
			"  consequence: this event records a cause, and that one can be traced. "+
			"Refusing without saying so ends the user's investigation at the first "+
			"obstacle.", want, errb)
	}
}

// numberIDAt writes a log where one event's id is a number that is also another
// event's seq.
//
// The collision is the fixture. An argument of "7" has two readings here, and the
// order between them is a decision no test on separate logs can pin: only a file
// holding both spellings can tell whether the id was tried first.
func numberIDAt(t *testing.T, dir, id string) {
	t.Helper()

	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"ev-root","seq":3,"type":"stage.entered","source":"runtime","depth":0,"payload":{"stage":"execute","index":0}}
{"id":"7","seq":4,"type":"agent.activated","source":"runtime","actor":"backend","caused_by":["ev-root"],"depth":1,"payload":{"agent":"backend"}}
{"id":"ev-tail","seq":5,"type":"llm.response","source":"agent","actor":"backend","caused_by":["7"],"depth":2,"payload":{"tokens_out":11}}
{"id":"ev-seven","seq":7,"type":"stage.advanced","source":"runtime","depth":0,"payload":{"to":"review"}}
`)
}

// A number is an id first and a seq second.
//
// caused_by names ids, so an id is the spelling that can be copied out of another
// event and pasted here. Reading the number as a line number first would shadow an
// event that really carries that id -- and the shadowing is silent, because the
// seq reading always succeeds on a log long enough.
func TestEventTraceReadsANumberAsAnIDBeforeASeq(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	numberIDAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "7")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}

	if marked := markedIDs(t, r.out); len(marked) != 1 || marked[0] != "7" {
		t.Errorf("the subject marker is on %v, want exactly [7]\n%s\n"+
			"  consequence: seq 7 in this log is ev-seven, a different event with no "+
			"chain at all. Reading the argument as a seq first would trace it and "+
			"look like a correct answer to a question nobody asked.", marked, r.out)
	}
	want := []string{"ev-root", "7", "ev-tail"}
	if got := traceIDs(t, r.out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the chain printed %v, want %v\n%s", got, want, r.out)
	}
	if !strings.Contains(r.out, "3 of 6 events on the chain of 7") {
		t.Errorf("the header does not say 3 of 6:\n%s", r.out)
	}
	if strings.Contains(r.out, "ev-seven") {
		t.Errorf("the event at seq 7 appears in the output:\n%s\n"+
			"  consequence: it is neither the subject nor on the subject's chain. Its "+
			"presence would mean the seq reading was used as well as the id one.", r.out)
	}
	if strings.Contains(r.out, "and that is the spelling caused_by uses") {
		t.Errorf("the seq note is printed for an argument matched by id:\n%s\n"+
			"  consequence: the note explains that a line number was translated into "+
			"an id. Printed here it says a translation happened that did not.", r.out)
	}
}

// A number that is no event's id falls through to the seq, and the output says so.
//
// Both halves are needed. The fallback is what makes `arxi event log` and this verb
// usable together -- the log prints a SEQ column, and a reader points at a line. The
// note is what stops the fallback from being a trap: the id is what the next
// caused_by will be spelled with, and a reader who typed a line number does not
// have it yet.
func TestEventTraceSaysWhichReadingResolvedTheArgument(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	numberIDAt(t, dir, id)

	r := arxi(t, dir, "event", "trace", "5")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s\n"+
			"  no event here carries the id \"5\", and refusing at that point would "+
			"make the SEQ column of `arxi event log` unusable as input.", r.code, r.out)
	}
	if marked := markedIDs(t, r.out); len(marked) != 1 || marked[0] != "ev-tail" {
		t.Errorf("the subject marker is on %v, want exactly [ev-tail]\n%s\n"+
			"  consequence: seq 5 is ev-tail. Any other row means the fallback landed "+
			"somewhere else.", marked, r.out)
	}
	if !strings.Contains(r.out, "seq 5 in this log is ev-tail, and that is the spelling caused_by uses:") {
		t.Errorf("the footer does not translate the seq into an id:\n%s\n"+
			"  consequence: the reader now has a tree whose rows reference each other "+
			"by id, and the thing they typed is not one. The note is where they learn "+
			"the id of the event they asked about.", r.out)
	}

	// The same claim, for a consumer that cannot read prose. A caller that passed a
	// seq and got a chain back has to know the argument was reinterpreted before it
	// stores the subject as an id.
	if got, _ := traceJSON(t, dir, "5")["matched_by"].(string); got != "seq" {
		t.Errorf("matched_by for \"5\" is %q, want \"seq\"", got)
	}
	if got, _ := traceJSON(t, dir, "7")["matched_by"].(string); got != "id" {
		t.Errorf("matched_by for \"7\" is %q, want \"id\"\n"+
			"  consequence: this log holds both readings of 7. A consumer told \"seq\" "+
			"would look up the wrong event next.", got)
	}
}

// An id that names one event in each of two runs is refused, not resolved.
//
// Ids are minted per run -- ev-000001 upward -- so a bare id is a spelling most
// logs hold. Taking the first match would answer a question about one run with a
// chain out of another, and the answer would look exactly like a correct one:
// same shape, same column widths, plausible types. The refusal is exit 2 rather
// than 1 because nothing is missing; the argument is under-specified, and the fix
// is to type more.
func TestEventTraceRefusesAnIDFoundInTwoRunsAndTheOfferedSpellingWorks(t *testing.T) {
	dir := t.TempDir()
	const first, second = "rmthws2dz-0f4c81aa", "rmthws2dz-93381f43"
	chainAt(t, dir, first)
	chainAt(t, dir, second)

	out, errb, code := arxiStreams(t, dir, "event", "trace", "ev-subj")
	if code != 2 {
		t.Fatalf("exit %d, want 2\nstdout:\n%s\nstderr:\n%s\n"+
			"  exit 1 would say the event does not exist. It exists twice, which is a "+
			"different problem with a different fix.", code, out, errb)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the refusal wrote to stdout:\n%s", out)
	}
	if !strings.Contains(errb, `"ev-subj" is an event id in 2 runs, so it does not name one event.`) {
		t.Errorf("the refusal does not say the id is not unique:\n%s\n"+
			"  consequence: without the count, \"say which run\" reads as a style "+
			"preference rather than as the only way to ask this question.", errb)
	}
	for _, run := range []string{first, second} {
		if want := "arxi event trace " + filepath.Join("runs", run, "ev-subj"); !strings.Contains(errb, want) {
			t.Errorf("the refusal does not offer %q:\n%s\n"+
				"  consequence: both runs hold the id, so both are candidates. A "+
				"refusal listing one of them sends the user to a coin flip.", want, errb)
		}
	}

	// The offered spelling, run verbatim. A refusal whose advice is untested is a
	// refusal that can send the user in a circle.
	qualified := filepath.Join("runs", first, "ev-subj")
	r := arxi(t, dir, "event", "trace", qualified)
	if r.code != 0 {
		t.Fatalf("arxi event trace %s: exit %d, want 0\n%s\n"+
			"  this is the command the refusal above prints.", qualified, r.code, r.out)
	}
	if marked := markedIDs(t, r.out); len(marked) != 1 || marked[0] != "ev-subj" {
		t.Errorf("the subject marker is on %v, want exactly [ev-subj]\n%s", marked, r.out)
	}
	if got, _ := traceJSON(t, dir, qualified)["dir"].(string); got != filepath.Join("runs", first) {
		t.Errorf("the traced run is %q, want %q\n"+
			"  consequence: both logs are identical here, so nothing in the tree says "+
			"which one was read. If the qualifier were ignored the output would still "+
			"look right -- which is the whole reason it is asserted on the dir.",
			got, filepath.Join("runs", first))
	}
}

// When nothing matches, the refusal names the readings it tried.
//
// The argument has up to three: an id, a seq when it is a number, and a
// <run>/<event> pair when it holds a separator. A user who mistyped the run half
// would otherwise be told their event does not exist -- true, and the wrong thing
// to go looking for.
func TestEventTraceSaysWhichReadingsItTriedWhenNothingMatches(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	chainAt(t, dir, id)

	for _, tc := range []struct {
		name string
		arg  string
		want []string
		gone []string
	}{
		{
			name: "a qualified run that holds no such event",
			arg:  filepath.Join("runs", id, "ev-nope"),
			want: []string{
				`holds no event "ev-nope".`,
				"its log holds 9 events, seq 1..9.",
				"the ids: arxi event log " + filepath.Join("runs", id) + " --json",
			},
			// Not a number, so the seq reading was never tried and claiming it was
			// would send the reader counting lines for nothing.
			gone: []string{"it was read as an id and as a seq"},
		},
		{
			// A number that is neither. Both readings are named in one line here,
			// because the alternative is a user who tries "99" again as a seq.
			name: "a number that is no id and no seq",
			arg:  "99",
			want: []string{
				`no run under runs/ holds an event "99".`,
				"searched 1 run.",
				"it was read as an id and as a seq: no log has either.",
			},
		},
		{
			// The whole argument is still read as an id, because an id may contain a
			// slash. So this refusal has to report two failures, not one.
			name: "a run part that is not a run",
			arg:  "nosuchrun/ev-subj",
			want: []string{
				`no run under runs/ holds an event "nosuchrun/ev-subj".`,
				`read as <run>/<event>, "nosuchrun" is not a run directory either`,
			},
			gone: []string{"it was read as an id and as a seq"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, code := arxiStreams(t, dir, "event", "trace", tc.arg)
			if code != 1 {
				t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("the refusal wrote to stdout:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(errb, want) {
					t.Errorf("the refusal does not say %q:\n%s\n"+
						"  consequence: the user has to guess whether the event, the "+
						"run, or the spelling is what went wrong.", want, errb)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(errb, gone) {
					t.Errorf("the refusal says %q for an argument it does not apply to:\n%s\n"+
						"  consequence: a reading that was never tried, reported as "+
						"tried, is a false negative the user cannot check.", gone, errb)
				}
			}
		})
	}
}

// No event and an empty event are both refused with the usage screen.
//
// The empty string is the interesting one. It satisfies a required positional, so
// the surface's own checking lets it through, and searching every log for the id
// "" would match every event that carries none -- a chain assembled out of the
// events that are least able to be on one.
func TestEventTraceRefusesNoEventAndAnEmptyOneWithItsUsage(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	chainAt(t, dir, id)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no argument at all", []string{"event", "trace"}},
		{"an empty argument", []string{"event", "trace", ""}},
		{"whitespace only", []string{"event", "trace", "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, code := arxiStreams(t, dir, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2\nstdout:\n%s\nstderr:\n%s\n"+
					"  a malformed invocation is exit 2: nothing was looked for, so "+
					"nothing can be reported missing.", code, out, errb)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("the refusal wrote to stdout:\n%s", out)
			}
			if !strings.Contains(errb, "usage: arxi event trace <event> [--json]") {
				t.Errorf("the refusal does not print the usage screen:\n%s\n"+
					"  consequence: the argument is an id the user does not have yet. "+
					"The usage line is where it says how to find one.", errb)
			}
		})
	}
}

// brakeAt writes a chain four deep under a blueprint the caller chooses.
//
// The snapshot is a parameter because MaxDepth is the entire subject of the test
// below, and there are three answers to give: no file at all, a brake the chain
// reaches, and a brake it comes close to. Everything else about the three runs is
// byte-identical, which is what makes the footer note the only difference.
func brakeAt(t *testing.T, dir, id, snapshot string) {
	t.Helper()

	run := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	// No snapshot is a real state rather than a broken fixture: the file appears
	// when a run freezes its blueprint, and a directory that never got that far
	// still holds a readable log. Written by hand here because runAt always
	// writes one, and one of the three cases is about not having it.
	if snapshot != "" {
		path := filepath.Join(run, "blueprint.snapshot.yaml")
		if err := os.WriteFile(path, []byte(snapshot), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	log := `{"id":"e1","seq":1,"type":"run.started","payload":{"actor":"deep-team","run_id":"` + id + `","budget_usd":1}}
{"id":"ev-root","seq":2,"type":"stage.entered","source":"runtime","depth":0,"payload":{"stage":"execute","index":0}}
{"id":"ev-a","seq":3,"type":"agent.activated","source":"runtime","actor":"backend","correlation_id":"ev-root","caused_by":["ev-root"],"depth":1,"payload":{"agent":"backend"}}
{"id":"ev-b","seq":4,"type":"llm.response","source":"agent","actor":"backend","correlation_id":"ev-root","caused_by":["ev-a"],"depth":2,"payload":{"tokens_out":120}}
{"id":"ev-c","seq":5,"type":"tool.call","source":"agent","actor":"backend","correlation_id":"ev-root","caused_by":["ev-b"],"depth":3,"payload":{"tool":"bash"}}
`
	if err := os.WriteFile(filepath.Join(run, "events.ndjson"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
}

// brakeSnapshot is runAt's blueprint with a max_depth of the caller's choosing.
//
// max_depth is a top-level key and the loader refuses ones it does not know, so a
// misspelling here fails loudly rather than leaving the default in place.
func brakeSnapshot(depth string) string {
	return "name: deep-team\n" +
		"max_depth: " + depth + "\n" +
		"members:\n" +
		"  - name: backend\n" +
		"    tools: [read, write, bash]\n" +
		"stages:\n" +
		"  - name: execute\n" +
		"    advance_when: all\n"
}

// The footer names the depth brake only when the run has one, and says whether
// the chain hit it.
//
// This is the note that turns a tree into a diagnosis. A cascade that stops has
// two explanations -- the work finished, or MaxDepth refused to wake anything
// deeper -- and the events look identical either way, because the event that was
// never derived left nothing behind. Only the config knows which happened.
//
// The no-blueprint case is the one worth the fixture. kernel.Config zeroes to
// MaxDepth 0, and a run whose blueprint was never frozen folds to exactly that,
// so an unguarded note would tell the reader this run "stops waking watchers at
// 0" -- describing a brake that does not exist as one clamped shut, on the runs
// that are hardest to reason about anyway.
func TestEventTraceNamesTheBrakeOnlyWhenTheRunHasOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot string
		want     []string
		gone     []string
		maxDepth int
	}{
		{
			name:     "no frozen blueprint, so no brake to name",
			snapshot: "",
			gone:     []string{"stops waking", "brake"},
		},
		{
			// depth 3 reached, brake at 3: nothing deeper could have been woken.
			name:     "the brake is exactly where the chain ends",
			snapshot: brakeSnapshot("3"),
			want: []string{
				"the deepest event here records depth 3 and this run stops waking",
				"watchers at 3: nothing below this row can have been woken, so the",
				"chain ends because the brake held, not because the work finished",
			},
			maxDepth: 3,
		},
		{
			// Two to go. Said in a different sentence, because "close to the brake"
			// and "stopped by the brake" are different facts about the run.
			name:     "the brake is two events further down",
			snapshot: brakeSnapshot("5"),
			want: []string{
				"the deepest event here records depth 3 and this run stops waking",
				"watchers at 5, so this cascade is 2 events from the brake",
			},
			gone:     []string{"the brake held"},
			maxDepth: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const id = "rmthws2dz-0f4c81aa"
			brakeAt(t, dir, id, tc.snapshot)

			r := arxi(t, dir, "event", "trace", "ev-b")
			if r.code != 0 {
				t.Fatalf("exit %d, want 0:\n%s", r.code, r.out)
			}
			for _, want := range tc.want {
				if !strings.Contains(r.out, want) {
					t.Errorf("the footer does not say %q:\n%s\n"+
						"  consequence: the reader is left to guess whether the "+
						"cascade stopped because the work was done or because the "+
						"brake refused to wake anything deeper.", want, r.out)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(r.out, gone) {
					t.Errorf("the footer says %q for a run that does not support it:\n%s\n"+
						"  consequence: a claim about the config, made about a run "+
						"whose config was never frozen, reads as a measurement.", gone, r.out)
				}
			}

			// The same distinction in the machine-readable form: the key is absent
			// rather than 0, so a caller cannot read "no brake" as "brake at zero".
			m := traceJSON(t, dir, "ev-b")
			raw, ok := m["max_depth"]
			if tc.maxDepth == 0 {
				if ok {
					t.Errorf("--json carries max_depth %v for a run with no frozen blueprint\n"+
						"  consequence: 0 is a number, and a caller comparing "+
						"depth against it would treat every event as over the "+
						"limit. Absent is the only honest answer.", raw)
				}
				return
			}
			if got := num(t, m, "max_depth"); got != tc.maxDepth {
				t.Errorf("--json max_depth is %d, want %d", got, tc.maxDepth)
			}
		})
	}
}
