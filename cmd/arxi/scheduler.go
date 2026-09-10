package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/michiTrader/arxi/internal/app"
	arxiexec "github.com/michiTrader/arxi/internal/exec"
	internalinbox "github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/jobstore"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/scheduler"
	"github.com/michiTrader/arxi/internal/supervisor"
	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/trigger"
)

// `arxi trigger run` — the caller the tick never had.
//
// internal/trigger decides WHEN (Due), internal/trigger decides WHETHER
// (Admit), internal/scheduler decides WHAT HAPPENS (Tick). All three were built
// and tested before this file existed, which means all three were unreachable:
// a scheduler with no caller outside its own tests is a library, not a feature.
// This is the file that makes a user able to reach it.

// lifecycleRegistry exposes resident scheduled workers to other command routes
// in this process. The exact run directory is the key because ids repeat across
// roots and renamed legacy directories remain addressable by the inbox CLI.
type lifecycleRegistry struct {
	mu   sync.Mutex
	runs map[string]*supervisor.Handle
}

var nativeLifecycles = lifecycleRegistry{runs: map[string]*supervisor.Handle{}}
var externalDecisionSequence atomic.Uint64

const externalDecisionTimeout = 5 * time.Second
const scheduledLeaseDuration = 30 * time.Second
const scheduledHeartbeatCadence = 10 * time.Second

type externalDecisionRequest struct {
	Event kernel.Event `json:"event"`
	Exact bool         `json:"exact"`
}

