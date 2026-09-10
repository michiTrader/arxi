package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/surface"
)

// cmdRunUnpause implements `arxi run unpause <run> [--budget N]`.
//
// # This is the command the rest of the tool has been recommending
//
// `run why` prints "arxi run unpause <run>" for a paused run and
// "arxi run unpause <run> --budget <higher>" for an exhausted budget;
// spec/events.md lists the second as THE remedy for a budget block; §20.6
// builds its whole narrative on it. Until now the binary answered "declared but
// not implemented" to both. A remedy a document names and the binary refuses is
// the worst kind of gap, because it is found by the person already in trouble.
//
// # Resuming is appending, and then driving
//
// The command does two things, and the split matters. First it appends
// run.unpaused -- that is the whole of the state change, and the reducer decides
// what it means (status back to running, parked causes handed back, a raised
// ceiling honoured). Second it drives the loop, because an event in a log moves
// nothing on its own: something has to fold it and carry out the effects.
//
// `arxi inbox` deliberately does only the first half and says so ("the run
// resumes when it is next driven"), because answering a question is not the same
// act as paying for the turns the answer unblocks. Unpause is the other half,
// and it is where "resume this run" becomes a thing a person can actually do.
//
// # THE CURSOR PROBLEM, and its durable answer
//
// A continuation must know which source event's effects were committed. The
// event-log tip cannot answer that: progress records and domain outcomes may sit
// above an unfinished source event. Restarting from zero is worse because it can
// repeat paid work. Modern runs therefore commit exec.step_completed and derive
// the cursor from the greatest validated source-event frontier. A run without
// those records remains inspectable but is refused before any continuation event
// is appended; guessing would turn recovery into either omission or duplicate
// external work.
func cmdRunUnpause(args []string) {
	c := surface.Lookup("run", "unpause")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run unpause: %v\n\n"+
			"usage: arxi run unpause <run> [--budget <usd>]\n", err)
		os.Exit(2)
	}

	runArg := vals["run"]
	if runArg == "" {
		fmt.Fprintf(os.Stderr, "arxi run unpause: which run?\n"+
			"  usage: arxi run unpause <run> [--budget <usd>]\n"+
			"  see what is waiting: arxi inbox\n")
		os.Exit(2)
	}

	// --budget is parsed BEFORE the run is opened, so a typo in the number is
	// reported without having touched the log. A resume is a write; refusing it
	// after appending would leave a run resumed against a ceiling the user
	// never got to see rejected.
	budget := 0.0
	if raw, ok := vals["budget"]; ok && raw != "" {
		budget, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi run unpause: --budget %q is not a "+
				"number of dollars.\n  it is the new ceiling for the whole tree, "+
				"e.g. --budget 20\n", raw)
			os.Exit(2)
		}
		if budget <= 0 {
			fmt.Fprintf(os.Stderr, "arxi run unpause: --budget %v is not a "+
				"ceiling a run can spend under.\n"+
				"  to stop a run, say so: arxi run cancel %s --reason \"...\"\n",
				budget, runArg)
			os.Exit(2)
		}
	}

	dir := resolveRunDir(runArg)

	// The state is read BEFORE the append, because what to say about this
	// resume depends on what it is resuming from -- and after the append the run
	// is already running, so the question cannot be asked any more.
	pre, _, simulated, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run unpause: %v\n", err)
		os.Exit(1)
	}
	// Resume validates the immutable execution contract before acquiring the writer
	// lock or appending run.unpaused. Legacy inspection remains available through
	// foldRunDir, but an execution that cannot be reconstructed is never guessed.
	effective, _, err := loadEffectiveForResume(dir, pre.RunID, events)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run unpause: %v\n", err)
		os.Exit(1)
	}
	if _, err := recoverExecution(events); err != nil {
		fmt.Fprintf(os.Stderr, "arxi run unpause: cannot resume safely: %v\n", err)
		os.Exit(1)
	}
	simulated = effective.Mode == "sim"

	// A run that is already running is refused rather than resumed. Appending a
	// second run.unpaused would be harmless to the reducer and dishonest in the
	// log: it would record a resume that resumed nothing, and `event trace`
	// would show a human intervening at a moment when nothing was wrong.
	//
	// The exception is a raise. Somebody who says --budget on a running run is
	// asking for headroom, and that is a request the reducer honours; refusing
	// it would send them to pause the run first, for no reason.
	switch {
	case pre.Status.Terminal():
		fmt.Fprintf(os.Stderr, "arxi run unpause: run %s is %s, which is final.\n"+
			"  a finished run has no work to hand back. to run it again: "+
			"arxi run start <blueprint> <prompt>\n", pre.RunID, pre.Status)
		os.Exit(1)
	case pre.Status == kernel.StatusRunning && budget == 0:
		fmt.Fprintf(os.Stderr, "arxi run unpause: run %s is already running, "+
			"so there is nothing to resume.\n"+
			"  if it is not making progress, that is a different question: "+
			"arxi run why %s\n", pre.RunID, pre.RunID)
		os.Exit(1)
	}

	// A ceiling at or below the current one is refused HERE rather than in the
	// reducer, and this is the division of labour the project already uses: the
	// reducer ignores a value it will not honour, because it has nobody to talk
	// to, and the CLI is where a person is told why. Silently accepting it is
	// the one option ruled out -- the user would resume expecting headroom and
	// get the same block back.
	if budget > 0 && budget <= pre.BudgetUSD {
		fmt.Fprintf(os.Stderr, "arxi run unpause: --budget %s is not above the "+
			"%s this run already has.\n"+
			"  unpause raises a ceiling, it does not lower one: the run has spent "+
			"%s, and a ceiling under that would block again on the next turn.\n"+
			"  to stop paying for it: arxi run cancel %s --reason \"...\"\n",
			usd(budget), usd(pre.BudgetUSD), usd(pre.TreeSpentUSD), pre.RunID)
		os.Exit(2)
	}

	// A budget block that is resumed WITHOUT a raise is allowed and warned
	// about, because it is occasionally what somebody means (they cancelled some
	// other run, or they are resuming to reach a terminal state). It is warned
	// about because the far more common case is somebody who read the remedy and
	// dropped the flag, and a run that blocks again three seconds later with no
	// explanation looks like the command failed.
	//
	// The condition asks about the CEILING and not about Status, and that is a
	// fix that `run pause` exposed. This used to require
	// `pre.Status == kernel.StatusBlocked`, and a pause overwrites Status: pause
	// a budget-blocked run, unpause it without --budget, and the warning was
	// skipped entirely on the one run where the user had the least context about
	// why it stopped. budgetIsExhausted reads TreeSpentUSD against BudgetUSD and
	// is true whatever the status says, which is the fact the warning is about.
	if budgetIsExhausted(pre) && budget == 0 {
		fmt.Printf("warning: run %s: the budget ran out "+
			"(%s of %s USD in the tree), and no new ceiling was given.\n"+
			"  it will resume and block again on the next cost.\n"+
			"  raise it in the same command: arxi run unpause %s --budget <higher>\n",
			pre.RunID, usd(pre.TreeSpentUSD), usd(pre.BudgetUSD), pre.RunID)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	// Both, and they are not redundant. The defer covers ordinary returns;
	// atExit covers os.Exit, which does not run defers -- including append and
	// stopped-early failures below. Configuration has already been validated by
	// preflightEffectiveRun, before this lock was acquired.
	defer store.Close()
	atExit(func() { store.Close() })

	payload := map[string]any{}
	if budget > 0 {
		// The field is named budget_usd because that is what run.started calls
		// it and what the reducer reads. spec/events.md declared run.unpaused
		// with no payload at all; this is that gap being closed in the schema
		// as well, not a private convention.
		payload["budget_usd"] = budget
	}

	ev := kernel.Event{
		ID:   "unpause-" + strconv.FormatInt(store.Head()+1, 10),
		Type: kernel.RunUnpaused,
		// SourceHuman, and load-bearing for the same reason it is on an inbox
		// reply: raising a ceiling is an authorisation, and an audit that cannot
		// say whether a human or the runtime raised a budget cannot answer the
		// only question anybody asks about a bill.
		Source: kernel.SourceHuman,
		Scope:  "run:" + pre.RunID,
		// Ts is stamped here because nothing else will. This append does not go
		// through the effect runner -- a human typed a command -- and the same
		// omission was measured on a real log for inbox replies, which landed
		// with "ts":"".
		Ts:      nowFunc().UTC().Format(time.RFC3339),
		Payload: payload,
	}

	written, err := store.Append([]kernel.Event{ev})
	if err != nil {
		fatal(fmt.Errorf("record run.unpaused: %w", err))
	}
	at := written[0].Seq

	if budget > 0 {
		fmt.Printf("run %s resumed (seq %d), ceiling raised %s -> %s USD\n",
			pre.RunID, at, usd(pre.BudgetUSD), usd(budget))
	} else {
		fmt.Printf("run %s resumed (seq %d)\n", pre.RunID, at)
	}

	// Driving is the second half, and a simulated run is driven too -- with the
	// fake executor, which is what --sim means. This used to return here and
	// print "not driven", which protected the user's money and cost them the
	// run: nothing else drives, so a rehearsal could never be resumed.
	//
	// The flag comes from the LOG and not from a --sim on this command, because
	// the run already answered this question when it started. Asking again would
	// let the two answers differ, and the direction that costs money is the one
	// a user would hit by simply forgetting the flag.
	if simulated {
		fmt.Printf("  this run was started with --sim, so it is continued with " +
			"the same fake executor: no model is called and no money is spent.\n")
	}

	driveEffectiveRun(dir, effective, store, pre.RunID)
}

