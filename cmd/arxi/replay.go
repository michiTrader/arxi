package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

const runReplayUsage = "usage: arxi run replay <run> [--until-seq N] [--json]\n" +
	"  it folds the run's own log again and prints the state at that seq\n" +
	"  nothing is executed and nothing is written: no turn, no tool, no spend\n" +
	"  --until-seq is inclusive and defaults to the head, which is printed\n" +
	"  short: -r run  -J json\n"

// cmdRunReplay implements `arxi run replay <run> [--until-seq N] [--json]`.
//
// # This is the verb the whole architecture was arranged for
//
// ADR-0001 claims Decide(State, Event, Config) -> (State', []Effect) is pure, and
// docs/design/20-use-cases.md:530 spends that claim: "replay is the fold with no
// executor. Same function as `run`, same code path -- not a reimplementation that
// drifts." Everything else in this package is a projection of a fold somebody
// else performed. This one performs the fold itself, with the executor simply
// absent, which is why it needs no new machinery at all: kernel.Decide, the
// frozen blueprint and the log were already enough.
//
// So the interesting engineering here is not the fold. It is refusing to lie
// about what the fold did, and there were two chances to.
//
// # §20.9's own transcript prints a wrong number, and this does not copy it
//
// The design doc shows:
//
//	state at seq 44: stage review, backend idle, frontend submitted
//	  spend: 0.0000 USD (replay does not execute effects)
//
// The parenthetical is right and the label is wrong. Two different amounts are
// in play at seq 44: what THIS PROCESS spent, which is exactly zero because it
// called nothing, and what the ORIGINAL RUN had spent by then, which is folded
// out of its llm.response events and is whatever it is. A run that burned $3.10
// by seq 44 replayed as "spend: 0.0000 USD" is the exact failure ADR-0002 warns
// about three paragraphs later -- "it would not error; it would just be
// confidently wrong, which is the failure mode a debugging tool can least
// afford" -- and it would be produced by the debugging tool itself.
//
// Both figures are printed, each labelled with whose it is. §20.9's parenthetical
// is kept verbatim on the zero, because that is the line it belongs to.
//
// # "No executor" is measured, not asserted
//
// The other chance to lie is subtler: the parenthetical asks the reader to take
// on faith that nothing was executed. The fold knows better than that. Every
// kernel.Decide call hands back the effects it wanted, and those are exactly the
// work an executor would have done -- so they are counted and the count is
// printed, split by whether the effect costs money (SpawnTurn and CallTool) or
// not. "12 skipped, 3 of them spend" is evidence; "replay does not execute
// effects" is a promise. The line costs one integer and turns one into the other.
//
// It also earns its place as a debugging figure in its own right. A replay whose
// tally is 40 turns is a run that asked for forty model calls to get where it
// got, and that number appears nowhere else in the tool.
//
// # Folded event by event, and not with kernel.Fold
//
// logstore.(*Store).Fold already does exactly this in three lines, and it is not
// used, for two reasons that are both about what it throws away. It returns the
// final state only, so the EVENT at the target seq -- which §20.9 puts on the
// headline, `stage.advanced build → review` -- is not recoverable from its
// result; and it discards the effects inside kernel.Fold, so the tally above
// cannot be taken. Both are available for free by running the same loop here.
//
// This is not a second reducer. The loop is `at, effs = kernel.Decide(at, e, cfg)`
// and nothing else; the ordering, the rules and the state transitions are the
// kernel's. Compare cmdRunFork, which folds event-by-event for the same class of
// reason (it needs the seq a run went terminal at) and is likewise not a fork of
// the reducer.
//
// # --until-seq is inclusive, and 0 is the same as absent
//
// Inclusive because `event log --since-seq` is (event.go:284), and the two flags
// would otherwise differ by one event in opposite directions on commands a user
// runs one after the other. Zero means the whole log for the same reason
// logstore.Read does (store.go:396) and parseSinceSeq does: a script computing
// `--until-seq $(...)` from an empty or fresh log passes 0, and treating that as
// "replay nothing" would answer a question nobody asked. A negative is refused
// rather than clamped -- it can only be a bug in whatever produced it.
//
// # It takes no lock and writes nothing
//
// Not even the snapshot. `run replay` is safe to run against a live run being
// appended to by another process, which is the case it is most wanted in, and the
// writer lock exists precisely to make that impossible. Nothing here needs
// atExit either: the reason that registry exists is os.Exit skipping the deferred
// release of a lock this command never takes.
func cmdRunReplay(args []string) {
	c := surface.Lookup("run", "replay")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run replay: %v\n\n%s", err, runReplayUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run replay: which run?\n\n"+runReplayUsage+
			"  see what exists: arxi run list\n")
		os.Exit(2)
	}

	// The flag is parsed before the log is opened, so a typo costs nothing. It
	// cannot be range-checked yet: the head it has to be below is not known until
	// the log is read, and that check is further down.
	want, err := parseUntilSeq(vals["until-seq"], runArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run replay: %v\n", err)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// The shared reader is used even though its own fold is not what this command
	// wants. It is the one place that knows how a run directory is read -- log,
	// frozen blueprint, and the simulated flag that lives only on run.started --
	// and a second reader here would be a second set of rules, whose first symptom
	// of drift is `run replay` describing a run `run show` says is not there.
	//
	// Its state is not discarded: it is the run's FINAL state, which is what the
	// not-at-the-head note reports below. A reader looking at seq 44 of a run that
	// ended cancelled wants to be told that, and this is the only place it is known
	// without folding twice on purpose.
	final, cfg, simulated, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run replay: %v\n", err)
		os.Exit(1)
	}

	id := final.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	if len(events) == 0 {
		fmt.Fprintf(os.Stderr, "arxi run replay: run %s has an empty log, so there "+
			"is no state to fold to.\n"+
			"  a run's history IS its log (ADR-0002), and this one holds no events.\n"+
			"  the file that is empty: %s\n",
			id, filepath.Join(dir, "events.ndjson"))
		os.Exit(1)
	}

	head := events[len(events)-1].Seq
	if want == 0 {
		// Defaulted to the head, and the resolved number goes on the headline rather
		// than being left implicit -- the same judgement `run fork` makes about
		// --at-seq (fork.go:208). The seq is what makes the reading reproducible: a
		// replay of "the whole log" means something different tomorrow if the run is
		// still going, and the number pins which log was folded.
		want = head
	}
	if want > head {
		fmt.Fprintf(os.Stderr, "arxi run replay: run %s only reaches seq %d, so there "+
			"is no seq %d to replay to.\n"+
			"  a replay cannot show a state the run has not been in.\n"+
			"  omit --until-seq to fold the whole log (seq %d), or look at what is "+
			"there: arxi event log %s\n", id, head, want, head, runArg)
		os.Exit(1)
	}

	// The fold. Every event at or below the target is applied, in the order the log
	// holds them, and the effects are counted instead of executed.
	//
	// `continue` and not `break`, and the highest seq is tracked rather than assumed
	// to be the last one applied. logstore.Open validates that a log's seqs run 1..N
	// with no gaps, but this command reads the bytes without the writer lock, so
	// nothing upstream has made that promise about these particular bytes. For a
	// well-formed log the two spellings are identical; for a malformed one this
	// still folds everything at or below the target rather than stopping at the
	// first surprise.
	at, tally, applied := kernel.State{}, effectTally{}, 0
	reached := int64(0)
	var atEvent kernel.Event
	for _, e := range events {
		if e.Seq > want {
			continue
		}
		var effs []kernel.Effect
		at, effs = kernel.Decide(at, e, cfg)
		tally.add(effs)
		applied++
		if e.Seq >= reached {
			reached, atEvent = e.Seq, e
		}
	}
	if applied == 0 {
		fmt.Fprintf(os.Stderr, "arxi run replay: no event in run %s has a seq at or "+
			"below %d, so there is nothing to fold.\n"+
			"  the log's first seq is %d.\n"+
			"  read it: arxi event log %s\n", id, want, events[0].Seq, runArg)
		os.Exit(1)
	}

	// Stat'ed rather than inferred from an empty Config, because those are different
	// facts: foldRunDirEvents tolerates a missing snapshot in silence
	// (unpause.go:444), and a blueprint that exists and declares no members would
	// otherwise be reported as a blueprint that is not there.
	_, snapErr := os.Stat(filepath.Join(dir, "blueprint.snapshot.yaml"))

	v := replayView{
		id: id, arg: runArg, dir: dir, st: at, cfg: cfg, simulated: simulated,
		want: want, reached: reached, head: head, applied: applied,
		at: atEvent, tally: tally, final: final.Status,
		noSnapshot: os.IsNotExist(snapErr),
		noStart:    events[0].Type != kernel.RunStarted,
		firstType:  events[0].Type,
	}

	if vals["json"] == "true" {
		emitJSON(runReplayPayload(v))
		return
	}
	printRunReplay(v)
	printReplayWarnings(v)
}

