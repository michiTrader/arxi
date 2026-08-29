package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/michiTrader/iash/internal/scheduler"
	"github.com/michiTrader/iash/internal/surface"
	"github.com/michiTrader/iash/internal/trigger"
)

// `iash trigger run` — the caller the tick never had.
//
// internal/trigger decides WHEN (Due), internal/trigger decides WHETHER
// (Admit), internal/scheduler decides WHAT HAPPENS (Tick). All three were built
// and tested before this file existed, which means all three were unreachable:
// a scheduler with no caller outside its own tests is a library, not a feature.
// This is the file that makes a user able to reach it.

// selfRunner starts each firing as a child process of this binary.
//
// # Why a subprocess and not a goroutine
//
// A goroutine is cheaper and would have been less code. Three reasons it is
// wrong here, in increasing order of how much they cost when ignored:
//
// First, re-entrancy. `--then "trigger run"` is a trigger that schedules the
// scheduler, and in-process that is an infinite recursion inside one process
// rather than a visible pile of subprocesses. One of those is diagnosable from
// `ps`; the other is a stack overflow with no clue as to why.
//
// Second, isolation is the entire value of the overlap policies.
// `cancel-previous` has to be able to stop work that is not cooperating; a
// goroutine cannot be killed, so cancel-previous would degrade to "ask nicely
// and hope", which is exactly the behaviour it exists to avoid. A process can
// be signalled.
//
// Third, a scheduled run that panics must not take the scheduler with it. An
// unattended process whose whole job is to keep firing hourly triggers cannot
// die because one audit hit a nil map.
type selfRunner struct {
	// self is the binary to re-invoke. Resolved once at construction, because
	// os.Executable() can start failing later (the file is replaced during an
	// upgrade) and discovering that per-firing would give one trigger a
	// different answer from the next.
	self string
}

// How children agree with the parent about where triggers live
//
// They inherit it. triggerDir is a relative path ("triggers"), resolved against
// the process's working directory, and a child inherits its parent's working
// directory — so a scheduler started in a temporary directory spawns children
// that read that same temporary directory, with nothing passed explicitly.
//
// This is written down because the first version of this file passed
// `--triggers <dir>` to every child, and that flag does not exist. It was
// written from what the code wished were true. Grepping found the only two
// mentions of `--triggers` in the repository were this file's own comment and
// its own code. Every child would have died on an unknown flag while the
// scheduler reported the firing as STARTED — because Start succeeds when the
// process starts, not when the invocation turns out to be valid. A bug that
// reports success is worse than one that crashes.
//
// The seam that WOULD need care is a future flag that changes triggerDir
// without changing the working directory (`iash -C <dir>`, say). There is none
// today, and TestChildrenInheritTheTriggerDirectory is what fails on the day
// one arrives.

// Start launches the action as a child process.
func (r selfRunner) Start(rec trigger.Record, a trigger.Action) (scheduler.Execution, error) {
	args := append(append([]string{}, a.Path...), a.Args...)

	cmd := exec.Command(r.self, args...)

	// Output goes to the scheduler's own stdout and stderr rather than being
	// captured. A long-running scheduler that buffered every child's output
	// would grow without bound and show nothing until the child exited, which
	// is precisely backwards for the audience: somebody watching an unattended
	// process wants to see it working now.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Its own process group, so Cancel can signal the whole tree. Without
	// this, killing `iash run start` would leave anything IT spawned running
	// and unparented — which is how a "cancelled" run keeps spending.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	ex := &childExec{cmd: cmd, done: make(chan struct{})}

	// One Wait per child, here and nowhere else. Wait is not safe to call
	// twice, and the scheduler learns a child is finished by watching Done()
	// — so the goroutine that waits is also the one that closes the channel.
	go func() {
		ex.err = cmd.Wait()
		close(ex.done)
	}()

	return ex, nil
}

// childExec is one running firing.
type childExec struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	once sync.Once
}

func (c *childExec) Done() <-chan struct{} { return c.done }

