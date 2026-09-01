// `arxi run attach` follows a live run: the events that happen from now on.
//
// # What it is for
//
// docs/design/20-use-cases.md §20.1 shows it directly after `run show`, which
// reported `run r1: running (seq 12)`:
//
//	$ arxi run attach r1
//	[r1 seq 13] tool.call read src/auth.go
//	[r1 seq 14] llm.response 0.09 USD
//	[r1 seq 15] run.result
//
// Seq 13 is the first line. That is the whole specification of the join point and
// it is worth stating out loud: attach does NOT replay the twelve events that
// already exist. `event log` prints those, `run replay` folds them, `run show`
// summarises them. What none of them can do is tell you what happens next, and a
// verb that backfilled would answer a question already answered three times while
// making the interesting part scroll past.
//
// # Why this is the one verb that needed a machine that did not exist
//
// Every other run verb is a projection of a fold: read the log once, decide, print.
// This one reads a file another process is writing, so it needs three things the
// tree had no use for until now.
//
// FOLLOWING WITHOUT THE LOCK. logstore.Open takes the writer lock (ADR-0002, one
// writer per log) and truncates a rolled-back tail on the way in. A viewer that did
// that would refuse to attach to exactly the runs worth attaching to -- a live one
// is holding its lock -- and, worse, would be able to modify the log it is only
// meant to watch. So the log is opened as a plain file. The other readers in this
// tree already do this: `run show` and the inbox read events.ndjson directly and
// say why.
//
// A JOIN OFFSET THAT CANNOT SKIP AN EVENT. The obvious shape -- fold the run to
// learn where it is, then open the file and seek to the end -- loses every event
// appended between the two reads, silently, and only under load. So the fold and
// the follow share one read: readRunLog returns the bytes, the offset of the last
// newline in THOSE bytes is the join point, and the follow starts there. Whatever
// arrives in between is read as new, which is the only direction of error a viewer
// can afford.
//
// PROVISIONAL BYTES ARE NOT PRINTED. A batch is not committed until logstore
// removes pending.commit, and the next Open rolls an uncommitted tail back --
// correctly, since Append never returned, so nothing downstream ever learned those
// events existed. Every other reader here ignores that marker, and can: they print
// one state and are rerun. This one cannot take a line back once it has scrolled
// past, so it holds whole lines until logstore.BatchInFlight says the tail is
// durable. The exported helper documents why sampling the marker AFTER the read is
// what makes the answer sound.
//
// # Termination, ordered so it cannot lose the tail
//
// The writer lock is sampled BEFORE each read, and that order is the argument: if
// the lock is absent at T, nothing will append after T, so a read at any T' > T has
// already seen everything there is. Sampling it after the read inverts that and can
// stop one batch early.
//
// A run nobody is driving is not waited for. There is no --timeout to declare (the
// surface gives this verb a run id and nothing else), and a viewer that sat silent
// forever on a paused run would look identical to one watching a run that is merely
// thinking. So attach says which of the two it is, in the words the rest of the
// binary uses for it, and leaves. A run being driven is followed for as long as it
// is driven, with the pid out of writer.lock named so the user knows who they are
// waiting on.
//
//	0  the run reached a terminal state while attached, or had already ended
//	3  the follow stopped and the run had NOT ended -- nothing is appending to it
//	1  the run could not be read at all
//	2  the invocation was wrong
//
// 3 rather than 1 for "stopped without ending", and the same 3 `run result` uses, so
// a script polling either verb treats them alike: neither is a broken run, and
// neither should file a storage bug for a run that is waiting on a person.
//
// # Streams
//
// stdout is only ever event rows, one per line. The join header, the closing
// explanation and every warning go to stderr. So `arxi run attach r1 > live.txt`
// leaves a file of rows and nothing else, and under --json that file is valid
// NDJSON -- which is also why --json emits one raw kernel.Event per line rather
// than `event log --json`'s single document. A document has to be complete to
// parse, and completeness is what a live stream does not have.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

const runAttachUsage = "usage: arxi run attach <run> [--json]\n" +
	"  prints what happens from now on · exit 0 the run ended · 3 it stopped without ending\n" +
	"  what already happened: arxi event log <run>\n" +
	"  short: -r run  -J json\n"

// exitAttachNotEnded is `run result`'s "not terminal yet", deliberately shared.
//
// Aliased rather than written as 3, so the two verbs cannot drift apart: a script
// that polls `run attach` and falls back to `run result` reads one number from both.
const exitAttachNotEnded = exitResultNotYet

