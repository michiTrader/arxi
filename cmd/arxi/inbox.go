package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/surface"
)

// runsDir is where run directories live, and it is a var so tests can point it
// somewhere disposable -- the same seam triggerDir and evalDir use.
var runsDir = "runs"

// cmdInbox routes the four inbox verbs.
func cmdInbox(args []string) {
	if len(args) == 0 {
		cmdInboxList(nil)
		return
	}
	switch args[0] {
	case "approve":
		cmdInboxAnswer("approve", args[1:])
	case "reject":
		cmdInboxAnswer("reject", args[1:])
	case "reply":
		cmdInboxAnswer("reply", args[1:])
	case "-h", "--help", "help":
		inboxUsage()
	default:
		// A bare `arxi inbox --json` has no subcommand. Anything that is not a
		// known verb and not a flag is most likely an id the user expected to
		// act on, and saying so beats listing the whole inbox as though nothing
		// had been typed.
		if !strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(os.Stderr, "arxi inbox: %q is not an inbox command.\n"+
				"  to act on an item, name the verb first:\n"+
				"    arxi inbox approve %s\n"+
				"    arxi inbox reject  %s --reason \"...\"\n"+
				"    arxi inbox reply   %s \"...\"\n", args[0], args[0], args[0], args[0])
			os.Exit(2)
		}
		cmdInboxList(args)
	}
}

func inboxUsage() {
	fmt.Println(`usage: arxi inbox [--json]
       arxi inbox approve <id>
       arxi inbox reject  <id> --reason <why>
       arxi inbox reply   <id> <text>

Questions an agent asked and cannot continue without.

reject and reply are different acts, on purpose. reject refuses a REQUEST
and its reason reaches the agent as context; reply answers a QUESTION,
where there was nothing to authorize. Collapsing them would force the
agent to guess whether "no" meant not allowed or not that way.`)
}

// cmdInboxList prints the pending questions across every run.
//
// Across every run, and not one named run, because the surface declares `inbox`
// with no run argument and the surface is frozen. That turns out to be the right
// shape anyway: the situation this command is for is "something stopped and I do
// not know what", and a command that required the run id would demand the answer
// as its input.
func cmdInboxList(args []string) {
	c := surface.Lookup("inbox")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi inbox: %v\n", err)
		os.Exit(2)
	}

	runs, err := discoverRuns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi inbox: %v\n", err)
		os.Exit(1)
	}

	type row struct {
		item inbox.Item
		dir  string
	}
	var rows []row
	// unreadable is collected rather than fatal. One damaged run directory must
	// not hide the pending questions of every healthy one -- that would make a
	// single bad directory look exactly like an empty inbox, and the user would
	// go looking for why their run is not blocked when it is.
	var unreadable []string

	for _, dir := range runs {
		r, err := inbox.OpenRun(dir)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		for _, it := range r.List(true) {
			rows = append(rows, row{item: it, dir: dir})
		}
	}

	if vals["json"] == "true" {
		out := make([]map[string]any, 0, len(rows))
		for _, rw := range rows {
			out = append(out, map[string]any{
				"id": rw.item.ID, "run": rw.item.RunID, "dir": rw.dir,
				"agent": rw.item.Agent, "kind": rw.item.Kind,
				"question": rw.item.Question, "on_timeout": rw.item.OnTimeout,
			})
		}
		payload := map[string]any{"items": out}
		if len(unreadable) > 0 {
			payload["unreadable"] = unreadable
		}
		emitJSON(payload)
		return
	}

	if len(rows) == 0 {
		fmt.Println("no pending questions.")
		// Where it looked matters when the answer is "nothing". Otherwise the
		// user cannot tell "no run is blocked" from "you are in the wrong
		// directory", and those have very different next steps.
		fmt.Printf("  looked in %s (%d run%s)\n", runsDir, len(runs), plural(len(runs)))
	} else {
		fmt.Printf("%-9s %-12s %-9s %-14s %s\n", "ID", "RUN", "AGENT", "KIND", "QUESTION")
		for _, rw := range rows {
			fmt.Printf("%-9s %-12s %-9s %-14s %s\n",
				rw.item.ID, truncateCol(rw.item.RunID, 12), truncateCol(rw.item.Agent, 9),
				truncateCol(rw.item.Kind, 14), rw.item.Question)
		}
	}

	for _, u := range unreadable {
		fmt.Fprintf(os.Stderr, "warning: %s\n", u)
	}
}

