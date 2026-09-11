package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/app"
	arxiexec "github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/jobstore"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/supervisor"
	"github.com/michiTrader/arxi/internal/trigger"
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

func TestScheduledStartUsesRecordBudgetAndRejectsAConflictingActionBudget(t *testing.T) {
	f, err := parseScheduledStartArgs([]string{"team.yaml", "audit", "--sim"}, 5)
	if err != nil {
		t.Fatalf("record budget was not supplied to an action that omitted it: %v", err)
	}
	if f.budget != 5 || !f.budgetSet {
		t.Fatalf("scheduled flags budget = %v (set %v), want record ceiling 5", f.budget, f.budgetSet)
	}
	for _, args := range [][]string{
		{"team.yaml", "audit", "--sim", "--budget", "4"},
		{"team.yaml", "audit", "--sim", "--budget=6"},
	} {
		if _, err := parseScheduledStartArgs(args, 5); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("conflicting action budget %v was not explicitly rejected: %v", args, err)
		}
	}
	if _, err := parseScheduledStartArgs([]string{"team.yaml", "audit", "--sim", "--budget", "5"}, 5); err != nil {
		t.Fatalf("equal explicit and record budgets should agree: %v", err)
	}
}

type schedulerLifecycle struct {
	mu       sync.Mutex
	launches int
	started  chan struct{}
}

func newSchedulerLifecycle() *schedulerLifecycle {
	return &schedulerLifecycle{started: make(chan struct{})}
}

func (l *schedulerLifecycle) prepare(t *testing.T, f startFlags, accepted func(string)) (cliSubmission, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), f.runID)
	blueprint := []byte("name: scheduled\nmembers:\n  - {name: agent}\nstages:\n  - {name: wait, advance_when: all}\n")
	cfg := kernel.Config{
		Workspace: "none",
		Members:   []kernel.MemberConfig{{Name: "agent"}},
		Stages:    []kernel.StageConfig{{Name: "wait", AdvanceWhen: "all"}},
	}
	digest := sha256.Sum256(blueprint)
	artifact := runconfig.New(f.runID, "sim", hex.EncodeToString(digest[:]), f.prompt, f.model, cfg, nil, nil)
	sup := supervisor.New(filepath.Dir(dir), supervisor.Options{Build: func(string, runconfig.Artifact) (arxiexec.Executor, error) {
		return &schedulerBlockingExecutor{lifecycle: l}, nil
	}})
	prepared := app.PreparedSubmission{
		JobID: f.runID, Actor: "scheduled", Blueprint: blueprint,
		Artifact: artifact, BudgetUSD: f.budget, Location: dir,
		OnAccepted: func(_ app.SubmitResult, dir string, _ kernel.Config) { accepted(dir) },
	}
	return cliSubmission{service: app.AcceptanceServices{RunsDir: filepath.Dir(dir), Lifecycle: sup}, supervisor: sup, prepared: prepared}, nil
}

type schedulerBlockingExecutor struct {
	lifecycle *schedulerLifecycle
}

func (e *schedulerBlockingExecutor) SpawnTurn(context.Context, kernel.SpawnTurn) ([]kernel.Event, error) {
	e.lifecycle.mu.Lock()
	e.lifecycle.launches++
	if e.lifecycle.launches == 1 {
		close(e.lifecycle.started)
	}
	e.lifecycle.mu.Unlock()
	return []kernel.Event{{Type: kernel.AgentTurnDone, Source: kernel.SourceRuntime}}, nil
}

func (*schedulerBlockingExecutor) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	return nil, nil
}

func (*schedulerBlockingExecutor) AskHuman(context.Context, kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}

func TestScheduledClaimExpiresAndReplacementResumesFrozenRun(t *testing.T) {
	originalNow := nowFunc
	defer func() { nowFunc = originalNow }()
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	store := jobstore.NewMemory(nowFunc)
	defer store.Close()
	nominal := now.Add(-time.Minute)
	occurrence := job.Occurrence{ID: job.OccurrenceIdentity("trigger", nominal), TriggerID: "trigger", NominalAt: nominal,
		State: job.OccurrenceAdmitted, JobID: "job-restart", ReservationID: "reservation-restart"}
	_, revision, err := store.Admit(0, jobstore.Admission{Occurrence: occurrence,
		Window:  job.LedgerWindow{TriggerID: "trigger", Period: job.PeriodDay, StartsAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)},
		Ceiling: job.NewAmount(5, 0), Reserved: job.NewAmount(5, 0)})
	if err != nil {
		t.Fatal(err)
	}
	first, revision, err := store.Claim(revision, occurrence.JobID, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = first.ExpiresAt
	second, _, err := store.Claim(revision, occurrence.JobID, "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("replacement claim after expiry: %v", err)
	}
	if second.AttemptID == first.AttemptID || second.Fence <= first.Fence {
		t.Fatalf("replacement claim = %#v after %#v: restart must create a fresh attempt and increasing fence", second, first)
	}
	if store.View().Attempts[first.AttemptID].State != job.AttemptExpired {
		t.Fatal("replacement claim did not durably expire the crashed worker before takeover")
	}
}