// replayView is everything one replay decided, gathered before anything is
// printed.
//
// A struct rather than a dozen arguments because the human and the JSON readings
// take exactly the same inputs, and the way those two drift is one of them
// growing a parameter. `event log` uses eventLogView for the same reason.
type replayView struct {
	id, dir   string
	st        kernel.State
	cfg       kernel.Config
	simulated bool

	// arg is what the user typed to name this run, and every command this file
	// SUGGESTS uses it rather than id.
	//
	// The two are usually the same string and diverge exactly where it matters. A
	// run directory copied aside for inspection keeps the original id inside its
	// log, so `arxi run replay gap --until-seq 11` printed
	//
	//	fold all of it: arxi run replay rmtizyc28-ba2261a1
	//
	// which names a DIFFERENT directory -- the pristine original, whose log has
	// none of the damage the reader was looking at. Following that advice quietly
	// answers a question nobody asked. Found by running the verb against a log
	// with an event removed, which is a thing only a copied directory can be.
	//
	// id is still the right name for IDENTIFYING the run, which is why the
	// warnings below keep using it: "run rmtizyc28-ba2261a1 has no frozen
	// blueprint" is true of the run, and the directory appears on its own line.
	// Only a command line has to round-trip, and arg is the only value that is
	// guaranteed to, because resolveRunDir(arg) is what produced dir.
	arg string

	// want is the seq asked for, reached is the highest seq actually applied. They
	// differ only on a log with a gap, and the difference is reported rather than
	// hidden: the headline says which state is on screen, so it has to be the seq
	// that produced it.
	want, reached, head int64
	applied             int

	at    kernel.Event
	tally effectTally

	// final is the status after the WHOLE log, not at reached. It is what makes the
	// not-at-the-head note worth printing.
	final kernel.RunStatus

	noSnapshot bool
	noStart    bool

	// firstType is the log's actual first event type, carried so the noStart warning
	// can name it. "does not begin with run.started" sends the reader to look; "begins
	// with agent.turn_done, not run.started" has already told them what happened.
	firstType kernel.EventType
}

