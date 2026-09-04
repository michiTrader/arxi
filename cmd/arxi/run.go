package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/modelstore"
	"github.com/michiTrader/arxi/internal/provider"
	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/toolrun"
)

// startFlags is what `run start` was invoked with, after parsing but before any
// of it is trusted.
type startFlags struct {
	actor      string
	prompt     string
	promptFlag string
	budget     float64
	budgetSet  bool
	maxTurns   int
	workspace  string
	model      string
	sim        bool
	runID      string
	dir        string
}

// cmdRunStart implements `arxi run start <actor> <prompt> --budget N`.
//
// This is the command that joins the four packages: blueprint loads the actor,
// logstore holds the events, kernel decides, exec executes. It contains no
// decisions of its own beyond wiring, and that is the point — every question of
// what should happen next is answered by kernel.Decide, so `run`, `--sim` and
// `replay` cannot drift apart.
//
// Both paths run today. `--sim` swaps in exec.Fake and a virtual clock; without
// it the run uses provider.Executor against a real endpoint and a real clock.
//
// The two differ in exactly two objects, and that is deliberate. Everything that
// decides anything -- the reducer, the log, the loop, the effect ordering -- is
// shared, so a simulation exercises the code a real run will take rather than a
// parallel implementation of it. The alternative, a `--sim` with its own path,
// drifts silently and is then trusted for exactly the runs nobody wants to pay
// to test.
func cmdRunStart(args []string) {
	// Short flags are expanded before parsing, from the surface's own assignment,
	// so this parser only ever sees long names. See expandShort in flags.go.
	args, err := expandShort(surface.Lookup("run", "start"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run start: %v\n", err)
		os.Exit(2)
	}

	f, err := parseStartFlags(args)
	if err != nil {
		// --model is in this usage because it was missing from it, and the cost of
		// that was measured on a reader rather than guessed: the flag is parsed
		// here, declared in the surface and consumed by the executor, and the one
		// place a user meets it -- the message printed when they get the
		// invocation wrong -- did not mention it. A flag absent from the usage of
		// the command that requires it reads as a flag the command does not have,
		// and the next thing that reader does is edit a blueprint by hand.
		fmt.Fprintf(os.Stderr, "arxi run start: %v\n\n"+
			"usage: arxi run start <actor> <prompt> --budget <usd> [--model <id>]\n"+
			"                      [--max-turns N] [--sim]\n"+
			"                      [--workspace shared|worktree|copy|none]\n"+
			"short: -a actor  -p prompt  -b budget  -m model  -w workspace  -S sim\n", err)
		os.Exit(2)
	}

	// The actor is a path or the name of a stored agent, resolved in that order.
	// resolveActor in agent.go argues the precedence; what matters here is that a
	// name nothing answers to gets its own message. `blueprint is not valid` about
	// a file that does not exist is the wrong sentence for a typo, and a typo is
	// the common case now that a bare name is a legal argument.
	bp, err := resolveActor(f.actor)
	if errors.Is(err, errUnresolvedActor) {
		// Exit 1 as well: the invocation is well formed and the thing it names is
		// missing, which is the same class of failure as an unreadable file.
		fmt.Fprintf(os.Stderr, "arxi run start: %v\n"+
			"  looked for a file at %s\n"+
			"  and a stored agent at %s\n"+
			"  what is stored: arxi agent list\n"+
			"  or write it:    arxi agent create %s --model <id> --tools read\n",
			err, f.actor, readAgents().Path(f.actor), f.actor)
		os.Exit(1)
	}
	if err != nil {
		// Exit 1, not 2: the invocation was correct, the blueprint is what is
		// wrong. CI needs to tell "you called this wrong" from "your file is bad".
		fmt.Fprintf(os.Stderr, "blueprint is not valid.\n\n%v\n", err)
		os.Exit(1)
	}

	// A live run needs a model for every member BEFORE the run directory exists,
	// and the check happens here for that reason. Discovering halfway through
	// that one member has no model leaves a half-written run whose log records a
	// start that never produced anything, and the operator has to clean it up.
	//
	// Nothing is resolved against the provider store yet -- that is the
	// executor's job, and doing it twice would let the two answers differ. This
	// only refuses the case no resolution could fix.
	if !f.sim {
		if err := checkEveryMemberHasAModel(bp.Config, f.model); err != nil {
			fmt.Fprintf(os.Stderr, "arxi run start: %v\n", err)
			os.Exit(2)
		}
	}

	dir, out, err := executeRun(f, bp, func(dir string, cfg kernel.Config) {
		// usd and not %.2f. This line printed `--budget 0.005` as `budget 0.01
		// USD`: a ceiling ROUNDED UP, so the banner promised twice the headroom
		// the run actually had, and the summary four lines later contradicted it
		// with `of 0.0050`. One command, one field, two numbers.
		//
		// Rounding a ceiling up is the worse of the two directions, because the
		// reader is shown more room than the run has and the block that follows
		// looks premature. eval run was already held to this rule by
		// TestABudgetTooSmallToRoundIsNotPrintedAsZero; the run that actually
		// spends the money was not.
		fmt.Printf("run %s started (budget %s USD, workspace %s)\n",
			f.runID, usd(f.budget), workspaceNote(f.workspace, cfg.Workspace))
	})

	// The summary is printed even when the loop failed. A run that died halfway
	// still spent money and still moved through stages, and hiding that behind
	// the error would leave the user with a bill and no account of it.
	printRunSummary(f.runID, dir, out)

	if err != nil {
		fmt.Fprintf(os.Stderr, "\narxi: the run stopped early: %v\n", err)
		os.Exit(1)
	}
}

// executeRun starts the run and hands back where it got to.
//
// It is everything `run start` does after the invocation has been checked, split
// out because the quick path (`arxi -p "ping"`, ask.go) has to do the same thing
// and print a different thing. What differs between the two is only output: which
// stream the accounting goes to, and whether the model's reply is printed at all.
// None of that belongs in here, so the only thing this takes from its caller is
// announce -- called once, after run.started is in the log and before the first
// turn opens, which is the last moment a line can be printed that is still true
// if the very first model call hangs.
//
// The error is RETURNED rather than exited on, because both callers have
// something to print after a failed loop: a run that stopped early still spent
// money, and the account of it is the caller's to render.
func executeRun(f startFlags, bp *blueprint.Blueprint, announce func(dir string, cfg kernel.Config)) (string, exec.Outcome, error) {
	cfg := bp.Config
	if f.workspace != "" && f.workspace != "auto" {
		cfg.Workspace = f.workspace
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
	// The defer is for ordinary returns; atExit is for os.Exit, which skips
	// defers. `run start` has the same exposure `run unpause` was measured to
	// have: openPolicies and openProviders both fatal, and they are called after
	// this lock is taken, so a bad policy or provider file used to leave
	// writer.lock behind holding a dead pid.
	defer store.Close()
	atExit(func() { store.Close() })

	// The clock and the executor are the ONLY two things that differ between a
	// simulation and a real run. Everything downstream -- the reducer, the log,
	// the loop, the effect ordering -- is the same code, which is what makes
	// --sim worth trusting.
	//
	// The virtual clock is what makes --sim finish in milliseconds instead of
	// waiting out a 30-minute stage timeout. It is also why Now is derived from
	// it rather than from the wall: two simulations of identical input must
	// produce identical logs, or `run diff` has nothing to compare. A real run
	// takes the opposite trade deliberately: its timestamps are wall time,
	// because a log that says a turn happened at t=0 is useless for explaining
	// an incident that happened at 3am.
	// Timekeeper and not the raw Clock, because the LOOP needs the difference:
	// VirtualTime jumps to the next deadline, RealTime waits for it. Both already
	// exist in internal/exec, which is the design anticipating this exact moment.
	var (
		clock    exec.Clock
		timekeep exec.Timekeeper
		executor exec.Executor
		now      func() string
	)
	if f.sim {
		vc := exec.NewVirtualClock()
		clock, timekeep, executor = vc, exec.VirtualTime{C: vc}, exec.NewFake()
		now = func() string {
			return time.UnixMilli(vc.NowMs()).UTC().Format(time.RFC3339Nano)
		}
	} else {
		rc := exec.NewRealClock()
		clock, timekeep = rc, exec.RealTime{C: rc}
		// The workspace lives inside the run directory, beside the log and the
		// frozen blueprint, and is NOT deleted when the run ends. That is the
		// same argument as freezing the blueprint: what the agents actually
		// produced is evidence, and `run why` sends the user to look at it. A
		// runner that tidied up after itself would answer "why did this fail?"
		// with an empty directory.
		//
		// Shared follows the blueprint rather than a flag of its own.
		// cfg.Workspace has already resolved to "worktree" if any member holds
		// write/bash/edit, so this is the decision the config recorded and
		// `blueprint validate` printed -- not a second, invisible one taken here.
		// Tool policy overrides are read HERE, at the start, and copied into the
		// executor. They are deliberately not re-read per turn: the rules a run
		// is judged by must not move underneath it, which is the same argument
		// that freezes blueprint.snapshot.yaml.
		//
		// The visible consequence, which `agent tool policy` prints in its own
		// output rather than leaving to be discovered: a run already waiting on
		// an approval is not unblocked by a policy change. This is the next run.
		//
		// Overrides sit outside the frozen snapshot on purpose. They are an
		// operator's standing answer to "stop asking me about this", declared
		// with --agent and no --run, so baking them into one run's snapshot
		// would mean the next run could not see them.
		overrides, err := openPolicies().LoadAll()
		if err != nil {
			fatal(err)
		}

		executor = &provider.Executor{
			Resolver:     providerResolver{openProviders()},
			DefaultModel: f.model,
			Members:      cfg.Members,
			Prompt:       f.prompt,
			ToolPolicy:   overrides,
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

			// simulated records WHICH executor produced this log, and it is
			// f.sim rather than a constant. It was a hardcoded true, correct
			// while --sim was the only mode and false the moment the live
			// executor landed -- found by reading the log of a real run that
			// had really cost money and really called a real server, and that
			// described itself as a simulation.
			//
			// This field is the one thing in the log that a reader cannot
			// recover from anything else in it. Everything else -- the costs,
			// the turns, the replies -- looks identical either way, by design:
			// --sim drives the same reducer through the same loop, and that is
			// what makes it worth trusting. So the log is the only place the
			// distinction can live, and a log that mislabels a real run as a
			// simulation is worse than one that omits the field, because it
			// invites exactly the conclusion a reader would otherwise check.
			"simulated": f.sim,
		},
	}
	if _, err := store.Append([]kernel.Event{started}); err != nil {
		fatal(fmt.Errorf("record run.started: %w", err))
	}

	announce(dir, cfg)

	loop := &exec.Loop{
		Runner: runner,
		Log:    store,
		Time:   timekeep,
		Config: cfg,
	}

	out, err := loop.Run(context.Background())

	// The store is CLOSED on the way out, and the deferred Close is what does it
	// now: this used to be a bare os.Exit(1) on a failed loop, which does not run
	// defers, so a run that had merely failed left writer.lock on disk holding a
	// pid that no longer exists. The next command touching that run then refused
	// with "already open for writing by pid N" and told the operator to delete a
	// lock file by hand -- for a run that had failed, which is exactly when
	// somebody wants to retry it.
	//
	// The advice is also dangerous to generalise: an operator who learns to delete
	// writer.lock after a crash will eventually delete a live one. Returning
	// instead of exiting is what makes the release automatic; the manual remedy is
	// for a hard kill, which nothing here can help with.
	return dir, out, err
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
		fmt.Printf("\nwhy is it not finished?  arxi run why %s\n", runID)
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

// providerResolver adapts the provider store to what the live executor needs.
//
// It calls model.Resolve and NOT store.Owner, and the difference is the whole
// reason this type exists. Owner deliberately finds a DISABLED model, because
// `model enable` has to act on one. A run must do the opposite: a disabled model
// is an operator's cost decision (§20.11 lists `model disable` as exactly that),
// and honouring it is the difference between the command meaning something and
// being decorative.
//
// Resolve is also what refuses an AMBIGUOUS ref rather than picking by sort
// order -- which matters here more than anywhere, because the wrong pick is a
// different bill.
type providerResolver struct{ store *modelstore.Store }

func (r providerResolver) Resolve(ref string) (model.Resolution, error) {
	ps, err := r.store.List()
	if err != nil {
		return model.Resolution{}, err
	}
	return model.Resolve(ps, ref)
}

// checkEveryMemberHasAModel refuses a live run that could not name a model for
// one of its members.
//
// It runs BEFORE the run directory is created, because the alternative is a
// half-written run: a log recording a start, a frozen blueprint, and a first
// turn that fails on a configuration mistake the user could have been told about
// for free. A refusal that costs nothing is worth more than a failure that
// leaves debris.
//
// Existence is deliberately NOT checked here. Whether a model is registered and
// enabled is the executor's question, and asking it in two places lets the two
// answers disagree -- the classic version of that bug is a pre-flight check that
// passes and a run that then fails, which teaches the user to distrust the
// check. This refuses only what no amount of resolution could fix: a member with
// no model named anywhere.
func checkEveryMemberHasAModel(cfg kernel.Config, def string) error {
	if def != "" {
		return nil
	}
	var missing []string
	for _, m := range cfg.Members {
		if m.Model == "" {
			missing = append(missing, m.Name)
		}
	}
	// A blueprint with no members at all is a single-actor run, and the actor
	// still needs a model to think with.
	if len(cfg.Members) == 0 {
		return fmt.Errorf("a live run needs a model, and none was given.\n" +
			"  fix: arxi run start ... --model <id>\n" +
			"  see: arxi model list, for the models this machine has enabled")
	}
	if len(missing) > 0 {
		return fmt.Errorf("these members name no model and the run has no default: %s\n"+
			"  fix: give each a `model:` in the blueprint, or start the run with --model <id>\n"+
			"  see: arxi model list, for the models this machine has enabled\n"+
			"  note: no default is invented here on purpose -- it would be a spend "+
			"decision taken in the binary, and upgrading arxi could then change what "+
			"an unchanged command costs",
			strings.Join(missing, ", "))
	}
	return nil
}

// parseStartFlags parses `run start`'s command line: the actor is the first
// positional word, the prompt is the rest, and --budget is required.
func parseStartFlags(args []string) (startFlags, error) {
	return parseStartArgs(args, startFlags{workspace: "auto"})
}

// parseStartArgs is that parser with its starting point supplied by the caller.
//
// Two entry points now reach a live run, and they differ only in what they
// arrive already knowing. `run start` is told everything on the command line.
// The quick path -- `arxi -p "ping"`, in ask.go -- arrives with the actor and the
// spend ceiling already chosen, and must otherwise accept exactly the same
// flags. That is the reason this is one parser with a seed rather than two
// parsers: a second switch would drift, and the way it drifts is silent. Someone
// adds --max-turns to one of them, and the same flag keeps working on one path
// and stops working on the other with no test able to notice, because each
// parser passes its own tests.
//
// The seed carries the whole difference. A non-empty actor in it also selects the
// grammar for the positional words: a caller that already knows who runs has no
// leading word to skip, so every word is prompt. Without that, `arxi -m X "hola
// mundo"` would go looking for an agent named hola.
func parseStartArgs(args []string, f startFlags) (startFlags, error) {
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Everything after `--` is a positional, never a flag. A prompt is free
		// text and can legitimately start with a dash ("--sim is broken"); with
		// no escape hatch that objective is simply unpassable, and the closest
		// thing the user can do is quote it, which does not help because quotes
		// are the shell's and are gone by now.
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}

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
		// The two positional parameters are also accepted by name. The registry
		// declares them as `actor` and `prompt` and publishes those names to the
		// tool schema and the protocol, so --actor is not a spelling invented
		// here; refusing it would mean -a expands to a flag this parser drops,
		// and a flag that parses and is then ignored starts a run with no
		// objective while looking like it worked.
		case "--actor":
			v, err := next()
			if err != nil {
				return f, err
			}
			// Assigned, not pushed onto the front of the positionals. Both put the
			// same word in the same field for every invocation `run start` accepts
			// -- the difference is that a seeded actor stays seeded. The quick path
			// arrives already knowing who runs, and a parser that could only learn
			// the actor from a positional word would have to be handed a fake one.
			f.actor = v
		case "--prompt":
			v, err := next()
			if err != nil {
				return f, err
			}
			f.promptFlag = v
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
		case "--model":
			v, err := next()
			if err != nil {
				return f, err
			}
			f.model = v
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
			// Accepted and ignored, still. `arxi run attach` is built now, so the
			// following it promises does exist -- but it is a separate process
			// watching from another terminal, and this flag would have to become
			// that follower inside the run's own writer lock to mean anything.
			//
			// Rejecting it would be worse, since the surface declares it and a
			// declared flag that errors reads like a bug. Silently doing nothing is
			// honest here in a way it usually is not: the events the flag would show
			// are all in the log, and `arxi run attach <run>` from a second terminal
			// shows them arriving.
		default:
			if strings.HasPrefix(a, "-") {
				return f, fmt.Errorf("unknown flag %s", a)
			}
			positional = append(positional, a)
		}
	}

	// The actor may already be known: --actor named it above, or the caller seeded
	// it. Only when it is not known does the leading word become the actor, and
	// then the prompt is whatever follows.
	if f.actor == "" {
		if len(positional) < 1 {
			return f, fmt.Errorf("missing the actor to run")
		}
		f.actor, positional = positional[0], positional[1:]
	}

	// A prompt given as --prompt is taken whole; a positional prompt is joined
	// from the remaining words, because a shell that was not quoted splits it and
	// silently truncating to the first word would run a different objective.
	if f.promptFlag != "" {
		if len(positional) > 0 {
			return f, fmt.Errorf("the prompt was given twice: once as --prompt %q "+
				"and once positionally (%q). Guessing which one is meant would run "+
				"an objective the user did not choose",
				f.promptFlag, strings.Join(positional, " "))
		}
		f.prompt = f.promptFlag
	} else {
		if len(positional) == 0 {
			return f, fmt.Errorf("missing the prompt: a run with no objective has " +
				"nothing to put in the agents' context")
		}
		f.prompt = strings.Join(positional, " ")
	}

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

// newRunID mints a run id that sorts chronologically and does not collide.
//
// §20.1 shows `r1`, which is what the docs use for readability. A real id needs
// more than a timestamp, because the id IS the run directory: two runs that mint
// the same one open the same log and append their events into each other. Both
// become unreplayable and neither can be separated from the other afterwards, so
// the work and the audit trail are lost together.
//
// The first version of this used millisecond resolution alone and repeated on
// the second mint in a tight loop, which is exactly what a script or a CI job
// does. So the timestamp — which makes ids sortable, and therefore `run list`
// readable — is combined with random bytes, which make them unique. Time alone
// sorts but collides; randomness alone is unique but says nothing about when the
// run happened.
func newRunID() string {
	ms := time.Now().UTC().UnixMilli()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Degrade to the nanosecond clock rather than refuse to start. A weaker
		// id is a smaller problem than a run that cannot begin over entropy.
		return "r" + strconv.FormatInt(ms, 36) +
			"-" + strconv.FormatInt(int64(time.Now().Nanosecond()), 36)
	}
	return "r" + strconv.FormatInt(ms, 36) + "-" + hex.EncodeToString(b[:])
}
