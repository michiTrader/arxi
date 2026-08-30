// Package internal_test verifies the architectural boundaries with the compiler
// as a witness, not with the good will of whoever writes the next commit.
//
// This is the concrete answer to the concession made in ADR-0007: Rust
// guarantees more things in the type system, but Go lets us inspect the import
// graph with `go list`, and that is a guarantee Rust does NOT give by default.
// If the kernel imports net/http, this test fails and explains why it is wrong.
package internal_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/michiTrader/iash/"

type pkgInfo struct {
	ImportPath string
	Imports    []string
	Deps       []string
}

func list(t *testing.T, pkg string) pkgInfo {
	t.Helper()
	out, err := exec.Command("go", "list", "-json", pkg).Output()
	if err != nil {
		if _, err2 := exec.LookPath("go"); err2 != nil {
			t.Skip("go is not in the PATH")
		}
		// If go does exist and still fails, it is a real error: skipping here
		// would turn this test into decoration.
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var p pkgInfo
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ownClosure returns only the PROJECT packages in the dependency closure.
//
// This function exists because of a real false positive: the first version used
// the full `go list -deps`, and the closure of `fmt` includes os, time, syscall
// and half the runtime. The test failed claiming the kernel imported `os` when
// all it did was import `fmt`. What matters is what the package imports
// DIRECTLY, and which of our own packages it drags along.
func ownClosure(t *testing.T, pkg string) []pkgInfo {
	t.Helper()
	root := list(t, pkg)
	seen := map[string]bool{root.ImportPath: true}
	out := []pkgInfo{root}
	for _, d := range root.Deps {
		if strings.HasPrefix(d, mod) && !seen[d] {
			seen[d] = true
			out = append(out, list(t, d))
		}
	}
	return out
}

// forbidden maps every import banned in the kernel to its reason. The reason
// goes into the error message because a test that says "do not import time"
// without explaining why ends up silenced with a //nolint.
var forbidden = map[string]string{
	"time":         "time.Now() inside the reducer breaks replay and the virtual clock of --sim",
	"net":          "the reducer talks to nobody: it returns effects and somebody else runs them",
	"net/http":     "the reducer talks to nobody: it returns effects and somebody else runs them",
	"os":           "reading env or files makes the same log yield different states depending on the machine",
	"os/exec":      "running processes is an effect, and effects are returned, not performed",
	"math/rand":    "randomness without an explicit seed makes the fold irreproducible",
	"crypto/rand":  "randomness without an explicit seed makes the fold irreproducible",
	"database/sql": "persistence belongs to the executor; state is derived from the log",
	"io":           "I/O belongs to the executor by definition",
	"bufio":        "I/O belongs to the executor by definition",
}

func TestKernelIsPure(t *testing.T) {
	for _, p := range ownClosure(t, mod+"internal/kernel") {
		for _, imp := range p.Imports {
			if why, bad := forbidden[imp]; bad {
				t.Errorf("%s imports %q.\n  why this is wrong: %s\n"+
					"  what to do: return an Effect describing the operation and "+
					"let the executor perform it (see docs/design/10-execution.md §10.1)",
					p.ImportPath, imp, why)
			}
		}
	}
}

func TestKernelDoesNotImportOtherLayers(t *testing.T) {
	p := list(t, mod+"internal/kernel")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod) {
			t.Errorf("the kernel depends on %s.\n"+
				"The kernel is the bottom layer: it cannot know anybody else exists. "+
				"If it needs a piece of data, it is handed to it through Config or Event.", d)
		}
	}
}

// TestBlueprintDependsOnlyOnTheKernel keeps blueprint loading a leaf.
//
// The temptation as the run loop lands will be to have the loader open the log
// or start a run itself, because it already holds the digest everything else
// needs. That inverts the layering: loading a blueprint is reading a file and
// resolving defaults, and it has to stay callable by `blueprint validate` with
// no runtime present. If validating a file required a log directory, the one
// command a user runs BEFORE owning a run would need a run to exist.
func TestBlueprintDependsOnlyOnTheKernel(t *testing.T) {
	p := list(t, mod+"internal/blueprint")
	for _, d := range p.Deps {
		if !strings.HasPrefix(d, mod) {
			continue
		}
		if d == mod+"internal/kernel" {
			continue
		}
		t.Errorf("the blueprint loader depends on %s.\n"+
			"  why it is wrong: loading is reading a file and resolving defaults; it must "+
			"stay runnable with no log, no clock and no executor.\n"+
			"  what to do: return the resolved Config and let the caller wire it to the "+
			"runtime (see docs/design/10-execution.md 10.2)", d)
	}
}

