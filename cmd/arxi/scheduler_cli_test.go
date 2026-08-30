package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `arxi trigger run`, exercised as a process.
//
// These use the harness in trigger_cli_test.go (TestMain, arxi, workdir) for
// the reason recorded there: cmdTriggerRun calls os.Exit on every refusal, and
// os.Exit in an in-process test kills the test binary. Exit codes and the
// difference between misuse and failure cannot be asserted any other way.
//
// Nothing here loops. Every test uses --once, because a test that started the
// ticker would have to sleep for an interval to observe anything, and a suite
// that sleeps is a suite people stop running. The loop's one interesting
// decision — that the first tick happens before the ticker is armed — is
// observable through --once, which is the same code path.

// mkTrigger creates a trigger in dir and fails the test if it is refused.
//
// The scheme prefix, the budget and the budget period are all mandatory, which
// is worth stating in one place: the first three attempts at the smoke test
// were all rejected for missing one of them, and a helper that hides the
// requirement means the next person rediscovers it the same way.
func mkTrigger(t *testing.T, dir, name, on, then string) {
	t.Helper()
	r := arxi(t, dir, "trigger", "create", "--name", name, "--on", on,
		"--then", then, "--budget", "5.00", "--budget-period", "day")
	if r.code != 0 {
		t.Fatalf("creating trigger %q: exit %d\n%s", name, r.code, r.out)
	}
}

