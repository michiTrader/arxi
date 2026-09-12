package exec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"

	"github.com/michiTrader/arxi/internal/authorization"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

type WorkClass string

type OutcomeStatus string

const (
	WorkIdempotent    WorkClass     = "idempotent"
	WorkNonIdempotent WorkClass     = "non_idempotent"
	OutcomeSucceeded  OutcomeStatus = "succeeded"
	OutcomeFailed     OutcomeStatus = "failed"
	OutcomeCancelled  OutcomeStatus = "cancelled"
	OutcomeUnknown    OutcomeStatus = "unknown"
)

// DispatchMetadata is immutable external-call identity. A key is useful only
// when SupportsIdempotency is true; otherwise it remains correlation evidence.
type DispatchMetadata struct {
	JobID               string
	WorkID              string
	Provider            string
	RequestDigest       string
	DispatchKey         string
	WorkClass           WorkClass
	SupportsIdempotency bool
}

// DispatchReceipt binds a stable external identifier to the exact canonical
// outcome returned by an adapter.
type DispatchReceipt struct {
	ExternalID       string
	Status           OutcomeStatus
	CanonicalOutcome json.RawMessage
}

// DispatchCoordinator installs fenced cross-job registration and receipts.
// It is absent for legacy, uncoordinated execution.
type DispatchCoordinator interface {
	RegisterDispatch(DispatchMetadata) error
	RecordReceipt(DispatchMetadata, DispatchReceipt) error
	Receipt(DispatchMetadata) (DispatchReceipt, bool, error)
}

// DispatchClassifier is optional. Without an explicit adapter declaration,
// external work is non-idempotent even when its operation looks read-only.
type DispatchClassifier interface {
	ClassifyDispatch(kernel.Effect) (provider string, class WorkClass, honorsKey bool)
}

// MetadataExecutor additively lets a legacy effect adapter receive the key it
// declared it honors and return trustworthy receipt evidence.
type MetadataExecutor interface {
	Dispatch(context.Context, kernel.Effect, DispatchMetadata) ([]kernel.Event, *DispatchReceipt, error)
}

// TurnDispatchClassifier classifies concrete native model and tool adapters.
type TurnDispatchClassifier interface {
	ClassifyModelDispatch(turn.Request) (provider string, class WorkClass, honorsKey bool)
	ClassifyToolDispatch(kernel.SpawnTurn, turn.ToolCall) (provider string, class WorkClass, honorsKey bool)
}

// MetadataTurnExecutor is the additive native-call form. The original
// TurnExecutor remains valid and is conservatively non-idempotent.
type MetadataTurnExecutor interface {
	CompleteTurnDispatch(context.Context, turn.Request, DispatchMetadata) (turn.Response, *DispatchReceipt, error)
	ExecuteTurnToolDispatch(context.Context, kernel.SpawnTurn, turn.ToolCall, DispatchMetadata) (TurnToolOutcome, *DispatchReceipt, error)
}

func (r *Runner) metadataFor(w Work) DispatchMetadata {
	meta := DispatchMetadata{JobID: r.JobID, WorkID: w.ID, Provider: "executor",
		RequestDigest: w.Digest, WorkClass: WorkNonIdempotent}
	if classifier, ok := r.Executor.(DispatchClassifier); ok {
		provider, class, honors := classifier.ClassifyDispatch(w.Effect)
		if provider != "" {
			meta.Provider = provider
		}
		if class == WorkIdempotent && honors {
			meta.WorkClass, meta.SupportsIdempotency = class, true
		}
	}
	meta.DispatchKey = dispatchIdentity(meta.JobID, meta.WorkID, meta.RequestDigest)
	return meta
}

func childMetadata(r *Runner, child *turnChild, provider string, class WorkClass, honors bool) DispatchMetadata {
	meta := DispatchMetadata{JobID: r.JobID, WorkID: child.ID, Provider: provider,
		RequestDigest: requestDigest([]byte(child.PreparedJSON)), WorkClass: WorkNonIdempotent}
	if class == WorkIdempotent && honors {
		meta.WorkClass, meta.SupportsIdempotency = class, true
	}
	meta.DispatchKey = dispatchIdentity(meta.JobID, meta.WorkID, meta.RequestDigest)
	return meta
}

func dispatchIdentity(jobID, workID, digest string) string {
	return hashDispatchParts("arxi.dispatch/v1", jobID, workID, digest)
}

func requestDigest(body []byte) string {
	return hashDispatchParts("arxi.request/v1", string(body))
}

func hashDispatchParts(parts ...string) string {
	h := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func exactActionDigest(jobID, runID, requester, parentWorkID, providerCallID, toolName, argumentDigest, toolSchemaVersion, policyVersion, workspaceProfileID string) string {
	action, err := authorization.NewAction(authorization.ActionInput{
		JobID:                 jobID,
		RunID:                 runID,
		RequesterPrincipal:    requester,
		SuspendedParentWorkID: parentWorkID,
		ProviderCallID:        providerCallID,
		ToolName:              toolName,
		ArgumentDigest:        argumentDigest,
		ToolSchemaVersion:     toolSchemaVersion,
		PolicyVersion:         policyVersion,
		WorkspaceProfileID:    workspaceProfileID,
	})
	if err != nil {
		return ""
	}
	return action.Digest()
}

func (r *Runner) register(meta DispatchMetadata) error {
	if r.Dispatches == nil {
		return nil
	}
	return r.Dispatches.RegisterDispatch(meta)
}

func (r *Runner) recordReceipt(meta DispatchMetadata, receipt *DispatchReceipt) error {
	if r.Dispatches == nil || receipt == nil {
		return nil
	}
	return r.Dispatches.RecordReceipt(meta, *receipt)
}