func TestSurfaceDoesNotImportTheExecutor(t *testing.T) {
	p := list(t, mod+"internal/surface")
	for _, d := range p.Deps {
		if strings.HasPrefix(d, mod+"internal/exec") {
			t.Errorf("the surface imports %s.\n"+
				"The surface is a declaration of capabilities: it describes what "+
				"can be done, not how. If it imports the executor, the manifest and "+
				"the docs can no longer be generated without dragging in the whole runtime.", d)
		}
	}
}

// TestExecutorDependsOnlyOnTheKernel keeps the runner's dependency direction
// pointing downwards.
//
// internal/exec deliberately DECLARES what it needs from storage (the exec.Log
// interface) instead of importing the concrete store. If that inverted and exec
// imported the store package, two things would break at once: the runner would
// stop being testable without a filesystem, and storage details (file layout,
// fsync policy, torn-write recovery) would start leaking into the run loop,
// where nobody would think to look for them.
//
// The kernel is the one exception: exec must import it, because effects and
// events are the vocabulary the two speak.
func TestExecutorDependsOnlyOnTheKernel(t *testing.T) {
	p := list(t, mod+"internal/exec")
	for _, d := range p.Deps {
		if !strings.HasPrefix(d, mod) {
			continue
		}
		if d == mod+"internal/kernel" {
			continue
		}
		t.Errorf("internal/exec depends on %s.\n"+
			"  why this is wrong: the executor may only depend on the kernel, whose "+
			"effects and events are the vocabulary they share. Everything else it "+
			"needs is DECLARED as an interface here (see exec.Log) so the runner "+
			"stays testable without a filesystem and storage details cannot leak "+
			"into the run loop.\n"+
			"  what to do: add the methods you need to an interface inside "+
			"internal/exec and let the concrete type satisfy it at the wiring "+
			"site in cmd/iash.", d)
	}
}

// TestExecutorIsNotPure is the inverse of TestKernelIsPure, and it exists so
// that nobody "cleans up" the executor by accident.
//
// The imports banned in the kernel are exactly the ones the executor is FOR:
// somebody has to touch time, processes and the network, and concentrating that
// in one package is the whole point of the split. Without this test, the purity
// rule looks like a global style preference, and the next person could try to
// satisfy it everywhere, quietly moving real-world work back into the reducer.
func TestExecutorIsNotPure(t *testing.T) {
	var allowed []string
	for _, p := range ownClosure(t, mod+"internal/exec") {
		if p.ImportPath != mod+"internal/exec" {
			continue
		}
		for _, imp := range p.Imports {
			if _, banned := forbidden[imp]; banned {
				allowed = append(allowed, imp)
			}
		}
	}
	if len(allowed) == 0 {
		t.Errorf("internal/exec imports none of the packages that are forbidden in " +
			"the kernel (time, os, io, net...).\n" +
			"  why this is suspicious: those imports are what the executor EXISTS " +
			"for. The kernel is pure so that one package can be impure; if the " +
			"executor is pure too, then either the real-world work never got " +
			"written, or it drifted back into the reducer and replay is now lying.\n" +
			"  what to do: if the runner genuinely no longer touches the outside " +
			"world, delete this test and say so in an ADR, because the layering " +
			"argument in ADR-0003 no longer describes the code.")
	}
}

