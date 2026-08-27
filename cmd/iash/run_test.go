package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The flag parsing of `run start` is worth testing on its own because every
// mistake it can make is silent. A misparsed budget starts a run with the wrong
// ceiling; a misparsed prompt starts one with the wrong objective. Neither
// announces itself, and both cost money before anybody notices.

// --budget has no default and the BINARY must enforce that, not just the
// registry. The surface declares it required and TestBudgetIsMandatory proves
// the declaration; if the entry point then defaulted it, the registry would be
// describing a promise the binary does not keep and the user would meet their
// real ceiling on the invoice.
func TestRunStartRefusesWithoutABudget(t *testing.T) {
	_, err := parseStartFlags([]string{"team.yaml", "do the thing", "--sim"})
	if err == nil {
		t.Fatal("run start accepted no --budget.\n" +
			"  consequence: the run gets a spend ceiling the user never chose, " +
			"which they discover from the bill. Every other ceiling in iash can " +
			"default; this is the one that cannot.")
	}
	if !strings.Contains(err.Error(), "--budget") {
		t.Errorf("the error does not name --budget, so the user has to guess "+
			"which flag is missing: %v", err)
	}
}

// A zero or negative budget is refused rather than read as "no limit". Zero is
// the plausible typo, and treating it as unlimited would make the most cautious
// number the user could type the most dangerous one.
func TestRunStartRefusesANonPositiveBudget(t *testing.T) {
	for _, v := range []string{"0", "-1", "0.00"} {
		if _, err := parseStartFlags([]string{"t.yaml", "p", "--budget", v}); err == nil {
			t.Errorf("--budget %s was accepted.\n"+
				"  consequence: if zero means unlimited, the most cautious number "+
				"the user could type is the most dangerous one. A run that may not "+
				"spend anything also cannot take a single turn, so it is never what "+
				"was meant.", v)
		}
	}
}

// --flag=value and --flag value must both work, because supporting only one makes
// the other a silent misparse rather than an error: `--budget=2.00` consumed as a
// positional becomes part of the PROMPT, so the run starts with no budget and a
// nonsense objective.
func TestRunStartAcceptsBothFlagSpellings(t *testing.T) {
	inline, err := parseStartFlags([]string{"t.yaml", "p", "--budget=2.50", "--max-turns=7"})
	if err != nil {
		t.Fatalf("--flag=value was rejected: %v", err)
	}
	spaced, err := parseStartFlags([]string{"t.yaml", "p", "--budget", "2.50", "--max-turns", "7"})
	if err != nil {
		t.Fatalf("--flag value was rejected: %v", err)
	}

	if inline.budget != spaced.budget || inline.maxTurns != spaced.maxTurns {
		t.Errorf("the two spellings disagree: inline (%.2f, %d) vs spaced (%.2f, %d).\n"+
			"  consequence: one of them is being swallowed into the prompt, so the "+
			"run silently starts with a different ceiling than the one typed",
			inline.budget, inline.maxTurns, spaced.budget, spaced.maxTurns)
	}
	if inline.budget != 2.50 || inline.maxTurns != 7 {
		t.Errorf("parsed budget=%.2f turns=%d, expected 2.50 and 7",
			inline.budget, inline.maxTurns)
	}
}

// A prompt is required. A run with no objective has nothing to put in the agents'
// context, so it would pay for turns asking models to work on nothing.
func TestRunStartRequiresAnActorAndAPrompt(t *testing.T) {
	if _, err := parseStartFlags([]string{"--budget", "1"}); err == nil {
		t.Error("no actor was accepted: there is nothing to run")
	}
	if _, err := parseStartFlags([]string{"t.yaml", "--budget", "1"}); err == nil {
		t.Error("no prompt was accepted.\n" +
			"  consequence: the run pays for turns whose context contains no " +
			"objective, so every agent is asked to work on nothing")
	}
}

