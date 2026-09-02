// `arxi event trace <event>` -- the causal chain of one event, root first.
//
// # Why this verb when `event log` already prints the log
//
// `event log` answers "what happened", in order. It cannot answer "what made
// THIS happen", because the answer is not a neighbouring line: caused_by names
// an id, and the event it names can be twenty lines up. Following it by eye means
// reading --json and grepping for an id, which is what this file automates.
//
// It adds no field, no index and no store. kernel.derived writes CausedBy,
// CorrelationID and Depth on every event the reducer emits, and this is the
// reader those fields were written for.
//
// # MEASURED: most events in a real log carry no cause, and that is the producer
//
// From a real 21-event --sim log written by this binary before this file existed:
//
//	 1 run.started      ev-start             src=human   depth 0  caused_by []
//	 2 stage.entered    ev-000002            src=runtime depth 1  caused_by [ev-start]
//	 3 agent.activated  sim-backend-act-1    src=runtime depth 0  caused_by []
//	11 stage.advanced   ev-000003            src=runtime depth 1  caused_by [sim-frontend-submit-1]
//	21 run.result       ev-000005            src=runtime depth 1  caused_by [sim-backend-submit-2]
//
// Five of twenty-one carry a cause. Every event the EXECUTOR wrote carries none,
// because exec.stamp fills in only ID and Ts: an agent turn is a hole in the
// chain, and every correlation group in that log is rooted at an executor event
// instead of at run.started.
//
// So the common answer here is a chain of one or two, and that branch is the one
// written most carefully. It reports the gap as a defect in the producers, with
// the count measured on the log in front of it -- a reader told "16 of 21 events
// here carry no cause" goes and fixes the executor, where a reader shown one
// lonely event concludes this view is broken.
//
// # Why an id, and why a seq is taken too
//
// caused_by names ids, so the chain can only be keyed on ids -- and `event log`'s
// human table has no ID column, so the only handle a reader arrives with is the
// seq they just read. Refusing it would make the answer to "trace line 12" be
// "run a different command with --json first". A numeric argument is therefore
// tried as an id FIRST and as a seq only when no log holds that id, and the
// header prints the id it resolved to so the next invocation can be keyed
// properly.
//
// # Why an id needs a run
//
// Ids are minted per run: exec.stamp counts from the log tip and formats
// ev-%06d, so ev-000002 exists in nearly every run directory. A bare id is
// therefore searched across every run and REFUSED when more than one holds it,
// with the qualified spellings printed, because picking the first would trace a
// stranger's log and look like it worked. `<run>/<event>` is the qualified form,
// and whether the left side is a run is decided by whether it holds an event log
// -- resolveRunDir's own rule rather than a second guess about slashes.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

const eventTraceUsage = "usage: arxi event trace <event> [--json]\n" +
	"  <event>  an event id, the thing caused_by names: arxi event log <run> --json\n" +
	"           a seq from the SEQ column works too, and is resolved to its id\n" +
	"  ids are minted per run, so ev-000002 exists in most logs; write\n" +
	"  <run>/<event> when a bare id names more than one\n" +
	"  short: -J json\n" +
	"  the runs there are: arxi run list\n"

// cmdEventTrace walks the causal chain of one event.
func cmdEventTrace(args []string) {
	c := surface.Lookup("event", "trace")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event trace: %v\n\n%s", err, eventTraceUsage)
		os.Exit(2)
	}

	// parseInvocation already refuses a MISSING positional, since pos() marks it
	// required. This catches the spelling it cannot: `arxi event trace ""` passes
	// requiredness with an empty string, and searching every log for the id ""
	// would match every event that has no id at all.
	needle := strings.TrimSpace(vals["event"])
	if needle == "" {
		fmt.Fprint(os.Stderr, "arxi event trace: which event?\n\n"+eventTraceUsage)
		os.Exit(2)
	}

	subject, warnings := resolveTraceSubject(needle)

	// Printed before the answer rather than after it, unlike `run tree`'s notes:
	// an unreadable run directory is a reason the search may have MISSED the id,
	// so it belongs beside the resolution it qualifies.
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	v := buildTraceView(subject)
	if vals["json"] == "true" {
		emitJSON(eventTracePayload(v))
		return
	}
	printEventTrace(v)
}

// traceRun is one run's log, read once.
type traceRun struct {
	dir       string
	runID     string
	st        kernel.State
	cfg       kernel.Config
	simulated bool
	events    []kernel.Event
}

// traceSubject is the event the argument turned out to mean, and its run.
type traceSubject struct {
	run   traceRun
	ev    kernel.Event
	bySeq bool
	// dupes is how many events in that log carry the subject's id. More than one
	// makes caused_by ambiguous INSIDE a run, which the footer reports; the first
	// is the one traced, the same choice byID makes.
	dupes int
}

