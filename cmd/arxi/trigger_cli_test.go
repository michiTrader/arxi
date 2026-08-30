package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/surface"
)

// The trigger CLI, exercised as a process.
//
// # Why a second test file, and why it costs a build
//
// trigger_test.go tests the parts that return: parseInvocation, the column
// functions, the JSON projection. It cannot test the rest, because the rest
// calls os.Exit — and os.Exit in an in-process test kills the test binary, so
// there is no way to assert on what happened. Four mutations survived the
// in-process suite for exactly that reason and for no other: the code they
// changed was only reachable through a path that exits.
//
// Two of those four were real. `trigger delete` answering with a plain "unknown
// subcommand" instead of the pause explanation, and the list table reverting to
// fixed-width columns, both left the suite green. A third — an impossible
// schedule reported as an operational failure rather than misuse — looked
// unobservable until the thing it changes was named: the exit code. That is the
// distinction a CI job acts on, so it is worth a test.
//
// The price is a `go build` per run, paid once and cached in arxiBin. Spending
// it buys the only assertions that can exist about exit codes, about stderr on
// the failure paths, and about whether a refused create left anything behind.
//
// # The two mutations that are left, and why they are not gaps
//
// Recorded here so nobody re-derives it. 24 mutations of trigger.go, 19 caught,
// 2 not killable and 3 invalid:
//
//   - Removing `.UTC()` from the NEXT column changes nothing, because
//     Spec.Next normalises `now` to UTC on entry and builds its answer from
//     that, so the value is already UTC when it arrives. The call is a guard
//     against that upstream changing, and a guard whose invariant currently
//     holds elsewhere cannot be observed from here. Testing it would mean
//     asserting on a fact about internal/trigger from cmd/arxi, which is the
//     wrong package to state it in — internal/trigger already does.
//
//   - Deleting the declared-but-unbuilt fallback changes nothing, because all
//     four declared trigger subcommands are built, so the switch never reaches
//     its default with a declared name. The branch is unreachable TODAY and
//     load-bearing on the day a fifth is declared, which is precisely when it
//     stops being unreachable; TestADeclaredButUnbuiltSubcommandIsNotCalledUnknown
//     is the test that starts failing then. Deleting the guard to raise a
//     mutation score would trade a real future protection for a number.
//
// Three others were invalid, not survivors: one no longer matched the source
// and two did not compile. The harness compile-checks before running the tests
// for that reason — a mutation that fails to build is indistinguishable from
// one that was caught if the only thing read is an exit code.

var (
	binDir  string // lives for the whole package run; see TestMain
	arxiBin string // the built binary, or "" until the first subprocess test
)

// TestMain owns the binary's lifetime.
//
// This is not ceremony. The obvious version — build into t.TempDir() and cache
// the path — passes the first test and fails every later one with "no such file
// or directory", because t.TempDir() is removed when THAT test finishes and the
// cached path outlives the directory it points into. The binary is shared
// across tests, so its lifetime has to be the package run, and TestMain is the
// only scope that is.
//
// The directory is created here and the build stays lazy: a run of `go test
// ./cmd/arxi/` that touches none of these tests should not pay for a compile.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "arxi-cli-test")
	if err != nil {
		panic("creating a directory for the test binary: " + err.Error())
	}
	binDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// buildIash compiles the binary under test, once.
//
// The binary and not a script: the exit code is the thing being tested, and
// anything that wraps the process can substitute its own.
func buildIash(t *testing.T) string {
	t.Helper()
	if arxiBin != "" {
		return arxiBin
	}
	bin := filepath.Join(binDir, "arxi")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building arxi: %v\n%s", err, out)
	}
	arxiBin = bin
	return bin
}

type result struct {
	out  string
	code int
}

// arxi runs the binary in dir and reports what a caller sees.
//
// CombinedOutput and not separate streams: every assertion here is about
// whether an explanation reached the user, and a test that asserted on stderr
// alone would pass if a diagnostic were printed to stdout — which is a real bug
// for anybody piping the output, but not the bug these tests are about.
func arxi(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(buildIash(t), args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running arxi %v: %v", args, err)
	}
	return result{out: string(out), code: code}
}