// attachPollInterval is how long the follower sleeps when the log has not grown.
//
// Short enough that a turn's events feel live, long enough that watching an idle
// run is not a spin loop on the filesystem. There is no flag for it: a viewer's
// refresh rate is not part of the declared surface, and inventing a parameter here
// would be inventing surface.
const attachPollInterval = 120 * time.Millisecond

// attachView is what both the header and the follow loop need, resolved once.
//
// It carries the id AND the argument the user typed. Every suggested command below
// uses arg, for the reason `run replay` had to be fixed for: a run reached by path
// prints suggestions naming a run id that resolves to a different directory.
type attachView struct {
	id        string
	arg       string
	dir       string
	st        kernel.State
	cfg       kernel.Config
	simulated bool

	// consumed is the offset of the end of the last COMPLETE line in the bytes
	// that were folded into st. The follow starts here, so nothing between the
	// fold and the follow can be skipped.
	consumed int64
}

func cmdRunAttach(args []string) {
	c := surface.Lookup("run", "attach")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run attach: %v\n\n%s", err, runAttachUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run attach: which run?\n\n"+runAttachUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// The three steps foldRunDirEvents takes, taken here instead, because this is
	// the one command that needs the byte offset as well as the events. See the
	// file header: a second read to find the join point is a lost event.
	raw, err := readRunLog(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run attach: %v\n", err)
		os.Exit(1)
	}
	events, err := decodeRunEvents(dir, raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run attach: %v\n", err)
		os.Exit(1)
	}
	cfg, err := runFrozenConfig(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run attach: %v\n", err)
		os.Exit(1)
	}

	st, _ := kernel.Fold(kernel.State{}, events, cfg)

	v := attachView{
		id:        st.RunID,
		arg:       runArg,
		dir:       dir,
		st:        st,
		cfg:       cfg,
		simulated: runWasSimulated(events),

		// LastIndexByte+1 is the offset just past the final newline, and 0 when
		// there is none -- an empty log, or one whose only line is torn. Both are
		// correct starting points: the torn line will be re-read and completed by
		// the writer, and re-reading a line nobody printed costs nothing.
		consumed: int64(bytes.LastIndexByte(raw, '\n') + 1),
	}
	if v.id == "" {
		v.id = filepath.Base(dir)
	}

	// An empty log is FOLLOWED, where `run replay` refuses it. The two verbs are
	// asked different questions: replaying nothing can only print an empty state,
	// while a directory that exists with nothing appended yet is a run about to
	// start, and being told to come back is the wrong answer for the one verb whose
	// job is to wait. The lock decides below, which is the same test every other
	// case gets.
	if v.st.Status.Terminal() {
		// Not an error, and not exit 3. The run ended; the honest report is that
		// there is nothing to follow and where to read what happened.
		fmt.Fprintf(os.Stderr, "arxi run attach: %s already ended (%s at seq %d), so nothing more will arrive.\n"+
			"  what happened:  arxi event log %s\n"+
			"  the result:     arxi run result %s\n",
			v.id, v.st.Status, v.st.Seq, v.arg, v.arg)
		os.Exit(0)
	}

	os.Exit(followRunLog(v, vals["json"] == "true"))
}