// TestTheScheduleParserDoesNotReadTheWallClock keeps internal/trigger's testing
// property enforceable by the compiler rather than by review.
//
// The package needs `time` — a calendar is not optional for cron — so it cannot
// live in the kernel, and TestKernelIsPure already guarantees it will not drift
// back in. What this test protects is the other half: the package must not reach
// for anything that makes the ANSWER depend on the machine it runs on. Every
// function that says when a trigger fires takes `now` as a parameter, which is
// what makes "the last day of February", "the minute before midnight" and "the
// machine was asleep for four days" three-line tests instead of untestable.
//
// os is what would break that first, and it would break it invisibly: a TZ read
// from the environment would make the leap-year tests pass here and the nightly
// audit fire an hour off on a server in another region.
func TestTheScheduleParserDoesNotReadTheWallClock(t *testing.T) {
	// `time` is the point of the package; everything else banned in the kernel
	// is banned here too, for the reasons the kernel bans it.
	permitted := map[string]bool{"time": true}

	for _, p := range ownClosure(t, mod+"internal/trigger") {
		if p.ImportPath != mod+"internal/trigger" {
			continue
		}
		for _, imp := range p.Imports {
			if permitted[imp] {
				continue
			}
			if why, bad := forbidden[imp]; bad {
				t.Errorf("internal/trigger imports %q.\n"+
					"  why this is wrong: %s. This package answers \"when does "+
					"this fire\" and every answer must be a function of the `now` "+
					"it is handed, or the cases worth testing (leap day, a "+
					"schedule already missed, the minute before midnight) become "+
					"the cases that cannot be tested.\n"+
					"  what to do: take the value as a parameter. If it is I/O, it "+
					"belongs in the caller: parsing a schedule and storing one are "+
					"different jobs.", imp, why)
			}
		}
	}
}

// TestTheScheduleParserDependsOnDeclarationsAndNotOnTheRuntime.
//
// internal/trigger may import internal/surface and nothing else of ours.
//
// This test was written as a strict leaf rule — no project imports at all — and
// it failed one commit later, on the import that makes `--then` correct. Worth
// recording which of the two was wrong, because "the test fired, so loosen the
// test" is how a rule stops meaning anything.
//
// The rule was wrong, and it was wrong because it named three packages
// (kernel, exec, store) while the reason it gave was about RUNTIME: no log, no
// clock, no run present. internal/surface is not runtime. It imports `sort` and
// `strings`, it is a declaration of what the system can do, and importing it is
// what lets `--then` accept any command the registry publishes instead of
// carrying its own hand-written list of actions — which is precisely the
// duplication that had already gone stale (`notify:` named a command that does
// not exist) before any trigger code was written.
//
// So the line is drawn at declarations versus runtime, and it is enforced on the
// CLOSURE rather than on the direct import. That matters: TestSurfaceDoesNotImportTheExecutor
// keeps the surface itself clean today, but if the surface ever grew a runtime
// dependency, this test would fail here too instead of quietly inheriting it.
func TestTheScheduleParserDependsOnDeclarationsAndNotOnTheRuntime(t *testing.T) {
	// Declarations, not runtime. A schedule needs to know which commands exist;
	// it must never need a log, a clock or a run to answer that.
	permitted := map[string]bool{mod + "internal/surface": true}

	for _, d := range list(t, mod+"internal/trigger").Deps {
		if !strings.HasPrefix(d, mod) || permitted[d] {
			continue
		}
		t.Errorf("internal/trigger depends on %s.\n"+
			"  why this is wrong: parsing `--on cron:0 3 * * *` and deciding what "+
			"`--then` may invoke are a calendar and a lookup in the declared "+
			"surface. Both must stay answerable with no log, no clock and no run "+
			"present, exactly as internal/blueprint stays callable by `blueprint "+
			"validate`.\n"+
			"  the one exception is internal/surface, and it is an exception "+
			"because it is a DECLARATION: %s is runtime, and a schedule that "+
			"needs the runtime to parse cannot be validated before it is "+
			"installed.\n"+
			"  what to do: return the parsed Spec and Action, and let cmd/iash "+
			"join them to whatever runs them.", d, d)
	}
}

// TestTheTriggerStoreIsTheOnlyPlaceTriggersTouchTheDisk.
//
// internal/trigstore exists because internal/trigger is forbidden to import
// `os`, and this test is what keeps that separation from being pointless.
//
// The rule: trigstore may depend on internal/trigger (it stores those records)
// and on nothing else of ours. In particular not on the executor and not on the
// log. A store that reached into either would make `trigger list` — a command
// that just reads five files — require a run to be present, and the whole reason
// triggers are readable without a running system is that `trigger list` is what
// a user types when they suspect nothing is running.
//
// The alternative was to put these functions in cmd/iash beside the flag
// parsing, which passes every architecture test by not being a package. It fails
// the first time anything other than the CLI needs to read a trigger, which is
// the scheduler — the very next step. Persistence reachable only from a main
// package is persistence that gets copy-pasted.
func TestTheTriggerStoreIsTheOnlyPlaceTriggersTouchTheDisk(t *testing.T) {
	permitted := map[string]bool{
		mod + "internal/trigger": true,
		mod + "internal/surface": true, // inherited through trigger, a declaration
	}
	for _, d := range list(t, mod+"internal/trigstore").Deps {
		if !strings.HasPrefix(d, mod) || permitted[d] {
			continue
		}
		t.Errorf("internal/trigstore depends on %s.\n"+
			"  why this is wrong: the store's whole job is bytes on disk for the "+
			"records internal/trigger defines. Reading a trigger must not need a "+
			"log, a clock or a run, because `trigger list` is exactly what a user "+
			"types when they suspect nothing is running.\n"+
			"  what to do: keep the store reading and writing trigger.Record, and "+
			"let cmd/iash join it to whatever executes the actions.", d)
	}
}