// cmdInboxAnswer approves, rejects or replies.
func cmdInboxAnswer(verb string, args []string) {
	c := surface.Lookup("inbox", verb)
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi inbox %s: %v\n", verb, err)
		os.Exit(2)
	}

	reply := inbox.Reply{}
	switch verb {
	case "approve":
		reply.Decision = inbox.DecisionApprove
	case "reject":
		reply.Decision = inbox.DecisionReject
		reply.Text = vals["reason"]
	case "reply":
		reply.Decision = inbox.DecisionAnswer
		reply.Text = vals["text"]
	}

	id := vals["id"]

	// The run is found by searching, because the surface gives these verbs an id
	// and nothing else. An id like inbox-1 is minted per run, so the search can
	// legitimately match more than one -- and picking one would authorise a tool
	// in a run the user was not looking at. Ambiguity is refused, and both
	// candidates are named so the user can act on the right one.
	dirs, err := runsHolding(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi inbox %s: %v\n", verb, err)
		os.Exit(1)
	}
	switch len(dirs) {
	case 0:
		fmt.Fprintf(os.Stderr, "arxi inbox %s: no pending question %q in any run "+
			"under %s.\n  see what is waiting: arxi inbox\n", verb, id, runsDir)
		os.Exit(1)
	case 1:
		// The normal case.
	default:
		fmt.Fprintf(os.Stderr, "arxi inbox %s: %q is pending in %d runs, and "+
			"answering the wrong one acts on a run you are not looking at:\n",
			verb, id, len(dirs))
		for _, d := range dirs {
			fmt.Fprintf(os.Stderr, "    %s\n", d)
		}
		fmt.Fprintf(os.Stderr, "  ids are minted per run, so inbox-1 exists in every "+
			"blocked run.\n")
		os.Exit(1)
	}

	// Read the agent BEFORE answering. §20.2 prints "backend unblocked", and
	// after the reply the member is no longer waiting -- so the name has to be
	// taken from the question while the question is still pending.
	who, runID := "", ""
	if r, rerr := inbox.OpenRun(dirs[0]); rerr == nil {
		runID = r.RunID()
		if it, ierr := r.Item(id); ierr == nil {
			who = it.Agent
		}
	}

	ev, err := inbox.Answer(dirs[0], id, reply)
	if err != nil {
		// A usage mistake and an operational failure get different exit codes,
		// because a script that separates them should not file a broken-storage
		// report for a missing --reason.
		code := 1
		if errors.Is(err, inbox.ErrNoSuchItem) || errors.Is(err, inbox.ErrAlreadyAnswered) ||
			strings.Contains(err.Error(), "needs a reason") {
			code = 2
		}
		fmt.Fprintf(os.Stderr, "arxi inbox %s: %v\n", verb, err)
		os.Exit(code)
	}

	// §20.2 prints "approved. backend unblocked (r1 seq 6)". The seq is the
	// useful part: it is where the answer landed in the log, which is what
	// `run replay --until-seq` and any later argument about what happened are
	// anchored to.
	switch {
	case who != "" && runID != "":
		fmt.Printf("%s. %s unblocked (%s seq %d)\n", pastTense(verb), who, runID, ev.Seq)
	case runID != "":
		fmt.Printf("%s. (%s seq %d)\n", pastTense(verb), runID, ev.Seq)
	default:
		fmt.Printf("%s. (seq %d)\n", pastTense(verb), ev.Seq)
	}

	// The run does not continue by itself, and saying so is the difference
	// between a command that worked and a user who concludes it did not. The
	// answer is in the log; something has to fold it and act, and that is a
	// different process from the one that just appended an event.
	fmt.Println("  the answer is in the log. the run resumes when it is next driven.")
}

func pastTense(verb string) string {
	switch verb {
	case "approve":
		return "approved"
	case "reject":
		return "rejected"
	default:
		return "replied"
	}
}

// runsHolding returns the run directories with this id still pending.
//
// Pending, not merely present: a run that already answered inbox-1 is not a
// candidate for answering it again, so an id answered in one run and waiting in
// another resolves cleanly instead of reporting a false ambiguity.
func runsHolding(id string) ([]string, error) {
	runs, err := discoverRuns()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, dir := range runs {
		r, err := inbox.OpenRun(dir)
		if err != nil {
			continue
		}
		for _, it := range r.List(true) {
			if it.ID == id {
				out = append(out, dir)
				break
			}
		}
	}
	return out, nil
}

// discoverRuns lists run directories.
//
// A run directory is one that holds an event log. Requiring that, rather than
// taking every subdirectory, is what stops a stray folder under ./runs from
// being reported as an unreadable run in every listing.
func discoverRuns() ([]string, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Not an error. No runs have been started here, which is an ordinary
			// state and not a broken installation.
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", runsDir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "events.ndjson")); err != nil {
			continue
		}
		out = append(out, dir)
	}
	sort.Strings(out)
	return out, nil
}

func truncateCol(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
