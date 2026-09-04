package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi -p "..."` -- the short way in.
//
// # Why this is not a new capability
//
// The surface declares 50 capabilities and the binary is measured against that
// count in three tests and three documents. A 51st verb added for convenience
// would turn all six red, and the honest fix would be to widen the frozen surface
// so a shortcut could exist. So this is a main.go-level alias, in the manner of
// `arxi why`: the flags are `run start`'s, expanded through `run start`'s own
// registry entry, parsed by `run start`'s own parser, and driven by the same
// executeRun. Nothing here is a second pipeline, so a flag added to `run start`
// arrives on this path already working.
//
// # Why it exists anyway
//
// Everything the binary could do was reachable only by naming an actor first. The
// first five minutes were: install, add a provider, write a YAML file, learn what
// a blueprint is, learn what a stage is, then get one answer. This path runs the
// stored agent named `default`, so the first five minutes are: install, add a
// provider, ask.
//
// # The one rule it bends, and how
//
// --budget is required on `run start` and has no default, because an invisible
// spend ceiling is a surprise bill. The objection there is to invisibility, not to
// defaults, so the ceiling is answered rather than dropped: every invocation
// prints the ceiling and where it came from before the first model call is made,
// and no flag on this path can silence that line.
//
// # Why stdout carries the reply alone
//
// `arxi -p "..." > answer.txt` should leave an answer in the file and not a
// transcript, so the ceiling, the spend and every complaint go to stderr. That is
// the visible difference from `run start`, which prints a summary table for
// somebody watching a terminal.
const (
	// The one stored name the short form looks for. It is an ordinary agent --
	// `arxi agent list` shows it, `arxi agent show default` prints it, the file is
	// editable by hand -- so the only privilege it has is being the name this path
	// guesses when nobody said one.
	defaultAgentName = "default"

	// The ceiling is read from the environment rather than from a config key,
	// because the ceiling of a one-liner belongs to the shell somebody is typing
	// in, not to the project directory they happen to be standing in.
	askBudgetEnv = "ARXI_BUDGET"

	// Small enough that meeting it by accident costs cents, large enough for a real
	// question with a few tool calls behind it. It is printed on every run with its
	// source, so it is a number the user is told rather than one they inherit.
	askBudgetDefault = 0.05
)

// looksLikeAsk reports whether main should hand args to the short path.
//
// The rule is "the first word is a flag", because the first word of every other
// invocation is a noun. -h, --help and --version are excluded here rather than
// left to the order of main's switch: they are flags that mean something else, and
// reordering that switch should not be able to turn `arxi --version` into a prompt
// with no prompt in it. A bare - and a bare -- are excluded for the same reason
// they are excluded from expandShort: neither names a flag.
func looksLikeAsk(arg string) bool {
	switch arg {
	case "-h", "--help", "--version", "-", "--":
		return false
	}
	return strings.HasPrefix(arg, "-")
}

// askUsage is a function and not a constant so that usd(askBudgetDefault) is the
// only place the default ceiling is written down. A usage screen that names a
// number the code does not use is worse than one that names no number at all.
func askUsage() string {
	return "usage: arxi -p \"<prompt>\" [--model <id>] [--budget <usd>] [--sim]\n" +
		"       arxi --prompt \"<prompt>\" [--actor <agent|file.yaml>]\n\n" +
		"runs the stored agent named " + defaultAgentName +
		" (arxi agent create " + defaultAgentName + " --model <id> --tools read,grep)\n" +
		"with --model it needs no stored agent at all\n" +
		"ceiling: " + usd(askBudgetDefault) + " USD, unless " + askBudgetEnv +
		" or --budget says otherwise\n" +
		"stdout is the reply; the ceiling, the spend and any trouble go to stderr\n"
}

// askBudget resolves the ceiling out of the environment and says where it came
// from, so the caller can print both in one line.
//
// An unparseable ARXI_BUDGET is refused rather than ignored. Somebody who exported
// `ARXI_BUDGET=0,50` was lowering their ceiling; falling back to the built-in
// default would silently install the very number they meant to replace, and they
// would only find out on the invoice. Pure, so the refusals are testable without
// touching the process environment.
func askBudget(env string) (float64, string, error) {
	env = strings.TrimSpace(env)
	if env == "" {
		return askBudgetDefault, "built in; " + askBudgetEnv + " or --budget changes it", nil
	}
	n, err := strconv.ParseFloat(env, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%s=%q is not a number of dollars, and guessing what "+
			"it meant would set a spend ceiling the user did not choose:\n"+
			"  fix: %s=0.25, or drop it and pass --budget", askBudgetEnv, env, askBudgetEnv)
	}
	if n <= 0 {
		return 0, "", fmt.Errorf("%s=%q leaves nothing to spend, and a run that may not "+
			"spend anything cannot take a single turn", askBudgetEnv, env)
	}
	return n, "from " + askBudgetEnv, nil
}

// budgetWasTyped reports whether the command line carried its own ceiling, so the
// printed note credits the flag instead of the environment the flag overrode.
//
// Reading the expanded args is what makes -b count as well as --budget. It stops
// at --, because every word after that is prompt: `arxi -p -- --budget` asks a
// question about a flag, it does not set one.
func budgetWasTyped(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--budget" || strings.HasPrefix(a, "--budget=") {
			return true
		}
	}
	return false
}

// askModelNote names the model the ceiling is about to be spent on.
//
// It reads the roster and not the flag, because provider.Executor lets a member's
// own `model:` win over its DefaultModel. Printing f.model here would name the
// wrong model on exactly the runs where the two differ, which is the one case the
// line exists for. Empty when nothing is known, and the caller drops the field
// rather than printing "model: unknown".
func askModelNote(cfg kernel.Config, def string, sim bool) string {
	if sim {
		return "--sim, no model calls"
	}
	var seen []string
	for _, m := range cfg.Members {
		id := m.Model
		if id == "" {
			id = def
		}
		if id == "" {
			continue
		}
		dup := false
		for _, s := range seen {
			if s == id {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, id)
		}
	}
	switch len(seen) {
	case 0:
		// A blueprint with no members at all is a single-actor run, and --model is
		// then the only model there is.
		return def
	case 1:
		return seen[0]
	}
	return fmt.Sprintf("%d models: %s", len(seen), strings.Join(seen, ", "))
}

// ephemeralAgent builds a one-member team in memory, for `arxi -p "..." -m <id>`
// with nothing stored yet.
//
// It renders an agentstore.Record and loads those bytes back, rather than
// assembling a kernel.Config in place. That buys the name validation, the
// load-bearing `work` stage, and a bp.Raw that is a real blueprint -- which
// matters because executeRun freezes Raw into the run directory, and a snapshot
// that does not parse would leave the run unreplayable by `arxi run why`. Nothing
// is written under agents/: a run that needed a model for one question should not
// leave a team behind for the user to discover later and wonder about.
//
// No tools, deliberately. Tool policy is automatic, so a granted `bash` would let
// a one-liner run commands inside a directory the user never chose to hand over,
// and `ask` would turn into a blocking inbox question instead of an answer.
func ephemeralAgent(name, model string) (*blueprint.Blueprint, error) {
	raw, err := agentstore.Record{Name: name, Model: model}.Render()
	if err != nil {
		return nil, err
	}
	return blueprint.Load(raw)
}

// askNoModelMessage is the refusal for `arxi -p "..."` with no stored default and
// no --model, and it names both fixes because both are right: one for the user who
// wants this to work today, one for the user who wants it to work tomorrow too.
//
// Same house style as checkEveryMemberHasAModel, for the same reason: the error a
// newcomer meets is the documentation they actually read. "open agents/default.yaml:
// no such file" would be true and would teach them nothing.
func askNoModelMessage(name, path string) string {
	return fmt.Sprintf("no agent named %q is stored and no --model was given, so there "+
		"is nothing to ask.\n"+
		"  once: arxi agent create %s --model <id> --tools read,grep\n"+
		"        after that `arxi -p \"...\"` needs no flags, and %s is a normal file to edit\n"+
		"  now:  arxi -p \"...\" --model <id>, which stores nothing\n"+
		"  see:  arxi model list, for the models this machine has enabled\n"+
		"        arxi provider add, if that list is empty\n"+
		"  note: no model is invented here on purpose -- it would be a spend decision "+
		"taken in the binary, and upgrading arxi could then change what an unchanged "+
		"command costs", name, name, path)
}