// mkDueTrigger creates a trigger that is due right now, without sleeping.
//
// # Why backdating, and why the two obvious approaches do not work
//
// Nothing is due at the moment it is created: dueness is derived from
// CreatedAt (or LastFiredAt once it has fired), so a fresh `every:1m` trigger
// is due in a minute, and a fresh `cron:* * * * *` is due at the next minute
// boundary. Both were tried; both reported "not due until".
//
// A past `at:` instant looks like the answer and is not. `at:` describes a
// single firing, so a past one has already gone: NEXT reads `unresolvable` and
// the scheduler says "no firing left in this schedule". It is never due, which
// makes it useless for a test about firing — though it would be a fine test of
// an exhausted schedule.
//
// `every:1s` also looks like the answer and is refused at creation: there is a
// one-minute floor, because the scheduler evaluates on the minute and a
// per-day budget would be spent in an hour at that rate. That refusal is
// correct and it means no schedule can make this test fast.
//
// So the trigger is created through the CLI — which keeps every validation
// rule in force — and then its CreatedAt is rewritten on disk. The record is
// plain JSON with a `created_at` field, and moving it into the past is exactly
// the state a trigger reaches by existing for a minute. This buys nine tests
// that run instantly instead of nine that sleep a minute each.
func mkDueTrigger(t *testing.T, dir, name, then string) {
	t.Helper()
	mkTrigger(t, dir, name, "every:1m", then)

	path := filepath.Join(dir, "triggers", name+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	// Targeted at the field rather than rebuilding the document, so a new
	// field added to Record does not silently vanish from these fixtures.
	re := regexp.MustCompile(`"created_at":\s*"[^"]*"`)
	out := re.ReplaceAll(b, []byte(`"created_at": "2020-01-01T00:00:00Z"`))
	if bytes.Equal(out, b) {
		t.Fatalf("created_at not found in %s; the record format changed "+
			"and this helper no longer makes anything due:\n%s", path, b)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// arxiBounded runs the binary and kills it if it does not finish in time.
//
// # Why the harness needed this
//
// `arxi` waits for the child forever, which was safe while every command in the
// registry terminated on its own. `trigger run` does not: without --once it
// loops until interrupted, which is its entire purpose.
//
// That turned TestADeclaredButUnbuiltSubcommandIsNotCalledUnknown into a hang.
// It walks the registry and invokes each declared trigger subcommand with no
// arguments, deliberately refusing to hand-list them — which is the right
// design, and is exactly why it reached `trigger run` the moment the capability
// was declared. The whole `cmd/arxi` package then sat in os/exec copying a
// child's output until the test timeout, while every other package stayed
// green. `go test ./...` reported FAIL with a goroutine dump and no test name.
//
// So a test that invokes arbitrary declared commands has to bound them. The
// timeout is the assertion's tool, not a workaround: whether a command exits is
// not what that test is about, and the thing it does check — that the CLI never
// calls a declared subcommand unknown — is answerable from what was printed
// before the deadline.
//
// A killed child is not an error here. The exit code is meaningless once the
// signal decides it, so only the output is returned, and callers that care
// about exit codes should use arxi instead.
func arxiBounded(t *testing.T, dir string, d time.Duration, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	cmd := exec.CommandContext(ctx, buildIash(t), args...)
	cmd.Dir = dir

	// Its own process group, and killed as a group. `trigger run` spawns
	// children; killing only the parent would leave them running past the end
	// of the test, writing into a t.TempDir() that is being deleted.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	out, _ := cmd.CombinedOutput()
	return string(out)
}

// eventually polls until cond holds, or gives up.
//
// The children are separate processes, so `--once` returns while one may still
// be starting; there is no moment at which their effect is guaranteed visible.
// Polling rather than sleeping a fixed amount because a fixed sleep is either
// flaky or slow and usually both — this returns as soon as the effect lands,
// and only spends the full budget when the answer is genuinely no.
func eventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// lastCell returns the LAST cell for a trigger, as `trigger list` prints it.
//
// Read out of the table rather than out of the file on purpose. LAST is what a
// user looks at to answer "did my trigger run?", and the bug this file exists
// to pin was visible precisely there.
func lastCell(t *testing.T, dir, name string) string {
	t.Helper()
	r := arxi(t, dir, "trigger", "list")
	if r.code != 0 {
		t.Fatalf("trigger list: exit %d\n%s", r.code, r.out)
	}
	for _, line := range strings.Split(r.out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == name {
			return f[3] // NAME ON STATUS LAST NEXT
		}
	}
	t.Fatalf("no row for %q in:\n%s", name, r.out)
	return ""
}

// TestDryRunDoesNotConsumeTheSlotItPreviews is the regression test for the
// worst bug in this command's history.
//
// --dry-run faked the runner and left the store real, so it recorded
// LastFiredAt for a firing that never happened. The slot stopped being due,
// and the next REAL run answered "not due until" — a preview that silently
// cancelled a scheduled run.
//
// The assertion is deliberately the whole sequence and not just "LAST is
// never". A fix that made Save fail rather than no-op would also leave LAST
// alone, while turning every dry-run firing into an error instead of a plan;
// checking that the dry run REPORTED the firing and that the real run then
// still fires is what distinguishes the two.
func TestDryRunDoesNotConsumeTheSlotItPreviews(t *testing.T) {
	dir := workdir(t)
	mkDueTrigger(t, dir, "preview", "schema")

	before := lastCell(t, dir, "preview")

	dry := arxi(t, dir, "trigger", "run", "--dry-run", "--once")
	if dry.code != 0 {
		t.Fatalf("dry run: exit %d\n%s", dry.code, dry.out)
	}
	if !strings.Contains(dry.out, "would run") {
		t.Errorf("a dry run that previews nothing is not a preview; got:\n%s", dry.out)
	}

	if got := lastCell(t, dir, "preview"); got != before {
		t.Fatalf("--dry-run moved LAST from %q to %q: the preview consumed "+
			"the slot, so the real firing was skipped", before, got)
	}

	// The slot must still be there to fire.
	real1 := arxi(t, dir, "trigger", "run", "--once")
	if real1.code != 0 {
		t.Fatalf("real run: exit %d\n%s", real1.code, real1.out)
	}
	if got := lastCell(t, dir, "preview"); got == before {
		t.Fatalf("after a real run LAST is still %q: the firing did not "+
			"record, so it will fire again forever", got)
	}
}

// TestDryRunStartsNothing checks the other half of the fake.
//
// dryRunner must not spawn the child. `--then trigger create` is chosen
// because its effect is a file, which is observable after the process is gone;
// asserting on stdout would only prove the child printed nothing, not that it
// never ran.
func TestDryRunStartsNothing(t *testing.T) {
	dir := workdir(t)
	mkDueTrigger(t, dir, "spawner",
		"trigger create --name spawned --on every:1h --then schema "+
			"--budget 1.00 --budget-period day")

	r := arxi(t, dir, "trigger", "run", "--dry-run", "--once")
	if r.code != 0 {
		t.Fatalf("dry run: exit %d\n%s", r.code, r.out)
	}

	if _, err := os.Stat(filepath.Join(dir, "triggers", "spawned.json")); err == nil {
		t.Fatal("--dry-run started the child: the action's effect is on disk")
	}
}

// TestARealRunStartsTheChild is the same shape, inverted.
//
// This is the test that would have failed on the --triggers bug: a child dying
// on an unknown flag is reported as started by the scheduler, because Start
// succeeds when the process starts and not when the invocation turns out to be
// valid. Only the child's EFFECT distinguishes the two, which is why this
// asserts on the file and not on the report.
func TestARealRunStartsTheChild(t *testing.T) {
	dir := workdir(t)
	mkDueTrigger(t, dir, "spawner",
		"trigger create --name spawned --on every:1h --then schema "+
			"--budget 1.00 --budget-period day")

	r := arxi(t, dir, "trigger", "run", "--once")
	if r.code != 0 {
		t.Fatalf("run --once: exit %d\n%s", r.code, r.out)
	}

	if !eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "triggers", "spawned.json"))
		return err == nil
	}) {
		t.Fatal("the child never produced its effect: it was reported as " +
			"started but did not run — the shape the --triggers bug had")
	}
}