// parseUntilSeq reads --until-seq, returning 0 for "the whole log".
//
// The three answers it can give are the whole contract of the flag: 0 for absent
// or explicitly zero, a positive seq, and an error. Zero and absent are the same
// answer on purpose -- see the note on the sentinel in cmdRunReplay's comment --
// which is why the caller cannot distinguish them and does not need to.
//
// It does NOT check the seq against the head, because the head is not known
// before the log is read and this runs before that. Splitting the two checks is
// deliberate: the shape error is free, the range error costs a file read, and a
// user who typed `--until-seq forty-four` should not have to wait for I/O to
// learn it.
func parseUntilSeq(raw, runArg string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("--until-seq %q is not a seq; it is a sequence number "+
			"within one run, as printed in the SEQ column of arxi event log %s",
			raw, runArg)
	}
	// Refused rather than clamped to 0. Clamping would fold the WHOLE log for
	// somebody who asked for a prefix, and the output would look entirely
	// reasonable -- a wrong state with no indication that the flag was ignored.
	if n < 0 {
		return 0, fmt.Errorf("--until-seq %d is below the first seq; a run's log "+
			"starts at seq 1, and seq 1 is run.started.\n"+
			"  omit the flag to fold the whole log", n)
	}
	return n, nil
}

// effectTally counts the effects a replay did not execute.
//
// It is the measurement behind §20.9's parenthetical. Splitting by variant rather
// than keeping one integer is what makes the number readable: "12 effects" says
// nothing a user can act on, and "2 agent turns, 1 tool call, 9 derived events"
// says which of them would have cost money and which were bookkeeping.
//
// The default branch is the honest part. Effect is a sealed interface and
// TestEffectExhaustive fails when a variant is added without registering it, but
// nothing makes that test look at this switch. So an unknown variant is counted
// into other and NAMED as unnamed in the output, rather than being folded into a
// category it may not belong to. total is exact whatever happens, which is the
// figure the "nothing was executed" claim rests on; only the breakdown can go
// stale, and it says so when it does.
type effectTally struct {
	total, turns, tools, asks, emits, timers, snapshots, other int
}

