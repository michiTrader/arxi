// Package supervisor owns resident per-run execution workers.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/workspace"
	"github.com/michiTrader/arxi/internal/workspacefs"
)

const DefaultCommandLimit = 32

var (
	ErrClosed    = errors.New("supervisor is closed")
	ErrQueueFull = errors.New("supervisor command queue is full")
	ErrUnknown   = errors.New("run contains external work with an unknown outcome")
	ErrLegacy    = errors.New("run has no durable execution progress")
)

// Command runs one serialized mutation while the resident worker owns the Store.
type Command func(*logstore.Store) error

// Result describes one completed drive pass.
type Result struct {
	Outcome exec.Outcome
	Err     error
}

// Build supplies the run-specific executor. Configuration is always loaded from
// the immutable effective-config artifact before Build is called.
type Build func(dir string, effective runconfig.Artifact) (exec.Executor, error)

// Claim keeps a resident worker behind one renewable fenced attempt. The
// coordinator, not the run log writer, decides whether the worker remains active.
type Claim interface {
	Checkpoint(cursor, revision int64) error
	Heartbeat() error
	Finish(exec.Outcome, error) error
}

// DispatchClaim extends a claim with fenced external dispatch coordination.
type DispatchClaim interface {
	Claim
	exec.DispatchCoordinator
}

// Reconciler may establish a canonical terminal outcome for ambiguous external
// work. It is intentionally absent from built-in providers until their APIs
// offer trustworthy receipt lookup rather than only returning response IDs.
type Reconciler interface {
	Reconcile(context.Context, string) (bool, error)
}

type workspaceLifecycle interface {
	CloseWorkspaces() error
	ReleaseWorkspaces(context.Context) error
}

// Options are process-level dependencies; run state is never supplied here.
type Options struct {
	Build        Build
	Now          func() time.Time
	CommandLimit int
	Claim        Claim
	Heartbeat    time.Duration
	Reconciler   Reconciler
}

// Supervisor guarantees at most one resident worker for each run id.
type Supervisor struct {
	root    string
	opts    Options
	mu      sync.Mutex
	workers map[string]*worker
	closed  bool
	closing bool
}

func New(root string, opts Options) *Supervisor {
	if opts.CommandLimit <= 0 {
		opts.CommandLimit = DefaultCommandLimit
	}
	return &Supervisor{root: root, opts: opts, workers: map[string]*worker{}}
}

// ConfigureClaim installs coordination before the first worker opens. Refusing
// a late change prevents an existing log writer from silently switching fences.
func (s *Supervisor) ConfigureClaim(claim Claim, heartbeat time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.workers) != 0 {
		return errors.New("supervisor claim must be configured before opening a worker")
	}
	if claim == nil || heartbeat <= 0 {
		return errors.New("supervisor claim and positive heartbeat cadence are required")
	}
	s.opts.Claim, s.opts.Heartbeat = claim, heartbeat
	return nil
}

// Open starts (or returns) the one worker for id under the configured root and
// waits for restoration.
func (s *Supervisor) Open(ctx context.Context, id string) (*Handle, error) {
	return s.openAt(ctx, id, filepath.Join(s.root, id), false)
}

// OpenAt is Open for a privately selected run location. The location is part of
// worker identity, so two equal ids at different locations never share a writer.
func (s *Supervisor) OpenAt(ctx context.Context, id, dir string) (*Handle, error) {
	return s.openAt(ctx, id, dir, false)
}

func (s *Supervisor) openAt(ctx context.Context, id, dir string, allowFresh bool) (*Handle, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	key, err := locationKey(dir)
	if err != nil {
		return nil, err
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		w := s.workers[key]
		if w == nil {
			w = newWorker(dir, id, s.opts)
			w.allowFresh = allowFresh
			s.workers[key] = w
			go w.run()
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-w.ready:
			if w.readyErr != nil {
				s.drop(key, w)
				return nil, w.readyErr
			}
		}
		select {
		case <-w.done:
			s.drop(key, w)
			continue
		default:
			return &Handle{w: w}, nil
		}
	}
}