// TestTheScheduleParserStillCannotReachTheDisk.
//
// The companion to the test above, and the reason the split is worth two
// packages instead of one. If `os` ever becomes importable from
// internal/trigger, trigstore stops being a boundary and becomes a file people
// bypass — and the schedule parser becomes untestable without a filesystem,
// which is how 29 February and the minute before midnight turn into cases that
// only get exercised when the calendar happens to cooperate.
//
// Stated as its own test rather than folded into the existing purity check,
// because it is a different claim: that one says the parser does not read the
// wall clock, this one says it does not read the disk.
func TestTheScheduleParserStillCannotReachTheDisk(t *testing.T) {
	banned := map[string]string{
		"os":       "a schedule that needs the filesystem to parse cannot be validated before it is stored",
		"os/exec":  "deciding what `--then` may invoke is a lookup, not an execution",
		"io":       "reading and writing trigger files belongs to internal/trigstore",
		"bufio":    "reading and writing trigger files belongs to internal/trigstore",
		"net":      "a trigger fires an effect; it does not perform one",
		"net/http": "a trigger fires an effect; it does not perform one",
	}
	for _, p := range ownClosure(t, mod+"internal/trigger") {
		for _, imp := range p.Imports {
			if why, bad := banned[imp]; bad {
				t.Errorf("%s imports %q.\n  why this is wrong: %s\n"+
					"  what to do: internal/trigstore already owns this; have it "+
					"hand a trigger.Record in and out.", p.ImportPath, imp, why)
			}
		}
	}
}

// TestEvalDoesNotDependOnTheExecutorItMeasures.
//
// internal/eval may depend on internal/blueprint (a suite names one per case,
// and a suite pointing at an invalid blueprint is wrong before anything runs)
// and on nothing else of ours. In particular not on internal/exec.
//
// The rule is about the direction of the dependency, and the direction is the
// whole design. `eval run` folds over cases and asks something to execute each
// one, but it asks through an interface it declares itself — CaseRunner, one
// method — so the thing being measured is supplied by the caller. That is what
// lets 22 tests drive the budget arithmetic, the reserve, the prefix bias and
// the error path with a fake that returns a string, and it is why those tests
// take milliseconds instead of needing a loop, a log and a clock.
//
// Point it the other way and the tests that matter become the tests nobody
// writes. "What does a suite report when the money runs out two cases in" would
// need a real executor spending real budget; "what does it report when a case
// errors" would need one that fails on demand. Both are one line with a fake.
//
// There is a second reason, and it is the one that would bite first. An eval
// package that imported the executor could not be imported BY it — Go forbids
// the cycle — and the natural next feature is exactly that: a run that
// evaluates its own output. Keeping eval a leaf keeps that possible.
func TestEvalDoesNotDependOnTheExecutorItMeasures(t *testing.T) {
	permitted := map[string]bool{
		mod + "internal/blueprint": true,
		mod + "internal/kernel":    true, // inherited through blueprint
	}
	for _, d := range list(t, mod+"internal/eval").Deps {
		if !strings.HasPrefix(d, mod) || permitted[d] {
			continue
		}
		t.Errorf("internal/eval depends on %s.\n"+
			"  why this is wrong: eval measures a runner it does not own. It "+
			"declares CaseRunner — one method — and the caller supplies the "+
			"thing being measured, which is what lets the budget arithmetic, "+
			"the reserve, the prefix bias and the error path be tested with a "+
			"fake that returns a string instead of a loop, a log and a clock.\n"+
			"  and: an eval that imported the executor could not be imported "+
			"BY it, and a run that evaluates its own output is the obvious "+
			"next feature.\n"+
			"  what to do: widen CaseRunner if the fold needs more, and let "+
			"cmd/iash join eval to whatever executes a case.", d)
	}
}

