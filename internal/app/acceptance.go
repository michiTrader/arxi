package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/runread"
	"github.com/michiTrader/arxi/internal/supervisor"
	"github.com/michiTrader/arxi/internal/workspace"
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
	JobID          string
	Actor          string
	Blueprint      []byte
	Prompt         string
	BudgetUSD      float64
	MaxTurns       int
	Simulated      bool
	IdempotencyKey string
	RequestDigest  job.Digest
}

// SubmissionBinding is the application-level coordination value. Keeping the
// port here lets acceptance depend on durable semantics rather than an adapter.
type SubmissionBinding struct {
	Key           string
	RequestDigest job.Digest
	JobID         job.JobID
}

// SubmissionCoordinator durably binds one caller key to one canonical request
// and deterministic job identity before per-run publication begins.
type SubmissionCoordinator interface {
	BindSubmission(SubmissionBinding) (SubmissionBinding, error)
}

var ErrSubmissionConflict = errors.New("submission identity conflicts with existing binding")

// SubmitResult identifies the confirmed run.started acceptance record.
type SubmitResult struct {
	JobID       string
	AcceptedSeq int64
	Status      string
}

// PreparedSubmission is the private shared acceptance contract after a caller
// has composed the exact immutable runtime artifact it needs.
type PreparedSubmission struct {
	JobID          string
	Actor          string
	Blueprint      []byte
	Artifact       runconfig.Artifact
	BudgetUSD      float64
	MaxTurns       int
	Location       string
	IdempotencyKey string
	RequestDigest  job.Digest
	// Prepare completes and verifies external prerequisites while the run is still
	// unpublished. It must be durably restartable because a crash may leave its
	// side effects behind without a run.started record.
	Prepare        func(context.Context, string) error
	AbortPrepare   func(context.Context, string) error
	OnAccepted     func(SubmitResult, string, kernel.Config)
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
	Submissions  SubmissionCoordinator
	NewID        func() string
	Now          func() time.Time
	DefaultModel string
	Routes       []runconfig.Route
	Platform     string
	Source       workspace.SourceIdentity
	Capabilities *workspace.Capabilities
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
	artifact := runconfig.New(id, mode, bp.SHA, req.Prompt, s.DefaultModel, bp.Config, s.Routes, nil)
	legacyDigest, err := submissionDigest(actor, bp.Raw, artifact, req.BudgetUSD, req.MaxTurns)
	if err != nil {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: err}
	}
	artifact, err = s.freezeWorkspace(artifact)
	if err != nil {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: err}
	}
	artifactForDigest := artifact
	artifactForDigest.RunID = ""
	digest, err := submissionDigest(actor, bp.Raw, artifactForDigest, req.BudgetUSD, req.MaxTurns)
	if err != nil {
		return SubmitResult{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: err}
	}
	if req.RequestDigest != "" && req.RequestDigest != digest {
		return SubmitResult{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: errors.New("supplied request digest disagrees with canonical submission")}
	}
	if req.IdempotencyKey != "" {
		bindingDigest := digest
		// A durable key created before workspace contracts landed keeps naming its
		// original request. Accepting that exact legacy digest prevents an upgrade
		// from inventing a second job while the new artifact still freezes the
		// stronger contract before publication.
		if req.RequestDigest == "" {
			bindingDigest = legacyDigest
		}
		bound, err := s.bindSubmission(SubmissionBinding{Key: req.IdempotencyKey, RequestDigest: bindingDigest, JobID: job.JobID(id)})
		if err != nil {
			return SubmitResult{}, err
		}
		id = string(bound.JobID)
		artifact.RunID = id
	}
	prepared := PreparedSubmission{
		JobID: id, Actor: actor, Blueprint: bp.Raw,
		Artifact:      artifact,
		BudgetUSD:     req.BudgetUSD,
		MaxTurns:      req.MaxTurns,
		RequestDigest: "",
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
	if req.Artifact.WorkspaceContract == nil {
		var freezeErr error
		req.Artifact, freezeErr = s.freezeWorkspace(req.Artifact)
		if freezeErr != nil {
			return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: freezeErr}
		}
	}
	blueprintSum := sha256.Sum256(req.Blueprint)
	if got := hex.EncodeToString(blueprintSum[:]); got != req.Artifact.BlueprintSHA {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id,
			Cause: fmt.Errorf("prepared blueprint digest %s disagrees with artifact %s", got, req.Artifact.BlueprintSHA)}
	}
	digest, digestErr := submissionDigest(req.Actor, req.Blueprint, req.Artifact, req.BudgetUSD, req.MaxTurns)
	if digestErr != nil {
		return Submission{}, &Error{Kind: InvalidArgument, Op: "submit", JobID: id, Cause: digestErr}
	}
	if req.RequestDigest != "" && req.RequestDigest != digest {
		return Submission{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: errors.New("supplied request digest disagrees with canonical prepared submission")}
	}
	if req.IdempotencyKey != "" {
		bound, bindErr := s.bindSubmission(SubmissionBinding{Key: req.IdempotencyKey, RequestDigest: digest, JobID: job.JobID(id)})
		if bindErr != nil {
			return Submission{}, bindErr
		}
		if bound.JobID != job.JobID(id) {
			return Submission{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: errors.New("prepared submission job disagrees with its durable binding")}
		}
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
		if os.IsExist(err) {
			return s.adoptPublished(id, dir, req)
		}
		return result, &Error{Kind: StorageUnavailable, Op: "submit", JobID: id, Cause: err}
	}
		accepted := false
		defer func() {
			if !accepted {
				if req.AbortPrepare != nil {
					_ = req.AbortPrepare(context.Background(), dir)
				}
				_ = os.RemoveAll(dir)
			}
		}()
		if req.Prepare != nil {
			if err := req.Prepare(ctx, dir); err != nil {
				return result, &Error{Kind: StorageUnavailable, Op: "prepare", JobID: id, Cause: err}
			}
		}
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