// followRunLog prints events as they are appended, and returns the exit code.
//
// Returning the code rather than calling os.Exit is what lets the deferred Close
// actually run: os.Exit skips defers, and this file's whole subject is a process
// that holds a handle on a file another process is writing. atExit exists for the
// same reason elsewhere in this binary, and a function boundary is the cheaper
// version of it when there is exactly one handle.
func followRunLog(v attachView, asJSON bool) int {
	// Opened read-only, and NOT through logstore.Open: see the file header. On
	// Windows this matters more than on Unix -- Go's os.Open passes FILE_SHARE_READ
	// and FILE_SHARE_WRITE, so a live writer is not blocked by the follower and the
	// follower is not blocked by it.
	f, err := os.Open(logstore.EventsPath(v.dir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run attach: open the log of %s to follow it: %v\n", v.dir, err)
		return 1
	}
	defer f.Close()

	if _, err := f.Seek(v.consumed, io.SeekStart); err != nil {
		fmt.Fprintf(os.Stderr, "arxi run attach: seek to the join point (offset %d) in the log of %s: %v\n",
			v.consumed, v.dir, err)
		return 1
	}

	owner, held := writerLockOwner(v.dir)
	if !held {
		// Nothing is appending, so there is nothing to wait for. Said in one
		// sentence with the remedy, rather than waited out in silence.
		fmt.Fprintf(os.Stderr, "arxi run attach: nothing is writing to %s, so no events will arrive.\n%s",
			v.id, attachStopReason(v))
		return exitAttachNotEnded
	}

	fmt.Fprintf(os.Stderr, "attached to %s at seq %d · %s is writing · Ctrl-C to stop\n",
		v.id, v.st.Seq, owner)
	if v.simulated {
		// Worth one line, because the rows are indistinguishable from a live run's
		// and somebody reading over a shoulder would take them for model calls that
		// were paid for.
		fmt.Fprintf(os.Stderr, "  (this run is simulated: --sim, so no model was called)\n")
	}

	st := v.st
	buf := make([]byte, 32*1024)
	var pending []byte

	for {
		// Sampled BEFORE the read. The file header's argument for this order is the
		// reason the tail cannot be lost.
		_, stillHeld := writerLockOwner(v.dir)

		n, rerr := f.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			fmt.Fprintf(os.Stderr, "\narxi run attach: reading the log of %s: %v\n", v.dir, rerr)
			return 1
		}

		// Whole lines are emitted only once the batch they belong to is committed.
		// logstore.BatchInFlight is asked AFTER the read for the reason its own
		// comment gives: "no marker now" proves the bytes just read are durable.
		if !logstore.BatchInFlight(v.dir) {
			var lines [][]byte
			lines, pending = wholeLines(pending)
			for _, line := range lines {
				e, ok := decodeAttachLine(v, line)
				if !ok {
					return 1
				}
				emitAttachEvent(v, e, asJSON)

				// Decided, not just printed, so the loop knows when the run is over
				// without folding the whole log again. The effects Decide returns are
				// dropped on purpose: the process that owns this run is running them,
				// and a viewer tallying effects would be reporting work it is not
				// doing.
				st, _ = kernel.Decide(st, e, v.cfg)
				if st.Status.Terminal() {
					fmt.Fprintf(os.Stderr, "\n%s %s at seq %d.\n  the result:  arxi run result %s\n",
						v.id, st.Status, e.Seq, v.arg)
					return 0
				}
			}
		}

		// Bytes arrived, so there may be more waiting: read again before sleeping.
		if n > 0 {
			continue
		}
		if !stillHeld {
			v.st = st
			fmt.Fprintf(os.Stderr, "\narxi run attach: %s is no longer being written to.\n%s",
				v.id, attachStopReason(v))
			if len(pending) > 0 {
				// The one case where held-back bytes are worth naming: a writer that
				// died mid-batch left a tail that the next Open will roll back. Saying
				// so beats printing lines that are about to stop existing, and beats
				// silence, which would look like the log simply stopped.
				fmt.Fprintf(os.Stderr, "  %d byte(s) at the end of the log are part of a batch that was never "+
					"committed, so they are not shown: the next command to open this run rolls them back.\n",
					len(pending))
			}
			return exitAttachNotEnded
		}
		time.Sleep(attachPollInterval)
	}
}

// attachStopReason says what the run is waiting for, as one indented block.
//
// Called from both places the follow ends while the run has NOT ended -- no
// writer at the join point, and the writer going away mid-follow -- because the
// user's next question is the same in both: nothing is appending, so what is it
// waiting on? "Nothing is writing to r1" is a symptom. That the run is blocked on
// a question only a person can answer is the cause, and it is one command away.
//
// The cases mirror `run show`'s closing switch on purpose. Two verbs looking at
// the same stuck run must not send the reader to different places.
//
// Every suggestion names v.arg, never v.id: a run reached by path resolves to a
// directory that a bare id may not, which is the defect `run replay` was fixed
// for. Printing a command that fails is worse than printing none.
func attachStopReason(v attachView) string {
	switch {
	case v.st.Status == "":
		// No run.started at all, so this is not a run that stopped -- it is a
		// directory nothing has run in. Printing the empty status as though it
		// were a state would be noise; naming `run start` is the whole answer.
		return "  its log has no run.started, so nothing has run in it yet.\n" +
			"  start one:  arxi run start <blueprint> <prompt>\n"

	case v.st.Status == kernel.StatusPaused:
		return fmt.Sprintf("  it is paused at seq %d.\n  pick it up:  arxi run unpause %s\n",
			v.st.Seq, v.arg)

	case v.st.Status == kernel.StatusBlocked && pendingAsks(v.st) > 0:
		return fmt.Sprintf("  it is blocked on %d question(s) it cannot continue without.\n"+
			"  answer them:  arxi inbox\n", pendingAsks(v.st))

	case v.st.Status == kernel.StatusBlocked:
		return fmt.Sprintf("  it is blocked at seq %d, with nothing in its inbox.\n"+
			"  what is holding it:  arxi run why %s\n", v.st.Seq, v.arg)
	}

	// Running with no writer is the case worth saying out loud rather than
	// diagnosing generically: the log claims a live run and the lock says
	// otherwise, so whatever was driving it is gone without recording an end.
	//
	// `run unpause` is deliberately NOT suggested here. It refuses a run that is
	// already running (unpause.go: "there is nothing to resume") and sends the
	// reader to `run why` itself, so naming it would cost the user one command to
	// arrive at the same place. Sending somebody to a verb that refuses them is
	// how a remedy becomes a second problem.
	return fmt.Sprintf("  the log says %s at seq %d, but no process holds its writer lock, "+
		"so whatever was driving it is gone.\n  what it was waiting for:  arxi run why %s\n",
		v.st.Status, v.st.Seq, v.arg)
}