func (t *effectTally) add(effs []kernel.Effect) {
	for _, e := range effs {
		t.total++
		switch e.(type) {
		case kernel.SpawnTurn:
			t.turns++
		case kernel.CallTool:
			t.tools++
		case kernel.AskHuman:
			t.asks++
		case kernel.Emit:
			t.emits++
		case kernel.SetTimer, kernel.CancelTimer:
			t.timers++
		case kernel.Snapshot:
			t.snapshots++
		default:
			t.other++
		}
	}
}

// costly is the count of effects that would have spent money.
//
// SpawnTurn and CallTool, and not AskHuman, which is ClassIndependent alongside
// them and free: it writes an inbox item and waits for a person. Grouping by
// EffectClass instead would have put a question in with the model calls and
// reported a spend that could not happen.
func (t effectTally) costly() int { return t.turns + t.tools }

// breakdown names only the kinds that actually occurred.
//
// An unconditional list would print "0 tool calls, 0 questions, 0 snapshots" on
// almost every replay, and the same argument printEventLogFooter makes applies: a
// note that is always printed is a note that is always skipped.
func (t effectTally) breakdown() string {
	var parts []string
	add := func(n int, one, many string) {
		if n == 0 {
			return
		}
		word := many
		if n == 1 {
			word = one
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, word))
	}
	add(t.turns, "agent turn", "agent turns")
	add(t.tools, "tool call", "tool calls")
	add(t.asks, "question", "questions")
	add(t.emits, "derived event", "derived events")
	add(t.timers, "timer", "timers")
	add(t.snapshots, "snapshot", "snapshots")
	add(t.other, "effect this command cannot name",
		"effects this command cannot name")
	return strings.Join(parts, ", ")
}

// wireTally is the machine reading of the same counts.
//
// Zeroes are included here and omitted from breakdown, and the difference is not
// an inconsistency: a consumer reading .effects_discarded.call_tool must get 0
// rather than a missing key, or every caller has to defend against absence.
func (t effectTally) wire() map[string]any {
	return map[string]any{
		"total": t.total, "would_have_spent": t.costly(),
		"spawn_turn": t.turns, "call_tool": t.tools, "ask_human": t.asks,
		"emit": t.emits, "timer": t.timers, "snapshot": t.snapshots,
		"unnamed": t.other,
	}
}