// TestChildrenInheritTheTriggerDirectory pins how a child finds the store.
//
// There is no --triggers flag; children inherit the directory by inheriting
// the working directory. The first version of scheduler.go passed a flag that
// does not exist, so every child would have died on an unknown flag while the
// scheduler reported success.
//
// The proof is that the spawned trigger appears in THIS test's temporary
// directory. If the child read the real triggers/ instead, the file would land
// somewhere else and this fails. Named in the comment on selfRunner as the
// test that breaks when a directory flag is added without threading it.
func TestChildrenInheritTheTriggerDirectory(t *testing.T) {
	dir := workdir(t)
	mkDueTrigger(t, dir, "spawner",
		"trigger create --name inherited --on every:1h --then schema "+
			"--budget 1.00 --budget-period day")

	if r := arxi(t, dir, "trigger", "run", "--once"); r.code != 0 {
		t.Fatalf("run --once: exit %d\n%s", r.code, r.out)
	}

	if !eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "triggers", "inherited.json"))
		return err == nil
	}) {
		t.Fatal("the child wrote its trigger somewhere other than the " +
			"parent's directory, so parent and child disagree about the store")
	}
}

// TestDryRunWithoutOnceIsRefused checks a misuse, and checks it is misuse.
//
// Looping while starting nothing prints the same report forever. Exit 2 and
// not 1: nothing failed, the combination is contradictory. That distinction is
// what a CI job acts on.
func TestDryRunWithoutOnceIsRefused(t *testing.T) {
	dir := workdir(t)
	mkTrigger(t, dir, "any", "every:1h", "schema")

	r := arxi(t, dir, "trigger", "run", "--dry-run")
	if r.code != 2 {
		t.Errorf("--dry-run without --once: exit %d, want 2 (misuse)\n%s",
			r.code, r.out)
	}
	if !strings.Contains(r.out, "--once") {
		t.Errorf("the refusal does not name the flag that fixes it:\n%s", r.out)
	}
}