// emitAttachEvent prints one event row, and rows are the only thing on stdout.
//
// The human form is §20.1's: `[r1 seq 13] tool.call read src/auth.go`. The run id
// is repeated on every row rather than stated once in the header, because a
// follower's output is the output most likely to end up interleaved with another
// run's -- two terminals, or two attaches into one file -- and a row that does not
// name its run cannot be attributed afterwards.
//
// --json writes ONE event per line, not a document: see the file header. It uses
// json.Marshal rather than this binary's emitJSON helper because that one indents,
// and an event spread over eight lines is not NDJSON no matter how valid it is.
func emitAttachEvent(v attachView, e kernel.Event, asJSON bool) {
	if asJSON {
		b, err := json.Marshal(e)
		if err != nil {
			// Cannot happen for an event that just came out of json.Unmarshal, and
			// reported rather than dropped anyway: a row that disappeared quietly
			// would make the stream lie by omission about what the run did. On
			// stderr, so the NDJSON on stdout stays parseable.
			fmt.Fprintf(os.Stderr, "arxi run attach: re-encode seq %d of %s: %v\n", e.Seq, v.id, err)
			return
		}
		fmt.Println(string(b))
		return
	}

	if d := attachEventDetail(e); d != "" {
		fmt.Printf("[%s seq %d] %s %s\n", v.id, e.Seq, e.Type, d)
		return
	}
	fmt.Printf("[%s seq %d] %s\n", v.id, e.Seq, e.Type)
}

// attachEventDetail is the rest of the row, after `[r1 seq 13] tool.call`.
//
// §20.1's rows are summaries -- `tool.call read src/auth.go`, not the whole
// payload -- and this renderer is allowed to be one. That is the difference from
// replayEventLine, which holds itself to `event log`'s "no key is dropped" rule:
// there, the line printed IS the output. Here it is a view of a log that still
// exists, `event log` prints every key of it, and --json on this very command
// prints the whole event. A stream somebody watches fill up earns being narrow
// enough to read at a glance.
//
// Only the types §20.1 shows get a case. Thirty hand-written renderers would be
// thirty things to keep true, and a type without a case loses nothing: it falls
// through to the same payload cell `event log` prints in its own payload column.
func attachEventDetail(e kernel.Event) string {
	switch e.Type {
	case kernel.ToolCall:
		return attachJoin(e.Str("tool"), e.Str("args"))

	case kernel.ToolCallCompleted:
		// A failed call must not render as a bare tool name, because that is
		// exactly what a successful one renders as. Both spellings are read
		// because the two executors in the tree disagree, and this is not the
		// command that gets to settle it: the catalogue and the LIVE executor say
		// `result` (spec/events.md:108, internal/provider/executor.go:352), and the
		// simulated one writes `ok` with `output` or `error`
		// (internal/exec/fake.go:387). Reading one spelling would render half the
		// logs in this tree as a bare tool name -- and the half it broke would be
		// whichever executor the reader was not using.
		if ok, present := e.Payload["ok"].(bool); present && !ok {
			return attachJoin(e.Str("tool"), "failed:", e.Str("error"))
		}
		return attachJoin(e.Str("tool"), e.Str("result"), e.Str("output"))

	case kernel.ToolCallDenied:
		// A denial with `policy: ask` is not an error, it is a question
		// (spec/events.md), so the policy is named instead of being reported as a
		// refusal. `arxi inbox` is where the question is waiting, and the footer
		// says so if the run stops here.
		return attachJoin(e.Str("tool"), "policy", e.Str("policy"))

	case kernel.LLMResponse:
		// The doc's own row, in trimUSD's spelling of money rather than its literal
		// `0.0900`: `run show` already writes dollars that way, and one binary
		// should not have two ways of writing a cost.
		d := trimUSD(e.Num("cost_usd")) + " USD"
		if ok, present := e.Payload["ok"].(bool); present && !ok {
			// A refused turn costs nothing, so it renders as `0 USD` -- which reads
			// as a cheap success. What happened is a provider that said no, and the
			// row has to say which.
			why := e.Str("error")
			if why == "" {
				why = "no reason recorded"
			}
			d += " · refused: " + why
		}
		return attachJoin(e.Str("agent"), d)
	}

	if len(e.Payload) == 0 {
		// Guarded because formatPayloadCell renders an empty map as "-": right for
		// a table with a column to fill, wrong at the end of a row, where it reads
		// as something the renderer failed to print.
		return ""
	}
	// The elision bool is dropped. `event log` accounts for its … in a footer
	// because it can promise a complete table; a live stream cannot promise
	// anything, and the answer for a value that got cut is --json, on this same
	// command.
	cell, _ := formatPayloadCell(e.Payload)
	return cell
}

