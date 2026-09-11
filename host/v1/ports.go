package v1

import (
	"context"
	"encoding/json"
	"errors"
)

// TextRequest is the deliberately text-only Phase 1 provider request. Structured
// transcripts, native tool calls, streaming, and provider wire types are absent.
type TextRequest struct {
	Model       string   `json:"model,omitempty"`
	System      string   `json:"system,omitempty"`
	Prompt      string   `json:"prompt"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// TextResponse is one completed Phase 1 text response. Provider refusals,
// accounting, structured content, and native tool calls are outside this contract.
type TextResponse struct {
	Text string `json:"text"`
}

// TextProvider completes text requests. A non-nil error means no trustworthy
// completion can be asserted; callers must not fabricate a durable response.
type TextProvider interface {
	CompleteText(context.Context, TextRequest) (TextResponse, error)
}

// ToolInvocation is one opaque invocation. Arguments are copied JSON so the host
// does not publish reducer values or provider-native calls.
type ToolInvocation struct {
	JobID     JobID           `json:"job_id"`
	Actor     string          `json:"actor"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Workspace Workspace       `json:"workspace,omitempty"`
}

// ToolResult is a known tool/process outcome, including an ordinary non-zero
// exit. A Go error instead means the execution outcome is not trustworthy.
type ToolResult struct {
	Content  json.RawMessage `json:"content,omitempty"`
	ExitCode int             `json:"exit_code,omitempty"`
	Failed   bool            `json:"failed,omitempty"`
}

// ToolExecutor performs an invocation that the host has already authorized.
type ToolExecutor interface {
	Execute(context.Context, ToolInvocation) (ToolResult, error)
}

// Workspace is an opaque provisioner-defined handle, never a filesystem path.
type Workspace string

// WorkspaceRequest identifies the job and actor needing isolation.
type WorkspaceRequest struct {
	JobID JobID  `json:"job_id"`
	Actor string `json:"actor"`
}

// WorkspaceProvisioner owns allocation and explicit release of opaque handles.
type WorkspaceProvisioner interface {
	Provision(context.Context, WorkspaceRequest) (Workspace, error)
	Release(context.Context, Workspace) error
}

// Revision is an opaque optimistic-concurrency token.
type Revision string

// Artifact is immutable job input or output. Data must be copied by both sides;
// Digest is the content digest verified when the artifact is published or read.
type Artifact struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Digest    string `json:"digest"`
	Data      []byte `json:"data"`
}

// JobRecord is storage-owned opaque job metadata.
type JobRecord struct {
	ID       JobID           `json:"id"`
	Revision Revision        `json:"revision,omitempty"`
	Data     json.RawMessage `json:"data"`
}

// StoredRecord is one opaque durable record. Sequence is logical and assigned by
// storage; Data does not freeze the public Event DTO as a persistence format.
type StoredRecord struct {
	Sequence int64           `json:"sequence"`
	Data     json.RawMessage `json:"data"`
}

// Continuation is an opaque confirmed-read resume token. It reveals no storage
// position and is meaningful only to the JobStorage that produced it.
type Continuation string

