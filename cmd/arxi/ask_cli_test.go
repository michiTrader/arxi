package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi -p "..."`, exercised as a process.
//
// # Why this file exists next to ask_test.go
//
// The helpers next door are pure and tested as such. The one thing no unit test in
// this package can reach is the wiring: cmdAsk is called from a single `if` after
// main's dispatch switch, and looksLikeAsk answering "yes, a prompt" proves nothing
// about whether anybody asks it. That `if` is also the fragile part -- it lives
// after twenty cases, and moving it above them would turn `arxi --version` into a
// prompt with no prompt in it.
//
// # Why every run here is simulated
//
// The point of the path is to call a model, and a test suite must not. --sim leaves
// the model call itself untested and still proves the four things that break
// silently: the dispatch, the ceiling and where it was read from, which stream each
// line went to, and the exit code a pipe reads.
//
// arxiStreams and not arxi() for the reason runresult_cli_test.go gives: stdout
// belongs to the answer here, and a combined-output test cannot tell a diagnostic on
// stderr from the same sentence printed into the middle of somebody's answer file.

// The environment is set through t.Setenv rather than through a helper of its own,
// so the child inherits it and Go puts it back afterwards. Setting it to empty is
// deliberate in the tests that assert the built-in ceiling: a developer with
// ARXI_BUDGET exported in their own shell would otherwise change the number under
// test, or fail the suite outright with a value the binary is right to refuse.

// TestALeadingFlagRunsTheShortPath is the wiring, end to end.
func TestALeadingFlagRunsTheShortPath(t *testing.T) {
	t.Setenv(askBudgetEnv, "")
	dir := workdir(t)

	out, errs, code := arxiStreams(t, dir,
		"-p", "hola mundo", "-m", "openai/gpt-4o-mini", "--sim")
	if code != 0 {
		t.Fatalf("`arxi -p \"hola mundo\" -m ... --sim` exited %d.\n"+
			"  consequence: either main never reached cmdAsk -- the leading flag fell "+
			"through to the not-implemented refusal -- or the run did not finish.\n"+
			"stdout: %q\nstderr: %s", code, out, errs)
	}

	// The ceiling, its source, and the fact that no model was called. All three are
	// printed before the first turn opens, which is the last moment a line about this
	// run is still true if a live call hangs.
	for _, want := range []string{
		"ceiling " + usd(askBudgetDefault) + " USD",
		askBudgetEnv, // the source note, which is also where the user reads how to change it
		"--sim, no model calls",
		"no agent named " + defaultAgentName + " is stored",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr does not mention %q, and every one of those is a thing the "+
				"user is owed before the money moves:\n%s", want, errs)
		}
	}

	// Not one diagnostic on stdout. Asserted as "no arxi: prefix" and not as an empty
	// stdout, because exec.Fake records no reply text today and may later: the promise
	// is that the answer is alone in there, not that there is never an answer.
	if strings.Contains(out, "arxi:") {
		t.Errorf("a diagnostic reached stdout:\n%s\n"+
			"  consequence: `arxi -p \"...\" > answer.txt` leaves the ceiling and the "+
			"spend in the file, and whatever reads that file next reads them as the "+
			"answer.", out)
	}

	// The printed id names the directory the log is in. That id is quoted back by the
	// refusal below as `arxi run why <id>`, so an id that resolves to nothing would be
	// a dead end printed in the one place a stuck user looks.
	id := askRunID(errs)
	if id == "" {
		t.Fatalf("no run id was printed:\n%s", errs)
	}
	if _, err := os.Stat(filepath.Join(runDirOf(dir, id), "events.ndjson")); err != nil {
		t.Errorf("the run printed as %s has no log: %v", id, err)
	}

	// Nothing was stored. The one-off exists for the length of the run and not after
	// it, or a question asked with --model leaves a team behind for the user to find
	// later and wonder who wrote it.
	if _, err := os.Stat(filepath.Join(dir, "agents")); !os.IsNotExist(err) {
		t.Errorf("agents/ was created (err=%v), so the one-off was persisted", err)
	}
}

// askRunID pulls the id out of the ceiling line, which is where the user reads it.
func askRunID(stderr string) string {
	const marker = ", run "
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.LastIndex(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):])
		}
	}
	return ""
}