// TestEvalDoesNotReadTheClockOrTheNetwork.
//
// A narrower claim than the leaf rule above, and a separate one: eval reads
// files (a suite is a file, and LoadFile is its entry point) so `os` cannot be
// banned here the way the kernel bans it. What must stay out is anything that
// makes the same suite produce a different verdict on a different machine or a
// different afternoon.
//
// `time` is the one worth stating. A run summary carries StartedAt, and the
// obvious convenience is to have the package fill it in — which is exactly what
// makes a report irreproducible and a test of "what does a truncated run say"
// depend on when it ran. The field is a string, set by the caller, for the same
// reason every trigger function takes `now` as a parameter.
//
// The consequence is visible in the eval tests: they assert on complete output
// blocks, including the run id, because nothing in the package invented one.
func TestEvalDoesNotReadTheClockOrTheNetwork(t *testing.T) {
	banned := map[string]string{
		"time":         "a summary that timestamps itself cannot be compared byte for byte, and StartedAt is the caller's fact to state",
		"net":          "a suite is a file and a verdict is a comparison; neither is a request",
		"net/http":     "a suite is a file and a verdict is a comparison; neither is a request",
		"os/exec":      "executing a case is the CaseRunner's job, and eval must not be able to become one",
		"math/rand":    "a pass rate that varies between reads of the same suite is not a measurement",
		"crypto/rand":  "a pass rate that varies between reads of the same suite is not a measurement",
		"database/sql": "persisting a run belongs to a store package, as it does for the log and for triggers",
	}
	for _, imp := range list(t, mod+"internal/eval").Imports {
		if why, bad := banned[imp]; bad {
			t.Errorf("internal/eval imports %q.\n  why this is wrong: %s\n"+
				"  what to do: take the value as a parameter. Every number "+
				"this package reports is quoted in a decision, so every one "+
				"of them has to be a function of the suite and the results, "+
				"not of the machine.", imp, why)
		}
	}
}

// TestTheEvalStoreIsTheOnlyPlaceRunsTouchTheDisk.
//
// internal/evalstore exists because internal/eval is forbidden database/sql by
// TestEvalDoesNotReadTheClockOrTheNetwork, with the reason written out there:
// persisting a run belongs to a store package, as it does for the log and for
// triggers. This test is what keeps that separation from being pointless.
//
// The rule: evalstore may depend on internal/eval and nothing else of ours. In
// particular not on the executor. `eval compare e1 e2` reads two files and does
// arithmetic — a comparison of runs from last month must not require the thing
// that produced them to still exist, still be configured, or still be able to
// reach a model. A store that pulled in the executor would make yesterday's
// evidence unreadable on a machine with no API key.
func TestTheEvalStoreIsTheOnlyPlaceRunsTouchTheDisk(t *testing.T) {
	permitted := map[string]bool{
		mod + "internal/eval":      true,
		mod + "internal/blueprint": true, // inherited through eval
		mod + "internal/kernel":    true, // inherited through blueprint
	}
	for _, d := range list(t, mod+"internal/evalstore").Deps {
		if !strings.HasPrefix(d, mod) || permitted[d] {
			continue
		}
		t.Errorf("internal/evalstore depends on %s.\n"+
			"  why this is wrong: the store's whole job is bytes on disk for the "+
			"summaries internal/eval defines. Reading a run must not need an "+
			"executor, a log or a clock, because `eval compare` is how a decision "+
			"made last month gets defended — on whatever machine is asking.\n"+
			"  what to do: keep the store reading and writing eval.RunSummary, "+
			"and let cmd/iash join it to whatever produced the runs.", d)
	}
}

