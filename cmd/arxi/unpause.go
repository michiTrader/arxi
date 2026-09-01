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
	"github.com/michiTrader/arxi/internal/provider"
	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/toolrun"
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
// # THE CURSOR PROBLEM, and why the log tip is the honest answer
//
// exec.Loop needs a Cursor: the seq whose effects were already carried out. Its
// doc is explicit that both ways of guessing are wrong -- Head() skips the
// effects of anything a previous pass had not reached, and zero re-spawns every
// turn the run already paid for. `run start` knows the cursor because it just
// built it, prints it as "resume from here", and nothing persists it anywhere.
//
// A resume in a new process therefore cannot know it. This command uses the log
// tip, which is Head(), and that is a deliberate choice of which failure to
// take:
//
//   - Head() risks stranding the effects of events a previous pass folded but
//     did not execute. Those runs are the ones that crashed mid-fold.
//   - Zero risks re-spawning every turn in the log. On a run that has done real
//     work, that is the whole bill a second time.
//
// The first costs a run that may be stuck anyway; the second costs money on
// every healthy run. So the tip wins, and the newly appended run.unpaused sits
// ABOVE it -- which is what makes the resume work at all: that event is unread
// by construction, the loop reads it first, and the drain it triggers is what
// hands back the parked work.
//
// This is a real limitation and not a hidden one: `run unpause` reports what it
// resumed from, so a resume that produced nothing can be recognised as such
// rather than blamed on the blueprint.
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
	pre, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run unpause: %v\n", err)
		os.Exit(1)
	}

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
	// Both, and they are not redundant. The defer covers the ordinary returns;
	// atExit covers os.Exit, which does not run defers -- and there are four
	// such paths under this lock (two fatals in driveResumedRun's wiring, the
	// append error, and the stopped-early branch). Measured: a run resumed with
	// an unparseable policy file left writer.lock holding a dead pid, and the
	// next command on that run refused with advice to delete a file by hand.
	//
	// Close is idempotent, so whichever fires first is the one that matters.
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

	driveResumedRun(dir, cfg, store, pre.RunID, simulated)
}

// driveResumedRun folds the log forward and carries out what the reducer decides.
//
// The executor is built exactly as `run start` builds it, including the tool
// policy overrides, and that is the point: a resumed run must be judged by the
// same rules as a fresh one. Duplicating the wiring is a real risk -- if the two
// drift, a resumed run and a new run behave differently on the same blueprint,
// which is the class of bug nobody thinks to look for.
//
// # simulated is honoured here rather than by refusing to drive
//
// This used to build a live executor unconditionally, and every caller therefore
// had to check the flag itself and print "this run was started with --sim, so it
// is not driven here" instead of driving. That was correct about the danger --
// resuming a rehearsal with a live executor charges real money for a run that
// was explicitly not real -- and wrong about the remedy, because it left a
// simulated run with NO way to be driven at all after `run start` returned.
//
// Measured, and the reason this moved: `run prompt` on a quiescent simulated run
// appended the cause, printed success, and changed nothing; its own closing line
// then sent the user to `run unpause`, which refuses a running run outright. A
// rehearsal that cannot be advanced is not a rehearsal of anything.
//
// The fake executor and the virtual clock are what --sim means, and they are
// already what `run start` uses. Driving with them is the honest continuation:
// no provider is called, no money is spent, and the loop, the reducer and the
// log are the same ones a real run would use -- which is the property that makes
// --sim worth trusting in the first place.
func driveResumedRun(dir string, cfg kernel.Config, store *logstore.Store, runID string, simulated bool) {
	var (
		clock    exec.Clock
		timekeep exec.Timekeeper
		executor exec.Executor
		now      func() string
	)

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
		clock, timekeep, executor = vc, exec.VirtualTime{C: vc}, exec.NewFake()
		now = func() string {
			return time.UnixMilli(vc.NowMs()).UTC().Format(time.RFC3339Nano)
		}
	} else {
		overrides, err := openPolicies().LoadAll()
		if err != nil {
			fatal(err)
		}

		rc := exec.NewRealClock()
		clock, timekeep = rc, exec.RealTime{C: rc}
		executor = &provider.Executor{
			Resolver: providerResolver{openProviders()},
			Members:  cfg.Members,
			// No Prompt. The run's opening instruction is already in its log,
			// and inventing one here would inject a second cause into a run
			// that asked only to continue.
			ToolPolicy: overrides,
			Tools: &toolrun.Runner{
				Root:   filepath.Join(dir, "workspace"),
				Shared: cfg.Workspace == "shared",
			},
		}
		now = func() string { return nowFunc().UTC().Format(time.RFC3339Nano) }
	}

	runner := &exec.Runner{
		Log:      store,
		Clock:    clock,
		Executor: executor,
		Config:   cfg,
		Now:      now,
	}

	// Cursor is the log tip MINUS the event just appended, so run.unpaused is
	// read and decided rather than absorbed into the starting state. Absorbing
	// it would set the status to running and never run drainParked, so the
	// resume would hand back nothing -- exactly the silent failure drainParked's
	// own doc describes.
	//
	// See this file's header for why the tip is the honest cursor for a resume in
	// a fresh process.
	cursor := store.Head() - 1
	if cursor < 0 {
		cursor = 0
	}

	loop := &exec.Loop{
		Runner: runner,
		Log:    store,
		Time:   timekeep,
		Config: cfg,
		Cursor: cursor,
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
	raw, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"%s holds no event log, so it is not a run directory.\n"+
					"  runs live under ./%s/<run-id> unless --dir said otherwise\n"+
					"  see what is waiting: arxi inbox", dir, runsDir)
		}
		return nil, fmt.Errorf("read the log of %s: %w", dir, err)
	}
	return raw, nil
}

// runFrozenConfig loads the blueprint snapshot a run was started with.
//
// A missing snapshot is not fatal, matching internal/inbox: the events still
// fold. What is lost is the roster, so the reducer cannot resume a member it
// cannot find -- and driveResumedRun would then spawn nothing. Refusing to
// invent a Config is the honest half of that.
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