// preflightEffectiveRun folds the confirmed log and verifies the immutable
// execution contract before any caller takes the writer lock or appends.
// driveEffectiveRun receives the already-loaded artifact, so no mutable provider
// or policy store is consulted after the run begins.
func preflightEffectiveRun(dir string) (kernel.State, runconfig.Artifact, []kernel.Event, error) {
	pre, _, _, events, err := foldRunDirEvents(dir)
	if err != nil {
		return kernel.State{}, runconfig.Artifact{}, nil, err
	}
	effective, _, err := loadEffectiveForResume(dir, pre.RunID, events)
	if err != nil {
		return kernel.State{}, runconfig.Artifact{}, nil, err
	}
	if _, err := recoverExecution(events); err != nil {
		return kernel.State{}, runconfig.Artifact{}, nil, fmt.Errorf("cannot resume safely: %w", err)
	}
	return pre, effective, events, nil
}

func recoverExecution(events []kernel.Event) (exec.Recovery, error) {
	recovery, err := exec.Recover(events)
	if err != nil {
		return recovery, err
	}
	if !recovery.HasProgress {
		return recovery, fmt.Errorf("this run has no durable exec.step_completed records; inspect it with run show or replay it, but starting external work from an inferred cursor could duplicate paid effects")
	}
	if len(recovery.Unknown) > 0 {
		return recovery, fmt.Errorf("%d external work item(s) have unknown outcomes (%s); automatic redispatch is disabled", len(recovery.Unknown), recovery.Unknown[0])
	}
	return recovery, nil
}

