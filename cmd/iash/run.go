package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/iash/internal/blueprint"
	"github.com/michiTrader/iash/internal/exec"
	"github.com/michiTrader/iash/internal/kernel"
	"github.com/michiTrader/iash/internal/logstore"
)

// startFlags is what `run start` was invoked with, after parsing but before any
// of it is trusted.
type startFlags struct {
	actor     string
	prompt    string
	budget    float64
	budgetSet bool
	maxTurns  int
	workspace string
	sim       bool
	runID     string
	dir       string
}

// cmdRunStart implements `iash run start <actor> <prompt> --budget N`.
//
// This is the command that joins the four packages: blueprint loads the actor,
// logstore holds the events, kernel decides, exec executes. It contains no
// decisions of its own beyond wiring, and that is the point — every question of
// what should happen next is answered by kernel.Decide, so `run`, `--sim` and
// `replay` cannot drift apart.
//
// Only `--sim` runs today. There is no LLM-backed Executor in the tree yet, so
// the real path is refused explicitly rather than approximated: a `run start`
// that silently simulated would print a plausible run, a plausible cost and a
// plausible result for work nobody did, and the user would have no way to tell.
// Saying so costs a line of output and is the only honest option.
func cmdRunStart(args []string) {
	f, err := parseStartFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash run start: %v\n\n"+
			"usage: iash run start <actor> <prompt> --budget <usd> [--max-turns N]\n"+
			"                      [--workspace shared|worktree|copy|none] [--sim]\n", err)
		os.Exit(2)
	}

	bp, err := blueprint.LoadFile(f.actor)
	if err != nil {
		// Exit 1, not 2: the invocation was correct, the blueprint is what is
		// wrong. CI needs to tell "you called this wrong" from "your file is bad".
		fmt.Fprintf(os.Stderr, "blueprint is not valid.\n\n%v\n", err)
		os.Exit(1)
	}

	cfg := bp.Config
	if f.workspace != "" && f.workspace != "auto" {
		cfg.Workspace = f.workspace
	}

	if !f.sim {
		// Refused, not faked. See the doc comment above.
		fmt.Fprintf(os.Stderr,
			"iash run start has no live executor yet: there is no LLM-backed "+
				"Executor in this build, so a real run would spend nothing and "+
				"produce nothing while looking exactly like one that worked.\n\n"+
				"  what works today: iash run start %s %q --budget %.2f --sim\n\n"+
				"--sim drives the SAME reducer, the same log and the same loop as a "+
				"real run; only the executor is fake. So the run you get is the run "+
				"you would get, minus the model calls and the bill.\n",
			f.actor, f.prompt, f.budget)
		os.Exit(2)
	}

	dir := f.dir
	if dir == "" {
		dir = filepath.Join("runs", f.runID)
	}

	// The blueprint is FROZEN into the run directory before the first event is
	// written, and the run records its sha.
	//
	// Without this, editing the blueprint after a run has started makes the log
	// unreplayable: kernel.Fold needs the Config the events were decided
	// against, and a mutated file yields a different one. The run would then
	// explain itself with rules that were never applied, which is worse than
	// refusing to explain itself at all.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal(fmt.Errorf("create the run directory: %w", err))
	}
	snapPath := filepath.Join(dir, "blueprint.snapshot.yaml")
	if err := os.WriteFile(snapPath, bp.Raw, 0o644); err != nil {
		fatal(fmt.Errorf("freeze the blueprint: %w", err))
	}

	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	defer store.Close()

	// The virtual clock is what makes --sim finish in milliseconds instead of
	// waiting out a 30-minute stage timeout. It is also why Now is derived from
	// it rather than from the wall: two simulations of identical input must
	// produce identical logs, or `run diff` has nothing to compare.
	clock := exec.NewVirtualClock()
	fake := exec.NewFake()
	runner := &exec.Runner{
		Log:      store,
		Clock:    clock,
		Executor: fake,
		Config:   cfg,
		Now: func() string {
			return time.UnixMilli(clock.NowMs()).UTC().Format(time.RFC3339Nano)
		},
	}

	// run.started is APPENDED, not handed to the loop. The loop reads it back out
	// of the log and decides it like any other event, which is what makes a fresh
	// run and a resumed run the same code path: neither is given a starting
	// state, both fold one out of the events.
	started := kernel.Event{
		ID:     "ev-start",
		Type:   kernel.RunStarted,
		Scope:  "run:" + f.runID,
		Source: kernel.SourceHuman,
		Payload: map[string]any{
			"run_id":        f.runID,
			"actor":         bp.Name,
			"blueprint_sha": bp.SHA,
			"budget_usd":    f.budget,
			"max_turns":     float64(f.maxTurns),
			"prompt":        f.prompt,
			"workspace":     cfg.Workspace,
			"simulated":     true,
		},
	}
	if _, err := store.Append([]kernel.Event{started}); err != nil {
		fatal(fmt.Errorf("record run.started: %w", err))
	}

	fmt.Printf("run %s started (budget %.2f USD, workspace %s)\n",
		f.runID, f.budget, workspaceNote(f.workspace, cfg.Workspace))

	loop := &exec.Loop{
		Runner: runner,
		Log:    store,
		Time:   exec.VirtualTime{C: clock},
		Config: cfg,
	}

	out, err := loop.Run(context.Background())

	// The summary is printed even when the loop failed. A run that died halfway
	// still spent money and still moved through stages, and hiding that behind
	// the error would leave the user with a bill and no account of it.
	printRunSummary(f.runID, dir, out)

	if err != nil {
		fmt.Fprintf(os.Stderr, "\niash: the run stopped early: %v\n", err)
		os.Exit(1)
	}
}