// skipped is the whole evidence line: how many, how many of them cost money, and
// which kinds they were.
//
// The "n of them spend" clause is the part that turns the count into an argument.
// "12 effects skipped" is a statistic; "12 skipped, 3 of them spend" says what a
// run of this replay avoided doing, which is the claim §20.9's parenthetical makes
// and does not support.
//
// "none of them spend" is spelled out rather than left as "0 of them", because a
// replay that discarded nothing expensive is the reassuring case and a zero in the
// middle of a sentence is the easiest thing on the line to misread.
func (t effectTally) skipped() string {
	if t.total == 0 {
		return "none"
	}
	spend := fmt.Sprintf("%d of them spend", t.costly())
	if t.costly() == 0 {
		spend = "none of them spend"
	}
	return fmt.Sprintf("%d, %s (%s)", t.total, spend, t.breakdown())
}

// printRunReplay is the human reading: a headline, a state line, and four figures.
//
// The shape follows §20.9 -- `[replay] seq N <event>` and then `state at seq N:` --
// and departs from it in exactly one place, the spend block, for the reason
// cmdRunReplay's comment gives at length: the doc's single spend line cannot be
// right about both of the two amounts in play.
//
// It is deliberately NOT a table of everything the state holds. `run show` is that
// verb, over the same fold, and reprinting its members-locks-questions blocks here
// would make `run replay` the worse of two spellings of one view. What this prints
// is what is true of the REPLAY: which seq, which event, what this process spent,
// and what the fold threw away.
func printRunReplay(v replayView) {
	line, elided := replayEventLine(v.at)
	fmt.Printf("[replay] seq %d of %d  %s\n", v.reached, v.head, line)
	fmt.Printf("state at seq %d: %s\n", v.reached, replayStateLine(v.st))

	// §20.9's parenthetical, kept word for word, on the one line it is true of.
	fmt.Printf("  replay spend:    %s USD (replay does not execute effects)\n",
		trimUSD(0))

	// The [simulated] marker sits on this line and not the headline, which is where
	// `run show` puts it. That command prints money in four places, so the qualifier
	// has to lead; this one prints exactly one recorded figure, and marking it in
	// place beats a marker five lines above it.
	sim := ""
	if v.simulated {
		sim = "  [simulated]"
	}
	fmt.Printf("  recorded spend:  %s%s\n", showSpend(v.st), sim)
	fmt.Printf("  effects skipped: %s\n", v.tally.skipped())

	// The count of events, not the highest seq, and the two are printed separately
	// on purpose: they are equal for every well-formed log, and their difference is
	// the only visible symptom of a log with a hole in it.
	fmt.Printf("  events applied:  %d\n", v.applied)
	fmt.Printf("  log:             %s\n", filepath.Join(v.dir, "events.ndjson"))

	if v.want != v.reached {
		fmt.Printf("\nnote: this log holds no seq %d, so the fold stopped at seq %d, "+
			"the highest seq at or below it.\n", v.want, v.reached)
	}
	if v.reached < v.head {
		fmt.Printf("\nnote: this is not the head. the log runs to seq %d, where the "+
			"run is %s.\n  fold all of it: arxi run replay %s\n",
			v.head, v.final, v.arg)
	}
	if elided {
		fmt.Printf("\nnote: a payload value on the headline was shortened to fit "+
			"one line.\n  the event entire: arxi event log %s --since-seq %d\n",
			v.arg, v.reached)
	}
}