// Resident returns the existing root-based worker for id without opening one.
func (s *Supervisor) Resident(id string) (*Handle, bool) {
	return s.residentAt(filepath.Join(s.root, id))
}

func (s *Supervisor) residentAt(dir string) (*Handle, bool) {
	key, err := locationKey(dir)
	if err != nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workers[key]
	if w == nil {
		return nil, false
	}
	select {
	case <-w.done:
		if s.workers[key] == w {
			delete(s.workers, key)
		}
		return nil, false
	default:
		return &Handle{w: w}, true
	}
}

// Launch implements the root-based durable-acceptance lifecycle handoff.
func (s *Supervisor) Launch(ctx context.Context, id string) error {
	return s.LaunchAt(ctx, id, filepath.Join(s.root, id))
}

// LaunchAt takes responsibility for a freshly accepted run at dir.
func (s *Supervisor) LaunchAt(ctx context.Context, id, dir string) error {
	_, err := s.openAt(ctx, id, dir, true)
	return err
}

// Close stops every worker after its current external call reaches a safe boundary.
func (s *Supervisor) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed && !s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.closing = true
	workers := make([]*worker, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, w)
	}
	s.mu.Unlock()
	for _, w := range workers {
		w.close()
	}
	var closeErr error
	for _, w := range workers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
			if err := w.err(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	}
	s.mu.Lock()
	s.closing = false
	s.mu.Unlock()
	return closeErr
}

func (s *Supervisor) drop(id string, w *worker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers[id] == w {
		delete(s.workers, id)
	}
}

func locationKey(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("empty run location")
	}
	key, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve run location: %w", err)
	}
	return filepath.Clean(key), nil
}

func validID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return fmt.Errorf("invalid run id %q", id)
	}
	return nil
}

// Handle is a bounded, serialized command and wake endpoint for one run.
type Handle struct{ w *worker }

func (h *Handle) Command(ctx context.Context, fn Command) error {
	if fn == nil {
		return errors.New("nil supervisor command")
	}
	reply := make(chan error, 1)
	request := request{fn: fn, reply: reply}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.w.done:
		return ErrClosed
	case h.w.commands <- request:
		h.w.signal()
	default:
		return ErrQueueFull
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.w.done:
		return ErrClosed
	case err := <-reply:
		return err
	}
}

// Wake asks the worker to re-read decisions or other externally appended input.
func (h *Handle) Wake() { h.w.signal() }

// Completion returns the latest completed generation without consuming it. A
// caller can retain the generation and wait for a strictly later pass.
func (h *Handle) Completion() (uint64, Result, bool) {
	return h.w.completion()
}

// WaitCompletion waits for a completion generation newer than after. Completed
// generations are retained, so subscribing after a fast pass cannot miss it and
// any number of observers can independently wait on the same worker.
func (h *Handle) WaitCompletion(ctx context.Context, after uint64) (uint64, Result, error) {
	for {
		generation, result, ok, changed := h.w.completionState()
		if ok && generation > after {
			return generation, result, nil
		}
		select {
		case <-ctx.Done():
			return 0, Result{}, ctx.Err()
		case <-h.w.done:
			generation, result, ok := h.w.completion()
			if ok && generation > after {
				return generation, result, nil
			}
			return 0, Result{}, ErrClosed
		case <-changed:
		}
	}
}

// Completions is the legacy lossy observation stream. New code should use
// WaitCompletion, which is retained and supports multiple observers.
func (h *Handle) Completions() <-chan Result { return h.w.results }

// Close releases this worker's long-held writer ownership at a safe boundary.
func (h *Handle) Close(ctx context.Context) error {
	h.w.close()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.w.done:
		return h.w.err()
	}
}

type request struct {
	fn    Command
	reply chan error
}

type worker struct {
	dir, id      string
	opts         Options
	allowFresh   bool
	commands     chan request
	wake         chan struct{}
	stop         chan struct{}
	done         chan struct{}
	ready        chan struct{}
	readyErr     error
	results      chan Result
	completionMu sync.Mutex
	generation   uint64
	latest       Result
	hasLatest    bool
	changed      chan struct{}
	stopOnce     sync.Once
	errMu        sync.Mutex
	closeErr     error
	workspace    workspaceLifecycle
	released     bool
}