// printRunSummary reports where the run got to, in the terms the user paid in.
//
// The cursor is printed because it is what a resume needs. A run that stopped
// early is resumable only if somebody knows the seq its effects were carried out
// through, and that number exists nowhere else.
func printRunSummary(runID, dir string, out exec.Outcome) {
	fmt.Printf("run %s %s (seq %d, stopped by %s)\n",
		runID, out.State.Status, out.State.Seq, stoppedNote(out.StoppedBy))

	if out.State.Stage != "" {
		fmt.Printf("  stage:  %s\n", out.State.Stage)
	}
	fmt.Printf("  turns:  %d\n", out.State.Turns)
	fmt.Printf("  spent:  %.4f of %.4f USD in the tree\n",
		out.State.TreeSpentUSD, out.State.BudgetUSD)
	fmt.Printf("  cursor: %d (resume from here)\n", out.Cursor)
	fmt.Printf("  log:    %s\n", filepath.Join(dir, "events.ndjson"))

	// Unanswered questions are the reason a run sits still, so they are named
	// rather than counted: "1 pending" tells the user something is wrong without
	// telling them what to do about it.
	for _, it := range out.State.Inbox {
		if !it.Replied {
			fmt.Printf("  asks:   %s (%s) %s\n", it.ID, it.Kind, it.Question)
		}
	}

	// Effect errors are not step failures: the run continued past them. They are
	// still reported, because an effect that failed silently is a turn somebody
	// is waiting on that will never arrive.
	for _, e := range out.Errs {
		fmt.Printf("  error:  %v\n", e)
	}

	if !out.State.Status.Terminal() {
		fmt.Printf("\nwhy is it not finished?  iash run why %s\n", runID)
	}
}

// stoppedNote translates the loop's stop reason into the user's problem.
// "idle" is accurate and unhelpful; what the user needs to know is whether they
// should wait, answer something, or look for a bug.
func stoppedNote(by string) string {
	switch by {
	case exec.StopTerminal:
		return "reaching a terminal status"
	case exec.StopIdle:
		return "running out of events with no timer armed"
	case exec.StopCancelled:
		return "cancellation"
	case "":
		return "an error"
	default:
		return by
	}
}

// workspaceNote echoes the workspace AND whether it was chosen or resolved.
// §20.1 prints `workspace auto→none` for exactly this reason: a value the user
// never typed needs to announce that it was inferred, or the first time it
// matters they will assume they set it.
func workspaceNote(requested, resolved string) string {
	if requested == "" || requested == "auto" {
		return "auto→" + resolved
	}
	return resolved
}

func parseStartFlags(args []string) (startFlags, error) {
	f := startFlags{workspace: "auto"}
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// --flag=value and --flag value are both accepted. Supporting only one
		// makes the other a silent misparse: `--budget=2.00` consumed as a
		// positional would become the prompt, and the run would start with no
		// budget and a nonsense objective.
		name, val, inline := strings.Cut(a, "=")
		next := func() (string, error) {
			if inline {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}

		switch name {
		case "--sim":
			f.sim = true
		case "--budget":
			v, err := next()
			if err != nil {
				return f, err
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return f, fmt.Errorf("--budget %q is not a number", v)
			}
			f.budget, f.budgetSet = n, true
		case "--max-turns":
			v, err := next()
			if err != nil {
				return f, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return f, fmt.Errorf("--max-turns %q is not a whole number", v)
			}
			f.maxTurns = n
		case "--workspace":
			v, err := next()
			if err != nil {
				return f, err
			}
			switch v {
			case "shared", "worktree", "copy", "none", "auto":
				f.workspace = v
			default:
				return f, fmt.Errorf("--workspace %q is not one of "+
					"shared, worktree, copy, none", v)
			}
		case "--run-id":
			v, err := next()
			if err != nil {
				return f, err
			}
			f.runID = v
		case "--dir":
			v, err := next()
			if err != nil {
				return f, err
			}
			f.dir = v
		case "--attach":
			// Accepted and ignored: with a virtual clock the run finishes before
			// anything could be followed. Rejecting it would be worse, since the
			// surface declares it and a declared flag that errors reads like a bug.
		default:
			if strings.HasPrefix(a, "-") {
				return f, fmt.Errorf("unknown flag %s", a)
			}
			positional = append(positional, a)
		}
	}

	if len(positional) < 1 {
		return f, fmt.Errorf("missing the actor to run")
	}
	if len(positional) < 2 {
		return f, fmt.Errorf("missing the prompt: a run with no objective has " +
			"nothing to put in the agents' context")
	}
	f.actor, f.prompt = positional[0], strings.Join(positional[1:], " ")

	// --budget is checked here and has NO default, which is the one piece of
	// validation this function is not free to relax. Every other ceiling can
	// default; a spend ceiling cannot, because a default is a number the user
	// never chose and only meets on the invoice. Enforced in the surface by
	// TestBudgetIsMandatory, and it has to hold at the entry point too or the
	// registry is describing a promise the binary does not keep.
	if !f.budgetSet {
		return f, fmt.Errorf("--budget is required and has no default: an " +
			"invisible spend ceiling is a surprise bill")
	}
	if f.budget <= 0 {
		return f, fmt.Errorf("--budget must be greater than zero (got %.4f): a "+
			"run that may not spend anything cannot take a single turn", f.budget)
	}

	if f.runID == "" {
		f.runID = newRunID()
	}
	return f, nil
}

// newRunID mints a run id that sorts chronologically and does not collide within
// a second. §20.1 shows `r1`, which is what the docs use for readability; a real
// id has to survive two runs started in the same minute.
func newRunID() string {
	return "r" + strconv.FormatInt(time.Now().UTC().UnixNano()/1e6, 36)
}