// CreateJob atomically publishes immutable artifacts, initial metadata, and
// initial records before transferring writer ownership.
type CreateJob struct {
	Record    JobRecord      `json:"record"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	Records   []StoredRecord `json:"records,omitempty"`
}

// CreateResult confirms publication and transfers exclusive writer ownership to
// the caller in the same atomic operation.
type CreateResult struct {
	Record JobRecord `json:"record"`
	Writer JobWriter `json:"-"`
}

// AppendBatch atomically appends all records and updates metadata if Expected
// still matches. No partial batch may become visible to ReadConfirmed.
type AppendBatch struct {
	Expected Revision        `json:"expected_revision"`
	Records  []StoredRecord  `json:"records"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// AppendResult reports assigned logical sequences and the new revision.
type AppendResult struct {
	Revision Revision       `json:"revision"`
	Records  []StoredRecord `json:"records"`
}

// ConfirmedRead identifies an exclusive logical starting point and an optional
// record limit. It contains no byte position or filesystem detail.
type ConfirmedRead struct {
	AfterSequence int64        `json:"after_sequence,omitempty"`
	Continuation  Continuation `json:"continuation,omitempty"`
	Limit         int          `json:"limit,omitempty"`
}

// ConfirmedBatch is a confirmed prefix and its continuation/end state.
type ConfirmedBatch struct {
	Records       []StoredRecord `json:"records"`
	AfterSequence int64          `json:"after_sequence"`
	Continuation  Continuation   `json:"continuation,omitempty"`
	Revision      Revision       `json:"revision"`
	End           bool           `json:"end"`
}

// Snapshot is a non-authoritative projection cache. Data may be discarded at
// any time; implementations and callers must rebuild truth from confirmed
// records when it is absent, stale, or invalid.
type Snapshot struct {
	AtSequence int64           `json:"at_sequence"`
	Data       json.RawMessage `json:"data"`
}

// JobWriter is the exclusive writer for one job.
type JobWriter interface {
	Append(context.Context, AppendBatch) (AppendResult, error)
	WriteSnapshot(context.Context, Snapshot) error
	Close() error
}

// ErrJobNotFound lets storage implementations report an absent job without
// exposing implementation-specific error types.
var ErrJobNotFound = errors.New("job not found")

// ErrStorageConflict reports failed exclusive ownership or optimistic revision.
var ErrStorageConflict = errors.New("storage conflict")

// JobStorage supplies Phase 1 lifecycle and confirmed-read operations.
type JobStorage interface {
	Create(context.Context, CreateJob) (CreateResult, error)
	List(context.Context) ([]JobRecord, error)
	Load(context.Context, JobID) (JobRecord, error)
	ReadConfirmed(context.Context, JobID, ConfirmedRead) (ConfirmedBatch, error)
	OpenWriter(context.Context, JobID) (JobWriter, error)
}

// CoordinationRevision is an opaque cross-job concurrency token.
type CoordinationRevision string

// SubmissionBinding binds a caller key to one canonical public request.
type SubmissionBinding struct {
	Key           string `json:"key"`
	RequestDigest string `json:"request_digest"`
	JobID         JobID  `json:"job_id"`
}

// ExecutionClaim is renewable authority to execute one job attempt. Its fields
// are exchanged only with Coordination and never appear in lifecycle projections.
type ExecutionClaim struct {
	JobID     JobID  `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	Fence     uint64 `json:"fence"`
}

// ExecutionCheckpoint records confirmed continuation evidence, not reducer state.
type ExecutionCheckpoint struct {
	Claim           ExecutionClaim `json:"claim"`
	RunRevision     int64          `json:"run_revision"`
	CompletedCursor int64          `json:"completed_cursor"`
}

// ExecutionOutcome is the fenced terminal classification of one attempt.
type ExecutionOutcome string

const (
	ExecutionSucceeded ExecutionOutcome = "succeeded"
	ExecutionFailed    ExecutionOutcome = "failed"
	ExecutionCancelled ExecutionOutcome = "cancelled"
	ExecutionUnknown   ExecutionOutcome = "unknown"
)

// Coordination installs restart recovery without changing JobStorage. The
// implementation owns lease duration, clock judgments, and optimistic retries.
type Coordination interface {
	RegisterJob(context.Context, JobID) error
	BindSubmission(context.Context, SubmissionBinding) (SubmissionBinding, error)
	Claim(context.Context, JobID) (ExecutionClaim, error)
	Validate(context.Context, ExecutionClaim) error
	Heartbeat(context.Context, ExecutionClaim) error
	Checkpoint(context.Context, ExecutionCheckpoint) error
	Complete(context.Context, ExecutionClaim, ExecutionOutcome) error
	Close() error
}

// CoordinatedJobStorageV1 is the optional storage contract required for durable
// execution. A returned writer must reject every mutation after claim expiry or
// replacement; checking only when the writer opens leaves stale hosts able to append.
type CoordinatedJobStorageV1 interface {
	JobStorage
	OpenClaimedWriter(context.Context, ExecutionClaim) (JobWriter, error)
}

// AuthorizationRequest asks whether a principal may use one capability. JobID
// is present only for resource-scoped revalidation.
type AuthorizationRequest struct {
	Principal  Principal  `json:"principal"`
	Capability Capability `json:"capability"`
	JobID      JobID      `json:"job_id,omitempty"`
}

// AuthorizationDecision separates denial from optional capability concealment.
type AuthorizationDecision struct {
	Allowed bool `json:"allowed"`
	Conceal bool `json:"conceal,omitempty"`
}

// Authorizer evaluates capability and resource access outside handlers.
type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) (AuthorizationDecision, error)
}