// TestTheEvalStoreDoesNotDecideWhatARunMeans.
//
// The store may not compute a rate. Totals, PassRate, Compare and Validate all
// live in internal/eval, and the reason is the same one that put Decide in the
// kernel: the denominator of a pass rate is the single most consequential
// decision in this repository, and there must be exactly one place it is made.
//
// The specific failure this prevents is a store that grows a convenience — a
// PassRate on a listing row, a "summary" struct for `eval list` — computed from
// the results it happens to have in hand. It would divide by the wrong
// denominator once and then be quoted forever, and it would disagree with
// `eval run`'s own output while both looked authoritative.
//
// `math` is the giveaway import, so it is banned outright rather than checked
// for by reading the code.
func TestTheEvalStoreDoesNotDecideWhatARunMeans(t *testing.T) {
	banned := map[string]string{
		"math":     "arithmetic on a run belongs to internal/eval: Totals and PassRate are one decision made in one place, and a second implementation would disagree with the first while both looked authoritative",
		"time":     "a run states its own StartedAt, which the caller supplies; a store that reads the clock would stamp a run with when it was SAVED and invite that to be read as when it RAN",
		"net":      "a store is a directory",
		"net/http": "a store is a directory",
		"os/exec":  "producing a run is the CaseRunner's job; a store that could execute one would make `eval list` able to spend money",
	}
	for _, imp := range list(t, mod+"internal/evalstore").Imports {
		if why, bad := banned[imp]; bad {
			t.Errorf("internal/evalstore imports %s.\n  why this is wrong: %s\n"+
				"  what to do: put the decision in internal/eval, where the "+
				"existing tests for it are.", imp, why)
		}
	}
}

// TestTheSchedulerDoesNotDecideWhenAThingIsDue is the rule that keeps the
// scheduler thin.
//
// internal/trigger answers two questions purely — Due says whether a slot has
// arrived and how many runs are owed, Admit says what to do about that given
// what is already running — and internal/scheduler is the caller that owns a
// clock, a store and a subprocess. The value of that split is entirely in it
// being one-directional: the moment the scheduler computes dueness itself,
// there are two implementations of "is it 3am yet", one of which is tested with
// literal timestamps and one of which is tested by waiting.
//
// The specific drift this prevents is a plausible-looking convenience. A
// scheduler holding a Record and a `now` is one line away from
// `r.Next(now).Before(now)` inlined into the loop as a cheap pre-filter, or a
// `strings.HasPrefix(r.On, "cron:")` special case to skip records it thinks
// cannot be due. Both would work on the day they were written and both would
// then disagree with Due about a leap day, a paused trigger, or a schedule with
// no future.
//
// So the ban is on the two packages that make a second schedule parser
// possible. `time` is NOT banned: the scheduler's whole job is to hold a clock.
func TestTheSchedulerDoesNotDecideWhenAThingIsDue(t *testing.T) {
	banned := map[string]string{
		"regexp":  "a schedule is parsed by internal/trigger. A regexp in the scheduler is a second parser for `on`, and it will disagree with the first about exactly the cases nobody tests twice",
		"strconv": "turning schedule text into numbers is ParseSpec's job. A scheduler that reads `every:15m` for itself has an opinion about what a trigger means, and Due's tests no longer cover the thing that runs",
	}
	for _, imp := range list(t, mod+"internal/scheduler").Imports {
		if why, bad := banned[imp]; bad {
			t.Errorf("internal/scheduler imports %s.\n  why this is wrong: %s\n"+
				"  what to do: ask internal/trigger. If the answer it gives is "+
				"wrong, fix it there, where a four-day outage is a table entry "+
				"rather than a four-day test.", imp, why)
		}
	}
}

// TestTheSchedulerDoesNotReachPastItsInterfaces.
//
// The scheduler declares Store, Runner and Execution and takes them as
// parameters. It must not import trigstore or exec to get at the real ones,
// and this is not tidiness — it is the only reason its tests can arrange the
// cases that matter.
//
// With a *trigstore.Store, "the firing started and could not be recorded" needs
// a read-only directory, which behaves differently as root and is therefore
// tested nowhere; with the interface it is a struct field. With a real
// executor, "the work ignored its cancellation and is still spending" needs a
// process that ignores signals; with the interface it is a fake whose Cancel
// deliberately does nothing. Those two cases are the worst failures this
// package has, and both would be comments instead of tests.
//
// internal/trigger is permitted and is the point: Due, Admit and Record are
// what the scheduler is a caller OF.
func TestTheSchedulerDoesNotReachPastItsInterfaces(t *testing.T) {
	permitted := map[string]bool{
		mod + "internal/trigger": true,
		mod + "internal/surface": true, // inherited through trigger's ParseAction
		mod + "internal/kernel":  true, // inherited
	}
	for _, d := range list(t, mod+"internal/scheduler").Deps {
		if !strings.HasPrefix(d, mod) || permitted[d] {
			continue
		}
		t.Errorf("internal/scheduler depends on %s.\n"+
			"  why this is wrong: the scheduler names what it needs as Store, "+
			"Runner and Execution, and taking the concrete types instead would "+
			"cost the two tests that matter most — a firing that starts and "+
			"cannot be recorded (which repeats forever), and work that ignores "+
			"cancellation while it is still spending. Both are struct fields "+
			"today; against the real types they are a read-only directory and a "+
			"process that ignores signals.\n"+
			"  what to do: widen the interface, and let cmd/iash pass the real "+
			"implementation in.", d)
	}
}