// workdir is a directory the binary may write triggers/ into.
func workdir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// declaredTriggerSubcommands reads the registry rather than listing them.
//
// The same argument as the parser's: a test that hand-lists the subcommands
// stops covering the one added next, and the check below — that a declared
// subcommand is never called unknown — is precisely about subcommands that were
// declared and not yet built.
func declaredTriggerSubcommands() []string {
	var out []string
	for i := range surface.Registry {
		c := &surface.Registry[i]
		if len(c.Path) == 2 && c.Path[0] == "trigger" {
			out = append(out, c.Path[1])
		}
	}
	return out
}

// TestTheDocumentedSessionWorksEndToEnd runs §20.10 verbatim.
//
// The design doc's session is the specification; a paraphrase of it is a test
// of my paraphrase. The one thing not asserted literally is the timestamp,
// which depends on when the suite runs.
func TestTheDocumentedSessionWorksEndToEnd(t *testing.T) {
	dir := workdir(t)

	got := arxi(t, dir, "trigger", "create", "nightly-audit",
		"--on", "cron:0 3 * * *",
		"--then", "run start security-team 'audit dependencies for new CVEs'",
		"--budget", "5.00", "--budget-period", "day",
		"--on-missed", "skip", "--overlap", "skip")
	if got.code != 0 {
		t.Fatalf("create failed with %d:\n%s", got.code, got.out)
	}
	if !strings.HasPrefix(got.out, "trigger nightly-audit created (next: ") {
		t.Errorf("create said:\n%s\nwant the §20.10 line", got.out)
	}

	// The file is where the store says, and named after the trigger.
	if _, err := os.Stat(filepath.Join(dir, "triggers", "nightly-audit.json")); err != nil {
		t.Errorf("after create: %v", err)
	}

	got = arxi(t, dir, "trigger", "list")
	if got.code != 0 {
		t.Fatalf("list failed with %d:\n%s", got.code, got.out)
	}
	for _, want := range []string{
		"NAME", "ON", "STATUS", "LAST", "NEXT",
		"nightly-audit", "cron:0 3 * * *", "active", "never",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("list output is missing %q:\n%s", want, got.out)
		}
	}

	got = arxi(t, dir, "trigger", "show", "nightly-audit")
	if got.code != 0 {
		t.Fatalf("show failed with %d:\n%s", got.code, got.out)
	}
	for _, want := range []string{
		"run start security-team 'audit dependencies for new CVEs'",
		"5.00 per day", "on-missed:", "overlap:", "created:",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("show output is missing %q:\n%s", want, got.out)
		}
	}

	got = arxi(t, dir, "trigger", "pause", "nightly-audit")
	if got.code != 0 {
		t.Fatalf("pause failed with %d:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "paused") {
		t.Errorf("pause said %q", got.out)
	}

	// Pausing twice reports rather than pretending to have just done it.
	got = arxi(t, dir, "trigger", "pause", "nightly-audit")
	if got.code != 0 {
		t.Fatalf("second pause failed with %d:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "already paused") {
		t.Errorf("second pause said %q, want it to report the trigger was already paused", got.out)
	}

	// And a paused trigger's NEXT is not a timestamp.
	got = arxi(t, dir, "trigger", "list")
	if !strings.Contains(got.out, "(paused)") {
		t.Errorf("after pausing, list said:\n%s\nwant the NEXT column to say (paused)", got.out)
	}
}

// TestARefusedCreateWritesNothing is the assertion the in-process tests cannot
// make: that exit 2 happened BEFORE the disk was touched.
//
// A trigger that was refused and stored anyway is worse than either outcome on
// its own — the user was told it failed, and it is in `trigger list` marked
// active.
func TestARefusedCreateWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		why  string
	}{
		{"an impossible date", []string{"trigger", "create", "feb30",
			"--on", "cron:0 3 30 2 *", "--then", "run start t 'x'",
			"--budget", "1", "--budget-period", "day"},
			"February 30th exists in no year"},
		{"a bad budget", []string{"trigger", "create", "cheap",
			"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
			"--budget", "free", "--budget-period", "day"},
			"--budget is a number"},
		{"a missing flag", []string{"trigger", "create", "half",
			"--on", "cron:0 3 * * *"},
			"three required flags are absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := workdir(t)
			got := arxi(t, dir, tc.args...)
			if got.code != 2 {
				t.Errorf("exit %d, want 2 (%s):\n%s", got.code, tc.why, got.out)
			}
			ents, err := os.ReadDir(filepath.Join(dir, "triggers"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("reading triggers/: %v", err)
			}
			if len(ents) != 0 {
				t.Errorf("a refused create left %d file(s) behind: %v", len(ents), ents)
			}
		})
	}
}