// Cancel asks the process group to stop, and does not wait for it.
//
// SIGTERM and not SIGKILL: a cancelled run may hold a half-written file, and
// the scheduler's own store writes are the obvious example of something that
// deserves the chance to finish a line. SIGKILL is what an operator sends when
// SIGTERM was ignored; it is not the first thing a policy should reach for.
//
// Negative PID, so the signal reaches the whole group rather than just the
// child — see Setpgid above.
//
// sync.Once because cancelAll runs on every tick while the work is still
// counted as inflight, so Cancel is called repeatedly for one execution. Once
// keeps that from being N signals, and — more importantly — keeps it from
// signalling a PID the OS has since recycled onto somebody else's process.
func (c *childExec) Cancel() {
	c.once.Do(func() {
		if c.cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
	})
}

// dryRunner reports what would start, and starts nothing.
//
// It returns an already-finished Execution rather than nil. The scheduler
// counts inflight work by what Start handed back, so a nil would either panic
// or make every dry-run firing look permanently in-flight — and a dry run whose
// second trigger is suppressed by the overlap policy of a run that never
// existed is not a preview of anything.
type dryRunner struct{ n int }

func (d *dryRunner) Start(rec trigger.Record, a trigger.Action) (scheduler.Execution, error) {
	d.n++
	fmt.Printf("  would run: %s\n", a.CLI())
	return finished{}, nil
}

// dryStore reads the real triggers and throws away every write.
//
// # Why faking the runner was not enough
//
// A firing has TWO effects, and --dry-run has to suppress both. The obvious one
// is the child process, which dryRunner handles. The one I missed is the store
// write: Tick records LastFiredAt for every firing it admits, because that is
// what makes the slot stop being due.
//
// So the first version of --dry-run consumed the slot it was previewing.
// Running it showed LAST move from `never` to `started` and NEXT advance a
// minute, and the REAL run a second later answered "not due until" — the
// preview had cancelled the firing. That is the worst shape a bug can take
// here: --dry-run exists to be the safe thing to type, and it was the one
// command that could silently skip a scheduled run.
//
// No test caught it. The scheduler's 31 tests use a fake store and assert that
// Tick DOES save, which is correct at that layer. The CLI is where the two
// fakes are chosen, so the CLI is the only place the omission was visible, and
// it was only visible by running the binary twice in a row and reading LAST.
//
// A no-op Save rather than an error: a dry run should report what would happen,
// and a store that refused writes would make every firing report an error
// instead of a plan.
type dryStore struct{ scheduler.Store }

func (dryStore) Save(trigger.Record) error { return nil }

// finished is an Execution that is already over.
type finished struct{}

func (finished) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (finished) Cancel() {}

// cmdTriggerRun is `iash trigger run`.
func cmdTriggerRun(args []string) {
	c := surface.Lookup("trigger", "run")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger run: %v\n", err)
		os.Exit(2)
	}

	interval, err := time.ParseDuration(vals["interval"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger run: --interval %q is not a "+
			"duration: %v\n  examples: 30s, 1m, 15m\n", vals["interval"], err)
		os.Exit(2)
	}
	if interval <= 0 {
		// Refused rather than clamped. A zero interval is a spin loop that
		// reads the trigger directory as fast as the disk allows, and the user
		// who typed it meant something else. Clamping to a default would hide
		// the typo behind behaviour that looks correct.
		fmt.Fprintf(os.Stderr, "iash trigger run: --interval must be positive, "+
			"got %s.\n  a zero interval is a spin loop, not a fast scheduler; "+
			"for a single pass use --once\n", interval)
		os.Exit(2)
	}

	once := vals["once"] == "true"
	dry := vals["dry-run"] == "true"

	// Checked here, before the store is opened, because this is misuse and
	// misuse is refused before anything happens. The first version of this
	// function tested it after the --once branch had already ticked, so the
	// only path that reached the message was the one where it was pointless.
	if dry && !once {
		fmt.Fprintln(os.Stderr, "iash trigger run: --dry-run loops forever "+
			"printing the same report, because nothing it reports ever runs.\n"+
			"  use: iash trigger run --dry-run --once")
		os.Exit(2)
	}

	var store scheduler.Store = openStore()

	var runner scheduler.Runner
	if dry {
		// Both sinks are faked, not just the runner. See dryStore.
		runner = &dryRunner{}
		store = dryStore{store}
	} else {
		self, err := os.Executable()
		if err != nil {
			// Exit 1, not 2: nothing the user typed is wrong. The environment
			// cannot tell us what we are, and every firing needs it.
			fmt.Fprintf(os.Stderr, "iash trigger run: cannot find my own "+
				"binary, which is what runs each trigger's --then: %v\n", err)
			os.Exit(1)
		}
		runner = selfRunner{self: self}
	}

	sched, err := scheduler.New(store, runner, printReport)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger run: %v\n", err)
		os.Exit(1)
	}

	if once {
		if err := sched.Tick(nowFunc()); err != nil {
			fmt.Fprintf(os.Stderr, "iash trigger run: %v\n", err)
			os.Exit(1)
		}
		return
	}

	loop(sched, interval)
}