func TestAmbiguousScheduledOutcomeFinishesUnknownAndKeepsReservation(t *testing.T) {
	originalNow := nowFunc
	defer func() { nowFunc = originalNow }()
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	store := jobstore.NewMemory(nowFunc)
	defer store.Close()
	nominal := now.Add(-time.Minute)
	occurrence := job.Occurrence{ID: job.OccurrenceIdentity("trigger", nominal), TriggerID: "trigger", NominalAt: nominal,
		State: job.OccurrenceAdmitted, JobID: "job-unknown", ReservationID: "reservation-unknown"}
	_, revision, err := store.Admit(0, jobstore.Admission{Occurrence: occurrence,
		Window:  job.LedgerWindow{TriggerID: "trigger", Period: job.PeriodDay, StartsAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)},
		Ceiling: job.NewAmount(5, 0), Reserved: job.NewAmount(5, 0)})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := store.Claim(revision, occurrence.JobID, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	coordination := &scheduledClaim{store: store, claim: claim, occurrence: occurrence.ID, reservation: occurrence.ReservationID}
	if err := coordination.Finish(arxiexec.Outcome{}, arxiexec.ErrUnknownWork); err != nil {
		t.Fatalf("finish ambiguous scheduled work: %v", err)
	}
	view := store.View()
	if view.Jobs[occurrence.JobID].State != job.JobUnknown || view.Occurrences[occurrence.ID].State != job.OccurrenceUnknown ||
		view.Reservations[occurrence.ReservationID].State != job.ReservationUnknown {
		t.Fatalf("ambiguous projection job=%s occurrence=%s reservation=%s: no redispatch is safe and the full budget must remain held",
			view.Jobs[occurrence.JobID].State, view.Occurrences[occurrence.ID].State, view.Reservations[occurrence.ReservationID].State)
	}
}

func TestScheduledRunStartAcceptsNativelyAndOnceDoesNotAbandonIt(t *testing.T) {
	life := newSchedulerLifecycle()
	runner := selfRunner{prepare: func(f startFlags, accepted func(string)) (cliSubmission, error) {
		return life.prepare(t, f, accepted)
	}}
	rec := trigger.Record{Name: "nightly", Budget: 3}
	action := trigger.Action{Path: []string{"run", "start"}, Args: []string{"team.yaml", "audit", "--sim"}}
	execution, err := runner.Start(rec, action)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(logstore.EventsPath(execution.(*runExec).dir)); err != nil {
		t.Fatalf("Start returned before durable acceptance: %v", err)
	}
	select {
	case <-execution.Done():
		t.Fatal("accepted idle run was abandoned when its first drive pass stopped")
	case <-time.After(50 * time.Millisecond):
	}
	execution.Cancel()
	select {
	case <-execution.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after terminal cancellation observation")
	}
}

func TestScheduledRunCancelIsOneLifecycleRequestAndNotCompletion(t *testing.T) {
	life := newSchedulerLifecycle()
	runner := selfRunner{prepare: func(f startFlags, accepted func(string)) (cliSubmission, error) {
		return life.prepare(t, f, accepted)
	}}
	execution, err := runner.Start(trigger.Record{Budget: 2}, trigger.Action{
		Path: []string{"run", "start"}, Args: []string{"team.yaml", "audit", "--sim"},
	})
	if err != nil {
		t.Fatal(err)
	}
	execution.Cancel()
	execution.Cancel()
	select {
	case <-execution.Done():
		t.Fatal("Cancel closed Done before the lifecycle observed terminal state")
	default:
	}
	if !eventually(t, func() bool {
		return eventTypeCount(execution.(*runExec).dir, kernel.RunCancelled) == 1
	}) {
		t.Fatal("cancellation request did not reach the resident lifecycle")
	}
	select {
	case <-execution.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("terminal cancellation was not observed")
	}
	time.Sleep(25 * time.Millisecond)
	if got := eventTypeCount(execution.(*runExec).dir, kernel.RunCancelled); got != 1 {
		t.Fatalf("run.cancelled events = %d, want exactly one", got)
	}
}