// TestACommandWithheldFromAgentsIsRefusedAtCreateTime walks the security
// boundary through the whole binary.
//
// internal/trigger tests ParseAction directly, which proves the rule; this
// proves the rule is reached. A create path that validated the schedule and not
// the action would store a trigger that approves inbox items on a schedule, and
// every unit test in internal/trigger would still pass.
func TestACommandWithheldFromAgentsIsRefusedAtCreateTime(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "trigger", "create", "self-approve",
		"--on", "cron:0 3 * * *",
		"--then", "inbox approve abc123",
		"--budget", "1", "--budget-period", "day")
	if got.code != 2 {
		t.Fatalf("exit %d, want 2:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "inbox approve") {
		t.Errorf("the refusal does not name the command:\n%s", got.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "triggers", "self-approve.json")); !os.IsNotExist(err) {
		t.Errorf("a trigger that approves inbox items reached the disk (%v)", err)
	}
}

// TestDeleteIsAnsweredWithPauseAndTheReason covers the mutation that survived
// in-process: replacing the explanation with a generic "unknown subcommand".
//
// delete is not a typo, it is a reasonable guess at a command that does not
// exist on purpose, and the answer has to say which.
func TestDeleteIsAnsweredWithPauseAndTheReason(t *testing.T) {
	dir := workdir(t)
	for _, verb := range []string{"delete", "rm", "remove"} {
		got := arxi(t, dir, "trigger", verb, "nightly")
		if got.code != 2 {
			t.Errorf("trigger %s exited %d, want 2:\n%s", verb, got.code, got.out)
		}
		if !strings.Contains(got.out, "arxi trigger pause") {
			t.Errorf("trigger %s does not point at pause:\n%s", verb, got.out)
		}
		if strings.Contains(got.out, "is not a trigger command") {
			t.Errorf("trigger %s was answered as a typo:\n%s", verb, got.out)
		}
	}
}

// TestADeclaredButUnbuiltSubcommandIsNotCalledUnknown is the regression guard
// for the bug this switch reintroduced once.
//
// Every declared trigger subcommand is currently built, so today this test can
// only assert that none of them is refused. That is worth having anyway: it is
// the check that fails on the day one is declared and not wired up, which is
// exactly when "unknown subcommand" would start lying.
func TestADeclaredButUnbuiltSubcommandIsNotCalledUnknown(t *testing.T) {
	dir := workdir(t)
	subs := declaredTriggerSubcommands()
	if len(subs) == 0 {
		t.Fatal("the registry declares no trigger subcommands; this test is testing nothing")
	}
	checked := 0
	for _, sub := range subs {
		// Invoked with no arguments: the ones that need a name will refuse on
		// the missing argument, which is a different message and a fine
		// outcome. What must never appear is the claim that the subcommand
		// itself does not exist.
		//
		// Bounded, because not every declared command terminates. `trigger
		// run` without --once loops until interrupted, which is its purpose,
		// and this loop reaches it precisely because it reads the registry
		// instead of hand-listing subcommands. Before the bound, declaring
		// that capability hung the whole cmd/arxi package for the full test
		// timeout and reported FAIL with a goroutine dump and no test name.
		//
		// A second is plenty: every refusal here is printed before any work
		// starts, so the message this test reads is already out.
		out := arxiBounded(t, dir, time.Second, "trigger", sub)
		checked++
		if strings.Contains(out, "is not a trigger command") {
			t.Errorf("trigger %s is declared in the registry but the CLI calls it unknown:\n%s",
				sub, out)
		}
	}
	if checked == 0 {
		t.Error("no declared subcommand was actually invoked")
	}
}