// replayEventLine names the event the state is at, and reports whether it had to
// cut anything to stay on one line.
//
// §20.9's headline is `stage.advanced build → review`, and the arrow is the reason
// this is not just formatPayloadCell. `from=build to=review` carries the identical
// information and reads as two unrelated keys; the arrow says which way the
// transition went, which is the one thing a reader of a replay headline wants.
// Only `to` is a common case on its own (stage.entered has no `from`), and it keeps
// the arrow, because `→ review` still says "this moved into review".
//
// Every OTHER key goes through formatPayloadCell rather than being dropped. That is
// the same rule `event log` holds itself to (event.go:539, "no key is dropped"), and
// it matters more here than there: this line is the only event this command prints,
// so anything it silently omits is omitted from the whole output.
//
// The bool is whether formatPayloadCell elided a value. It is returned rather than
// swallowed for the reason `event log` grew its footer: a … that nobody accounts for
// makes the reader wonder what was in the part they cannot see, and here the answer
// is one `event log` away.
func replayEventLine(e kernel.Event) (string, bool) {
	var b strings.Builder
	b.WriteString(string(e.Type))

	// Copied, not mutated. e.Payload belongs to the caller's event, which the JSON
	// reading marshals in full a few lines later -- deleting from it here would make
	// --json and the human view disagree about what the event contained, and only on
	// the events that happen to have a from/to.
	rest := make(map[string]any, len(e.Payload))
	for k, val := range e.Payload {
		rest[k] = val
	}
	from, hasFrom := rest["from"].(string)
	to, hasTo := rest["to"].(string)
	switch {
	case hasFrom && hasTo:
		fmt.Fprintf(&b, "  %s → %s", from, to)
		delete(rest, "from")
		delete(rest, "to")
	case hasTo:
		fmt.Fprintf(&b, "  → %s", to)
		delete(rest, "to")
	}

	// Guarded, because formatPayloadCell renders an empty map as "-": right for a
	// table that must fill a column, wrong for a sentence, where it would read as a
	// dash somebody left behind.
	if len(rest) == 0 {
		return b.String(), false
	}
	cell, elided := formatPayloadCell(rest)
	b.WriteString("  " + cell)
	return b.String(), elided
}

// replayStateLine is §20.9's second line: the whole state, on one line.
//
// The doc shows `stage review, backend idle, frontend submitted` and the status is
// added in front of it. That is not padding: a replay to seq 44 of a run that was
// cancelled at seq 12 shows every member idle and a stage that is no longer being
// worked, and without the word `cancelled` that reads as a healthy run gone quiet.
//
// One line, and not `run show`'s member block, because the value of this view is
// that two replays of adjacent seqs can be diffed by eye. A twelve-line block per
// seq loses that, and `run show` already exists for the reader who wants the block.
func replayStateLine(st kernel.State) string {
	// An empty status is possible on a log that does not begin with run.started,
	// which printReplayWarnings reports separately. It is named rather than printed
	// as the empty string it is: `state at seq 3: , stage build` reads as a bug in
	// the renderer, and sends the reader after the wrong problem.
	status := string(st.Status)
	if status == "" {
		status = "status never recorded"
	}
	parts := []string{status}
	if st.Stage != "" {
		parts = append(parts, "stage "+st.Stage)
	}
	for _, m := range st.Members {
		parts = append(parts, m.Name+" "+replayMemberWord(m))
	}
	return strings.Join(parts, ", ")
}

// replayMemberWord is one member in one or two words.
//
// The state word alone, which gives §20.9's literal `frontend submitted`, plus a
// `(submitted)` qualifier in the one case where the word would hide it. A member
// that submitted and then went to `tool` or `waiting` has Submitted true and a State
// that says otherwise -- decide.go:717 keeps a finished turn from overwriting the
// state precisely so that flag survives -- and a stage's advance rule is judged on
// Submitted, not on State. Printing only `waiting` for such a member invites the
// reader to ask why the stage has not advanced when it already has what it needs.
//
// It is NOT `run show`'s memberNote. That function answers "what is this member
// waiting for", which needs the run's status and the pending causes and takes four
// lines per member to say. This answers "where was this member at seq N", in a form
// that fits a dozen of them on one line.
func replayMemberWord(m kernel.Member) string {
	if m.Submitted && m.State != kernel.MemberSubmitted {
		return fmt.Sprintf("%s (submitted)", m.State)
	}
	return string(m.State)
}