// resolveTraceSubject turns the argument into exactly one event, or exits saying
// why it could not.
//
// It exits rather than returning an error because each of the three failures
// wants a different message, a different exit code and different remedies, and
// funnelling them through one error string would flatten "you must say which
// run" (fixable by typing more) into "no such event" (fixable by looking
// elsewhere).
func resolveTraceSubject(arg string) (traceSubject, []string) {
	var warnings []string

	// Scope first. A run-qualified spelling searches one log; a bare id searches
	// every one. Both separators are accepted because the qualified form is a
	// path on the platform the user is typing on.
	needle, scope := arg, []string(nil)
	qualified, badRunPart := "", ""
	if i := strings.LastIndexAny(arg, `/\`); i > 0 && i < len(arg)-1 {
		left, right := arg[:i], arg[i+1:]
		dir := resolveRunDir(left)
		if _, err := os.Stat(filepath.Join(dir, "events.ndjson")); err == nil {
			scope, needle, qualified = []string{dir}, right, left
		} else {
			// Not a run, so the WHOLE argument is read as an id -- an id is
			// allowed to contain a slash. Remembered so the failure below can say
			// both readings were tried: a user who mistyped the run part would
			// otherwise be told their event does not exist.
			badRunPart = left
		}
	}
	if scope == nil {
		found, err := discoverRuns()
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi event trace: %v\n", err)
			os.Exit(1)
		}
		scope = found
	}
	if len(scope) == 0 {
		fmt.Fprintf(os.Stderr, "arxi event trace: no runs under %s/, so there is no event to trace.\n"+
			"  start one: arxi run start <blueprint> \"<prompt>\" --budget 5 --sim\n", runsDir)
		os.Exit(1)
	}

	var runs []traceRun
	for _, dir := range scope {
		st, cfg, sim, events, err := foldRunDirEvents(dir)
		if err != nil {
			// A named run's failure IS the answer. One bad directory among many
			// is a warning, for runlist.go's reason: "I found nothing" and "I
			// found nine and one is damaged" must not print the same thing.
			if qualified != "" {
				fmt.Fprintf(os.Stderr, "arxi event trace: %v\n", err)
				os.Exit(1)
			}
			warnings = append(warnings, fmt.Sprintf("%s: %v -- not searched", dir, err))
			continue
		}
		id := st.RunID
		if id == "" {
			id = filepath.Base(dir)
		}
		runs = append(runs, traceRun{dir: dir, runID: id, st: st, cfg: cfg, simulated: sim, events: events})
	}

	// A numeric argument is a candidate seq, but only after every log has been
	// asked about it as an id. The order is what keeps a log that really has an
	// event whose id is "12" from being shadowed by the twelfth line of another.
	seq, numeric := int64(0), false
	if n, err := strconv.ParseInt(needle, 10, 64); err == nil && n > 0 {
		seq, numeric = n, true
	}

	var byID, bySeq []traceSubject
	for _, r := range runs {
		if s, ok := firstTraceMatch(r, func(e kernel.Event) bool { return e.ID == needle }); ok {
			s.dupes = countTraceMatches(r, func(e kernel.Event) bool { return e.ID == needle })
			byID = append(byID, s)
			continue
		}
		if !numeric {
			continue
		}
		if s, ok := firstTraceMatch(r, func(e kernel.Event) bool { return e.Seq == seq }); ok {
			s.bySeq = true
			s.dupes = countTraceMatches(r, func(e kernel.Event) bool { return e.ID != "" && e.ID == s.ev.ID })
			bySeq = append(bySeq, s)
		}
	}

	matches := byID
	if len(matches) == 0 {
		matches = bySeq
	}

	switch len(matches) {
	case 1:
		m := matches[0]
		if m.ev.ID == "" {
			exitTraceBlankID(m)
		}
		return m, warnings
	case 0:
		exitTraceNoMatch(needle, qualified, badRunPart, numeric, runs)
	default:
		exitTraceAmbiguous(needle, matches)
	}

	// Unreachable: every branch above either returns or ends in os.Exit. Spelled
	// out rather than panicking, because a panic here would replace a message
	// that has already been printed with a worse one.
	return traceSubject{}, warnings
}

// firstTraceMatch returns the FIRST event in a log satisfying want.
//
// First rather than all, so that two events sharing an id inside one run cannot
// reach the ambiguity refusal below -- that message offers run-qualified
// spellings, and two identical spellings are not advice. The duplication is
// counted instead and reported in the footer, where it can say which one was
// taken.
func firstTraceMatch(r traceRun, want func(kernel.Event) bool) (traceSubject, bool) {
	for _, e := range r.events {
		if want(e) {
			return traceSubject{run: r, ev: e}, true
		}
	}
	return traceSubject{}, false
}

func countTraceMatches(r traceRun, want func(kernel.Event) bool) int {
	n := 0
	for _, e := range r.events {
		if want(e) {
			n++
		}
	}
	return n
}

// exitTraceBlankID refuses an event that has no id, and points at what it can.
//
// Reachable: a seq names any line, including one whose id is empty, and a log is
// a file that can be written by hand. Nothing in such a log can reference that
// event -- caused_by names ids -- so there is no chain to walk downward from it.
// Refusing beats printing a one-row "chain" that would read as "nothing caused
// this", which is a claim about the run rather than about the log's shape.
func exitTraceBlankID(m traceSubject) {
	fmt.Fprintf(os.Stderr, "arxi event trace: seq %d of run %s is a %s with no id.\n"+
		"  caused_by names ids, so nothing in this log can reference this event and\n"+
		"  there is no chain below it. Events this binary writes always carry one\n"+
		"  (exec.stamp mints ev-000001, ev-000002, ...), so a blank id means a\n"+
		"  hand-written log or one written by something else.\n",
		m.ev.Seq, m.run.runID, m.ev.Type)
	if len(m.ev.CausedBy) > 0 {
		fmt.Fprintf(os.Stderr, "  it does name a cause, and that one can be traced:\n    arxi event trace %s\n",
			filepath.Join(m.run.dir, m.ev.CausedBy[0]))
	}
	os.Exit(1)
}

// exitTraceNoMatch says nothing holds the argument, in the terms it was read in.
func exitTraceNoMatch(needle, qualified, badRunPart string, numeric bool, runs []traceRun) {
	if qualified != "" && len(runs) == 1 {
		r := runs[0]
		fmt.Fprintf(os.Stderr, "arxi event trace: run %s holds no event %q.\n", r.runID, needle)
		if len(r.events) == 0 {
			fmt.Fprint(os.Stderr, "  its log is empty, which means the run never got as far as starting.\n")
			os.Exit(1)
		}
		head := int64(0)
		for _, e := range r.events {
			if e.Seq > head {
				head = e.Seq
			}
		}
		fmt.Fprintf(os.Stderr, "  its log holds %d event%s, seq 1..%d.\n", len(r.events), plural(len(r.events)), head)
		if numeric {
			fmt.Fprint(os.Stderr, "  it was read as an id and as a seq, and this log has neither.\n")
		}
		fmt.Fprintf(os.Stderr, "  the ids: arxi event log %s --json\n", r.dir)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "arxi event trace: no run under %s/ holds an event %q.\n"+
		"  searched %d run%s.\n", runsDir, needle, len(runs), plural(len(runs)))
	if numeric {
		fmt.Fprint(os.Stderr, "  it was read as an id and as a seq: no log has either.\n")
	}
	if badRunPart != "" {
		fmt.Fprintf(os.Stderr, "  read as <run>/<event>, %q is not a run directory either: a run is a\n"+
			"  directory holding events.ndjson, under ./%s/ unless --dir said otherwise.\n",
			badRunPart, runsDir)
	}
	fmt.Fprint(os.Stderr, "  ids are minted per run, so an id from one run means nothing in another:\n"+
		"  arxi event log <run> --json lists the ones that exist.\n")
	os.Exit(1)
}

// exitTraceAmbiguous refuses a spelling that names one event in several runs.
//
// Exit 2 and not 1: nothing is missing, the argument is under-specified, and the
// fix is to type more rather than to look somewhere else.
func exitTraceAmbiguous(needle string, matches []traceSubject) {
	if matches[0].bySeq {
		fmt.Fprintf(os.Stderr, "arxi event trace: %q is a seq in %d runs, so it does not name one event.\n"+
			"  no log holds it as an ID, so it was read as a seq -- and every log has\n"+
			"  a line %s.\n", needle, len(matches), needle)
	} else {
		fmt.Fprintf(os.Stderr, "arxi event trace: %q is an event id in %d runs, so it does not name one event.\n"+
			"  ids are minted per run (ev-000001, ev-000002, ...), so the same id\n"+
			"  exists in most logs.\n", needle, len(matches))
	}

	// The DIRECTORY is offered rather than the run id from the log, because the
	// directory is the spelling that always resolves: a run started with --dir
	// outside runs/ has an id that resolveRunDir cannot find.
	fmt.Fprint(os.Stderr, "  say which run:\n")
	const show = 8
	for i, m := range matches {
		if i == show {
			fmt.Fprintf(os.Stderr, "    ... and %d more (arxi run list)\n", len(matches)-show)
			break
		}
		fmt.Fprintf(os.Stderr, "    arxi event trace %s\n", filepath.Join(m.run.dir, m.ev.ID))
	}
	fmt.Fprint(os.Stderr, "  refusing rather than taking the first: tracing a stranger's log would\n"+
		"  look exactly like an answer.\n")
	os.Exit(2)
}

// traceRow is one printed line of the chain.
type traceRow struct {
	prefix  string // box drawing, already assembled
	ev      kernel.Event
	sdepth  int // structural depth: how deep this row sits in the printed tree
	expect  int // the depth the reducer's rule implies for this row
	subject bool
	repeat  bool // reached twice: printed once, marked here
	cycle   bool // the second reach was through itself, which a log should not record
}

// traceView is everything the two renderers print, measured once.
type traceView struct {
	run     traceRun
	subject kernel.Event
	bySeq   bool
	dupes   int

	byID map[string]int      // event id -> index into run.events, first wins
	kids map[string][]string // cause id -> the ids it caused, in seq order

	ancestorSet map[string]bool
	shown       map[string]bool
	rows        []traceRow

	ancestors, descendants, repeats int
	blankIDs                        int
	causeless                       int
	dangling, cycles, depthMismatch []string

	corrID      string
	corrGroup   int
	corrOnChain int
}

// buildTraceView indexes the log and walks the chain both ways.
//
// Upward the walk is the transitive closure of caused_by. Downward it is the
// reverse index. The two are printed as ONE tree rather than two lists, because
// "what caused this" and "what this caused" are the same question asked from two
// ends, and a reader following a cascade needs to see the subject's place in it
// rather than to join two tables by id.
func buildTraceView(s traceSubject) traceView {
	v := traceView{
		run: s.run, subject: s.ev, bySeq: s.bySeq, dupes: s.dupes,
		byID: map[string]int{}, kids: map[string][]string{},
		ancestorSet: map[string]bool{}, shown: map[string]bool{},
	}

	for i, e := range v.run.events {
		if e.ID == "" {
			v.blankIDs++
			continue
		}
		if _, seen := v.byID[e.ID]; !seen {
			v.byID[e.ID] = i
		}
	}

	// The reverse index. An id repeated inside one caused_by list is added once:
	// two references from the same event are one edge, and printing the child
	// twice would report a diamond that is really a typo.
	for _, e := range v.run.events {
		if e.ID == "" {
			continue
		}
		for _, p := range e.CausedBy {
			v.kids[p] = appendOnce(v.kids[p], e.ID)
		}
	}
	for p := range v.kids {
		v.sortBySeq(v.kids[p])
	}

	v.walkAncestors()
	roots := v.chainRoots()
	for _, id := range roots {
		// Roots hang from nothing, so they get no branch glyph: a `├─` at the
		// left margin implies a parent above it that does not exist.
		v.rows = v.appendTraceRows(id, "", "", 0, v.depthOf(id), map[string]bool{}, v.rows)
	}

	v.measure()
	return v
}

// walkAncestors collects every event the subject transitively records as a cause.
//
// Iterative with a visited set rather than recursive, because the same event can
// be reached along two paths (a diamond) and a log is a file somebody can edit
// into a cycle. Neither is a reason to stop walking; both are reasons not to walk
// the same node twice.
func (v *traceView) walkAncestors() {
	queue := append([]string{}, v.subject.CausedBy...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || v.ancestorSet[id] {
			continue
		}
		if id == v.subject.ID {
			// The subject is among its own causes. Recorded and not followed: an
			// event cannot cause one that was written before it.
			v.cycles = appendOnce(v.cycles, id)
			continue
		}
		i, ok := v.byID[id]
		if !ok {
			continue // dangling; counted over the printed rows, in measure()
		}
		v.ancestorSet[id] = true
		queue = append(queue, v.run.events[i].CausedBy...)
	}
}

// chainRoots are the ancestors with no cause of their own on the chain.
func (v *traceView) chainRoots() []string {
	var roots []string
	for id := range v.ancestorSet {
		e := v.run.events[v.byID[id]]
		linked := false
		for _, p := range e.CausedBy {
			if v.ancestorSet[p] {
				linked = true
				break
			}
		}
		if !linked {
			roots = append(roots, id)
		}
	}
	if len(roots) > 0 {
		v.sortBySeq(roots)
		return roots
	}
	if len(v.ancestorSet) == 0 {
		// The ordinary case, and by the measurement at the top of this file the
		// common one: nothing caused the subject, so it is its own root.
		return []string{v.subject.ID}
	}
	// Ancestors exist and every one of them has a cause on the chain, which a
	// directed acyclic graph cannot do. Start at the lowest seq -- the least
	// wrong place -- rather than printing nothing, and say so in the footer.
	lowest := ""
	for id := range v.ancestorSet {
		if lowest == "" || v.seqOf(id) < v.seqOf(lowest) || (v.seqOf(id) == v.seqOf(lowest) && id < lowest) {
			lowest = id
		}
	}
	v.cycles = appendOnce(v.cycles, lowest)
	return []string{lowest}
}

// appendTraceRows renders one node and its chain children.
//
// The child's own indent is passed down rather than rebuilt from the depth, for
// runtree.go's reason: a prefix assembled by repeating one indent per level puts
// the continuation bars under the wrong ancestors as soon as a branch closes.
//
// path is the ids on the way in, and it is what tells a cycle from a diamond:
// both arrive at an event already shown, but only a cycle arrives at one that is
// still open above it. Distinguishing them matters because a diamond is ordinary
// and a cycle means the log is not a log.
func (v *traceView) appendTraceRows(id, prefix, childIndent string, sdepth, expect int, path map[string]bool, rows []traceRow) []traceRow {
	i, ok := v.byID[id]
	if !ok {
		return rows // dangling reference; the footer names it
	}
	e := v.run.events[i]

	if path[id] {
		v.cycles = appendOnce(v.cycles, id)
		v.repeats++
		return append(rows, traceRow{prefix: prefix, ev: e, sdepth: sdepth, expect: expect,
			repeat: true, cycle: true})
	}
	if v.shown[id] {
		v.repeats++
		return append(rows, traceRow{prefix: prefix, ev: e, sdepth: sdepth, expect: expect, repeat: true})
	}
	v.shown[id] = true
	rows = append(rows, traceRow{prefix: prefix, ev: e, sdepth: sdepth, expect: expect,
		subject: id == v.subject.ID})

	path[id] = true
	kids := v.chainChildren(id)
	for k, cid := range kids {
		branch, cont := "├─ ", "│  "
		if k == len(kids)-1 {
			branch, cont = "└─ ", "   "
		}
		rows = v.appendTraceRows(cid, childIndent+branch, childIndent+cont, sdepth+1, expect+1, path, rows)
	}
	delete(path, id)
	return rows
}

// chainChildren is why a root does not drag the whole log in with it.
//
// Above the subject, an ancestor shows only the children that lead BACK to the
// subject: ev-start caused stage.entered which caused everything else, so an
// unrestricted walk from it would print the log and bury the one event asked
// about. From the subject downward there is no such restriction -- everything it
// caused is the answer.
func (v *traceView) chainChildren(id string) []string {
	all := v.kids[id]
	if id == v.subject.ID || !v.ancestorSet[id] {
		return all
	}
	out := make([]string, 0, len(all))
	for _, c := range all {
		if c == v.subject.ID || v.ancestorSet[c] {
			out = append(out, c)
		}
	}
	return out
}

// measure counts, over the rows actually printed, every fact the footer states.
//
// Over the rows and not over the sets, so that no note can describe something
// the reader cannot see -- a footer that counts differently from the table is
// worse than no footer, because it is the part that sounds authoritative.
func (v *traceView) measure() {
	for _, r := range v.rows {
		if r.repeat {
			continue
		}
		switch {
		case r.subject:
		case v.ancestorSet[r.ev.ID]:
			v.ancestors++
		default:
			v.descendants++
		}
		for _, p := range r.ev.CausedBy {
			if _, ok := v.byID[p]; !ok {
				v.dangling = appendOnce(v.dangling, p)
			}
		}
		// Only an event with a single cause is checked. kernel.derived takes ONE
		// cause and sets depth to that cause's depth plus one, so an event with two
		// causes has a depth that agrees with one of its parents and disagrees with
		// the other by however far apart they sit. Reporting that as a mismatch
		// would be reporting the design, and a footer note that fires on correct
		// logs is one the reader learns to skip.
		if len(r.ev.CausedBy) <= 1 && r.ev.Depth != r.expect {
			v.depthMismatch = append(v.depthMismatch,
				fmt.Sprintf("%s records %d, sits at %d", r.ev.ID, r.ev.Depth, r.expect))
		}
	}

	v.causeless, _ = causalDepthSplit(v.run.events)

	// The correlation group INCLUDES the event whose ID is the correlation id.
	// kernel.derived sets corr to cause.CorrelationID, or to cause.ID when the
	// cause has none -- so the event a group is named after does not carry the
	// name, and counting only the carriers reports a group of 2 for three events
	// that belong together.
	//
	// A subject with no correlation id of its own is not chased the other way
	// (looking for events carrying the subject's id): anything that carries it
	// descends from the subject and is therefore already on the chain below.
	v.corrID = v.subject.CorrelationID
	if v.corrID == "" {
		return
	}
	for _, e := range v.run.events {
		if e.CorrelationID == v.corrID || e.ID == v.corrID {
			v.corrGroup++
		}
	}
	for _, r := range v.rows {
		if !r.repeat && (r.ev.CorrelationID == v.corrID || r.ev.ID == v.corrID) {
			v.corrOnChain++
		}
	}
}

// sortBySeq orders ids by the seq of the event they name.
//
// A total order, with the id breaking a tie, because map iteration feeds this and
// two runs of the same command must produce the same bytes -- the reason
// formatPayloadCell sorts its keys.
func (v *traceView) sortBySeq(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		a, b := v.seqOf(ids[i]), v.seqOf(ids[j])
		if a != b {
			return a < b
		}
		return ids[i] < ids[j]
	})
}

func (v *traceView) seqOf(id string) int64 {
	if i, ok := v.byID[id]; ok {
		return v.run.events[i].Seq
	}
	return 0
}

func (v *traceView) depthOf(id string) int {
	if i, ok := v.byID[id]; ok {
		return v.run.events[i].Depth
	}
	return 0
}

// appendOnce keeps a list of ids free of duplicates.
//
// Linear, and that is the right cost here: these lists are the roots of a chain
// and the dangling references in one log, which are counted in single figures. A
// set would need a second pass to print them in a stable order.
func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// printEventTrace renders the chain root first, with the subject marked.
//
// The columns are `event log`'s, in the same order, with ID added -- the column
// that verb does not print, and the one this verb is about. Matching it is not
// tidiness: a reader arrives here from `event log`, and a second layout for the
// same rows makes them re-read the header to find the type.
func printEventTrace(v traceView) {
	sim := ""
	if v.run.simulated {
		sim = "  [simulated]"
	}
	// len(v.shown), not len(v.rows): a diamond prints an event twice and it is
	// still one event, so counting rows would claim a chain longer than the log
	// can hold.
	fmt.Printf("run %s  %d of %d events on the chain of %s  %s%s\n\n",
		v.run.runID, len(v.shown), len(v.run.events), v.subject.ID, v.run.st.Status, sim)

	seqW, idW, typeW, actorW := len("SEQ"), len("ID"), len("TYPE"), len("ACTOR")
	for _, r := range v.rows {
		if n := len(strconv.FormatInt(r.ev.Seq, 10)); n > seqW {
			seqW = n
		}
		// colWidth, because the id cell carries the box-drawing prefix: "└─ " is
		// nine bytes and three columns, and sizing this one column with len() is
		// what bent runtree.go's tree to the right.
		if n := colWidth(r.prefix + r.ev.ID); n > idW {
			idW = n
		}
		if n := len(string(r.ev.Type)); n > typeW {
			typeW = n
		}
		if n := len(r.ev.Actor); n > actorW {
			actorW = n
		}
	}

	fmt.Printf("  %*s  %s  %-*s  %-7s  %-*s  %5s  %s\n",
		seqW, "SEQ", pad("ID", idW), typeW, "TYPE", "SOURCE",
		actorW, "ACTOR", "DEPTH", "PAYLOAD")

	elided := false
	for _, r := range v.rows {
		// Two columns of leader, so the subject sits in the same tree as
		// everything else instead of being pulled out of it into a separate line.
		lead := "  "
		if r.subject {
			lead = "> "
		}

		cell := ""
		switch {
		case r.cycle:
			cell = "(shown above, and it is above itself)"
		case r.repeat:
			cell = "(shown above)"
		default:
			c, cut := formatPayloadCell(r.ev.Payload)
			cell, elided = c, elided || cut
		}

		fmt.Printf("%s%*d  %s  %-*s  %-7s  %-*s  %5d  %s\n",
			lead, seqW, r.ev.Seq, pad(r.prefix+r.ev.ID, idW),
			typeW, string(r.ev.Type), dashIfEmpty(string(r.ev.Source)),
			actorW, dashIfEmpty(r.ev.Actor), r.ev.Depth, cell)
	}

	printEventTraceFooter(v, elided)
}

// printEventTraceFooter says what the tree cannot show, and nothing it has not
// counted.
//
// Every line below except the legend is conditional on a figure taken off the
// printed rows, for printEventLogFooter's reason: a note that always appears is
// a note that is never read, and the ones here are the difference between "the
// chain is one event" and "the chain is broken".
func printEventTraceFooter(v traceView, elided bool) {
	// The legend is the exception, because the marker is on every rendering and a
	// glyph with no key is a puzzle set for the reader.
	notes := []string{fmt.Sprintf("> marks %s, the event asked about", v.subject.ID)}

	if v.bySeq {
		notes = append(notes, fmt.Sprintf(
			"seq %d in this log is %s, and that is the spelling caused_by uses:\n"+
				"    the SEQ column is this file's line numbering, ids are what events\n"+
				"    name each other by", v.subject.Seq, v.subject.ID))
	}

	switch {
	case v.ancestors == 0 && v.descendants == 0:
		// The common case, by the measurement at the top of this file, so it gets
		// the sentence that explains itself rather than a bare count.
		note := fmt.Sprintf(
			"%s records no cause and nothing in this log records it as one, so\n"+
				"    the chain is this one event", v.subject.ID)
		if len(v.subject.CausedBy) > 0 {
			// Reached when nothing the subject names can be followed -- the id is
			// dangling, or it is the subject itself. Had one of them resolved to
			// another event here, that event would be an ancestor and this branch
			// would not have been taken.
			//
			// "records no cause" is false in that case, and the dangling or cycle
			// note two lines further down says so in the same breath. A footer that
			// contradicts itself reads as broken rather than as precise, and the
			// reader cannot tell which of the two sentences to believe.
			note = fmt.Sprintf(
				"the chain is this one event: nothing in this log records %s as a\n"+
					"    cause, and the %d it names cannot be followed either (below)",
				v.subject.ID, len(v.subject.CausedBy))
		}
		// Phrased so the count does not sit in front of a verb. "1 of the 3 events
		// here do carry one" is the disagreement isAre exists to prevent, and it is
		// reachable by any log whose only cause-carrying event is the subject --
		// which is exactly the dangling case above.
		if deep := len(v.run.events) - v.causeless; deep > 0 {
			note += fmt.Sprintf(";\n    a cause is recorded on %d of the %d events in this log, so this is\n"+
				"    a gap in the chain rather than a log that has none",
				deep, len(v.run.events))
		}
		notes = append(notes, note)
	default:
		notes = append(notes, fmt.Sprintf("%d cause%s above %s, %d event%s below it",
			v.ancestors, plural(v.ancestors), v.subject.ID,
			v.descendants, plural(v.descendants)))

		// Said here as well as in the both-zero branch above, because a chain that
		// HAS a shape can still be cut short by the same producer gap, and the
		// reader of a two-row tree needs to know the tree is not the whole story.
		if deep := len(v.run.events) - v.causeless; v.causeless > 0 && deep > 0 {
			notes = append(notes, fmt.Sprintf(
				"%d of these %d events carry no cause: caused_by and correlation_id\n"+
					"    are written by the reducer and not by the executor, so anything\n"+
					"    that ran through a turn is absent from the tree above rather than\n"+
					"    unrelated to it", v.causeless, len(v.run.events)))
		}
	}

	if v.dupes > 1 {
		notes = append(notes, fmt.Sprintf(
			"%d events in this log carry the id %s. The first is the one traced,\n"+
				"    and it is also the one caused_by resolves to -- so the reference is\n"+
				"    ambiguous in the file, not just in this view", v.dupes, v.subject.ID))
	}

	// Phrased without a verb that has to agree with the count. "1 events have no
	// id" is the mistake isAre exists to prevent, and the cheapest way not to make
	// it is a sentence that does not conjugate.
	if v.blankIDs > 0 {
		notes = append(notes, fmt.Sprintf(
			"no id on %d of the %d events in this log, so nothing can name them as\n"+
				"    a cause and they cannot appear on any chain",
			v.blankIDs, len(v.run.events)))
	}

	if len(v.dangling) > 0 {
		notes = append(notes, fmt.Sprintf(
			"caused_by names %d id%s no event here carries: %s. Ids are minted per\n"+
				"    run, so either this log is truncated or the id belongs to another\n"+
				"    run -- the tree above stops at the reference either way",
			len(v.dangling), plural(len(v.dangling)), traceList(v.dangling, 4, ", ")))
	}

	if len(v.cycles) > 0 {
		notes = append(notes, fmt.Sprintf(
			"this log records a cycle at %s: an event is caused by one written after\n"+
				"    it, which appending in order cannot produce. The walk stops at the\n"+
				"    repeat instead of looping, so the tree above is not a faithful\n"+
				"    picture of the file -- read it with arxi event log %s --json",
			traceList(v.cycles, 4, ", "), v.run.dir))
	}

	if len(v.depthMismatch) > 0 {
		notes = append(notes, fmt.Sprintf(
			"depth disagrees with this tree for %d event%s: %s. Depth is stamped\n"+
				"    when the event is derived and the tree is rebuilt from caused_by, so\n"+
				"    one of the two is wrong -- and it is the stamp that MaxDepth stops",
			len(v.depthMismatch), plural(len(v.depthMismatch)),
			traceList(v.depthMismatch, 3, "; ")))
	}

	// Siblings are the events this walk deliberately does not follow, and the
	// correlation id is the only thing in the log that knows they exist. Without
	// this line a two-row tree looks like the whole story of a turn that actually
	// produced four events.
	//
	// The line breaks right after the id rather than mid-sentence: ids are data and
	// can be long, and the first hand run of this note put a 21-character id in the
	// middle of the first line and pushed it past 80 columns.
	if v.corrGroup > v.corrOnChain {
		notes = append(notes, fmt.Sprintf(
			"%d events share the correlation id %s;\n"+
				"    %d %s on this chain and the rest are siblings -- caused by the same\n"+
				"    event rather than by this one, which is why the walk does not reach\n"+
				"    them", v.corrGroup, v.corrID, v.corrOnChain, isAre(v.corrOnChain)))
	}

	// Guarded on MaxDepth > 0 because a run whose blueprint was never frozen
	// folds to a zero Config, and "stops waking watchers at 0" would describe a
	// brake that is not set as one clamped shut.
	if v.run.cfg.MaxDepth > 0 {
		deepest := 0
		for _, r := range v.rows {
			if r.ev.Depth > deepest {
				deepest = r.ev.Depth
			}
		}
		switch left := v.run.cfg.MaxDepth - deepest; {
		case left <= 0:
			notes = append(notes, fmt.Sprintf(
				"the deepest event here records depth %d and this run stops waking\n"+
					"    watchers at %d: nothing below this row can have been woken, so the\n"+
					"    chain ends because the brake held, not because the work finished",
				deepest, v.run.cfg.MaxDepth))
		case left <= 2:
			notes = append(notes, fmt.Sprintf(
				"the deepest event here records depth %d and this run stops waking\n"+
					"    watchers at %d, so this cascade is %d event%s from the brake",
				deepest, v.run.cfg.MaxDepth, left, plural(left)))
		}
	}

	if elided {
		notes = append(notes, fmt.Sprintf(
			"a payload value longer than %d characters is shown elided with …;\n"+
				"    --json has every value whole", eventPayloadValueWidth))
	}

	fmt.Println()
	for _, n := range notes {
		fmt.Printf("  %s\n", n)
	}

	// The pointers spell the run as a DIRECTORY rather than as an id, because a
	// run started with --dir sits outside runs/ and resolveRunDir cannot find it
	// from the id -- so the id form would print a command that fails for exactly
	// the user who needs the pointer most. A dir works for both.
	fmt.Printf("\n  the log this is read from: arxi event log %s\n", v.run.dir)
	fmt.Printf("  every field of these events: arxi event trace %s --json\n",
		filepath.Join(v.run.dir, v.subject.ID))
	fmt.Printf("  what the fold makes of it: arxi run show %s\n", v.run.dir)
}

// traceList prints at most max items and says how many it withheld.
//
// Capped because these lists are defect reports -- dangling ids, depth
// disagreements -- and a log with a systematic producer bug has one per event.
// A footer that grows to the length of the log buries the notes above it.
func traceList(items []string, max int, sep string) string {
	if len(items) <= max {
		return strings.Join(items, sep)
	}
	return strings.Join(items[:max], sep) +
		fmt.Sprintf("%s and %d more", sep, len(items)-max)
}

// eventTracePayload is the machine reading: a flat list in print order.
//
// FLAT, unlike run tree's nesting, and that is a decision about the data rather
// than about the format. caused_by is a directed graph and not a tree: an event
// with two causes belongs under both, so nesting would either duplicate it -- a
// consumer counting nodes then counts one event twice -- or silently pick a
// parent. The list carries every edge in `parents` instead, and the rendering
// above is one walk of it.
//
// Each row holds the whole kernel.Event under `event`, eventLogPayload's rule:
// the wire shape of an event is the file's shape by construction, so a field
// added to Event appears here without anybody remembering.
func eventTracePayload(v traceView) map[string]any {
	chain := make([]map[string]any, 0, len(v.rows))
	for _, r := range v.rows {
		// parents are the causes this log can resolve, which is what an edge in the
		// tree above means. A dangling id stays in event.caused_by and out of here:
		// the difference between "names a cause" and "has one" is the whole
		// diagnosis when a chain stops early.
		parents := []string{}
		for _, p := range r.ev.CausedBy {
			if _, ok := v.byID[p]; ok {
				parents = appendOnce(parents, p)
			}
		}
		row := map[string]any{
			"structural_depth": r.sdepth,
			"depth_in_chain":   r.expect,
			"parents":          parents,
			"event":            r.ev,
		}
		// Written only when true. A row object carrying three false flags reads as
		// if each were a measurement; absence is the ordinary case.
		if r.subject {
			row["subject"] = true
		}
		if r.repeat {
			row["repeat"] = true
		}
		if r.cycle {
			row["cycle"] = true
		}
		chain = append(chain, row)
	}

	out := map[string]any{
		"run":       v.run.runID,
		"dir":       v.run.dir,
		"status":    string(v.run.st.Status),
		"simulated": v.run.simulated,
		"subject":   v.subject.ID,
		// count is unique events, total is the log: the same pair eventLogPayload
		// prints, so "2 of 21" is answerable without a second call.
		"count":       len(v.shown),
		"total":       len(v.run.events),
		"ancestors":   v.ancestors,
		"descendants": v.descendants,
		"chain":       chain,
		// The defects, always present so a consumer can assert on them rather than
		// having to test for the key's existence first. Empty is a finding too.
		"dangling":             emptyIfNil(v.dangling),
		"cycles":               emptyIfNil(v.cycles),
		"depth_mismatch":       emptyIfNil(v.depthMismatch),
		"blank_ids":            v.blankIDs,
		"events_without_cause": v.causeless,
	}
	// matched_by is here because the two readings of the argument are not
	// interchangeable: a consumer that passed a seq got an id back, and the id is
	// what it must use to follow caused_by next time.
	if v.bySeq {
		out["matched_by"] = "seq"
	} else {
		out["matched_by"] = "id"
	}
	if v.dupes > 1 {
		out["duplicate_ids"] = v.dupes
	}
	if v.corrID != "" {
		out["correlation_id"] = v.corrID
		out["correlation_group"] = v.corrGroup
		out["correlation_on_chain"] = v.corrOnChain
	}
	if v.run.cfg.MaxDepth > 0 {
		out["max_depth"] = v.run.cfg.MaxDepth
	}
	return out
}

// emptyIfNil keeps a nil slice from marshalling as null.
//
// eventLogPayload does this inline for its one slice; there are three here, and
// the failure it prevents is a consumer iterating the result: `for x in .cycles`
// over null is an error, over [] is zero iterations, and zero iterations is the
// truth being reported.
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