func newWorker(dir, id string, opts Options) *worker {
	return &worker{
		dir: dir, id: id, opts: opts,
		commands: make(chan request, opts.CommandLimit), wake: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{}),
		results: make(chan Result, 1), changed: make(chan struct{}),
	}
}

func (w *worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) close() {
	w.stopOnce.Do(func() { close(w.stop) })
	w.signal()
}

func (w *worker) run() {
	defer close(w.done)
	defer close(w.results)
	defer func() {
		if w.workspace != nil && !w.released {
			w.setErr(w.workspace.CloseWorkspaces())
		}
	}()
	store, effective, loop, err := w.restore()
	w.readyErr = err
	close(w.ready)
	if err != nil {
		return
	}
	defer func() { w.setErr(store.Close()) }()

	var heartbeatStop chan struct{}
	var heartbeatDone chan struct{}
	if w.opts.Claim != nil && w.opts.Heartbeat > 0 {
		heartbeatStop, heartbeatDone = make(chan struct{}), make(chan struct{})
		go w.heartbeat(heartbeatStop, heartbeatDone)
		defer func() { close(heartbeatStop); <-heartbeatDone }()
	}

	for {
		if w.stopping() {
			w.rejectPending()
			return
		}
		w.drain(store)
		if w.stopping() {
			w.rejectPending()
			return
		}

		boundary := make(chan struct{})
		passDone := make(chan struct{})
		var once sync.Once
		wakeDone := make(chan struct{})
		go func() {
			defer close(wakeDone)
			select {
			case <-w.stop:
				once.Do(func() { close(boundary) })
			case <-w.wake:
				once.Do(func() { close(boundary) })
			case <-passDone:
			}
		}()
		out, runErr := loop.RunUntilBoundary(context.Background(), boundary)
		close(passDone)
		once.Do(func() { close(boundary) })
		<-wakeDone
		loop.Cursor = out.Cursor
		result := Result{Outcome: out, Err: runErr}
		if w.opts.Claim != nil && (runErr != nil || out.StoppedBy == exec.StopTerminal) {
			if finishErr := w.opts.Claim.Finish(out, runErr); finishErr != nil {
				result.Err = errors.Join(result.Err, finishErr)
			}
		}
		if runErr == nil && out.StoppedBy == exec.StopTerminal && out.State.Status == kernel.StatusSucceeded && w.workspace != nil && !w.released {
			if releaseErr := w.workspace.ReleaseWorkspaces(context.Background()); releaseErr != nil {
				result.Err = errors.Join(result.Err, releaseErr)
			} else {
				w.released = true
			}
		}
		w.publish(result)

		drained := w.drain(store)
		if w.stopping() {
			w.rejectPending()
			return
		}
		if drained {
			continue
		}
		if runErr != nil || out.StoppedBy == exec.StopTerminal {
			w.wait()
			continue
		}
		if out.StoppedBy == exec.StopIdle && out.State.Status != kernel.StatusBlocked {
			w.wait()
		}
		_ = effective // retained by restored runtime for its immutable lifetime
	}
}