// TestTheModelRuleDoesNotReadTheDiskOrTheNetwork (rule 17).
//
// internal/model decides which models may be called: it must exist, be
// unambiguous, and be enabled. That rule is the gate the live executor consults
// before spending money, and the reason it lives in a package that cannot reach
// a filesystem is that the cases worth testing are the edges — two providers
// offering the same id, a model an operator disabled, a key pasted where a
// variable name belongs — and every one of them becomes a directory fixture the
// moment the package can read one.
//
// net/http is banned for a sharper reason than purity. This package holds the
// endpoint and the name of the variable holding the credential. If it could
// also make requests, the obvious next commit puts the request here too, and
// the one place in the tree that must never hold a secret becomes the place
// that sends it.
func TestTheModelRuleDoesNotReadTheDiskOrTheNetwork(t *testing.T) {
	banned := map[string]string{
		"os":       "reading env or files here means the answer to \"may this model be called\" depends on the machine, so a blueprint that resolves on one laptop fails on another with nothing in the log to say why",
		"net":      "this package decides what may be called; something else does the calling",
		"net/http": "this package holds the endpoint and the name of the variable holding the key. Give it a client and the request follows, and the one place that must never touch a secret becomes the place that sends it",
		"time":     "nothing here resolves against the clock: AddedAt is stamped by the caller precisely so this package stays off it",
		"os/exec":  "resolving a model is not running a process",
	}
	for _, imp := range list(t, mod+"internal/model").Imports {
		if why, bad := banned[imp]; bad {
			t.Errorf("internal/model imports %s.\n  why this is wrong: %s\n"+
				"  what to do: put it in internal/modelstore, which exists for "+
				"exactly this and is where the bytes already live.", imp, why)
		}
	}
}

// TestTheModelStoreIsTheOnlyPlaceProvidersTouchTheDisk (rule 18).
//
// Two obligations in one test, because they fail together.
//
// internal/model must not depend on modelstore: the pure package deciding what
// a model IS cannot depend on where it is kept, or the dependency inverts and
// the rule becomes untestable without a directory.
//
// And cmd/iash must not read or write providers/*.json itself. That is the
// obligation with teeth: the store writes the file 0600 and atomically, refuses
// an api_key field on the way in, and refuses a record whose name disagrees
// with its filename. A command that opened the file directly would get none of
// those, and the failure would be a world-readable map to a credential — which
// looks exactly like a working file.
func TestTheModelStoreIsTheOnlyPlaceProvidersTouchTheDisk(t *testing.T) {
	for _, d := range list(t, mod+"internal/model").Deps {
		if d == mod+"internal/modelstore" {
			t.Errorf("internal/model depends on internal/modelstore.\n" +
				"  why this is wrong: the package that decides which models may " +
				"be called would then need a directory to be tested, and the " +
				"edges worth testing (an ambiguous id, a disabled model, a key " +
				"pasted where a variable name belongs) become fixtures.\n" +
				"  what to do: the dependency goes the other way. modelstore " +
				"imports model.")
		}
	}

	// The CLI is allowed — required, in fact — to import the store. What it must
	// not do is bypass it, and the compiler cannot see that, so the check is on
	// the source.
	out, err := exec.Command("grep", "-rn", "providers/", "../cmd/iash").Output()
	if err != nil && len(out) == 0 {
		return // grep exits 1 with no matches, which is the passing case
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.Contains(line, "//") {
			continue // a comment naming the directory is documentation
		}
		t.Errorf("cmd/iash names the providers directory directly:\n  %s\n"+
			"  why this is wrong: modelstore writes that file 0600 and "+
			"atomically, refuses an api_key field on the way in, and refuses a "+
			"record whose name disagrees with its filename. A command that "+
			"opens it directly gets none of that, and the result is a "+
			"world-readable map to a credential that looks like a working "+
			"file.\n"+
			"  what to do: use modelstore.DefaultDir and the Store methods.", line)
	}
}
