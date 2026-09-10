package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/supervisor"
)

// Lifecycle is the narrow handoff between durable acceptance and a process
// supervisor. Launch must return after taking responsibility for the accepted job.
type Lifecycle interface {
	Launch(context.Context, string) error
}

// locationLifecycle is deliberately private: callers may choose an exact
// location without adding filesystem concerns to the public host API.
type locationLifecycle interface {
	LaunchAt(context.Context, string, string) error
}

// SubmitRequest is transport-independent input to durable job acceptance.
type SubmitRequest struct {
	JobID     string
	Actor     string
	Blueprint []byte
	Prompt    string
	BudgetUSD float64
	MaxTurns  int
	Simulated bool
}

// SubmitResult identifies the confirmed run.started acceptance record.
type SubmitResult struct {
	JobID       string
	AcceptedSeq int64
	Status      string
}

// PreparedSubmission is the private shared acceptance contract after a caller
// has composed the exact immutable runtime artifact it needs.
type PreparedSubmission struct {
	JobID      string
	Actor      string
	Blueprint  []byte
	Artifact   runconfig.Artifact
	BudgetUSD  float64
	MaxTurns   int
	Location   string
	OnAccepted func(SubmitResult, string, kernel.Config)
}

// Submission is an accepted run and its resident lifecycle endpoint.
type Submission struct {
	Result SubmitResult
	Dir    string
	Handle *supervisor.Handle
}

// WaitPolicy controls which private execution boundary satisfies a wait.
type WaitPolicy uint8

const (
	WaitTerminal WaitPolicy = iota
	WaitStandstill
)

// WaitResult retains the exact supervisor outcome used by CLI rendering.
type WaitResult struct {
	Outcome exec.Outcome
	Err     error
}

// AcceptanceServices publishes immutable run input before handing an accepted
// job to its lifecycle owner.
type AcceptanceServices struct {
	RunsDir      string
	Lifecycle    Lifecycle
	NewID        func() string
	Now          func() time.Time
	DefaultModel string
	Routes       []runconfig.Route
}

// Submit validates and durably accepts one job. Before run.started is confirmed,
// every failure removes the unpublished run directory. After confirmation, the
// job is retained even when lifecycle handoff fails.
func (s AcceptanceServices) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return SubmitResult{}, err
	}
	if s.Lifecycle == nil {
		return SubmitResult{}, &Error{Kind: StorageUnavailable, Op: "submit", Cause: errors.New("no lifecycle owner is installed")}
	}
	if strings.TrimSpace(s.RunsDir) == "" {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", Cause: errors.New("no run root configured")}
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", Cause: errors.New("prompt is required")}
	}
	if req.BudgetUSD <= 0 {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", Cause: errors.New("budget must be greater than zero")}
	}
	if req.MaxTurns < 0 {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", Cause: errors.New("max turns must not be negative")}
	}
	bp, err := blueprint.Load(req.Blueprint)
	if err != nil {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", Cause: fmt.Errorf("invalid blueprint: %w", err)}
	}
	id := req.JobID
	if id == "" {
		id = s.newID()
	}
	if !validJobID(id) {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: errors.New("invalid job id")}
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = bp.Name
	}
	mode := "live"
	if req.Simulated {
		mode = "sim"
	}
	prepared := PreparedSubmission{
		JobID: id, Actor: actor, Blueprint: bp.Raw,
		Artifact:  runconfig.New(id, mode, bp.SHA, req.Prompt, s.DefaultModel, bp.Config, s.Routes, nil),
		BudgetUSD: req.BudgetUSD, MaxTurns: req.MaxTurns,
	}
	submission, err := s.SubmitPrepared(ctx, prepared)
	return submission.Result, err
}