// askReply is the one answer a short invocation exists for, dug back out of the log.
type askReply struct {
	found  bool   // an llm.response was recorded at all
	ok     bool   // the last one reported success
	text   string // the last reply that carried any text
	model  string // provider/model of the last response
	failed string // the last response's error, when it carried one
	finish string // finish_reason, which is how a truncated reply is spotted
	sim    bool
}

// lastReply folds the log down to that answer.
//
// # Why the text is not read off Outcome.State
//
// State.Result looks like the field for this and is not: the reducer sets it to
// "all stages completed" (internal/kernel/decide.go:516). runresult.go gives the
// full reason -- spec/events.md declares stage.submitted with no payload keys at
// all, so no producer writes an answer onto the state, and a reader that expected
// one would be inventing a schema and hoping a producer agreed later. The reply
// text exists in exactly one place, the llm.response event. That is ADR-0002
// working as designed rather than an omission.
//
// The last response with text wins, which is a deliberate narrowing of
// cfg.ResultFrom. For the one-member agent this path is built for they are the
// same event; for a multi-member blueprint arriving by --actor, honouring
// result_from would mean printing nothing at all when the run ended some other
// way, and the last thing the team said is more use than silence.
func lastReply(events []kernel.Event) askReply {
	rep := askReply{sim: runWasSimulated(events)}
	for _, e := range events {
		if e.Type != kernel.LLMResponse {
			continue
		}
		rep.found = true

		// Status fields are the LAST response's, because that is the turn the run
		// stopped on. The text is the last NON-EMPTY one, because a final turn that
		// only called a tool carries no text and should not blank an answer that
		// was already given.
		rep.ok, _ = e.Payload["ok"].(bool)
		rep.model = e.Str("model")
		rep.failed = e.Str("error")
		rep.finish = e.Str("finish_reason")
		if t := e.Str("text"); t != "" {
			rep.text = t
		}
	}
	return rep
}

