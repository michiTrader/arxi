package v1

import (
	"context"
	"encoding/json"
)

// JobID is an opaque job identifier.
type JobID string

// ItemID is an opaque pending-decision identifier scoped to a job.
type ItemID string

// Principal identifies the caller for authorization and audit.
type Principal struct {
	ID         string            `json:"id"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// SubmitRequest contains source text and Phase 1 execution controls. Actor is a
// logical name; resolving filesystem paths remains an adapter responsibility.
type SubmitRequest struct {
	Principal Principal `json:"principal"`
	Actor     string    `json:"actor"`
	Blueprint string    `json:"blueprint"`
	Prompt    string    `json:"prompt"`
	BudgetUSD float64   `json:"budget_usd,omitempty"`
	MaxTurns  int       `json:"max_turns,omitempty"`
	Simulated bool      `json:"simulated,omitempty"`
}

// SubmitResult confirms durable acceptance. AcceptedSeq is the confirmed
// run-start sequence at which the job became inspectable.
type SubmitResult struct {
	JobID       JobID     `json:"job_id"`
	AcceptedSeq int64     `json:"accepted_seq"`
	Status      JobStatus `json:"status"`
}

// InspectRequest identifies a job to inspect.
type InspectRequest struct {
	Principal Principal `json:"principal"`
	JobID     JobID     `json:"job_id"`
}

// CancelRequest identifies a job to cancel.
type CancelRequest struct {
	Principal Principal `json:"principal"`
	JobID     JobID     `json:"job_id"`
	Reason    string    `json:"reason,omitempty"`
}

// ApproveRequest approves one approval item.
type ApproveRequest struct {
	Principal Principal `json:"principal"`
	JobID     JobID     `json:"job_id"`
	ItemID    ItemID    `json:"item_id"`
}

// RejectRequest rejects one approval item with a reason.
type RejectRequest struct {
	Principal Principal `json:"principal"`
	JobID     JobID     `json:"job_id"`
	ItemID    ItemID    `json:"item_id"`
	Reason    string    `json:"reason"`
}

// AnswerRequest answers one question item.
type AnswerRequest struct {
	Principal Principal `json:"principal"`
	JobID     JobID     `json:"job_id"`
	ItemID    ItemID    `json:"item_id"`
	Text      string    `json:"text"`
}

// WaitRequest identifies a job whose terminal projection is required.
type WaitRequest struct {
	Principal Principal `json:"principal"`
	JobID     JobID     `json:"job_id"`
}

// CapabilitiesRequest identifies the principal whose effective snapshot is
// requested. It is not a declaration of all vocabulary.
type CapabilitiesRequest struct {
	Principal Principal `json:"principal"`
}

// JobStatus is the selected public lifecycle state.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobBlocked   JobStatus = "blocked"
	JobPaused    JobStatus = "paused"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
	JobExpired   JobStatus = "expired"
)

// Terminal reports whether no further lifecycle mutation is accepted.
func (s JobStatus) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled, JobExpired:
		return true
	default:
		return false
	}
}

// Job is the selected public projection of one job.
type Job struct {
	ID           JobID             `json:"id"`
	Actor        string            `json:"actor,omitempty"`
	Status       JobStatus         `json:"status"`
	Terminal     bool              `json:"terminal"`
	Sequence     int64             `json:"sequence"`
	Stage        string            `json:"stage,omitempty"`
	StageIndex   int               `json:"stage_index"`
	Turns        int               `json:"turns"`
	MaxTurns     int               `json:"max_turns,omitempty"`
	Members      []Member          `json:"members,omitempty"`
	SpentUSD     float64           `json:"spent_usd,omitempty"`
	TreeSpentUSD float64           `json:"tree_spent_usd,omitempty"`
	BudgetUSD    float64           `json:"budget_usd,omitempty"`
	Simulated    bool              `json:"simulated,omitempty"`
	Pending      []PendingDecision `json:"pending,omitempty"`
	UnknownWork  int               `json:"unknown_work,omitempty"`
	Result       string            `json:"result,omitempty"`
}

// Member is one participant in the selected job projection.
type Member struct {
	Name      string  `json:"name"`
	Role      string  `json:"role,omitempty"`
	State     string  `json:"state"`
	Detail    string  `json:"detail,omitempty"`
	Submitted bool    `json:"submitted,omitempty"`
	Busy      bool    `json:"busy,omitempty"`
	Runnable  bool    `json:"runnable,omitempty"`
	SpentUSD  float64 `json:"spent_usd,omitempty"`
	Turns     int     `json:"turns,omitempty"`
}

// DecisionKind says which exact host method may resolve an item.
type DecisionKind string

const (
	DecisionApproval DecisionKind = "approval"
	DecisionQuestion DecisionKind = "question"
)

// PendingDecision is an unresolved human decision.
type PendingDecision struct {
	ID       ItemID       `json:"id"`
	Kind     DecisionKind `json:"kind"`
	Question string       `json:"question"`
	Actor    string       `json:"actor,omitempty"`
}

// Event is a copied, provider-independent confirmed event envelope.
type Event struct {
	Sequence      int64           `json:"sequence"`
	ID            string          `json:"id"`
	Time          string          `json:"time,omitempty"`
	Type          string          `json:"type"`
	Scope         string          `json:"scope,omitempty"`
	Source        string          `json:"source"`
	Actor         string          `json:"actor,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausedBy      []string        `json:"caused_by,omitempty"`
	Depth         int             `json:"depth,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// EventFilter is OR within each populated dimension and AND across dimensions.
type EventFilter struct {
	TypePrefixes []string `json:"type_prefixes,omitempty"`
	Sources      []string `json:"sources,omitempty"`
	Actors       []string `json:"actors,omitempty"`
}

// SubscribeRequest starts after AfterSeq; the logical sequence is exclusive.
type SubscribeRequest struct {
	Principal Principal   `json:"principal"`
	JobID     JobID       `json:"job_id"`
	AfterSeq  int64       `json:"after_seq,omitempty"`
	Filter    EventFilter `json:"filter,omitempty"`
}

// EventBatch contains confirmed events. AfterSeq advances to the greatest
// confirmed sequence scanned, even when the filter matched no events.
type EventBatch struct {
	Events   []Event `json:"events"`
	AfterSeq int64   `json:"after_seq"`
}

// Subscription is a bounded stream of confirmed event batches.
type Subscription interface {
	Next(context.Context) (EventBatch, error)
	Close() error
}