func (s AcceptanceServices) adoptPublished(id, dir string, req PreparedSubmission) (SubmitResult, error) {
	run, err := runread.Open(dir)
	if err != nil {
		return SubmitResult{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: fmt.Errorf("bound job is not a confirmed published run: %w", err)}
	}
	artifact, err := runconfig.VerifyBinding(dir, id, run.Events)
	if err != nil {
		return SubmitResult{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: err}
	}
	if !reflect.DeepEqual(artifact, req.Artifact) {
		return SubmitResult{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: errors.New("bound job has different immutable effective configuration")}
	}
	snapshot, err := os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml"))
	if err != nil || !reflect.DeepEqual(snapshot, req.Blueprint) {
		if err == nil {
			err = errors.New("bound job has different immutable blueprint bytes")
		}
		return SubmitResult{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: err}
	}
	if len(run.Events) == 0 || run.Events[0].Type != kernel.RunStarted || run.Events[0].Seq != 1 ||
		run.Events[0].Str("run_id") != id || run.Events[0].Str("actor") != req.Actor ||
		run.Events[0].Str("prompt") != req.Artifact.Prompt || run.Events[0].Num("budget_usd") != req.BudgetUSD ||
		int(run.Events[0].Num("max_turns")) != req.MaxTurns {
		return SubmitResult{}, &Error{Kind: Conflict, Op: "submit", JobID: id, Cause: errors.New("bound job run.started identity disagrees with the canonical submission")}
	}
	return SubmitResult{JobID: id, AcceptedSeq: 1, Status: string(run.State.Status)}, nil
}

func (s AcceptanceServices) bindSubmission(wanted SubmissionBinding) (SubmissionBinding, error) {
	if s.Submissions == nil {
		return SubmissionBinding{}, &Error{Kind: StorageUnavailable, Op: "submit", JobID: string(wanted.JobID), Cause: errors.New("idempotent submission requires a coordination store")}
	}
	bound, err := s.Submissions.BindSubmission(wanted)
	if err != nil {
		kind := StorageUnavailable
		if errors.Is(err, ErrSubmissionConflict) {
			kind = Conflict
		}
		return SubmissionBinding{}, &Error{Kind: kind, Op: "submit", JobID: string(wanted.JobID), Cause: err}
	}
	if bound.Key != wanted.Key || bound.RequestDigest != wanted.RequestDigest || bound.JobID == "" {
		return SubmissionBinding{}, &Error{Kind: Conflict, Op: "submit", JobID: string(wanted.JobID), Cause: errors.New("coordination store returned a conflicting submission binding")}
	}
	return bound, nil
}

