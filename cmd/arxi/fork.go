package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

const runForkUsage = "usage: arxi run fork <run> [--at-seq N] [--budget <usd>]\n" +
	"                    [--blueprint <file>] [--run-id <id>]\n" +
	"  it creates a NEW run out of the parent's history up to --at-seq\n" +
	"  the parent is not touched: nothing is appended to it, nothing is removed\n" +
	"  --at-seq defaults to the parent's head, and the seq used is printed\n" +
	"  short: -r run  -b budget\n"

// cmdRunFork implements `arxi run fork <run> [--at-seq N] [--budget U]
// [--blueprint F] [--run-id ID]`.
//
// # The command two other commands were already recommending
//
// `run prompt` prints `arxi run fork %s --at-seq %d` when it refuses a terminal
// run (prompt.go:128) and `event emit` prints the same when it refuses one
// (emit.go:152). Both were printing a command the binary answered "declared in
// the surface but not implemented yet" to -- the fourth instance of this
// project's worst defect class, and the one with the largest audience, because
// it is the only continuation offered for a run that has already ended.
//
// # Fork is a DIRECTORY operation, and that was measured, not chosen
//
// There is no run.forked event type. internal/kernel/event.go lists every type
// the reducer knows and none of them is a fork; grepping the kernel and the
// logstore for "fork" finds one string, in logstore/errors.go:69. So a fork
// cannot be an append to the parent -- there is nothing to append that any
// reader would fold into anything.
//
// What a fork is instead: a new run directory, holding a blueprint and a COPY of
// the parent's events with seq <= at-seq. That is the whole mechanism, and it
// follows from ADR-0002. If the log is the truth and state = fold(events), then
// a run whose log is the parent's first 44 events IS the parent as it was at
// seq 44 -- no state has to be transplanted, because state is not stored.
//
// # Copied through Append, not by copying bytes
//
// The events are re-appended with Seq zeroed rather than written out as raw
// lines. logstore.Append assigns the sequence itself and refuses any event that
// arrives with a seq already set, and logstore.Open validates that seq runs
// 1..N with no gaps. Going through that path means the fork's log is built by
// the same writer that built the parent's, and the numbers come out identical
// for a contiguous prefix -- so `--at-seq 44` really does mean "seq 44 in the
// fork is seq 44 in the parent", which is what makes the two comparable at all.
//
// # What is rewritten in the copy, and what deliberately is not
//
// run.started is rewritten: run_id, because otherwise the fork folds to the
// PARENT's RunID and every command in the tool would report the fork under the
// parent's name; budget_usd when --budget is given, because that field is the
// only place a ceiling exists; and forked_from / forked_at_seq, so the fork's
// own log says where it came from. Scope is rewritten from run:<parent> to
// run:<fork> for the same reason.
//
// Event IDs are NOT rewritten. The fork's history is literally the parent's
// history, caused_by holds ids rather than seqs, and renaming them would break
// every causal chain `event trace` walks while making the copy look like
// different events that happened to say the same things.
// # parent_run_id is deliberately NOT written, and this one is a money argument
//
// The obvious thing is to record the fork as a child of the parent so `run tree`
// shows the lineage. It would be wrong, and runtree.go:55 says why: that command
// computes a tree's total by SUMMING each node's own SpentUSD. The fork's copied
// prefix already contains the parent's llm.response events, so the fork's own
// spend includes money the parent also reports -- making the fork a tree child
// would double-count every dollar spent before the fork point and print a total
// nobody spent. runtree.go:324 would additionally flag a spawn_depth
// disagreement, because the structural walk and the recorded depth would differ.
//
// A fork is a sibling with a shared past, not a sub-run. forked_from in its
// run.started is where the lineage lives, and it is readable with `event log`.
//
// # The workspace is not copied, and that is said out loud
//
// runs/<parent>/workspace holds the files the agents actually produced. Copying
// it would hand the fork the parent's FINAL files, which are not the files as of
// at-seq -- a fork at seq 44 of a run that worked on until seq 90 would inherit
// state from a future its own log has never seen. Both choices misrepresent
// something; not copying misrepresents less, and the command prints a line about
// it when the parent's workspace is non-empty rather than leaving it to be
// discovered by a member who cannot find a file it remembers writing.
//
// # Fork does not drive
//
// Every other write in this package drives the loop afterwards, and this one
// must not. Driving a fresh fork would fold the parent's own events and produce
// the parent's own effects -- the same events yield the same decisions -- which
// re-runs history at full price instead of changing anything. §20.9's premise is
// "fork is how you change your mind", and the mind-changing happens in the NEXT
// command: `run prompt <fork> "..."` for a running fork, `run unpause <fork>`
// for one that inherited a pause or a block.
//
// So the closing line is branched on the fork's folded status, and each branch
// was run by hand -- the last four defects found in this project were all
// printed-and-refused commands, and three of them were printed by the command
// written to close the previous one.
func cmdRunFork(args []string) {
	c := surface.Lookup("run", "fork")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run fork: %v\n\n%s", err, runForkUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run fork: which run?\n\n"+runForkUsage+
			"  see what exists: arxi run list\n")
		os.Exit(2)
	}
	// Every flag is parsed and range-checked BEFORE any directory is created, for
	// the reason `run start` documents: a refusal that costs nothing is worth more
	// than a failure that leaves debris. Here the debris would be a runs/<id>
	// holding a frozen blueprint and no log, which `run list` then shows forever.
	atSeq := int64(-1)
	if raw, ok := vals["at-seq"]; ok && raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi run fork: --at-seq %q is not a seq.\n"+
				"  it is the point in the parent's history the fork starts from, "+
				"e.g. --at-seq 44\n"+
				"  the seqs of a run: arxi event log %s\n", raw, runArg)
			os.Exit(2)
		}
		// Seq 1 is run.started, so 1 is the smallest fork point that has a run in
		// it at all. Zero is not "the beginning" -- it is a log with no start
		// event, which folds to a State with no RunID and cannot be driven.
		if n < 1 {
			fmt.Fprintf(os.Stderr, "arxi run fork: --at-seq %d is before the run "+
				"began.\n  seq 1 is run.started, so a fork has to include it: the "+
				"earliest useful fork is --at-seq 1, which is the run as it was "+
				"before anything happened.\n", n)
			os.Exit(2)
		}
		atSeq = n
	}

	// --budget is optional here and mandatory on `run start`, and the difference
	// is not an inconsistency: a fork that says nothing about money inherits the
	// parent's recorded ceiling, which is a number the user did choose once. There
	// is no invisible default being invented.
	budget := 0.0
	if raw, ok := vals["budget"]; ok && raw != "" {
		budget, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi run fork: --budget %q is not a number of "+
				"dollars.\n  it is the ceiling for the fork's whole tree, "+
				"e.g. --budget 8.00\n", raw)
			os.Exit(2)
		}
		if budget <= 0 {
			fmt.Fprintf(os.Stderr, "arxi run fork: --budget %v is not a ceiling a "+
				"run can spend under.\n  a fork that may not spend anything cannot "+
				"take a single turn, and it would be a copy of the parent with no "+
				"way to continue.\n  omit the flag to inherit the parent's ceiling.\n",
				budget)
			os.Exit(2)
		}
	}
	bpPath := strings.TrimSpace(vals["blueprint"])
	newID := strings.TrimSpace(vals["run-id"])

	// The id is validated as a DIRECTORY NAME, because that is what it becomes.
	// `--run-id ../x` would put the fork's log outside runs/ and `--run-id a/b`
	// would nest it where `run list` cannot see it, and both would look like they
	// worked. resolveRunDir accepts a path deliberately (a run started with --dir
	// has to be reachable), so nothing downstream would catch this.
	if newID != "" && (strings.ContainsAny(newID, `/\`) || newID == "." || newID == "..") {
		fmt.Fprintf(os.Stderr, "arxi run fork: --run-id %q is not a run id.\n"+
			"  the id IS the directory name under ./%s, so it cannot contain a "+
			"path separator: the fork would land somewhere arxi run list does not "+
			"look.\n", newID, runsDir)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// The parent is read in ONE pass: foldRunDirEvents hands back the state, the
	// frozen config and the events it folded, and the events are what this command
	// actually copies. Reading the log twice -- once to fold, once to copy -- would
	// let the two reads disagree on a run that is being appended to right now, and
	// the fork would then hold a prefix of a history nobody had folded.
	parent, cfg, simulated, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run fork: %v\n", err)
		os.Exit(1)
	}
	if len(events) == 0 {
		fmt.Fprintf(os.Stderr, "arxi run fork: run %s has an empty log, so there "+
			"is nothing to fork from.\n", runArg)
		os.Exit(1)
	}

	head := events[len(events)-1].Seq
	if atSeq < 0 {
		// Defaulted to the head, and the resolved number is printed on the headline
		// rather than left implicit. §20.9's own example passes --at-seq, and a
		// fork whose start point was chosen by the binary must at minimum say
		// which one it chose -- the seq is the one fact that makes the fork
		// reproducible by hand.
		atSeq = head
	}
	if atSeq > head {
		fmt.Fprintf(os.Stderr, "arxi run fork: run %s only reaches seq %d, so it "+
			"has no seq %d to fork from.\n"+
			"  a fork cannot start in a future the parent has not had.\n"+
			"  omit --at-seq to fork from the head (seq %d), or look at what is "+
			"there: arxi event log %s\n", parent.RunID, head, atSeq, head, runArg)
		os.Exit(1)
	}
	prefix := make([]kernel.Event, 0, len(events))
	for _, e := range events {
		if e.Seq <= atSeq {
			prefix = append(prefix, e)
		}
	}
	// Checked rather than assumed. logstore.Open validates that seq runs 1..N with
	// no gaps, but this command folds the log without taking the writer lock (the
	// parent may legitimately be held by another process), so nothing upstream has
	// made that promise about these bytes.
	if len(prefix) == 0 || prefix[0].Type != kernel.RunStarted {
		fmt.Fprintf(os.Stderr, "arxi run fork: the log of %s does not begin with "+
			"run.started, so a prefix of it is not a run.\n"+
			"  a fork is a new run whose history is the parent's, and a history "+
			"with no beginning folds to a state with no run id and no members.\n"+
			"  read it: arxi event log %s\n", runArg, runArg)
		os.Exit(1)
	}

	// The fork's CONFIG is decided here, and there are only two possibilities:
	// re-read a blueprint the user names, or copy the parent's frozen snapshot.
	//
	// §20.9 prints "(blueprint: ./team.yaml, re-read)" and the path had to become a
	// parameter for that line to be implementable at all: run.started records the
	// blueprint's NAME and its SHA and never its path (run.go:227), so nothing in
	// the parent's log can say which file to read again.
	forkCfg, snap := cfg, []byte(nil)
	bpNote, bpName, bpSHA := "copied from the parent", "", ""
	if bpPath != "" {
		bp, berr := blueprint.LoadFile(bpPath)
		if berr != nil {
			// Exit 1 and not 2, matching `run start`: the invocation was right and
			// the file is what is wrong, and CI needs to tell those apart.
			fmt.Fprintf(os.Stderr, "arxi run fork: the blueprint is not valid, so "+
				"nothing was forked.\n\n%v\n", berr)
			os.Exit(1)
		}
		forkCfg, snap = bp.Config, bp.Raw
		bpNote, bpName, bpSHA = bpPath+", re-read", bp.Name, bp.SHA
	} else {
		snap, err = os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml"))
		if err != nil {
			// foldRunDirEvents tolerates a missing snapshot, because a projection
			// can still print a log without one. A fork cannot: the copy would be a
			// run directory with no rules, so it could not be folded with the
			// roster it needs or driven at all.
			fmt.Fprintf(os.Stderr, "arxi run fork: run %s has no frozen blueprint, "+
				"so the fork would have no rules to fold against.\n"+
				"  name one to read: arxi run fork %s --at-seq %d --blueprint <file>\n",
				runArg, runArg, atSeq)
			os.Exit(1)
		}
	}
	if newID == "" {
		// The same minter `run start` uses, and for the same reason: the id IS the
		// directory, so two forks that minted the same one would append into each
		// other's log and neither could be separated from the other afterwards.
		newID = newRunID()
	}
	parentID := parent.RunID
	if parentID == "" {
		parentID = filepath.Base(dir)
	}

	// The copy is built in MEMORY and folded before anything is created on disk.
	// The alternative -- create the directory, append, then discover the fork is
	// terminal -- leaves a run that exists, shows up in `run list`, and cannot do
	// anything, and an append-only log offers no way to take it back.
	copied := make([]kernel.Event, 0, len(prefix))
	for _, e := range prefix {
		// Seq zeroed because logstore.Append assigns it and refuses an event that
		// arrives with one already set. For a contiguous prefix it hands back the
		// same numbers, which is what makes "seq 44 in the fork" mean "seq 44 in
		// the parent".
		e.Seq = 0
		if e.Scope == "run:"+parentID {
			// Rewritten so the fork's events name the fork. Cosmetic today --
			// grepping the tree finds nothing that reads Scope -- and left correct
			// anyway, because the first reader of it would otherwise find a log
			// whose every line claims to belong to a different run.
			e.Scope = "run:" + newID
		}
		if e.Type == kernel.RunStarted {
			e.Payload = forkedStartPayload(e.Payload, newID, parentID, atSeq,
				budget, bpName, bpSHA)
		}
		copied = append(copied, e)
	}

	// Folded one event at a time rather than in one call, because the seq the run
	// ENDED at is the number the refusal below has to print, and kernel.Fold
	// returns only the final state. A message that says "this run is finished"
	// without saying where is not actionable: the whole point of the refusal is to
	// hand back an --at-seq that works.
	//
	// The seq is taken from prefix[i] and NOT from at.Seq, which is a mistake this
	// loop was written with: the events being folded have had their Seq zeroed for
	// the append, so the state's own Seq is 0 all the way down and the refusal
	// printed "--at-seq -1". The parent's numbering is in prefix.
	at, termSeq := kernel.State{}, int64(0)
	for i, e := range copied {
		at, _ = kernel.Decide(at, e, forkCfg)
		if at.Status.Terminal() && termSeq == 0 {
			termSeq = prefix[i].Seq
		}
	}
	// A fork that is born terminal is refused. This is the guard the whole command
	// is judged by, because the commands that recommend `run fork` recommend it FOR
	// a terminal run: prompt.go:128 prints `arxi run fork <run> --at-seq <head>`
	// when it refuses a finished run, and that head is exactly the seq whose fork
	// would be finished too. Without this, following the advice produces a new run
	// in the same terminal state, and the user has paid a directory for nothing.
	//
	// The remedy names the seq BEFORE the event that ended it. Seqs are contiguous
	// (logstore.Open enforces 1..N), and run.started is seq 1 and is not terminal,
	// so termSeq is at least 2 and termSeq-1 is always a real, non-terminal seq.
	if at.Status.Terminal() {
		fmt.Fprintf(os.Stderr, "arxi run fork: forking %s at seq %d would produce a "+
			"run that is already %s.\n"+
			"  the parent reached that at seq %d, and a fork inherits the events "+
			"before its fork point -- including the one that ended it.\n"+
			"  fork from before it instead:\n"+
			"    arxi run fork %s --at-seq %d\n"+
			"  what ended it: arxi run result %s\n",
			parentID, atSeq, at.Status, termSeq, runArg, termSeq-1, runArg)
		os.Exit(1)
	}

	newDir := filepath.Join(runsDir, newID)
	// Checked before MkdirAll, because logstore.Open would happily append the copy
	// onto an existing log: the fork's events would land after somebody else's and
	// the result would be one directory holding two runs' histories, with two
	// run.started events and a fold that reports whichever came first.
	if _, err := os.Stat(filepath.Join(newDir, "events.ndjson")); err == nil {
		fmt.Fprintf(os.Stderr, "arxi run fork: %s already holds a run, so the fork "+
			"was not written.\n"+
			"  appending a second history into one log makes both unfoldable, and "+
			"the log is append-only, so it could not be undone.\n"+
			"  choose another id: arxi run fork %s --at-seq %d --run-id <id>\n",
			newDir, runArg, atSeq)
		os.Exit(1)
	}

	if err := os.MkdirAll(newDir, 0o755); err != nil {
		fatal(fmt.Errorf("create the fork's run directory: %w", err))
	}
	// The blueprint is frozen into the fork BEFORE its first event, exactly as
	// `run start` does it and for the identical reason: kernel.Fold needs the
	// Config the events were decided against, and a fork whose rules live in a file
	// somebody may edit is a log that cannot be replayed.
	if err := os.WriteFile(filepath.Join(newDir, "blueprint.snapshot.yaml"),
		snap, 0o644); err != nil {
		fatal(fmt.Errorf("freeze the fork's blueprint: %w", err))
	}
	store, err := logstore.Open(newDir)
	if err != nil {
		fatal(err)
	}
	defer store.Close()
	atExit(func() { store.Close() })

	// Appended in ONE call, and that matters more here than anywhere else in this
	// package: logstore.Open validated 1..N on the parent, and a partial append --
	// half a history, interrupted -- would produce a fork whose log is a prefix of a
	// prefix, which folds to a run nobody asked for and cannot be truncated back.
	//
	// No CAS. AppendIfSeq guards against somebody else having written since a read,
	// and nobody has read this log: it did not exist a moment ago. The collision
	// check above is the guard that belongs here.
	written, err := store.Append(copied)
	if err != nil {
		fatal(fmt.Errorf("write the fork's history: %w", err))
	}

	// §20.9's headline, and the resolved --at-seq is in it rather than implied: a
	// fork is only reproducible by hand if the seq it started from is stated, and
	// when the flag was omitted the binary chose that number itself.
	fmt.Printf("run %s forked from %s at seq %d (blueprint: %s)\n",
		newID, parentID, atSeq, bpNote)
	fmt.Printf("  %d event(s) copied, seq 1..%d -- the same numbers they have in "+
		"the parent\n", len(written), written[len(written)-1].Seq)
	fmt.Printf("  %s is untouched: nothing was appended to it and nothing removed\n",
		parentID)

	// The ceiling is printed either way, because it is the one inherited number a
	// user is most likely to be wrong about: a fork with no --budget silently
	// carries the parent's ceiling AND the parent's spend against it.
	if budget > 0 {
		fmt.Printf("  budget %s (the parent's was %s), already spent %s of it\n",
			usd(budget), usd(parent.BudgetUSD), usd(at.SpentUSD))
	} else {
		fmt.Printf("  budget %s, inherited, already spent %s of it -- the copied "+
			"history includes the parent's spending\n",
			usd(at.BudgetUSD), usd(at.SpentUSD))
	}
	if simulated {
		fmt.Printf("  the parent was started with --sim, and the fork inherits that: " +
			"no model is called and no money is spent\n")
	}
	// The workspace is reported as absent only when the parent HAS one. A line about
	// files that were not copied, printed for a run that produced no files, is noise
	// that trains people to skip the lines that matter.
	if ents, derr := os.ReadDir(filepath.Join(dir, "workspace")); derr == nil &&
		len(ents) > 0 {
		fmt.Printf("  the parent's workspace (%d entr%s) was NOT copied: those are "+
			"its files as they are NOW, not as they were at seq %d\n",
			len(ents), map[bool]string{true: "y", false: "ies"}[len(ents) == 1], atSeq)
	}
	// An open turn is inherited as a FACT of the fold and cannot be executed: the
	// call that would have completed it belongs to the parent's process, which is
	// not coming back for this log. Saying so is the difference between a fork that
	// looks stalled and one a user knows to prompt.
	if open := openTurns(at); len(open) > 0 {
		fmt.Printf("  %s had a turn open at seq %d and still has it in the fork, "+
			"with no call in flight -- nothing completes it on its own\n",
			strings.Join(open, ", "), atSeq)
	}
	if n := pendingAsks(at); n > 0 {
		fmt.Printf("  %d unanswered ask(s) came with the history: arxi inbox %s\n",
			n, newID)
	}

	// The next step is branched on the fork's own folded status, and each branch
	// names a command that ACCEPTS a run in that status. This is the whole point of
	// writing this verb -- the four defects before it were printed remedies the
	// binary refused -- so the branches were checked against cmdRunUnpause's guards
	// rather than assumed:
	//
	//   paused                  -> unpause resumes it (bare)
	//   blocked, budget spent   -> unpause refuses bare (nothing to raise), needs
	//                              --budget above the copied spend
	//   blocked, budget left    -> unpause resumes it (bare)
	//   running                 -> unpause REFUSES ("already running, so there is
	//                              nothing to resume"), so prompt is the only verb
	//                              that drives it
	//
	// The raised figure is left as a placeholder and not computed. run.go:431
	// refuses to invent a spend ceiling in the binary -- "it would be a spend
	// decision taken in the binary" -- and a number printed here would be exactly
	// that, in the one message a user is most likely to paste unread.
	fmt.Printf("\n")
	switch {
	case at.Status == kernel.StatusPaused:
		fmt.Printf("the fork inherited a pause. resume it: arxi run unpause %s\n", newID)
	case at.Status == kernel.StatusBlocked && budgetIsExhausted(at):
		fmt.Printf("the fork inherited the parent's exhausted budget (%s of %s). "+
			"raise it to continue: arxi run unpause %s --budget <more than %s>\n",
			usd(at.SpentUSD), usd(at.BudgetUSD), newID, usd(at.BudgetUSD))
	case at.Status == kernel.StatusBlocked:
		fmt.Printf("the fork is blocked, with budget left. resume it: "+
			"arxi run unpause %s\n", newID)
	default:
		// Running, and this is the ordinary case: forking is how you change your
		// mind, and the change of mind is the next command. Nothing is driven here
		// -- the copied events yield the copied decisions, so driving would re-run
		// the parent's history at full price and end where the parent already is.
		fmt.Printf("nothing was driven: the same events yield the same decisions, so "+
			"driving now would re-run the parent's history at full price.\n"+
			"  change something: arxi run prompt %s \"what you want instead\"\n"+
			"  or look first:     arxi run why %s\n", newID, newID)
	}
}

// forkedStartPayload rewrites the ONE event in the copy that may not be verbatim.
//
// Everything else in the fork's log is the parent's event unchanged, because the
// fork's history IS the parent's history. run.started is the exception because it
// is the only event that names the run: left alone, the fork folds to the PARENT's
// RunID and every command in the tool -- `run list`, `run show`, `event log` --
// reports the fork under the parent's name, into the parent's own row.
//
// The map is COPIED rather than mutated. The payload handed in is the one
// decodeRunEvents produced for the caller's `events` slice, and the caller folds
// that same slice; mutating it in place would edit the parent's decoded history
// underneath a fold that has not finished, which is a bug that would only show up
// as the parent reporting the fork's budget.
//
// Absent values mean "keep the parent's", and that is why each write is guarded
// rather than unconditional: budget only when --budget was given, actor and
// blueprint_sha only when --blueprint was, because a fork that copies the frozen
// snapshot is running the parent's blueprint and must keep saying so.
func forkedStartPayload(payload map[string]any, newID, parentID string,
	atSeq int64, budget float64, bpName, bpSHA string) map[string]any {
	out := make(map[string]any, len(payload)+3)
	for k, v := range payload {
		out[k] = v
	}
	out["run_id"] = newID
	// The lineage, and the only place it exists. parent_run_id is deliberately not
	// written -- the doc comment on cmdRunFork makes the money argument -- so these
	// two keys are what an audit has to read to learn where a fork came from.
	out["forked_from"] = parentID
	// float64 because that is what encoding/json unmarshals a number into, so a key
	// written as int64 here would compare unequal to the same key read back.
	out["forked_at_seq"] = float64(atSeq)
	if budget > 0 {
		out["budget_usd"] = budget
	}
	if bpName != "" {
		out["actor"] = bpName
	}
	if bpSHA != "" {
		out["blueprint_sha"] = bpSHA
	}
	return out
}