// loop ticks until interrupted.
func loop(sched *scheduler.Scheduler, interval time.Duration) {
	fmt.Printf("watching %d trigger(s), checking every %s\n",
		len(sched.Names()), interval)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	// The first tick happens immediately, before the ticker is armed.
	//
	// time.Ticker does not fire at zero, so arming first would mean a
	// scheduler started with --interval 15m sits silent for fifteen minutes
	// with an already-overdue trigger in the store. Anybody starting it would
	// reasonably conclude it was broken, and would be right to: dueness is
	// derived from LastFiredAt, so that trigger was due before the process
	// existed.
	tick(sched)

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			tick(sched)
		case s := <-sig:
			// Running children are deliberately left alone.
			//
			// They are separate processes doing work the user asked for, and
			// stopping the scheduler is not a request to abandon a half-done
			// run. They keep their own stdout, so the shell that regains the
			// prompt may still see output — which is honest, and better than
			// the alternative: killing work at an arbitrary point because the
			// thing that started it was asked to stop scheduling.
			//
			// The exception is cancel-previous, which kills on purpose. That
			// is a policy the user chose per trigger, not a side effect of
			// Ctrl-C.
			fmt.Printf("\n%s — stopping. %d run(s) still going, left alone.\n",
				s, total(sched.Running()))
			return
		}
	}
}

// tick runs one pass and keeps going if it fails.
//
// A failed tick is almost always a transient read of the trigger directory —
// an editor's half-written temporary file, a directory being restored. Exiting
// on it would mean an unattended scheduler dies overnight from a condition that
// was gone a second later, and nothing fires until somebody notices. The error
// is printed, because a failure that repeats every interval should be visible
// in the log rather than silently absorbed.
func tick(sched *scheduler.Scheduler) {
	if err := sched.Tick(nowFunc()); err != nil {
		fmt.Fprintf(os.Stderr, "tick failed, continuing: %v\n", err)
	}
}

// total sums the per-trigger inflight counts.
func total(running map[string]int) int {
	n := 0
	for _, c := range running {
		n += c
	}
	return n
}

// printReport is what the user sees per firing.
//
// The scheduler takes this as a callback rather than printing for itself, which
// is what let all 31 of its tests assert on decisions without parsing text.
func printReport(r scheduler.Report) {
	if r.Err != nil {
		fmt.Fprintf(os.Stderr, "%-24s %-12s %v\n", r.Trigger, "error", r.Err)
		return
	}

	status := "waiting"
	switch {
	case r.Started > 0 && r.Cancel:
		status = "restarted"
	case r.Started > 0:
		status = "started"
	case r.Consume:
		status = "skipped"
	}

	// Missed is only worth reporting above 1, because an on-time firing counts
	// as one of its own missed slots — the slot it is firing for. Printing
	// "[1 missed]" on every healthy firing would train the reader to ignore
	// the field, which is the opposite of what a backlog warning is for.
	extra := ""
	if r.Missed > 1 {
		extra = fmt.Sprintf(" [%d missed]", r.Missed)
	}

	fmt.Printf("%-24s %-12s %s%s\n", r.Trigger, status, r.Why, extra)
}