func (w *worker) restore() (*logstore.Store, runconfig.Artifact, *exec.Loop, error) {
	store, err := logstore.Open(w.dir)
	if err != nil {
		return nil, runconfig.Artifact{}, nil, err
	}
	var effective runconfig.Artifact
	fail := func(err error) (*logstore.Store, runconfig.Artifact, *exec.Loop, error) {
		_ = store.Close()
		return nil, effective, nil, err
	}
	events, err := store.Read(1, 0)
	if err != nil {
		return fail(fmt.Errorf("read durable execution progress: %w", err))
	}
	effective, err = runconfig.VerifyBinding(w.dir, w.id, events)
	if err != nil {
		return fail(fmt.Errorf("verify immutable execution config: %w", err))
	}
	if effective.SupportsWorkspaceContract() {
		contract := effective.WorkspaceContract
		platform, platformErr := workspaceContractPlatform(contract)
		if platformErr != nil {
			return fail(platformErr)
		}
		if contract.Source.Kind == "git" {
			if _, verifyErr := workspacefs.Verify(context.Background(), contract.Source); verifyErr != nil {
				return fail(fmt.Errorf("verify frozen workspace source: %w", verifyErr))
			}
		}
		current, verifyErr := currentWorkspaceContract(effective.Config, contract.Source, platform)
		if verifyErr != nil {
			return fail(fmt.Errorf("verify live workspace capabilities: %w", verifyErr))
		}
		if verifyErr := effective.VerifyWorkspaceContract(current); verifyErr != nil {
			return fail(fmt.Errorf("verify live workspace contract: %w", verifyErr))
		}
	}
	recovery, err := exec.Recover(events)
	if err != nil {
		return fail(fmt.Errorf("recover durable execution progress: %w", err))
	}
	if !recovery.HasProgress && !w.allowFresh {
		return fail(ErrLegacy)
	}
	if len(recovery.Unknown) > 0 {
		if w.opts.Reconciler == nil {
			return fail(fmt.Errorf("%w: %s", ErrUnknown, recovery.Unknown[0]))
		}
		for _, workID := range recovery.Unknown {
			reconciled, reconcileErr := w.opts.Reconciler.Reconcile(context.Background(), workID)
			if reconcileErr != nil {
				return fail(fmt.Errorf("reconcile unknown work %s: %w", workID, reconcileErr))
			}
			if !reconciled {
				return fail(fmt.Errorf("%w: %s", ErrUnknown, workID))
			}
		}
		events, err = store.Read(1, 0)
		if err != nil {
			return fail(fmt.Errorf("read reconciled execution progress: %w", err))
		}
		recovery, err = exec.Recover(events)
		if err != nil {
			return fail(fmt.Errorf("recover reconciled execution progress: %w", err))
		}
		if len(recovery.Unknown) > 0 {
			return fail(fmt.Errorf("%w: %s", ErrUnknown, recovery.Unknown[0]))
		}
	}
	timers, err := exec.RecoverTimers(events)
	if err != nil {
		return fail(fmt.Errorf("restore durable timers: %w", err))
	}
	executor, err := w.opts.Build(w.dir, effective)
	if err != nil {
		return fail(fmt.Errorf("build executor: %w", err))
	}
	if lifecycle, ok := executor.(workspaceLifecycle); ok {
		w.workspace = lifecycle
	}

	var clock exec.Clock
	var timekeeper exec.Timekeeper
	var now func() string
	if effective.Mode == "sim" {
		vc := exec.NewVirtualClock()
		if err := vc.Restore(timers.NowMs, timers.Pending); err != nil {
			return fail(err)
		}
		clock, timekeeper = vc, exec.VirtualTime{C: vc}
		now = func() string { return time.UnixMilli(vc.NowMs()).UTC().Format(time.RFC3339Nano) }
	} else {
		rc := exec.NewRealClock()
		if w.opts.Now != nil {
			rc.Now = w.opts.Now
		}
		if err := rc.Restore(timers.Pending); err != nil {
			return fail(err)
		}
		clock, timekeeper = rc, exec.RealTime{C: rc}
		now = func() string {
			if w.opts.Now != nil {
				return w.opts.Now().UTC().Format(time.RFC3339Nano)
			}
			return time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	runner := &exec.Runner{Log: store, Clock: clock, Executor: executor,
		Config: effective.Config, RunID: w.id, JobID: w.id, Now: now,
		Authorization: exec.AuthorizationConfig{
			ToolSchemaVersion: effective.ToolSchemaVersion, PolicyVersion: effective.PolicyVersion,
			WorkspaceProfileID: effective.WorkspaceProfileID, TTLMS: effective.AuthorizationTTLMS,
		},
	}
	if dispatches, ok := w.opts.Claim.(DispatchClaim); ok {
		runner.JobID = w.id
		runner.Dispatches = dispatches
	}
	loop := &exec.Loop{Runner: runner, Log: store, Time: timekeeper,
		Config: effective.Config, Cursor: recovery.Cursor}
	if w.opts.Claim != nil {
		loop.Progress = w.opts.Claim.Checkpoint
	}
	return store, effective, loop, nil
}

func workspaceContractPlatform(contract *runconfig.WorkspaceContract) (string, error) {
	if contract == nil {
		return "", errors.New("workspace contract is absent")
	}
	if len(contract.Decisions) == 0 {
		if len(contract.Requirements) != 0 {
			return "", errors.New("workspace contract has requirements without platform decisions")
		}
		return "unknown", nil
	}
	platform := contract.Decisions[0].Platform
	for _, decision := range contract.Decisions[1:] {
		if decision.Platform != platform {
			return "", errors.New("workspace contract mixes platform decisions")
		}
	}
	return platform, nil
}

func currentWorkspaceContract(config kernel.Config, source workspace.SourceIdentity, platform string) (runconfig.WorkspaceContract, error) {
	topLevel := workspace.Mode(config.Workspace)
	if topLevel == workspace.ModeNone {
		for _, member := range config.Members {
			for _, tool := range member.Tools {
				if tool == "read" || tool == "grep" || tool == "write" || tool == "edit" || tool == "bash" {
					topLevel = ""
				}
			}
		}
	}
	members := make([]workspace.Member, len(config.Members))
	for i, member := range config.Members {
		members[i] = workspace.Member{Name: member.Name, Tools: append([]string(nil), member.Tools...), Stages: append([]string(nil), member.Stages...)}
	}
	stages := make([]workspace.Stage, len(config.Stages))
	for i, stage := range config.Stages {
		stages[i] = workspace.Stage{Name: stage.Name, Mode: workspace.Mode(stage.Workspace)}
	}
	requirements, err := workspace.Resolve(workspace.ResolutionInput{TopLevel: topLevel, Members: members, Stages: stages})
	if err != nil {
		return runconfig.WorkspaceContract{}, err
	}
	capabilities := workspace.CurrentCapabilities(platform)
	if source.Kind == "git" {
		probe, probeErr := workspacefs.Verify(context.Background(), source)
		if probeErr != nil {
			return runconfig.WorkspaceContract{}, probeErr
		}
		capabilities = probe.Capabilities
	}
	decisions, err := workspace.Preflight(requirements, capabilities)
	if err != nil {
		return runconfig.WorkspaceContract{}, err
	}
	return runconfig.WorkspaceContract{Schema: workspace.SchemaV1, Source: source, Requirements: requirements, Decisions: decisions}, nil
}

func (w *worker) heartbeat(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(w.opts.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := w.opts.Claim.Heartbeat(); err != nil {
				w.setErr(fmt.Errorf("renew worker claim: %w", err))
				w.close()
				return
			}
		}
	}
}

func (w *worker) stopping() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

func (w *worker) drain(store *logstore.Store) bool {
	drained := false
	for {
		select {
		case req := <-w.commands:
			drained = true
			req.reply <- req.fn(store)
		default:
			return drained
		}
	}
}

func (w *worker) wait() {
	select {
	case <-w.stop:
	case <-w.wake:
	}
}

func (w *worker) rejectPending() {
	for {
		select {
		case req := <-w.commands:
			req.reply <- ErrClosed
		default:
			return
		}
	}
}

func (w *worker) setErr(err error) {
	if err == nil {
		return
	}
	w.errMu.Lock()
	if w.closeErr == nil {
		w.closeErr = fmt.Errorf("close worker store: %w", err)
	}
	w.errMu.Unlock()
}

func (w *worker) err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.closeErr
}

func (w *worker) completion() (uint64, Result, bool) {
	w.completionMu.Lock()
	defer w.completionMu.Unlock()
	return w.generation, w.latest, w.hasLatest
}

func (w *worker) completionState() (uint64, Result, bool, <-chan struct{}) {
	w.completionMu.Lock()
	defer w.completionMu.Unlock()
	return w.generation, w.latest, w.hasLatest, w.changed
}

func (w *worker) publish(result Result) {
	w.completionMu.Lock()
	w.generation++
	w.latest = result
	w.hasLatest = true
	changed := w.changed
	w.changed = make(chan struct{})
	close(changed)
	w.completionMu.Unlock()

	select {
	case w.results <- result:
	default:
		select {
		case <-w.results:
		default:
		}
		w.results <- result
	}
}
