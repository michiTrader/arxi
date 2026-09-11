package exec

import (
	"context"
	"encoding/json"

	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

// DispatchMetadata is immutable external-call identity. A key is useful only
// when SupportsIdempotency is true; otherwise it remains correlation evidence.
type DispatchMetadata struct {
	JobID               job.JobID
	WorkID              job.WorkID
	Provider            string
	RequestDigest       job.Digest
	DispatchKey         job.DispatchKey
	WorkClass           job.WorkClass
	SupportsIdempotency bool
}

// DispatchReceipt binds a stable external identifier to the exact canonical
// outcome returned by an adapter.
type DispatchReceipt struct {
	ExternalID       string
	Status           job.OutcomeStatus
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
	ClassifyDispatch(kernel.Effect) (provider string, class job.WorkClass, honorsKey bool)
}

// MetadataExecutor additively lets a legacy effect adapter receive the key it
// declared it honors and return trustworthy receipt evidence.
type MetadataExecutor interface {
	Dispatch(context.Context, kernel.Effect, DispatchMetadata) ([]kernel.Event, *DispatchReceipt, error)
}

// TurnDispatchClassifier classifies concrete native model and tool adapters.
type TurnDispatchClassifier interface {
	ClassifyModelDispatch(turn.Request) (provider string, class job.WorkClass, honorsKey bool)
	ClassifyToolDispatch(kernel.SpawnTurn, turn.ToolCall) (provider string, class job.WorkClass, honorsKey bool)
}

// MetadataTurnExecutor is the additive native-call form. The original
// TurnExecutor remains valid and is conservatively non-idempotent.
type MetadataTurnExecutor interface {
	CompleteTurnDispatch(context.Context, turn.Request, DispatchMetadata) (turn.Response, *DispatchReceipt, error)
	ExecuteTurnToolDispatch(context.Context, kernel.SpawnTurn, turn.ToolCall, DispatchMetadata) (TurnToolOutcome, *DispatchReceipt, error)
}

func (r *Runner) metadataFor(w Work) DispatchMetadata {
	meta := DispatchMetadata{JobID: job.JobID(r.JobID), WorkID: job.WorkID(w.ID), Provider: "executor",
		RequestDigest: job.Digest(w.Digest), WorkClass: job.WorkNonIdempotent}
	if classifier, ok := r.Executor.(DispatchClassifier); ok {
		provider, class, honors := classifier.ClassifyDispatch(w.Effect)
		if provider != "" {
			meta.Provider = provider
		}
		if class == job.WorkIdempotent && honors {
			meta.WorkClass, meta.SupportsIdempotency = class, true
		}
	}
	meta.DispatchKey = job.DispatchIdentity(meta.JobID, meta.WorkID, meta.RequestDigest)
	return meta
}

func childMetadata(r *Runner, child *turnChild, provider string, class job.WorkClass, honors bool) DispatchMetadata {
	meta := DispatchMetadata{JobID: job.JobID(r.JobID), WorkID: job.WorkID(child.ID), Provider: provider,
		RequestDigest: job.RequestDigest([]byte(child.PreparedJSON)), WorkClass: job.WorkNonIdempotent}
	if class == job.WorkIdempotent && honors {
		meta.WorkClass, meta.SupportsIdempotency = class, true
	}
	meta.DispatchKey = job.DispatchIdentity(meta.JobID, meta.WorkID, meta.RequestDigest)
	return meta
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