// attachJoin puts one space between the parts that are actually there.
//
// Spelled out because every case above has at least one optional half, and
// fmt.Sprintf("%s %s", a, "") leaves a trailing space: invisible in review, and
// the kind of thing a golden test pins down months later as a diff nobody can see.
func attachJoin(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// decodeAttachLine turns one followed line into an event, or explains and stops.
//
// The bool is "keep following", and false makes the caller return 1. A line that
// does not parse is NOT skipped, for the reason decodeRunEvents gives about the
// same decision: the log is the run's only history, and a follower that stepped
// over a line would go on printing a state that no longer follows from what the
// user already read.
//
// The one thing it cannot be is a torn write. wholeLines only ever hands over
// bytes that ended in a newline, and the batch those bytes belong to was committed
// before they were read -- so an unparseable line here is a real corrupt line, not
// a half-written one, and saying so is not a guess.
//
// decodeRunEvents is not called even though the wording is deliberately close,
// because it numbers lines within the slice it was handed. Here that number would
// be a position inside one 32 KiB read: a number the user cannot find anything by.
// The run and `event log` are what the message names instead.
func decodeAttachLine(v attachView, line []byte) (kernel.Event, bool) {
	var e kernel.Event
	if err := json.Unmarshal(line, &e); err != nil {
		fmt.Fprintf(os.Stderr, "\narxi run attach: a line appended to %s does not parse, "+
			"so following stops here: %v\n"+
			"  the log is the run's only history, so this is not skipped\n"+
			"  read it directly:  arxi event log %s\n", v.dir, err, v.arg)
		return kernel.Event{}, false
	}
	return e, true
}

// wholeLines splits the complete lines out of a buffer and returns the remainder.
//
// The remainder is the whole point. A read of a file somebody else is appending to
// lands wherever that writer happened to be, so the last line of a read is often
// half an event -- and a follower that printed it would print half a row now and
// the other half a poll later, as two events, one of them nonsense. Everything
// after the final newline is held and re-examined with the next read.
//
// The returned lines ALIAS buf. That is safe here and worth stating, because it is
// what constrains the caller: the loop decodes the lines before it reads more
// bytes into the same buffer, and json.Unmarshal copies what it keeps. Reversing
// that order would corrupt a row already handed over.
//
// Blank lines are dropped rather than returned, matching decodeRunEvents: a blank
// line is not an event, and asking json.Unmarshal about one produces an alarming
// error about a log that is perfectly fine.
func wholeLines(buf []byte) ([][]byte, []byte) {
	var lines [][]byte
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return lines, buf
		}
		line := buf[:i]
		buf = buf[i+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
}

// writerLockOwner names the process holding a run's writer lock, if one does.
//
// The bool is what the follow turns on -- a run nobody is writing to has nothing
// to wait for -- and the string is who the user is waiting on, so attaching to a
// live run does not look the same as attaching to a dead one.
//
// The whole trimmed body is the owner and "an unknown process" is the default for
// an empty one, which is exactly what logstore.acquireLock reports when it refuses
// a second writer. Reading only the pid out of `pid 5871` would be a second
// spelling of one fact, and the day the file gains a line the two would disagree
// about what a lock says while both sounding certain.
//
// Any read error answers "not held", and that is the safe direction: the follower
// then stops and prints a diagnosis, where taking an unreadable lock for a held
// one would poll forever on a run it can never learn anything about.
func writerLockOwner(dir string) (string, bool) {
	body, err := os.ReadFile(logstore.LockPath(dir))
	if err != nil {
		return "", false
	}
	if owner := strings.TrimSpace(string(body)); owner != "" {
		return owner, true
	}
	return "an unknown process", true
}
