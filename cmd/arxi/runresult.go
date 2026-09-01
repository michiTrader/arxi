package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi run result <run>` -- the answer, and an exit code a script can gate on.
//
// # What the design promises, and what the log can actually deliver
//
// docs/design/20-use-cases.md §20.1 shows this verb printing "3 risks found: (1)
// token comparison is not constant-time ...", and §20.7 explains where that text
// is supposed to come from: "result_from defaults to last_submit, so the result of
// a run is the last submission rather than a concatenation of everything said."
//
// MEASURED, on a real log produced by this binary, before a line of this file was
// written. A completed --sim run ends:
//
//	19 stage.submitted frontend {"agent":"frontend","simulated":true,"stage":"review"}
//	21 run.result              {"result_from":"last_submit","summary":"all stages completed"}
//
// So last_submit resolves to a real event, and that event carries no text. It
// carries who submitted and to which stage, and nothing else -- internal/exec is
// the only producer of stage.submitted (internal/exec/fake.go:288) and it writes
// three keys. The reducer keeps one string, State.Result, and the two places that
// set it on success write CONSTANTS: "all stages completed" (decide.go:489) and
// "last stage expired, advancing" (decide.go:642).
//
// There is therefore no agent-authored answer anywhere in a run directory today,
// and no field in kernel.State that could hold one. This command prints what is
// recorded and says, in one line, that the summary is the reducer's rather than the
// agent's. The alternative was to print "all stages completed" alone and let it
// pass for the deliverable, which is the exact failure §10.7 calls the worst kind:
// a number, or a sentence, that is still displayed and is simply untrue. A user who
// delegated a code review and reads "all stages completed" as the review has been
// misled by their tooling, not by the model.
//
// The last submission is printed BESIDE the summary for that reason: it names the
// member whose answer this would be, so the reader knows who to go and read. When
// submissions eventually carry text, resultText picks it up and the note about the
// reducer disappears -- the shape is written against that day, and the tests pin
// both halves.
//
// # Three exit codes, because "no result yet" is not "no result"
//
// This is the one run verb whose output is meant to be consumed rather than read,
// which makes the exit code part of the answer. `arxi run result r1 && deploy` must
// not deploy because a run is still thinking, and it must not deploy because a run
// was cancelled. Neither of those is a failure of this command, so neither can be
// exit 1 -- 1 already means the command could not do its job (no such run,
// unreadable log) and a script that separates them should not file a broken-storage
// report for a run that is merely still working.
//
//	0  terminal and succeeded: the result is on stdout
//	3  not terminal yet: nothing has been decided, poll again or `run why`
//	4  terminal and NOT succeeded: failed, cancelled or expired
//	1  the run could not be read at all
//	2  the invocation was wrong
//
// With --json the negative answers are JSON on stdout too, not prose on stderr.
// A machine that asked in JSON and got a sentence has to parse English to find out
// that it should retry, and the retry is the whole reason it asked.
const runResultUsage = "usage: arxi run result <run> [--json]\n" +
	"  exit 0 the run succeeded · 3 no result yet · 4 it ended without succeeding\n" +
	"  see what exists: arxi run list\n"

// Exit codes are named because the tests assert on them and a bare 3 in an
// assertion is unreadable a month later.
const (
	exitResultNotYet       = 3
	exitResultUnsuccessful = 4
)

// resultView is everything the two renderers need, resolved once.
//
// Projected up front for the same reason runListRow is: the human and the JSON
// output must not be able to disagree about whether a result exists. A version
// where each renderer re-derived it from the State is one edit away from printing
// a summary while reporting has_result false.
type resultView struct {
	id         string
	dir        string
	st         kernel.State
	simulated  bool
	text       string     // the recorded result, empty if there is none
	textFrom   string     // which record the text came out of, for the note
	resultFrom string     // result_from as recorded, or as configured
	last       *submitRef // the submission result_from points at
	cancelled  string     // reason off run.cancelled, which the reducer drops
}

// submitRef is one stage.submitted, reduced to what is worth printing.
type submitRef struct {
	agent string
	stage string
	seq   int64
}

func cmdRunResult(args []string) {
	c := surface.Lookup("run", "result")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run result: %v\n\n%s", err, runResultUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run result: which run?\n\n"+runResultUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// foldRunDirEvents rather than foldRunDir: the two facts this command needs
	// beyond the State -- the last submission and the cancel reason -- exist only
	// in the events, and reading the log twice would let them come from a different
	// moment than the status printed beside them.
	st, cfg, simulated, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run result: %v\n", err)
		os.Exit(1)
	}

	id := st.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	v := resultView{
		id: id, dir: dir, st: st, simulated: simulated,
		resultFrom: resultFromOf(events, cfg),
		last:       lastSubmission(events),
		cancelled:  cancelReason(events),
	}
	v.text, v.textFrom = resultText(v, events)

	if vals["json"] == "true" {
		emitJSON(resultPayload(v))
		os.Exit(resultExitCode(st.Status))
	}

	printRunResult(v)
	os.Exit(resultExitCode(st.Status))
}

// resultExitCode maps the status onto the contract documented above.
//
// Terminal() is asked rather than listing the four statuses here, so a status
// added to the kernel cannot silently become "not finished yet" in a script that
// polls this command forever.
func resultExitCode(s kernel.RunStatus) int {
	switch {
	case s == kernel.StatusSucceeded:
		return 0
	case s.Terminal():
		return exitResultUnsuccessful
	}
	return exitResultNotYet
}

// resultText returns the recorded result and where it was recorded.
//
// # Why it does not go looking for the submission's text
//
// The obvious implementation reads the last stage.submitted and prints its text.
// spec/events.md:59 declares the payload of stage.submitted as "— (the actor is
// whoever submitted)": no keys at all. There is no field for an answer, so reading
// e.Str("text") here would be inventing a schema in a renderer and hoping a
// producer someday agrees with it -- and until one did, the invented key would
// read as an empty result rather than as a missing feature. The same discipline
// the registry gets: the catalogue is the contract, and a command does not extend
// it by guessing.
//
// So the text is State.Result, plus one thing the reducer throws away. run.cancelled
// declares `reason?` (spec/events.md:41) and applyEvent sets Status and reads no key
// from it (internal/kernel/decide.go:59), so a cancelled run has an empty Result
// while the reason it was cancelled sits in the log one line above. Printing "no
// result" for it would be a lie the log can disprove.
func resultText(v resultView, events []kernel.Event) (string, string) {
	if v.st.Result != "" {
		// A run.result event means the summary is that event's; without one, the
		// text was written straight onto the State by a failure path (a stage that
		// expired, a question nobody answered). Different provenance, and the note
		// that quotes it should not claim an event that is not there.
		for _, e := range events {
			if e.Type == kernel.RunResult {
				return v.st.Result, "run.result"
			}
		}
		return v.st.Result, "reducer"
	}
	if v.cancelled != "" {
		return v.cancelled, "run.cancelled"
	}
	return "", ""
}

// lastSubmission finds the stage.submitted that `result_from: last_submit` names.
//
// Last by position in the log rather than by seq, because the log IS the order --
// seq is assigned by the appender and a scan that sorted by it would be trusting a
// number over the sequence of bytes that produced it. They agree in every log this
// binary writes; when they do not, the file is what happened.
func lastSubmission(events []kernel.Event) *submitRef {
	var out *submitRef
	for _, e := range events {
		if e.Type != kernel.StageSubmitted {
			continue
		}
		who := e.Actor
		if who == "" {
			who = e.Str("agent")
		}
		out = &submitRef{agent: who, stage: e.Str("stage"), seq: e.Seq}
	}
	return out
}

// resultFromOf prefers what the log recorded over what the config says now.
//
// run.result carries result_from (spec/events.md:44, written at decide.go:490), and
// that is the value that was in force when this run finished. The frozen
// blueprint.snapshot.yaml normally agrees -- but the snapshot can be absent, in
// which case foldRunDirEvents hands back a zero Config rather than inventing one,
// and then the event is the only witness. Preferring the event also means a run
// finished under `result_from: first_submit` still says so after the blueprint on
// disk was changed.
//
// The last fallback is the word "last_submit" only because kernel.Config normalises
// the empty string to it (internal/kernel/config.go:121). Repeating the default
// here would be a second place for it to live; this reads it off the normalised
// Config and falls back to "unset" rather than asserting a default it did not see.
func resultFromOf(events []kernel.Event, cfg kernel.Config) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == kernel.RunResult {
			if rf := events[i].Str("result_from"); rf != "" {
				return rf
			}
			break
		}
	}
	if cfg.ResultFrom != "" {
		return cfg.ResultFrom
	}
	return "unset"
}

// cancelReason reads the reason the reducer discards.
//
// The FIRST run.cancelled wins, not the last. A terminal run ignores every event
// that arrives after it (kernel.applyEvent, pinned by TestTerminalRunIgnoresLateEvents),
// so a second cancel changes nothing about the State and quoting its reason would
// attribute the run's end to a message that had no effect on it.
func cancelReason(events []kernel.Event) string {
	for _, e := range events {
		if e.Type == kernel.RunCancelled {
			return e.Str("reason")
		}
	}
	return ""
}

// resultPayload is the machine reading, and it says out loud what it does not have.
//
// has_result is a separate field from result rather than being inferred from an
// empty string, because those are two different answers to a script: "" can mean
// the run has not finished, or that it finished and recorded nothing. The exit code
// says which, and a caller piping to jq should not have to shell out to find that
// out.
//
// result_is_from_agent is present and false on every run this binary can currently
// produce. A field that is always false looks like dead weight; it is the one field
// that stops a consumer treating "all stages completed" as a deliverable, and the
// day a submission carries text it becomes true without the consumer changing.
func resultPayload(v resultView) map[string]any {
	out := map[string]any{
		"run":         v.id,
		"dir":         v.dir,
		"status":      string(v.st.Status),
		"seq":         v.st.Seq,
		"simulated":   v.simulated,
		"has_result":  v.text != "",
		"result":      v.text,
		"result_from": v.resultFrom,
		// Always false today: nothing in the event catalogue carries submitted
		// text, so nothing this command can read was written by an agent.
		"result_is_from_agent": false,
	}
	if v.textFrom != "" {
		out["result_recorded_in"] = v.textFrom
	}
	if v.last != nil {
		out["last_submit"] = map[string]any{
			"agent": v.last.agent, "stage": v.last.stage, "seq": v.last.seq,
		}
	}
	return out
}

// printRunResult writes the answer first and the provenance under it.
//
// The result text is on its own line, unindented and unadorned, and everything that
// is ABOUT the result is indented beneath it -- the same convention the rest of the
// binary uses for detail under a headline. That makes the answer greppable, not
// pipe-clean: `run result r1 > answer.txt` on a succeeded run captures the headline
// and the provenance too, and a caller that wants only the text should ask with
// --json and read .result. Measured rather than assumed: the file is 414 bytes for a
// 20-byte summary.
//
// The empty-stdout rule below is not weakened by that. It exists for the callers who
// DO redirect this, and for them an explanation of why there is no answer is far
// worse than a header they can skip: the header is obviously not a review, and "run
// r1 has no result yet" reads exactly like one.
func printRunResult(v resultView) {
	if !v.st.Status.Terminal() {
		// To stderr, and stdout stays empty. `arxi run result r1 > answer.txt` on a
		// run that has not finished must leave answer.txt empty rather than writing
		// an explanation into it -- a file that says "no result yet" is worse than an
		// empty one, because the next step in the pipeline reads it as content.
		fmt.Fprintf(os.Stderr, "run %s has no result yet: it is %s (seq %d).\n",
			v.id, v.st.Status, v.st.Seq)
		fmt.Fprintf(os.Stderr,
			"  a result is recorded when the last stage resolves, and when a run\n"+
				"  fails, expires or is cancelled.\n"+
				"  what it is waiting on: arxi run why %s\n"+
				"  exit %d, which is how a poller tells \"not yet\" from \"no such run\".\n",
			v.id, exitResultNotYet)
		return
	}

	sim := ""
	if v.simulated {
		// Loudest on this verb of all of them. Every other view puts a dollar figure
		// beside the marker, so a reader who misses it misreads a number; here the
		// output IS the deliverable, and a simulated deliverable that does not
		// announce itself is indistinguishable from work that was done.
		sim = "  [simulated]"
	}
	fmt.Printf("run %s %s (seq %d)%s\n\n", v.id, v.st.Status, v.st.Seq, sim)

	if v.text != "" {
		fmt.Printf("%s\n", v.text)
	} else {
		fmt.Printf("(no result recorded, though the run is %s -- see arxi run why %s)\n",
			v.st.Status, v.id)
	}

	printResultProvenance(v)
}

// submitDesc names a submission the way every line here refers to one.
//
// One spelling rather than three format strings, because the stage is optional and
// the variant without it was the arm that got edited alone.
func submitDesc(s *submitRef) string {
	if s.stage != "" {
		return fmt.Sprintf("%s at seq %d (stage %s)", s.agent, s.seq, s.stage)
	}
	return fmt.Sprintf("%s at seq %d", s.agent, s.seq)
}

// printResultProvenance says where the text came from, and where it did not.
//
// Separate from printRunResult so the note has one home rather than being repeated
// per status. Its content is the finding this command exists to be honest about, and
// a note that a reader learns to skip is one they will skip on the run where it
// mattered -- so it names the member, and it does not moralise.
func printResultProvenance(v resultView) {
	fmt.Println()

	switch {
	case v.last == nil:
		// No submission at all is the normal case for a run that failed, expired or
		// was cancelled before any stage resolved. Saying so is more useful than
		// printing "result_from: last_submit" with nothing on the other side of the
		// arrow, which reads as a lookup that broke.
		fmt.Printf("  result_from: %s, and nothing was ever submitted\n", v.resultFrom)

	case v.resultFrom != "last_submit":
		// Found by writing the test for a run recorded under first_submit: the
		// arrow was drawn from whatever result_from said to the LAST submission,
		// because that is the only one this file looks for. On a two-submission run
		// that prints `result_from: first_submit → frontend`, naming the wrong
		// member with a straight face -- and the reader has no way to tell, since
		// both halves of the line are true separately.
		//
		// Nothing selects by result_from anywhere yet (decide.go:490 records the
		// value and no code reads it back), so the honest line is the one that says
		// which fact it actually has.
		fmt.Printf("  result_from: %s, which nothing resolves yet; the last\n"+
			"    submission was %s\n", v.resultFrom, submitDesc(v.last))

	default:
		fmt.Printf("  result_from: %s → %s\n", v.resultFrom, submitDesc(v.last))
	}

	if v.textFrom != "" {
		fmt.Printf("  recorded in: %s\n", v.textFrom)
	}

	if v.last != nil {
		// Only when there IS a submission that result_from points at. Without one
		// there is nothing being misattributed, and the note would be a lecture.
		fmt.Printf("  that text is the run's own record, not %s's answer:\n"+
			"    stage.submitted declares no payload (spec/events.md), so no\n"+
			"    submission in this log carries any text for result_from to select.\n",
			v.last.agent)
	}

	fmt.Printf("  the whole run: arxi run show %s\n", v.id)
}