func driveEffectiveRun(dir string, effective runconfig.Artifact, store *logstore.Store, runID string) {
	cfg := effective.Config
	simulated := effective.Mode == "sim"
	var (
		clock    exec.Clock
		timekeep exec.Timekeeper
		executor exec.Executor
		now      func() string
	)

	events, err := store.Read(1, 0)
	if err != nil {
		fatal(fmt.Errorf("read durable execution progress: %w", err))
	}
	recovery, err := recoverExecution(events)
	if err != nil {
		fatal(fmt.Errorf("cannot resume safely: %w", err))
	}
	timers, err := exec.RecoverTimers(events)
	if err != nil {
		fatal(fmt.Errorf("cannot restore durable timers: %w", err))
	}

	if simulated {
		// Built exactly as cmdRunStart builds it for --sim, and with the same
		// clock: VirtualTime JUMPS to the next deadline instead of waiting for
		// it, so a stage timeout that a real run would sit out for thirty
		// minutes is exercised here in microseconds.
		//
		// Now reads the VIRTUAL clock and not the wall clock. Stamping a
		// simulated continuation with real timestamps would make the log jump
		// from simulated time to now and back, and `event trace` reads those
		// timestamps.
		vc := exec.NewVirtualClock()
		if err := vc.Restore(timers.NowMs, timers.Pending); err != nil {
			fatal(fmt.Errorf("restore simulated timers: %w", err))
		}
		clock, timekeep, executor = vc, exec.VirtualTime{C: vc}, exec.NewFake()
		now = func() string {
			return time.UnixMilli(vc.NowMs()).UTC().Format(time.RFC3339Nano)
		}
	} else {
		rc := exec.NewRealClock()
		if err := rc.Restore(timers.Pending); err != nil {
			fatal(fmt.Errorf("restore live timers: %w", err))
		}
		clock, timekeep = rc, exec.RealTime{C: rc}
		executor = runtimeExecutor(dir, effective)
		now = func() string { return nowFunc().UTC().Format(time.RFC3339Nano) }
	}

	runner := &exec.Runner{
		Log:      store,
		Clock:    clock,
		Executor: executor,
		Config:   cfg,
		RunID:    runID,
		Now:      now,
	}

	loop := &exec.Loop{
		Runner: runner,
		Log:    store,
		Time:   timekeep,
		Config: cfg,
		Cursor: recovery.Cursor,
	}

	out, err := loop.Run(context.Background())
	printRunSummary(runID, dir, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\narxi: the resumed run stopped early: %v\n", err)
		// Closed explicitly, because os.Exit skips the deferred Close and would
		// leave the writer lock behind. See the same fix in cmdRunStart: a
		// resume that failed is precisely the run somebody will try to resume
		// again, and a stale lock would refuse them with advice to delete a
		// lock file by hand.
		store.Close()
		os.Exit(1)
	}
}