// The first thing a new user meets on a machine with a provider and no agent, and the
// exit code a script sees. Nothing on stdout: a refusal written there is
// indistinguishable from an answer to whatever reads the pipe next.
func TestTheShortPathRefusesWithAnEmptyPipe(t *testing.T) {
	t.Setenv(askBudgetEnv, "")
	dir := workdir(t)

	out, errs, code := arxiStreams(t, dir, "-p", "hola")
	if code != 2 {
		t.Fatalf("no stored agent and no --model exited %d, want 2\nstderr: %s", code, errs)
	}
	if out != "" {
		t.Errorf("the refusal put %q on stdout.\n"+
			"  consequence: `arxi -p \"...\" > answer.txt` writes the complaint into the "+
			"file as though it were the reply.", out)
	}
	for _, want := range []string{
		"arxi agent create " + defaultAgentName,
		"--model",
		"arxi model list",
		"arxi provider add",
		"on purpose",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal does not mention %q.\n"+
				"  consequence: this is the first thing a new user sees, and every fix it "+
				"leaves out is one they have to find elsewhere:\n%s", want, errs)
		}
	}
	// And it refused before starting anything, or `arxi run list` grows an entry for a
	// run that never had a model to call.
	if _, err := os.Stat(filepath.Join(dir, "runs")); !os.IsNotExist(err) {
		t.Errorf("a refused ask left runs/ behind (err=%v)", err)
	}
}

// The ceiling is read from the environment of the process. askBudget's own tests are
// pure and prove the parse; this proves the variable is consulted at all, which is the
// half of it that a pure test cannot reach.
func TestTheCeilingIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv(askBudgetEnv, "0.25")

	_, errs, code := arxiStreams(t, workdir(t), "-p", "hola", "-m", "p/m", "--sim")
	if code != 0 {
		t.Fatalf("%s=0.25 exited %d\nstderr: %s", askBudgetEnv, code, errs)
	}
	if !strings.Contains(errs, "ceiling 0.25 USD (from "+askBudgetEnv+")") {
		t.Errorf("the printed line does not credit %s for the 0.25 it set.\n"+
			"  consequence: the ceiling is honoured and attributed to something else, so "+
			"the user cannot tell which of their settings is in force:\n%s",
			askBudgetEnv, errs)
	}

	// A ceiling it cannot read stops the run before the run exists, which is the only
	// useful moment to stop it: after executeRun the first turn may already have been
	// taken and paid for.
	t.Setenv(askBudgetEnv, "0,50")
	dir := workdir(t)
	out, errs, code := arxiStreams(t, dir, "-p", "hola", "-m", "p/m", "--sim")
	if code != 2 {
		t.Errorf("%s=0,50 exited %d, want 2.\n"+
			"  consequence: a typo in a spend ceiling becomes the default it was written "+
			"to replace, and the difference arrives on the invoice.\nstderr: %s",
			askBudgetEnv, code, errs)
	}
	if out != "" {
		t.Errorf("the refusal put %q on stdout", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); !os.IsNotExist(err) {
		t.Errorf("an unreadable ceiling still started a run (err=%v)", err)
	}
}

// The two flags that are not prompts. Both are answered by main's switch above the
// hook and refused by looksLikeAsk as well, and the value of testing them through the
// binary is that the two layers cannot both be removed by accident.
//
// --help doubles as the discoverability check. A short form nobody can find is a short
// form nobody uses, and the whole argument for this path is the first five minutes.
func TestTheShortPathDoesNotSwallowVersionOrHelp(t *testing.T) {
	t.Setenv(askBudgetEnv, "")
	dir := workdir(t)

	out, errs, code := arxiStreams(t, dir, "--version")
	if code != 0 || !strings.HasPrefix(out, "arxi ") || !strings.Contains(out, "surface v") {
		t.Errorf("`arxi --version` exited %d saying %q.\n"+
			"  consequence: it was taken as a prompt, so the one command that has to work "+
			"before anything is configured starts a run instead.\nstderr: %s",
			code, out, errs)
	}

	out, errs, code = arxiStreams(t, dir, "--help")
	if code != 0 {
		t.Fatalf("`arxi --help` exited %d\nstderr: %s", code, errs)
	}
	for _, want := range []string{"arxi -p", "THE SHORT FORM", defaultAgentName} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage screen does not mention %q, so the short form exists and "+
				"nobody is told:\n%s", want, out)
		}
	}
}