type externalDecisionResult struct {
	Sequence int64  `json:"sequence,omitempty"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
}

func lifecycleKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return filepath.Clean(abs)
}

func (r *lifecycleRegistry) register(dir string, h *supervisor.Handle) func() {
	if h == nil {
		return func() {}
	}
	key := lifecycleKey(dir)
	r.mu.Lock()
	r.runs[key] = h
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.runs[key] == h {
			delete(r.runs, key)
		}
		r.mu.Unlock()
	}
}

func (r *lifecycleRegistry) decision(dir string, fn supervisor.Command) (bool, error) {
	r.mu.Lock()
	h := r.runs[lifecycleKey(dir)]
	r.mu.Unlock()
	if h == nil {
		return false, nil
	}
	if err := h.Command(context.Background(), fn); err != nil {
		if errors.Is(err, supervisor.ErrClosed) {
			return false, nil
		}
		return true, err
	}
	h.Wake()
	return true, nil
}

// appendExternalDecision is the cross-process fallback for a resident scheduled
// worker. The worker owns the writer lock, so the inbox process publishes an
// exact one-event request beside the run rather than contending for events.ndjson.
// The resident imports it through Handle.Command, then removes the request.
func appendExternalDecision(dir string, event kernel.Event, exact bool) (int64, error) {
	pending := filepath.Join(dir, "inbox.decisions")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		return 0, err
	}
	request := externalDecisionRequest{Event: event, Exact: exact}
	body, err := json.Marshal(request)
	if err != nil {
		return 0, err
	}
	body = append(body, '\n')
	name := fmt.Sprintf("%s-%d-%d", event.Str("inbox_id"), os.Getpid(), externalDecisionSequence.Add(1))
	requestPath := filepath.Join(pending, name+".request")
	resultPath := filepath.Join(pending, name+".result")
	tmp := filepath.Join(pending, "."+name+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, requestPath); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	defer os.Remove(requestPath)
	defer os.Remove(resultPath)
	deadline := time.Now().Add(externalDecisionTimeout)
	for {
		body, err := os.ReadFile(resultPath)
		if err == nil {
			var result externalDecisionResult
			if err := json.Unmarshal(body, &result); err != nil {
				return 0, fmt.Errorf("decode resident decision result: %w", err)
			}
			if result.Error != "" {
				return 0, externalDecisionError(result)
			}
			return result.Sequence, nil
		}
		if !os.IsNotExist(err) {
			return 0, err
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("resident run did not acknowledge inbox decision within %s", externalDecisionTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func importExternalDecisions(store *logstore.Store) (bool, error) {
	pending := filepath.Join(store.Dir(), "inbox.decisions")
	entries, err := os.ReadDir(pending)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	imported := false
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".request" {
			continue
		}
		if imported {
			break
		}
		requestPath := filepath.Join(pending, entry.Name())
		resultPath := strings.TrimSuffix(requestPath, ".request") + ".result"
		body, err := os.ReadFile(requestPath)
		if err != nil {
			return imported, err
		}
		var request externalDecisionRequest
		if err := json.Unmarshal(body, &request); err != nil {
			return imported, fmt.Errorf("decode external decision %s: %w", entry.Name(), err)
		}
		var event kernel.Event
		if request.Exact {
			reply := internalinbox.Reply{Decision: request.Event.Str("decision"), Text: request.Event.Str("text")}
			event, err = internalinbox.AnswerExactStore(store, request.Event.Str("inbox_id"), reply)
		} else {
			err = validateExternalDecision(store, request.Event, false)
			if err == nil {
				request.Event.Seq = 0
				var written []kernel.Event
				written, err = store.Append([]kernel.Event{request.Event})
				if err == nil && len(written) == 1 {
					event = written[0]
				}
			}
		}
		result := externalDecisionResult{Sequence: event.Seq}
		if err != nil {
			result.Error, result.Code = err.Error(), externalDecisionCode(err)
		}
		if err := writeExternalDecisionResult(resultPath, result); err != nil {
			return imported, err
		}
		if err := os.Remove(requestPath); err != nil && !os.IsNotExist(err) {
			return imported, err
		}
		if result.Error == "" {
			imported = true
		}
	}
	return imported, nil
}

func validateExternalDecision(store *logstore.Store, event kernel.Event, exact bool) error {
	if event.Type != kernel.InboxReplied || event.Source != kernel.SourceHuman {
		return errors.New("external decision is not a human inbox reply")
	}
	id := event.Str("inbox_id")
	decision := event.Str("decision")
	text := event.Str("text")
	reply := internalinbox.Reply{Decision: decision, Text: text}
	if decision != internalinbox.DecisionApprove && decision != internalinbox.DecisionReject && decision != internalinbox.DecisionAnswer {
		return fmt.Errorf("invalid external inbox decision %q", decision)
	}
	run, err := internalinbox.OpenRun(store.Dir())
	if err != nil {
		return err
	}
	item, err := run.Item(id)
	if err != nil {
		return err
	}
	if item.Replied {
		return internalinbox.ErrAlreadyAnswered
	}
	if run.State().Status.Terminal() {
		return internalinbox.ErrRunOver
	}
	if decision == internalinbox.DecisionReject && strings.TrimSpace(reply.Text) == "" {
		return errors.New("rejection needs a reason")
	}
	if decision == internalinbox.DecisionAnswer && strings.TrimSpace(reply.Text) == "" {
		return errors.New("answer text is required")
	}
	if item.Kind == "tool_approval" {
		if decision == internalinbox.DecisionAnswer {
			return internalinbox.ErrWrongDecisionKind
		}
	} else if decision != internalinbox.DecisionAnswer || exact && item.Kind != "question" {
		return internalinbox.ErrWrongDecisionKind
	}
	return nil
}

func writeExternalDecisionResult(path string, result externalDecisionResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func externalDecisionCode(err error) string {
	switch {
	case errors.Is(err, internalinbox.ErrAlreadyAnswered):
		return "already_answered"
	case errors.Is(err, internalinbox.ErrNoSuchItem):
		return "not_found"
	case errors.Is(err, internalinbox.ErrWrongDecisionKind):
		return "wrong_kind"
	case errors.Is(err, internalinbox.ErrRunOver):
		return "run_over"
	default:
		return ""
	}
}

type residentDecisionError struct {
	message string
	code    string
}

func (e *residentDecisionError) Error() string { return e.message }

func externalDecisionError(result externalDecisionResult) error {
	return &residentDecisionError{message: result.Error, code: result.Code}
}

func watchExternalDecisions(dir string, h *supervisor.Handle, stop <-chan struct{}) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "inbox.decisions")); err != nil {
				continue
			}
			_ = h.Command(context.Background(), func(store *logstore.Store) error {
				_, err := importExternalDecisions(store)
				return err
			})
			h.Wake()
		}
	}
}

// selfRunner dispatches native run starts through the shared acceptance and
// supervisor lifecycle. Other triggerable commands retain subprocess isolation.
type selfRunner struct {
	// self is the binary used by the fallback path. It is resolved once so all
	// fallback firings agree about which executable they invoke.
	self string

	// prepare is a test seam around the native CLI preparation path. Keeping this
	// seam here (rather than rebuilding app input in the scheduler) guarantees the
	// scheduled path freezes the same routes, prices, policy, workspace and tool
	// runner as an interactive run start.
	prepare func(startFlags, func(string)) (cliSubmission, error)

	coordinator jobstore.Store
	owner       string

	// resident retains process ownership of accepted native runs. Subprocess
	// fallbacks survive a one-shot scheduler process on their own; goroutines do
	// not, so --once waits on this group before allowing the process to exit.
	resident sync.WaitGroup
}

func newSelfRunner(self string) *selfRunner {
	return &selfRunner{
		self:  self,
		owner: fmt.Sprintf("scheduler-%d", os.Getpid()),
		prepare: func(f startFlags, accepted func(string)) (cliSubmission, error) {
			bp, err := resolveActor(f.actor)
			if err != nil {
				return cliSubmission{}, err
			}
			if !f.sim {
				if err := checkEveryMemberHasAModel(bp.Config, f.model); err != nil {
					return cliSubmission{}, err
				}
			}
			return prepareCLISubmission(f, bp, func(dir string, _ kernel.Config) {
				accepted(dir)
			})
		},
	}
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
// without changing the working directory (`arxi -C <dir>`, say). There is none
// today, and TestChildrenInheritTheTriggerDirectory is what fails on the day
// one arrives.

// Start routes only run start through the resident lifecycle. The command
// surface remains the fallback vocabulary for every other trigger action.
func (r *selfRunner) Start(rec trigger.Record, a trigger.Action) (scheduler.Execution, error) {
	if isRunStart(a) {
		return r.startRun(rec, scheduler.Slot{}, a)
	}
	return r.startSubprocess(a)
}

func (r *selfRunner) StartSlot(rec trigger.Record, slot scheduler.Slot, a trigger.Action) (scheduler.Execution, error) {
	if isRunStart(a) {
		return r.startRun(rec, slot, a)
	}
	return r.startSubprocess(a)
}

func isRunStart(a trigger.Action) bool {
	return len(a.Path) == 2 && a.Path[0] == "run" && a.Path[1] == "start"
}

func (r *selfRunner) startRun(rec trigger.Record, slot scheduler.Slot, a trigger.Action) (scheduler.Execution, error) {
	args := append([]string(nil), a.Args...)
	f, err := parseScheduledStartArgs(args, rec.Budget)
	if err != nil {
		return nil, err
	}
	if slot.JobID != "" {
		f.runID = string(slot.JobID)
	}
	prepare := r.prepare
	if prepare == nil {
		prepare = newSelfRunner(r.self).prepare
	}
	var acceptedDir string
	runtime, err := prepare(f, func(dir string) { acceptedDir = dir })
	if err != nil {
		return nil, err
	}
	if slot.OccurrenceID != "" {
		if r.coordinator == nil {
			_ = runtime.supervisor.Close(context.Background())
			return nil, errors.New("scheduled acceptance has no durable coordinator")
		}
		runtime.prepared.IdempotencyKey = string(slot.OccurrenceID)
		runtime.service.Submissions = coordinatorSubmissionAdapter{store: r.coordinator}
		runtime.service.Lifecycle = deferredLifecycle{}
	}
	submission, err := runtime.service.SubmitPrepared(context.Background(), runtime.prepared)
	if err != nil {
		_ = runtime.supervisor.Close(context.Background())
		return nil, err
	}
	if slot.OccurrenceID != "" {
		claim, err := claimScheduledJob(r.coordinator, slot.JobID, r.owner)
		if err != nil {
			_ = runtime.supervisor.Close(context.Background())
			return nil, fmt.Errorf("claim admitted job %s: %w", slot.JobID, err)
		}
		coordination := &scheduledClaim{store: r.coordinator, claim: claim, occurrence: slot.OccurrenceID, reservation: "reservation-" + string(slot.OccurrenceID)}
		if err := runtime.supervisor.ConfigureClaim(coordination, scheduledHeartbeatCadence); err != nil {
			_ = runtime.supervisor.Close(context.Background())
			return nil, err
		}
		if err := runtime.supervisor.LaunchAt(context.Background(), submission.Result.JobID, submission.Dir); err != nil {
			_ = runtime.supervisor.Close(context.Background())
			return nil, err
		}
		submission.Handle, err = runtime.supervisor.OpenAt(context.Background(), submission.Result.JobID, submission.Dir)
		if err != nil {
			_ = runtime.supervisor.Close(context.Background())
			return nil, err
		}
	}
	if acceptedDir == "" {
		acceptedDir = submission.Dir
	}
	ex := &runExec{
		jobID: submission.Result.JobID, dir: acceptedDir,
		supervisor: runtime.supervisor, submission: submission,
		done: make(chan struct{}), onDone: r.resident.Done,
		externalStop: make(chan struct{}),
	}
	ex.unregister = nativeLifecycles.register(acceptedDir, submission.Handle)
	go watchExternalDecisions(acceptedDir, submission.Handle, ex.externalStop)
	r.resident.Add(1)
	go ex.observe()
	return ex, nil
}

type deferredLifecycle struct{}

func (deferredLifecycle) Launch(context.Context, string) error { return nil }

func claimScheduledJob(store jobstore.Store, id job.JobID, owner string) (job.Claim, error) {
	for {
		view := store.View()
		claim, _, err := store.Claim(view.Revision, id, owner, scheduledLeaseDuration)
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		return claim, err
	}
}

type scheduledClaim struct {
	mu          sync.Mutex
	store       jobstore.Store
	claim       job.Claim
	occurrence  job.OccurrenceID
	reservation string
}

func (c *scheduledClaim) Checkpoint(cursor, revision int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	view := c.store.View()
	_, err := c.store.Checkpoint(view.Revision, job.Checkpoint{JobID: c.claim.JobID, AttemptID: c.claim.AttemptID,
		Fence: c.claim.Fence, RunRevision: uint64(revision), CompletedCursor: uint64(cursor), CreatedAt: nowFunc().UTC()})
	return err
}

func (c *scheduledClaim) Heartbeat() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	view := c.store.View()
	claim, _, err := c.store.Heartbeat(view.Revision, c.claim.JobID, c.claim.AttemptID, c.claim.Fence, scheduledLeaseDuration)
	if err == nil {
		c.claim = claim
	}
	return err
}

func (c *scheduledClaim) Finish(out arxiexec.Outcome, runErr error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	attemptState, jobState, occurrenceState, settlement := job.AttemptFailed, job.JobFailed, job.OccurrenceCompleted, jobstore.SettlementRelease
	if errors.Is(runErr, arxiexec.ErrUnknownWork) {
		attemptState, jobState, occurrenceState, settlement = job.AttemptUnknown, job.JobUnknown, job.OccurrenceUnknown, jobstore.SettlementUnknown
	} else if out.State.Status == kernel.StatusCancelled {
		attemptState, jobState = job.AttemptCancelled, job.JobCancelled
	} else if out.State.Status == kernel.StatusSucceeded {
		attemptState, jobState, settlement = job.AttemptSucceeded, job.JobSucceeded, jobstore.SettlementSpend
	} else if runErr == nil {
		return nil
	}
	spent, err := amountFromRuntimeUSD(out.State.TreeSpentUSD)
	if err != nil {
		return err
	}
	if settlement != jobstore.SettlementSpend {
		spent = job.Amount{}
	}
	view := c.store.View()
	_, err = c.store.Finalize(view.Revision, jobstore.Finalization{
		Completion: jobstore.Completion{JobID: c.claim.JobID, AttemptID: c.claim.AttemptID, Fence: c.claim.Fence, AttemptState: attemptState, JobState: jobState},
		Settlement: jobstore.Settlement{JobID: c.claim.JobID, AttemptID: c.claim.AttemptID, Fence: c.claim.Fence, ReservationID: c.reservation, Kind: settlement, Spent: spent},
		Occurrence: c.occurrence, State: occurrenceState,
	})
	return err
}

func amountFromRuntimeUSD(value float64) (job.Amount, error) {
	if value < 0 {
		return job.Amount{}, errors.New("confirmed spend cannot be negative")
	}
	if value == 0 {
		return job.Amount{}, nil
	}
	text := strconv.FormatFloat(value, 'f', 9, 64)
	var coefficient uint64
	var scale uint8
	fraction := false
	for _, ch := range text {
		if ch == '.' {
			fraction = true
			continue
		}
		coefficient = coefficient*10 + uint64(ch-'0')
		if fraction {
			scale++
		}
	}
	return job.NewAmount(coefficient, scale), nil
}

// parseScheduledStartArgs applies the trigger's per-period ceiling to the run
// when the action omits --budget. An explicit action budget must equal it: two
// different ceilings cannot both be honestly described as the limit, and taking
// either the larger or smaller one would silently change configuration.
func parseScheduledStartArgs(args []string, recordBudget float64) (startFlags, error) {
	seed := startFlags{workspace: "auto"}
	if !hasBudgetFlag(args) {
		seed.budget, seed.budgetSet = recordBudget, true
	}
	f, err := parseStartArgs(args, seed)
	if err != nil {
		return f, err
	}
	if f.budget != recordBudget {
		return f, fmt.Errorf("action --budget %s conflicts with trigger budget %s; remove --budget or make the two ceilings equal",
			usd(f.budget), usd(recordBudget))
	}
	return f, nil
}

func hasBudgetFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--budget" || len(arg) > len("--budget=") && arg[:len("--budget=")] == "--budget=" {
			return true
		}
	}
	return false
}

// startSubprocess preserves the existing execution and process-group cancellation
// semantics for commands which have no private in-process lifecycle adapter.
func (r *selfRunner) startSubprocess(a trigger.Action) (scheduler.Execution, error) {
	args := append(append([]string{}, a.Path...), a.Args...)
	cmd := exec.Command(r.self, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ex := &childExec{cmd: cmd, done: make(chan struct{})}
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

// runExec observes one durably accepted resident run. Idle and blocked outcomes
// keep it in flight; only a terminal fold or definitive observation failure closes
// Done. Cancel appends one lifecycle cancellation request through the worker and
// does not pretend that request is completion.
type runExec struct {
	jobID        string
	dir          string
	supervisor   interface{ Close(context.Context) error }
	submission   app.Submission
	done         chan struct{}
	onDone       func()
	unregister   func()
	externalStop chan struct{}
	cancelOnce   sync.Once
}

func (r *runExec) Done() <-chan struct{} { return r.done }

func (r *runExec) observe() {
	defer close(r.done)
	if r.onDone != nil {
		defer r.onDone()
	}
	if r.unregister != nil {
		defer r.unregister()
	}
	if r.externalStop != nil {
		defer close(r.externalStop)
	}
	defer r.supervisor.Close(context.Background())
	_, _ = app.Wait(context.Background(), r.submission, app.WaitTerminal)
}

func (r *runExec) Cancel() {
	r.cancelOnce.Do(func() {
		h := r.submission.Handle
		if h == nil {
			return
		}
		go func() {
			services := app.MutationServices{RunsDir: filepath.Dir(r.dir), Now: nowFunc}
			_ = h.Command(context.Background(), func(store *logstore.Store) error {
				_, err := services.CancelStore(store, r.jobID, "scheduler overlap policy")
				return err
			})
			h.Wake()
		}()
	})
}

type schedulerCoordinatorAdapter struct{ store jobstore.Store }

func (a schedulerCoordinatorAdapter) View() scheduler.CoordinationView {
	view := a.store.View()
	return scheduler.CoordinationView{Revision: uint64(view.Revision), Occurrences: view.Occurrences, Jobs: view.Jobs}
}

func (a schedulerCoordinatorAdapter) RecordOccurrence(revision uint64, occurrence job.Occurrence) (job.Occurrence, uint64, error) {
	got, next, err := a.store.RecordOccurrence(jobstore.Revision(revision), occurrence)
	return got, uint64(next), err
}

func (a schedulerCoordinatorAdapter) Admit(revision uint64, admission scheduler.Admission) (job.Occurrence, uint64, error) {
	got, next, err := a.store.Admit(jobstore.Revision(revision), jobstore.Admission{
		Occurrence: admission.Occurrence, Window: admission.Window, Ceiling: admission.Ceiling, Reserved: admission.Reserved,
	})
	return got, uint64(next), err
}

func (a schedulerCoordinatorAdapter) Cancel(revision uint64, cancellation scheduler.Cancellation) (uint64, error) {
	next, err := a.store.Cancel(jobstore.Revision(revision), jobstore.Cancellation{
		JobID: cancellation.JobID, Actor: cancellation.Actor, Reason: cancellation.Reason,
	})
	return uint64(next), err
}

type coordinatorSubmissionAdapter struct{ store jobstore.Store }

func (a coordinatorSubmissionAdapter) BindSubmission(wanted app.SubmissionBinding) (app.SubmissionBinding, error) {
	for {
		view := a.store.View()
		bound, _, err := a.store.BindSubmission(view.Revision, jobstore.Submission{Key: wanted.Key, RequestDigest: wanted.RequestDigest, JobID: wanted.JobID})
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		if errors.Is(err, jobstore.ErrConflict) {
			return app.SubmissionBinding{}, app.ErrSubmissionConflict
		}
		if err != nil {
			return app.SubmissionBinding{}, err
		}
		return app.SubmissionBinding{Key: bound.Key, RequestDigest: bound.RequestDigest, JobID: bound.JobID}, nil
	}
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

func (d *dryRunner) StartSlot(rec trigger.Record, _ scheduler.Slot, a trigger.Action) (scheduler.Execution, error) {
	return d.Start(rec, a)
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

// cmdTriggerRun is `arxi trigger run`.
func cmdTriggerRun(args []string) {
	c := surface.Lookup("trigger", "run")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi trigger run: %v\n", err)
		os.Exit(2)
	}

	interval, err := time.ParseDuration(vals["interval"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi trigger run: --interval %q is not a "+
			"duration: %v\n  examples: 30s, 1m, 15m\n", vals["interval"], err)
		os.Exit(2)
	}
	if interval <= 0 {
		// Refused rather than clamped. A zero interval is a spin loop that
		// reads the trigger directory as fast as the disk allows, and the user
		// who typed it meant something else. Clamping to a default would hide
		// the typo behind behaviour that looks correct.
		fmt.Fprintf(os.Stderr, "arxi trigger run: --interval must be positive, "+
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
		fmt.Fprintln(os.Stderr, "arxi trigger run: --dry-run loops forever "+
			"printing the same report, because nothing it reports ever runs.\n"+
			"  use: arxi trigger run --dry-run --once")
		os.Exit(2)
	}

	var store scheduler.Store = openStore()

	var runner scheduler.Runner
	var resident *selfRunner
	var schedulerCoordinator scheduler.Coordinator
	if dry {
		// Both sinks are faked, not just the runner. See dryStore.
		runner = &dryRunner{}
		store = dryStore{store}
	} else {
		self, err := os.Executable()
		if err != nil {
			// Exit 1, not 2: nothing the user typed is wrong. The environment
			// cannot tell us what we are, and every firing needs it.
			fmt.Fprintf(os.Stderr, "arxi trigger run: cannot find my own "+
				"binary, which is what runs each trigger's --then: %v\n", err)
			os.Exit(1)
		}
		coordination, err := jobstore.Open(filepath.Join(filepath.Dir(triggerDir), ".arxi", "coordination"), nowFunc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi trigger run: open durable coordination: %v\n", err)
			os.Exit(1)
		}
		defer coordination.Close()
		resident = newSelfRunner(self)
		resident.coordinator = coordination
		runner = resident
		schedulerCoordinator = schedulerCoordinatorAdapter{store: coordination}
	}

	var sched *scheduler.Scheduler
	if dry {
		sched, err = scheduler.New(store, runner, printReport)
	} else {
		sched, err = scheduler.NewDurable(store, runner, schedulerCoordinator, printReport)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi trigger run: %v\n", err)
		os.Exit(1)
	}

	if once {
		if err := sched.Tick(nowFunc()); err != nil {
			fmt.Fprintf(os.Stderr, "arxi trigger run: %v\n", err)
			os.Exit(1)
		}
		if resident != nil {
			resident.resident.Wait()
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