// resolveRunDir turns a run id into a directory.
//
// A path is accepted as well as an id, because --dir exists on `run start` and a
// run that was started somewhere else cannot be resumed by id. Deciding by "does
// this hold an event log" rather than by "does it look like a path" means the
// answer does not depend on whether the user typed a slash.
func resolveRunDir(arg string) string {
	if _, err := os.Stat(filepath.Join(arg, "events.ndjson")); err == nil {
		return arg
	}
	return filepath.Join(runsDir, arg)
}

// foldRunDir reads a run's log and its frozen blueprint.
//
// It folds rather than reading the snapshot, per ADR-0002: the log is the truth
// and state.snapshot.json is a cache nothing reads yet. It also does NOT take
// the writer lock, for the reason internal/inbox documents at length -- the run
// being resumed may still be held by another process, and failing with a lock
// error would say nothing about the run.
//
// The third return is whether the run was simulated; runWasSimulated says where
// that comes from and why it is not on the State.
func foldRunDir(dir string) (kernel.State, kernel.Config, bool, error) {
	st, cfg, sim, _, err := foldRunDirEvents(dir)
	return st, cfg, sim, err
}

// foldRunDirEvents is foldRunDir, also handing back the events it folded.
//
// It exists because `run result` needs two things the State does not keep: which
// stage.submitted came last (that is what result_from: last_submit points at) and
// the reason on run.cancelled, which the reducer reads no key from at all -- it
// sets Status and drops the payload (internal/kernel/decide.go:59).
//
// Returning the events instead of letting the caller read the log a second time
// is the whole point. Two reads of a file that is being appended to right now can
// legitimately disagree: the fold would report seq 18 and the rescan seq 20, and
// the command would print one run's status beside another moment's submission.
// One read, one decode, one fold means every figure on screen comes from the same
// bytes.
//
// foldRunDir stays as the three-value wrapper so the callers that only want the
// state -- `run show`, `run tree`, `run unpause` -- are not touched, and so there
// remains exactly one place that knows how a run directory is read.
//
// One caller does not go through here: `run attach` needs the byte offset the log
// was read to, so it composes the same three steps -- readRunLog, runFrozenConfig,
// decodeRunEvents -- itself. They were split out of this function for that, which
// is why the wording of each failure still has one home.
func foldRunDirEvents(dir string) (kernel.State, kernel.Config, bool, []kernel.Event, error) {
	raw, err := readRunLog(dir)
	if err != nil {
		return kernel.State{}, kernel.Config{}, false, nil, err
	}

	cfg, err := runFrozenConfig(dir)
	if err != nil {
		return kernel.State{}, kernel.Config{}, false, nil, err
	}

	events, err := decodeRunEvents(dir, raw)
	if err != nil {
		return kernel.State{}, kernel.Config{}, false, nil, err
	}

	st, _ := kernel.Fold(kernel.State{}, events, cfg)
	return st, cfg, runWasSimulated(events), events, nil
}