// A multi-word prompt survives an unquoted shell. Joining the tail rather than
// taking one word is what keeps `run start t.yaml fix the bug --budget 1` from
// briefing the agents on the single word "fix".
func TestRunStartKeepsTheWholePrompt(t *testing.T) {
	f, err := parseStartFlags([]string{"t.yaml", "fix", "the", "failing", "test",
		"--budget", "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.prompt != "fix the failing test" {
		t.Errorf("prompt = %q, expected the whole tail.\n"+
			"  consequence: an unquoted prompt is truncated to its first word and "+
			"the agents are briefed on a fragment", f.prompt)
	}
}

// An unknown flag is an error rather than a positional. Swallowing it would fold
// a typo into the prompt: `--budgt 2.00` becomes part of the objective and the
// error the user then gets names --budget, a flag they did type.
func TestRunStartRejectsUnknownFlags(t *testing.T) {
	if _, err := parseStartFlags([]string{"t.yaml", "p", "--budget", "1",
		"--budgt", "2"}); err == nil {
		t.Fatal("an unknown flag was swallowed as a positional.\n" +
			"  consequence: a mistyped flag silently becomes part of the prompt, " +
			"and the error the user gets names a different flag than the one they " +
			"got wrong")
	}
}

// An invalid --workspace is caught at parse time. The workspace is the isolation
// boundary between agents that write files, so a value nobody validated is a
// value that silently resolves to no isolation at all.
func TestRunStartValidatesTheWorkspace(t *testing.T) {
	if _, err := parseStartFlags([]string{"t.yaml", "p", "--budget", "1",
		"--workspace", "wroktree"}); err == nil {
		t.Fatal("a misspelled --workspace was accepted.\n" +
			"  consequence: two agents that both write files end up sharing one " +
			"directory and overwrite each other, which is the exact hole the " +
			"worktree default exists to close")
	}
	for _, ok := range []string{"shared", "worktree", "copy", "none", "auto"} {
		if _, err := parseStartFlags([]string{"t.yaml", "p", "--budget", "1",
			"--workspace", ok}); err != nil {
			t.Errorf("--workspace %s is declared in the surface but was rejected: %v",
				ok, err)
		}
	}
}

// Run ids must be unique even when minted back to back, because the id IS the run
// directory. Two runs that mint the same one open the same log and append their
// events into each other: both become unreplayable and neither can be told apart
// afterwards.
//
// This test was written expecting to pass and did not. The first newRunID used
// millisecond resolution alone and repeated on the second iteration — and a tight
// loop is the honest test here, because starting runs back to back is what
// scripts and CI jobs do.
func TestRunIDsDoNotCollide(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := newRunID()
		if seen[id] {
			t.Fatalf("newRunID returned %q twice within %d mints.\n"+
				"  consequence: the id is the run directory, so two runs share one "+
				"log and append into each other. Both are unreplayable afterwards "+
				"and neither can be separated from the other, so the work and the "+
				"audit trail are lost together.", id, n)
		}
		seen[id] = true
	}
}

// Ids must still SORT chronologically, which is the only reason a timestamp is in
// them. Randomness alone would be unique and tell nobody when the run happened,
// leaving `run list` an unordered pile.
func TestRunIDsSortChronologically(t *testing.T) {
	first := newRunID()
	time.Sleep(2 * time.Millisecond)
	second := newRunID()

	if first >= second {
		t.Errorf("ids do not sort by time: %q >= %q.\n"+
			"  consequence: `run list` cannot show runs oldest-first without "+
			"parsing them, and the id stops carrying the one piece of context "+
			"that makes it readable to a human", first, second)
	}
}

// workspaceNote must announce an INFERRED workspace as inferred. §20.1 prints
// `workspace auto→none` deliberately: a value the user never typed has to say it
// was resolved, or the first time the isolation matters they will assume they
// chose it and go looking for the bug elsewhere.
func TestWorkspaceNoteMarksResolvedValues(t *testing.T) {
	if got := workspaceNote("auto", "worktree"); got != "auto→worktree" {
		t.Errorf("workspaceNote(auto, worktree) = %q, expected the arrow form.\n"+
			"  consequence: an inferred isolation boundary looks like one the user "+
			"picked, so a surprising default is never questioned", got)
	}
	if got := workspaceNote("", "none"); got != "auto→none" {
		t.Errorf("an unset workspace must also read as resolved, got %q", got)
	}
	if got := workspaceNote("shared", "shared"); got != "shared" {
		t.Errorf("an explicit workspace must not claim to be inferred, got %q", got)
	}
}

// stoppedNote must translate every stop reason the loop can report. An unmapped
// one would print the raw constant, and "idle" tells the user nothing about
// whether to wait, answer something, or look for a bug.
func TestStoppedNoteExplainsEveryStopReason(t *testing.T) {
	for _, by := range []string{"terminal", "idle", "cancelled", ""} {
		got := stoppedNote(by)
		if got == "" || got == by {
			t.Errorf("stoppedNote(%q) = %q: the stop reason reaches the user "+
				"untranslated, so they are told the loop's internal vocabulary "+
				"instead of what to do next", by, got)
		}
	}
}

// The blueprint has to be frozen into the run directory, and the frozen copy must
// not track later edits to the source. Fold needs the Config the events were
// decided against, so a mutated blueprint makes the run explain itself with rules
// that never applied — worse than refusing to explain itself.
func TestFrozenBlueprintDoesNotTrackTheSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "team.yaml")
	body := "name: solo\nmembers:\n  - {name: a, tools: [read]}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "run1")
	f, err := parseStartFlags([]string{src, "do it", "--budget", "1", "--sim",
		"--run-id", "r1", "--dir", runDir})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if f.dir != runDir || f.runID != "r1" {
		t.Fatalf("--dir/--run-id were not honoured: dir=%q id=%q", f.dir, f.runID)
	}

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(runDir, "blueprint.snapshot.yaml")
	if err := os.WriteFile(snap, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mutate the source the way a user would between runs.
	if err := os.WriteFile(src, []byte("name: solo\nmembers: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("the frozen blueprint changed when the source did.\n" +
			"  consequence: Fold replays the log against a Config the events were " +
			"never decided against, so `run why` and `replay` describe rules that " +
			"never applied. A run that explains itself wrongly is worse than one " +
			"that cannot explain itself at all.")
	}
}