func TestResidentScheduledDecisionIsAcknowledgedAtActualSequence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r-scheduled")
	store := scheduledApprovalStore(t, dir)
	store.Close()

	sup := supervisor.New(filepath.Dir(dir), supervisor.Options{Build: func(string, runconfig.Artifact) (arxiexec.Executor, error) {
		return &schedulerBlockingExecutor{lifecycle: newSchedulerLifecycle()}, nil
	}})
	h, err := sup.Open(context.Background(), "r-scheduled")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go watchExternalDecisions(dir, h, stop)
	defer close(stop)
	defer sup.Close(context.Background())

	sequence, err := appendExternalDecision(dir, externalInboxEvent("inbox-1", inbox.Reply{
		Decision: inbox.DecisionApprove,
	}), true)
	if err != nil {
		t.Fatalf("external decision: %v", err)
	}
	if sequence != 9 {
		t.Fatalf("acknowledged sequence = %d, want actual appended sequence 9", sequence)
	}
	if got := eventTypeCount(dir, kernel.InboxReplied); got != 1 {
		t.Fatalf("inbox replies = %d, want 1", got)
	}
}

func TestResidentScheduledDecisionRefusesDuplicate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r-scheduled")
	store := scheduledApprovalStore(t, dir)
	store.Close()

	sup := supervisor.New(filepath.Dir(dir), supervisor.Options{Build: func(string, runconfig.Artifact) (arxiexec.Executor, error) {
		return &schedulerBlockingExecutor{lifecycle: newSchedulerLifecycle()}, nil
	}})
	h, err := sup.Open(context.Background(), "r-scheduled")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go watchExternalDecisions(dir, h, stop)
	defer close(stop)
	defer sup.Close(context.Background())

	reply := externalInboxEvent("inbox-1", inbox.Reply{Decision: inbox.DecisionApprove})
	if _, err := appendExternalDecision(dir, reply, true); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	if _, err := appendExternalDecision(dir, reply, true); err == nil || residentErrorCode(err) != "already_answered" {
		t.Fatalf("duplicate decision error = %v, code %q; want already_answered", err, residentErrorCode(err))
	}
	if got := eventTypeCount(dir, kernel.InboxReplied); got != 1 {
		t.Fatalf("duplicate appended %d inbox replies, want 1", got)
	}
}

func scheduledApprovalStore(t *testing.T, dir string) *logstore.Store {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := []byte("name: scheduled\nmembers:\n  - name: agent\n")
	if err := os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), snapshot, 0o644); err != nil {
		t.Fatal(err)
	}
	blueprintSum := sha256.Sum256(snapshot)
	blueprintSHA := hex.EncodeToString(blueprintSum[:])
	cfg := kernel.Config{Blueprint: "scheduled", Members: []kernel.MemberConfig{{Name: "agent"}}}.ResolveDefaults()
	digest, err := runconfig.Publish(dir, runconfig.New("r-scheduled", "sim", blueprintSHA, "prompt", "model", cfg, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		{Type: kernel.RunStarted, Payload: map[string]any{
			"run_id": "r-scheduled", "actor": "scheduled", "blueprint_sha": blueprintSHA,
			"effective_config_schema": runconfig.Schema, "effective_config_path": runconfig.FileName,
			"effective_config_sha": digest,
		}},
		{Type: kernel.ExecStepCompleted, Source: kernel.SourceRuntime, Payload: map[string]any{"source_seq": int64(1), "work_ids": []string{}}},
		{Type: kernel.StageEntered, Payload: map[string]any{"stage": "work", "index": float64(0)}},
		{Type: kernel.ExecStepCompleted, Source: kernel.SourceRuntime, Payload: map[string]any{"source_seq": int64(3), "work_ids": []string{}}},
		{Type: kernel.AgentActivated, Actor: "agent", Payload: map[string]any{"agent": "agent"}},
		{Type: kernel.ExecStepCompleted, Source: kernel.SourceRuntime, Payload: map[string]any{"source_seq": int64(5), "work_ids": []string{}}},
		{Type: kernel.InboxCreated, Payload: map[string]any{"inbox_id": "inbox-1", "agent": "agent", "kind": "tool_approval", "question": "allow?"}},
		{Type: kernel.ExecStepCompleted, Source: kernel.SourceRuntime, Payload: map[string]any{"source_seq": int64(7), "work_ids": []string{}}},
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store
}

func eventTypeCount(dir string, typ kernel.EventType) int {
	read, err := logstore.ReadConfirmed(dir, 0)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range bytes.Split(read.Bytes, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event kernel.Event
		if json.Unmarshal(line, &event) == nil && event.Type == typ {
			n++
		}
	}
	return n
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