// readRunLog reads a run's events.ndjson, or says why it could not.
//
// Split out of foldRunDirEvents for `run attach`, which needs the same bytes AND
// the offset it stopped at -- a follower that folds through one read and then
// reopens to find its join point would miss whatever was appended in between.
// Sharing the read means the two commands cannot disagree about what a run
// directory is, and in particular that "you passed something that is not a run"
// is phrased once. That sentence names ./runs/<id> and `arxi inbox`, and a second
// copy of it in attach.go would be the copy that keeps saying --dir after the
// flag is renamed.
func readRunLog(dir string) ([]byte, error) {
	read, err := logstore.ReadConfirmed(dir, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"%s holds no event log, so it is not a run directory.\n"+
					"  runs live under ./%s/<run-id> unless --dir said otherwise\n"+
					"  see what is waiting: arxi inbox", dir, runsDir)
		}
		return nil, fmt.Errorf("read the confirmed log of %s: %w", dir, err)
	}
	return read.Bytes, nil
}

// runFrozenConfig loads the blueprint snapshot a run was started with.
//
// A missing snapshot is not fatal for inspection, matching internal/inbox: the
// events still fold. Execution preflight is stricter and rejects a missing or
// mismatched snapshot because it cannot reproduce the original reducer config.
func runFrozenConfig(dir string) (kernel.Config, error) {
	snap, err := os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml"))
	switch {
	case err == nil:
		bp, berr := blueprint.Load(snap)
		if berr != nil {
			return kernel.Config{}, fmt.Errorf(
				"the frozen blueprint of %s does not parse, so this run cannot be "+
					"folded: %w", dir, berr)
		}
		return bp.Config, nil
	case !os.IsNotExist(err):
		return kernel.Config{}, fmt.Errorf(
			"read the frozen blueprint of %s: %w", dir, err)
	}
	return kernel.Config{}, nil
}

// runWasSimulated reads --sim off run.started.
//
// It is read off the event and not off the State, because kernel.State does not
// carry it: the reducer has no use for the distinction (that is exactly what
// makes --sim worth trusting), so the only place it exists is the event. Measured
// before writing this, rather than assumed -- the field was almost given a State
// that has no such field.
func runWasSimulated(events []kernel.Event) bool {
	for _, e := range events {
		if e.Type == kernel.RunStarted {
			b, _ := e.Payload["simulated"].(bool)
			return b
		}
	}
	return false
}

// decodeRunEvents parses the NDJSON log.
//
// A truncated final line is SKIPPED rather than treated as corruption, which is
// the same judgement internal/inbox.decodeEvents makes and for the same reason:
// a log being appended to right now legitimately ends mid-line, so refusing
// there would make this command fail exactly while a run is active. Any other
// unparseable line is fatal, because the log is the run's only history.
func decodeRunEvents(dir string, raw []byte) ([]kernel.Event, error) {
	var out []kernel.Event
	lineNo := 0
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := raw[:i]
		raw = raw[i+1:]
		lineNo++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e kernel.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("%s line %d of the log does not parse: %w\n"+
				"  the log is the run's only history, so this is not skipped",
				dir, lineNo, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// budgetIsExhausted reports whether the block is a budget block.
//
// It reads the spend against the ceiling rather than looking for a "budget"
// inbox item, because the question asked is about the money and not about
// whether anybody was asked about it. A run blocked for a tool approval has
// spend under its ceiling and must not be warned about a budget it has not hit.
func budgetIsExhausted(s kernel.State) bool {
	return s.BudgetUSD > 0 && s.TreeSpentUSD >= s.BudgetUSD
}