// askRunReply reads the run's own log back after the loop has stopped.
//
// Not through the still-open logstore.Store: executeRun holds the writer lock for
// the length of the run and releases it on a deferred Close, so a read that had to
// happen before that Close would be a second reason to keep the store open. The
// last time this path held the lock a moment longer than it needed to, a failed run
// left writer.lock behind with a dead pid in it and the next command refused.
// Reading the file is enough, and it cannot hold anything.
func askRunReply(dir string) (askReply, error) {
	raw, err := readRunLog(dir)
	if err != nil {
		return askReply{}, err
	}
	events, err := decodeRunEvents(dir, raw)
	if err != nil {
		return askReply{}, err
	}
	return lastReply(events), nil
}

// askExitCode decides what `echo $?` says, which is the whole contract for a
// command meant to be used in a pipe.
//
// The order is load-bearing and was measured rather than reasoned about. exec.Fake
// writes no `ok` key at all (internal/exec/fake.go:241), so every simulated run
// reads as found-and-not-ok, and the obvious ordering made `--sim` exit 1 on
// success. Below that: a recorded failure is a failure even when an earlier turn
// produced text, and text is success. No text and no error is still 1, because a
// caller piping stdout somewhere needs to know the pipe came up empty.
func askExitCode(rep askReply, loopErr error) int {
	switch {
	case loopErr != nil:
		return 1
	case rep.sim:
		return 0
	case rep.found && !rep.ok:
		return 1
	case rep.text != "":
		return 0
	}
	return 1
}