// TestABadIntervalIsRefusedAsMisuse covers the unparseable case.
func TestABadIntervalIsRefusedAsMisuse(t *testing.T) {
	dir := workdir(t)
	mkTrigger(t, dir, "any", "every:1h", "schema")

	r := arxi(t, dir, "trigger", "run", "--interval", "soon", "--once")
	if r.code != 2 {
		t.Errorf("--interval soon: exit %d, want 2\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "30s") {
		t.Errorf("the error does not show what a duration looks like:\n%s", r.out)
	}
}

// TestANonPositiveIntervalIsRefusedRatherThanClamped covers zero.
//
// Clamping would be the friendly choice and is wrong: `--interval 0` is a spin
// loop reading the trigger directory as fast as the disk allows, and silently
// substituting a default hides the typo behind behaviour that looks correct.
// The user who typed 0 meant something else and needs to be told.
//
// Zero and "0s" are checked here. The negative case is a different test,
// because it is refused by a different layer — see below.
func TestANonPositiveIntervalIsRefusedRatherThanClamped(t *testing.T) {
	dir := workdir(t)
	mkTrigger(t, dir, "any", "every:1h", "schema")

	for _, iv := range []string{"0", "0s"} {
		r := arxi(t, dir, "trigger", "run", "--interval", iv)
		if r.code != 2 {
			t.Errorf("--interval %s: exit %d, want 2 (refused, not clamped)\n%s",
				iv, r.code, r.out)
		}
		if !strings.Contains(r.out, "positive") {
			t.Errorf("--interval %s: the refusal does not say why:\n%s", iv, r.out)
		}
	}
}

// TestANegativeIntervalIsRefusedByTheFlagParserNotTheIntervalCheck records
// which layer actually says no, because it is not the one I expected.
//
// `--interval -5s` never reaches ParseDuration. The flag parser sees `-5s` as
// a grouped short-flag cluster and expands it letter by letter, so it refuses
// with "trigger run has no short flag -5" long before the value is read as a
// duration.
//
// That is the correct answer for the wrong-looking reason, and it is worth a
// test of its own rather than folding it into the case above. The first version
// of this file asserted that the message contained "positive" for all three
// values and failed here — the assertion was wrong, not the code, and the
// distinction only became visible by running it.
//
// The `interval <= 0` guard in cmdTriggerRun therefore cannot be reached by a
// negative through the CLI today. It stays, because `<= 0` is the honest
// predicate for "positive" and because the guard is what protects NewTicker
// from panicking if the parser's short-flag handling ever changes. This test is
// what will fail on the day it does, and it names the layer that refuses now.
func TestANegativeIntervalIsRefusedByTheFlagParserNotTheIntervalCheck(t *testing.T) {
	dir := workdir(t)
	mkTrigger(t, dir, "any", "every:1h", "schema")

	r := arxi(t, dir, "trigger", "run", "--interval", "-5s")
	if r.code != 2 {
		t.Errorf("--interval -5s: exit %d, want 2\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "short flag") {
		t.Errorf("expected the flag parser to refuse -5s as a short-flag "+
			"cluster; if the interval check refuses it now, this test should "+
			"be merged back into the non-positive case:\n%s", r.out)
	}
}

// TestAnUndueTriggerIsReportedAndNotRun makes "nothing happened" visible.
//
// A scheduler that printed nothing when nothing was due would be
// indistinguishable from one that was broken, which is the complaint that
// makes people stop trusting an unattended process.
func TestAnUndueTriggerIsReportedAndNotRun(t *testing.T) {
	dir := workdir(t)
	mkTrigger(t, dir, "later", "every:1h", "schema")

	r := arxi(t, dir, "trigger", "run", "--once")
	if r.code != 0 {
		t.Fatalf("run --once: exit %d\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "later") {
		t.Errorf("the undue trigger is not mentioned at all:\n%s", r.out)
	}
	if got := lastCell(t, dir, "later"); got != "never" {
		t.Errorf("LAST is %q: an undue trigger was recorded as fired", got)
	}
}

// TestAPausedTriggerDoesNotFire checks that pause reaches the scheduler.
//
// pause was built before anything could fire, so until this command existed
// the flag was only ever read by `trigger show`. This is the first test that
// can prove it does something.
func TestAPausedTriggerDoesNotFire(t *testing.T) {
	dir := workdir(t)
	mkDueTrigger(t, dir, "halted", "schema")

	if r := arxi(t, dir, "trigger", "pause", "--name", "halted"); r.code != 0 {
		t.Fatalf("pause: exit %d\n%s", r.code, r.out)
	}

	if r := arxi(t, dir, "trigger", "run", "--once"); r.code != 0 {
		t.Fatalf("run --once: exit %d\n%s", r.code, r.out)
	}

	if got := lastCell(t, dir, "halted"); got != "never" {
		t.Errorf("LAST is %q: a paused trigger fired", got)
	}
}

// TestRunOnceOnAnEmptyDirectorySucceeds covers the boring case that is easy to
// get wrong.
//
// A scheduler with no triggers has nothing to do, which is not an error. Found
// while smoke-testing: the first attempt printed literally nothing and exited
// 0, and while the exit code is right, silence is the same output a broken
// build gives.
func TestRunOnceOnAnEmptyDirectorySucceeds(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "trigger", "run", "--once")
	if r.code != 0 {
		t.Errorf("no triggers is not a failure: exit %d\n%s", r.code, r.out)
	}
}