// TestTheTableAlignsAroundTheLongestName covers the fixed-width mutation.
//
// §20.10's example table happens to be aligned for one name; a hardcoded width
// is invisible until a name is longer than the guess, and then the column a
// reader scans down is the one that stops lining up.
func TestTheTableAlignsAroundTheLongestName(t *testing.T) {
	dir := workdir(t)
	names := []string{"a", "nightly-audit-with-a-very-long-name"}
	for _, n := range names {
		got := arxi(t, dir, "trigger", "create", n,
			"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
			"--budget", "1", "--budget-period", "day")
		if got.code != 0 {
			t.Fatalf("creating %s: %d\n%s", n, got.code, got.out)
		}
	}

	got := arxi(t, dir, "trigger", "list")
	if got.code != 0 {
		t.Fatalf("list: %d\n%s", got.code, got.out)
	}
	lines := strings.Split(strings.TrimRight(got.out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header and two rows, got %d lines:\n%s", len(lines), got.out)
	}

	// The ON column must start at the same offset on every line. That is the
	// property alignment actually means, and it is the one a fixed width
	// breaks.
	at := strings.Index(lines[0], "ON")
	if at <= 0 {
		t.Fatalf("no ON column in the header: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l[at:], "cron:0 3 * * *") {
			t.Errorf("the ON column does not start at offset %d on %q", at, l)
		}
	}
}

// TestAnEmptyListSaysThereAreNoTriggers guards against the bare header row.
//
// A header with nothing under it is the output that makes a user re-run the
// command wondering whether it worked, and the fact they need — that nothing is
// scheduled — is exactly what the blank table fails to state.
func TestAnEmptyListSaysThereAreNoTriggers(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "trigger", "list")
	if got.code != 0 {
		t.Fatalf("exit %d on an empty store:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "no triggers") {
		t.Errorf("an empty list said:\n%s\nwant it to say there are none", got.out)
	}
	// No header row. Checked as a line and not as a substring, because the
	// guidance below it legitimately contains the word NAME as a placeholder —
	// a Contains check here passes for the wrong reason and fails for the
	// wrong reason, which is worse than not checking.
	for _, l := range strings.Split(got.out, "\n") {
		if strings.HasPrefix(l, "NAME") && strings.Contains(l, "STATUS") {
			t.Errorf("an empty list printed a header row: %q", l)
		}
	}
	// And it says how to make one, because the state is almost always the
	// state a first-time user is in.
	if !strings.Contains(got.out, "arxi trigger create") {
		t.Errorf("an empty list does not say how to create one:\n%s", got.out)
	}
}

// TestShowAnnouncesTheAmbiguousDayRuleToAPerson closes a gap the JSON tests
// looked like they covered and did not.
//
// showPayload's `note` was tested, and a mutation that removed the note from
// the HUMAN output survived — the two are separate code paths saying the same
// thing to different audiences, and the audience that needs this one is the
// person reading `0 3 1 * 1` and seeing a monthly job.
//
// It is four extra runs a month that nobody authorised, and the only place the
// warning can land is beside the schedule it is about.
func TestShowAnnouncesTheAmbiguousDayRuleToAPerson(t *testing.T) {
	dir := workdir(t)

	if got := arxi(t, dir, "trigger", "create", "both-days",
		"--on", "cron:0 3 1 * 1", "--then", "run start t 'x'",
		"--budget", "1", "--budget-period", "day"); got.code != 0 {
		t.Fatalf("create: %d\n%s", got.code, got.out)
	}
	got := arxi(t, dir, "trigger", "show", "both-days")
	if got.code != 0 {
		t.Fatalf("show: %d\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "EITHER") {
		t.Errorf("show does not announce cron's day rule:\n%s\n"+
			"  read as AND this looks like a once-a-month job", got.out)
	}

	// And it is not printed on schedules where it does not apply: a warning
	// that appears on every trigger is a warning nobody reads, which is the
	// same as not printing it on the one that needs it.
	if got := arxi(t, dir, "trigger", "create", "plain",
		"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
		"--budget", "1", "--budget-period", "day"); got.code != 0 {
		t.Fatalf("create: %d\n%s", got.code, got.out)
	}
	got = arxi(t, dir, "trigger", "show", "plain")
	if strings.Contains(got.out, "EITHER") {
		t.Errorf("an unambiguous schedule carried the day-rule note:\n%s", got.out)
	}
}

// TestShowReportsMissedFiringsAndThePolicyTogether covers the other surviving
// human-output mutation.
//
// `trigger show` is the command somebody runs when they suspect a trigger slept
// through its firings, so silence here answers the question wrongly. The count
// and --on-missed are printed together deliberately: "4 missed" on its own
// invites the assumption that they are queued, and whether they are is exactly
// what that flag decides.
//
// The record is written directly rather than fired, because nothing fires yet —
// this is the state a scheduler will produce, and the display of it can be
// correct before the scheduler exists.
func TestShowReportsMissedFiringsAndThePolicyTogether(t *testing.T) {
	dir := workdir(t)

	if got := arxi(t, dir, "trigger", "create", "overslept",
		"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
		"--budget", "1", "--budget-period", "day",
		"--on-missed", "run-once"); got.code != 0 {
		t.Fatalf("create: %d\n%s", got.code, got.out)
	}

	// Backdate the last firing well past several slots. Rewriting the stored
	// JSON is legitimate here: it is the store's own format, and the
	// alternative is waiting three days.
	path := filepath.Join(dir, "triggers", "overslept.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the stored trigger: %v", err)
	}
	// A fixed past date, so the number of missed slots is large and stable
	// rather than depending on when the suite runs.
	patched := strings.Replace(string(body),
		`"created_at"`, `"last_fired_at": "2020-01-01T03:00:00Z", "created_at"`, 1)
	if patched == string(body) {
		t.Fatalf("could not insert last_fired_at into:\n%s", body)
	}
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		t.Fatalf("writing the patched trigger: %v", err)
	}

	got := arxi(t, dir, "trigger", "show", "overslept")
	if got.code != 0 {
		t.Fatalf("show: %d\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "missed") {
		t.Errorf("show says nothing about missed firings:\n%s\n"+
			"  this is the command run precisely when that is suspected", got.out)
	}
	if !strings.Contains(got.out, "on-missed=run-once") {
		t.Errorf("the missed count is printed without the policy that decides "+
			"what happens to them:\n%s", got.out)
	}
	// Years of daily slots exceed the cap, so the count must be hedged rather
	// than stated as a fact the walk did not establish.
	if !strings.Contains(got.out, "at least") {
		t.Errorf("a capped count is reported as exact:\n%s", got.out)
	}
}

// TestExitCodesSeparateMisuseFromFailure is the reason two of these paths are
// worth a subprocess at all.
//
// 2 means "you called this wrong" and 1 means "the thing you named is missing
// or the disk refused" — a CI job branches on that difference, and collapsing
// the two turns a mistyped schedule into a broken-storage report. This is also
// what makes the CLI's own Validate call more than a duplicate of the store's:
// without it, an impossible schedule arrives as exit 1.
func TestExitCodesSeparateMisuseFromFailure(t *testing.T) {
	dir := workdir(t)

	// A trigger to load, so the "missing" cases are missing for the right
	// reason.
	if got := arxi(t, dir, "trigger", "create", "exists",
		"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
		"--budget", "1", "--budget-period", "day"); got.code != 0 {
		t.Fatalf("setup create: %d\n%s", got.code, got.out)
	}

	misuse := []struct {
		name string
		args []string
	}{
		{"no subcommand", []string{"trigger"}},
		{"a subcommand that is not declared", []string{"trigger", "frobnicate"}},
		{"an unknown flag", []string{"trigger", "list", "--verbose"}},
		{"a missing positional", []string{"trigger", "show"}},
		{"an illegal enum value", []string{"trigger", "create", "x",
			"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
			"--budget", "1", "--budget-period", "fortnight"}},
		// The case that killed the surviving mutation: an impossible schedule
		// is a mistake in the argument, not a storage failure.
		{"an impossible schedule", []string{"trigger", "create", "x",
			"--on", "cron:0 3 30 2 *", "--then", "run start t 'x'",
			"--budget", "1", "--budget-period", "day"}},
	}
	for _, tc := range misuse {
		t.Run("misuse: "+tc.name, func(t *testing.T) {
			if got := arxi(t, dir, tc.args...); got.code != 2 {
				t.Errorf("exit %d, want 2 (misuse):\n%s", got.code, got.out)
			}
		})
	}

	failure := []struct {
		name string
		args []string
	}{
		{"showing a trigger that does not exist", []string{"trigger", "show", "absent"}},
		{"pausing a trigger that does not exist", []string{"trigger", "pause", "absent"}},
		{"creating one that already exists", []string{"trigger", "create", "exists",
			"--on", "cron:0 3 * * *", "--then", "run start t 'x'",
			"--budget", "1", "--budget-period", "day"}},
	}
	for _, tc := range failure {
		t.Run("failure: "+tc.name, func(t *testing.T) {
			if got := arxi(t, dir, tc.args...); got.code != 1 {
				t.Errorf("exit %d, want 1 (operational failure):\n%s", got.code, got.out)
			}
		})
	}
}
