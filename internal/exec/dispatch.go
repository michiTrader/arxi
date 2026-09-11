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