// cmdAsk runs the short form. It takes the whole command line, args[0] included,
// because on this path the first word is already a flag.
func cmdAsk(args []string) {
	// The short letters are expanded through `run start`'s registry entry, so -p
	// cannot come to mean one thing here and another there. It is the same table
	// `arxi -h` prints from.
	args, err := expandShort(surface.Lookup("run", "start"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi: %v\n\n%s", err, askUsage())
		os.Exit(2)
	}

	budget, source, err := askBudget(os.Getenv(askBudgetEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi: %v\n", err)
		os.Exit(2)
	}

	// The seed is what makes this the same parser rather than a copy of it. A
	// pre-filled actor means every remaining word is prompt, so `arxi -m X "hola
	// mundo"` does not go looking for an agent named hola; a pre-filled budget means
	// --budget is satisfied without relaxing the check that requires it.
	f, err := parseStartArgs(args, startFlags{
		workspace: "auto",
		actor:     defaultAgentName,
		budget:    budget,
		budgetSet: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi: %v\n\n%s", err, askUsage())
		os.Exit(2)
	}
	if budgetWasTyped(args) {
		source = "from --budget"
	}

	// resolveActor accepts a stored name or a file, so --actor still works here and
	// a user with several agents is not locked out of the short form. Only the
	// missing-name case is special, and only then because this is the one command
	// that can answer it without a stored agent at all.
	bp, err := resolveActor(f.actor)
	switch {
	case errors.Is(err, errUnresolvedActor) && f.model == "":
		// The path is asked of the store rather than joined here, so the sentence
		// cannot start naming a file the store does not use.
		fmt.Fprintf(os.Stderr, "arxi: %s\n",
			askNoModelMessage(f.actor, readAgents().Path(f.actor)))
		os.Exit(2)

	case errors.Is(err, errUnresolvedActor):
		bp, err = ephemeralAgent(f.actor, f.model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi: build a one-off agent on %s: %v\n", f.model, err)
			os.Exit(1)
		}
		// Said out loud, because "no tools" explains a refusal the user is about to
		// meet if they asked for something on disk, and because a one-off team that
		// vanishes is worth mentioning once to somebody who may want it back.
		fmt.Fprintf(os.Stderr, "arxi: no agent named %s is stored, so this run uses a "+
			"one-off with no tools (arxi agent create %s --model %s --tools read,grep "+
			"to keep it)\n", f.actor, f.actor, f.model)

	case err != nil:
		fmt.Fprintf(os.Stderr, "arxi: %v\n", err)
		os.Exit(1)
	}

	// --model is pushed onto every member instead of being left to the executor's
	// DefaultModel, which only fills in members that name none. Against a stored
	// `default` that declares a model, `arxi -p "..." -m other` would otherwise be
	// accepted, ignored, and billed to the model it was told not to use -- the one
	// silent failure this flag must not have.
	if f.model != "" {
		for i := range bp.Config.Members {
			bp.Config.Members[i].Model = f.model
		}
	}

	// One shared check, not a second copy: two model checks would eventually
	// disagree about what a runnable team is. Its 0-member branch names `run start`,
	// so the command the reader actually typed is added underneath.
	//
	// Skipped under --sim, on the same condition cmdRunStart uses, because a run
	// that calls no model resolves no model either: refusing here would make --sim
	// stricter than the live path it stands in for, which is the one direction a
	// dry run must never be wrong in.
	if !f.sim {
		if err := checkEveryMemberHasAModel(bp.Config, f.model); err != nil {
			fmt.Fprintf(os.Stderr, "arxi: %v\n  here: arxi -p %q --model <id>\n", err, f.prompt)
			os.Exit(2)
		}
	}

	simNote := ""
	if f.sim {
		simNote = "simulated, "
	}
	dir, out, loopErr := executeRun(f, bp, func(dir string, cfg kernel.Config) {
		// Printed after run.started is in the log and before the first turn opens:
		// the last moment a line about this run is still true if the model call
		// hangs. On stderr, because stdout belongs to the answer.
		note := askModelNote(cfg, f.model, f.sim)
		if note != "" {
			note = ", " + note
		}
		fmt.Fprintf(os.Stderr, "arxi: %sceiling %s USD (%s)%s, run %s\n",
			simNote, usd(f.budget), source, note, f.runID)
	})

	rep, err := askRunReply(dir)
	if err != nil {
		// The run happened either way; what failed is reading it back. Reported and
		// not fatal, so the spend line below still prints.
		fmt.Fprintf(os.Stderr, "arxi: %v\n", err)
	}
	if rep.text != "" {
		fmt.Println(rep.text)
	}

	tail := ""
	if rep.model != "" {
		tail = ", " + rep.model
	}
	// simNote again, because exec.Fake reports a cost and the reducer adds it up
	// like any other: on a simulated run this line carries a number that was never
	// billed, and a number that says nothing about itself is read as money.
	fmt.Fprintf(os.Stderr, "arxi: %sspent %s of %s USD, %d turn(s)%s\n",
		simNote, usd(out.State.TreeSpentUSD), usd(out.State.BudgetUSD), out.State.Turns, tail)

	// Everything below is why an answer is missing or shorter than expected, in the
	// order a reader needs it: what happened to the reply, then what failed, then
	// what is waiting on them.
	if rep.finish == "length" {
		fmt.Fprintf(os.Stderr, "arxi: the reply hit the model's output limit, so it is "+
			"cut off rather than finished\n")
	}
	if rep.failed != "" {
		fmt.Fprintf(os.Stderr, "arxi: the model call failed: %s\n", rep.failed)
	}

	// Effect errors are printed verbatim: this is where "no provider is configured"
	// arrives, and paraphrasing it would cost the reader the one sentence that says
	// which provider and what to add.
	for _, e := range out.Errs {
		fmt.Fprintf(os.Stderr, "arxi: %v\n", e)
	}

	// A question nobody answered is the usual reason a short run comes back with
	// nothing, and it is invisible unless it is named: `ask` is not a prompt, it is
	// a denied tool call that turns into an inbox item and waits.
	for _, it := range out.State.Inbox {
		if !it.Replied {
			fmt.Fprintf(os.Stderr, "arxi: it is waiting on you: %s (%s) %s\n"+
				"  answer it: arxi inbox\n", it.ID, it.Kind, it.Question)
		}
	}
	if loopErr != nil {
		fmt.Fprintf(os.Stderr, "arxi: the run stopped early: %v\n", loopErr)
	}
	if rep.sim {
		fmt.Fprintf(os.Stderr, "arxi: --sim, so no model was called and nothing above "+
			"was billed\n")
	}

	code := askExitCode(rep, loopErr)
	if code != 0 && rep.text == "" {
		fmt.Fprintf(os.Stderr, "arxi: no reply was recorded.\n"+
			"  why: arxi run why %s\n  log: %s\n",
			f.runID, filepath.Join(dir, "events.ndjson"))
	}

	// Plain os.Exit, matching cmdRunStart: executeRun has returned, so its deferred
	// Close has already released writer.lock and there is no hook left to run.
	os.Exit(code)
}