// SubmitPrepared durably publishes an already composed artifact and then hands
// it to the lifecycle owner. The accepted callback runs after run.started is
// confirmed and before launch, which is the CLI's stable announcement boundary.
func (s AcceptanceServices) SubmitPrepared(ctx context.Context, req PreparedSubmission) (Submission, error) {
	if err := ctx.Err(); err != nil {
		return Submission{}, err
	}
	if s.Lifecycle == nil {
		return Submission{}, &Error{Kind: StorageUnavailable, Op: "submit", Cause: errors.New("no lifecycle owner is installed")}
	}
	id := req.JobID
	if id == "" {
		id = s.newID()
	}
	if !validJobID(id) {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: errors.New("invalid job id")}
	}
	if req.BudgetUSD <= 0 || req.MaxTurns < 0 {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: errors.New("invalid run limits")}
	}
	if req.Artifact.RunID != id || strings.TrimSpace(req.Artifact.Prompt) == "" {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: errors.New("prepared artifact does not identify this run and prompt")}
	}
	if len(req.Blueprint) == 0 || req.Artifact.BlueprintSHA == "" {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: errors.New("prepared blueprint is required")}
	}
	blueprintSum := sha256.Sum256(req.Blueprint)
	if got := hex.EncodeToString(blueprintSum[:]); got != req.Artifact.BlueprintSHA {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id,
			Cause: fmt.Errorf("prepared blueprint digest %s disagrees with artifact %s", got, req.Artifact.BlueprintSHA)}
	}
	dir := req.Location
	if dir == "" {
		if strings.TrimSpace(s.RunsDir) == "" {
			return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: errors.New("no run root configured")}
		}
		dir = filepath.Join(s.RunsDir, id)
	}
	result, err := s.publishPrepared(id, dir, req)
	submission := Submission{Result: result, Dir: dir}
	if err != nil {
		return submission, err
	}
	if req.OnAccepted != nil {
		req.OnAccepted(result, dir, req.Artifact.Config)
	}
	var handle *supervisor.Handle
	if located, ok := s.Lifecycle.(*supervisor.Supervisor); ok {
		err = located.LaunchAt(ctx, id, dir)
		if err == nil {
			handle, err = located.OpenAt(ctx, id, dir)
		}
	} else if located, ok := s.Lifecycle.(locationLifecycle); ok {
		err = located.LaunchAt(ctx, id, dir)
	} else {
		err = s.Lifecycle.Launch(ctx, id)
	}
	if err != nil {
		return submission, &Error{Kind: StorageUnavailable, Op: "launch", JobID: id, Cause: err}
	}
	submission.Handle = handle
	return submission, nil
}

func (s AcceptanceServices) publishPrepared(id, dir string, req PreparedSubmission) (result SubmitResult, err error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		kind := StorageUnavailable
		if os.IsExist(err) {
			kind = Conflict
		}
		return result, &Error{Kind: kind, Op: "submit", JobID: id, Cause: err}
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := writeSyncedFile(filepath.Join(dir, "blueprint.snapshot.yaml"), req.Blueprint, 0o644); err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	if err := syncDirectory(dir); err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	digest, err := runconfig.Publish(dir, req.Artifact)
	if err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	loaded, loadedDigest, err := runconfig.Load(dir)
	if err != nil || loadedDigest != digest {
		if err == nil {
			err = errors.New("effective config changed while it was being published")
		}
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	store, err := logstore.Open(dir)
	if err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	storeClosed := false
	defer func() {
		if !storeClosed {
			_ = store.Close()
		}
	}()
	event := kernel.Event{ID: "ev-start", Ts: s.now().UTC().Format(time.RFC3339Nano), Type: kernel.RunStarted,
		Scope: "run:" + id, Source: kernel.SourceHuman, Payload: map[string]any{
			"run_id": id, "actor": req.Actor, "blueprint_sha": loaded.BlueprintSHA,
			"effective_config_schema": loaded.Schema, "effective_config_path": runconfig.FileName,
			"effective_config_sha": digest, "budget_usd": req.BudgetUSD,
			"max_turns": float64(req.MaxTurns), "prompt": loaded.Prompt,
			"workspace": loaded.Config.Workspace, "simulated": loaded.Mode == "sim",
		}}
	written, err := store.Append([]kernel.Event{event})
	if err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	if err := store.Close(); err != nil {
		storeClosed = true
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	storeClosed = true
	if err := syncDirectory(dir); err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	if err := syncDirectory(parent); err != nil {
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
	accepted = true
	return SubmitResult{JobID: id, AcceptedSeq: written[0].Seq, Status: string(kernel.StatusRunning)}, nil
}

func writeSyncedFile(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	closed := false
	ok := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	ok = true
	return nil
}

func syncDirectory(dir string) error {
	opened, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.Sync()
}

// Wait observes retained supervisor generations according to a private policy.
func Wait(ctx context.Context, submission Submission, policy WaitPolicy) (WaitResult, error) {
	if submission.Handle == nil {
		return WaitResult{}, errors.New("submission has no resident lifecycle handle")
	}
	var generation uint64
	for {
		next, result, err := submission.Handle.WaitCompletion(ctx, generation)
		if err != nil {
			return WaitResult{}, err
		}
		generation = next
		terminal := result.Outcome.State.Status.Terminal() || result.Outcome.StoppedBy == exec.StopTerminal
		if result.Err != nil || terminal || (policy == WaitStandstill && result.Outcome.StoppedBy == exec.StopIdle) {
			return WaitResult{Outcome: result.Outcome, Err: result.Err}, nil
		}
	}
}

func (s AcceptanceServices) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	ms := s.now().UTC().UnixMilli()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "r" + strconv.FormatInt(ms, 36) + "-" + strconv.FormatInt(s.now().UnixNano(), 36)
	}
	return "r" + strconv.FormatInt(ms, 36) + "-" + hex.EncodeToString(suffix[:])
}

func (s AcceptanceServices) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func validJobID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id
}