func submissionDigest(actor string, blueprintBytes []byte, artifact runconfig.Artifact, budgetUSD float64, maxTurns int) (job.Digest, error) {
	artifact.RunID = ""
	canonical := struct {
		Actor     string             `json:"actor"`
		Blueprint []byte             `json:"blueprint"`
		Artifact  runconfig.Artifact `json:"artifact"`
		BudgetUSD float64            `json:"budget_usd"`
		MaxTurns  int                `json:"max_turns"`
	}{actor, blueprintBytes, artifact, budgetUSD, maxTurns}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("canonicalize submission: %w", err)
	}
	return job.RequestDigest(body), nil
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

func (s AcceptanceServices) freezeWorkspace(artifact runconfig.Artifact) (runconfig.Artifact, error) {
	topLevel := workspace.Mode(artifact.Config.Workspace)
	// ResolveDefaults historically materialized "none" for an omitted declaration.
	// A member that needs files still carries enough evidence to recognize that old
	// default; treating it as explicit would turn every pre-Phase-4 writer into an
	// impossible none request rather than applying ADR-0012's writer default.
	if topLevel == workspace.ModeNone {
		for _, member := range artifact.Config.Members {
			for _, tool := range member.Tools {
				if tool == "read" || tool == "grep" || tool == "write" || tool == "edit" || tool == "bash" {
					topLevel = ""
				}
			}
		}
	}
	members := make([]workspace.Member, len(artifact.Config.Members))
	for i, member := range artifact.Config.Members {
		members[i] = workspace.Member{Name: member.Name, Tools: append([]string(nil), member.Tools...), Stages: append([]string(nil), member.Stages...)}
	}
	stages := make([]workspace.Stage, len(artifact.Config.Stages))
	for i, stage := range artifact.Config.Stages {
		stages[i] = workspace.Stage{Name: stage.Name, Mode: workspace.Mode(stage.Workspace)}
	}
	requirements, err := workspace.Resolve(workspace.ResolutionInput{TopLevel: topLevel, Members: members, Stages: stages})
	if err != nil {
		return artifact, fmt.Errorf("resolve workspace requirements: %w", err)
	}
	platform := s.Platform
	if platform == "" {
		platform = "unknown"
	}
	capabilities := workspace.CurrentCapabilities(platform)
	source := s.Source
	needsSource := false
	for _, requirement := range requirements {
		needsSource = needsSource || requirement.RequiresSource
	}
	if s.Capabilities != nil {
		capabilities = *s.Capabilities
	} else if needsSource && source.Schema == "" && platform != "simulation" {
		modes := make([]string, 0, len(requirements))
		for _, requirement := range requirements {
			modes = append(modes, string(requirement.Mode))
		}
		return artifact, fmt.Errorf("workspace modes %s require source identity and probed capabilities before acceptance", strings.Join(modes, ", "))
	}
	decisions, err := workspace.Preflight(requirements, capabilities)
	if err != nil {
		return artifact, fmt.Errorf("workspace preflight: %w", err)
	}
	if source.Schema == "" {
		source = workspace.SourceIdentity{Schema: workspace.SchemaV1, Kind: "none",
			DirtyPolicy: "excluded", UntrackedPolicy: "excluded", IgnoredPolicy: "excluded",
			SubmodulePolicy: "refused", SymlinkPolicy: "internal-relative-only", SpecialFilePolicy: "refused"}
	}
	artifact.WorkspaceContract = &runconfig.WorkspaceContract{Schema: workspace.SchemaV1, Source: source,
		Requirements: requirements, Decisions: decisions}
	if len(decisions) > 0 {
		profile := decisions[0].ProfileIdentity
		for _, decision := range decisions[1:] {
			if decision.ProfileIdentity != profile {
				profile = workspace.MixedProfileIdentity(decisions)
				break
			}
		}
		artifact.WorkspaceProfileID = profile
	}
	return artifact, nil
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