// printReplayWarnings reports what was odd about the log, on stderr, after the
// answer has already been printed.
//
// stderr and not stdout, and after and not before, because these two are not part of
// the reading -- they are caveats on how much to trust it. A script doing
// `arxi run replay r1 > state.txt` gets the state in the file and the caveat on the
// terminal, which is the split every other command in this binary uses.
//
// Both WARN and continue, where `run fork` refuses on the second of them. That is
// not inconsistency: forking an odd log creates a new run that inherits the oddity,
// and folding one only reads it. A user replaying a log that is missing its
// blueprint is very often a user replaying it BECAUSE the log is broken, and a
// debugging verb that refuses to look at the broken case is not a debugging verb.
func printReplayWarnings(v replayView) {
	if v.noSnapshot {
		fmt.Fprintf(os.Stderr, "\narxi run replay: warning: run %s has no frozen "+
			"blueprint, so this fold used an empty config.\n"+
			"  the frozen copy is what makes a replay reproducible (ADR-0001): with "+
			"no stages, no watchers and no ceiling in the config, the reducer cannot "+
			"advance a stage or wake an agent, and the state above can differ from "+
			"the one the run was really in.\n"+
			"  the file that is missing: %s\n",
			v.id, filepath.Join(v.dir, "blueprint.snapshot.yaml"))
	}
	if v.noStart {
		fmt.Fprintf(os.Stderr, "\narxi run replay: warning: run %s's log begins with "+
			"%s, not %s, so the fold started from a state no run has been in.\n"+
			"  it is folded anyway, because reading a log that is wrong is what this "+
			"verb is for -- but a run that was never started has no roster and no "+
			"budget, so any member above is one a later event named.\n",
			v.id, v.firstType, kernel.RunStarted)
	}
}

// runReplayPayload is the machine reading, built on top of `run show`'s.
//
// Reusing runShowPayload is the point rather than a convenience. A replay to the head
// and a `run show` of the same run are the same fold, so their JSON has to be the
// same shape -- a consumer that can read one must be able to read the other, and a
// hand-built map here would drift from it the first time either gained a field.
//
// What is added is only what is true of the replay and of nothing else: which seq was
// asked for, which was reached, where the head is, the event at that seq, and the
// tally. `spent_usd` keeps run show's meaning, the RECORDED figure, and
// `replay_spent_usd` is beside it for the reason the human view prints two lines:
// a caller reading `spent_usd` out of a replay could otherwise reasonably take it for
// what the replay cost, and be wrong by whatever the run actually spent.
func runReplayPayload(v replayView) map[string]any {
	out := runShowPayload(v.id, v.dir, v.st, v.cfg, v.simulated)

	out["until_seq"] = v.want
	out["at_seq"] = v.reached
	out["head_seq"] = v.head
	out["at_head"] = v.reached == v.head
	out["events_applied"] = v.applied

	// Marshalled as kernel.Event, which is eventLogPayload's rule (event.go:607) and
	// for its reason: the wire shape and the log's own shape are then the same shape
	// by construction, rather than by two people remembering to update both.
	out["at_event"] = v.at

	// A literal 0.0 and not a computed figure, because it is not a measurement of
	// this run -- it is the count of provider calls this process made, which is zero
	// by construction. The tally beside it is the evidence for that.
	out["replay_spent_usd"] = 0.0
	out["effects_discarded"] = v.tally.wire()

	// The status after the WHOLE log, which is a different fact from `status`: that
	// one is the status AT at_seq. A consumer replaying seq 44 of a cancelled run
	// gets "running" from one and "cancelled" from the other, and both are true.
	out["final_status"] = string(v.final)
	out["blueprint_snapshot_missing"] = v.noSnapshot
	out["starts_with_run_started"] = !v.noStart
	return out
}
