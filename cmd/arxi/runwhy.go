package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi run why <run>` -- the remedy this binary printed and then refused.
//
// # Why this one, and why now
//
// Three places in this codebase tell a user to run `arxi run why <id>`:
// printRunSummary after every unfinished run, printRunShow, and cmdRunUnpause
// when it declines to resume a run that is already running. Following that
// advice verbatim answered:
//
//	arxi run why is declared in the surface but not implemented yet.
//
// A gap of that kind is discovered by the person already in trouble. Their run
// has stopped, the tool has told them what to type, and the thing it told them
// to type does not exist. The README calls this the worst class of gap in the
// project and counts them; this was a fourth, reintroduced by `run show` and
// inherited from `run start`.
//
// # The diagnosis was already built
//
// kernel.Explain walks the wait graph and returns a Why: the cause tree and the
// concrete commands that unblock it. It has been tested since the kernel was
// written. What was missing is the same thing that was missing for `run list`
// and `run show` -- a caller that reads a run off disk and hands it over.
//
// # Why `arxi why <file.json>` still works
//
// The older spelling takes a PATH to a state file, and it is how the reducer's
// purity is demonstrated: `run why` needs no executor, no store, and no run
// directory, only a State. Removing it to make room for run ids would delete
// that demonstration and break testdata/scenarios, so both are accepted and the
// argument is classified by what it is rather than by which verb was typed.
//
// Deciding by "does this name a run directory" and falling back to "treat it as
// a file" means a user never has to know which form a command wants. It is the
// same judgement resolveRunDir already makes for paths versus ids.

func cmdRunWhy(args []string) {
	c := surface.Lookup("run", "why")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run why: %v\n\n"+
			"usage: arxi run why <run>\n", err)
		os.Exit(2)
	}

	arg := strings.TrimSpace(vals["run"])
	if arg == "" {
		fmt.Fprintf(os.Stderr, "arxi run why: which run?\n"+
			"  usage: arxi run why <run>\n"+
			"  see what exists: arxi run list\n")
		os.Exit(2)
	}

	st, cfg, err := whySubject(arg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run why: %v\n", err)
		os.Exit(1)
	}

	emitWhy(kernel.Explain(st, cfg), vals["json"] == "true")
}

// whySubject resolves the argument to a state, from a run or from a file.
//
// The run directory is tried FIRST, and the order matters. A file named after a
// run id is possible but strange; a run id being interpreted as a missing file
// is the case that actually happens, because every message in this binary that
// mentions `run why` prints a run id after it. Getting that order wrong is what
// `arxi why <id>` already does today, and its answer -- "open <id>: no such
// file or directory" -- describes the user's run as a missing file.
func whySubject(arg string) (kernel.State, kernel.Config, error) {
	dir := resolveRunDir(arg)
	if _, err := os.Stat(dir); err == nil {
		st, cfg, _, ferr := foldRunDir(dir)
		return st, cfg, ferr
	}

	st, cfg, err := readStateFile(arg)
	if err != nil {
		// Both readings failed, so both are reported. Saying only "no such
		// file" to somebody who passed a run id sends them looking for a file
		// they never meant to name; saying only "no such run" to somebody who
		// passed a typo'd path hides the spelling mistake. The user knows which
		// of the two they meant, and this is the one place that does not.
		return kernel.State{}, kernel.Config{}, fmt.Errorf(
			"%q is neither a run nor a state file.\n"+
				"  as a run:  %s does not exist\n"+
				"  as a file: %v\n"+
				"  see what exists: arxi run list", arg, dir, err)
	}
	return st, cfg, nil
}

// readStateFile reads the {"state":...,"config":...} or bare-State form.
//
// Lifted from cmdWhy rather than reimplemented, so the two spellings cannot
// come to disagree about what a scenario file looks like.
func readStateFile(path string) (kernel.State, kernel.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return kernel.State{}, kernel.Config{}, err
	}

	var wrap struct {
		State  *kernel.State  `json:"state"`
		Config *kernel.Config `json:"config"`
	}
	var st kernel.State
	var cfg kernel.Config

	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.State != nil {
		st = *wrap.State
		if wrap.Config != nil {
			cfg = *wrap.Config
		}
		return st, cfg, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return kernel.State{}, kernel.Config{}, fmt.Errorf(
			"%s is not a state file: %w", path, err)
	}
	return st, cfg, nil
}

// emitWhy renders the diagnosis.
//
// Shared with `arxi why` so the two spellings produce identical output. A
// second renderer would drift, and the first thing to drift would be the
// remedies -- the part a user copies and runs.
func emitWhy(w kernel.Why, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(w); err != nil {
			fatal(err)
		}
		return
	}

	for _, l := range w.Lines {
		if l.Depth == 0 {
			fmt.Println(l.Text)
			continue
		}
		fmt.Printf("%s└─ %s\n", strings.Repeat("   ", l.Depth-1), l.Text)
	}
	if len(w.Fix) > 0 {
		fmt.Println("\npossible remedies:")
		for _, f := range w.Fix {
			fmt.Printf("  $ %s\n", f)
		}
	}
}
