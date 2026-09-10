// Package app provides reusable application-level run services.
package app

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrNotFound        = errors.New("job not found")
	ErrClosed          = errors.New("service is closed")
	ErrSlowConsumer    = errors.New("subscription backlog exceeds its delivery bound")
)

// SlowConsumerError terminates a subscription before an unbounded confirmed
// backlog is decoded. AfterSequence is the last cursor successfully delivered,
// so a replacement subscription can resume without silently dropping events.
type SlowConsumerError struct {
	AfterSequence int64
}

func (e *SlowConsumerError) Error() string {
	return "subscription backlog exceeds its delivery bound"
}

func (e *SlowConsumerError) Unwrap() error { return ErrSlowConsumer }

// Projection is a selected run view independent of adapters and reducer types.
type Projection struct {
	ID           string
	Actor        string
	Status       string
	Terminal     bool
	Sequence     int64
	Stage        string
	StageIndex   int
	Turns        int
	MaxTurns     int
	Members      []Member
	SpentUSD     float64
	TreeSpentUSD float64
	BudgetUSD    float64
	Simulated    bool
	Pending      []PendingDecision
	UnknownWork  []string
	Result       string
}

// Member is the selected application view of one run participant.
type Member struct {
	Name      string
	Role      string
	State     string
	Detail    string
	Submitted bool
	Busy      bool
	Runnable  bool
	SpentUSD  float64
	Turns     int
}

// PendingDecision is one unresolved human decision.
type PendingDecision struct {
	ID       string
	Kind     string
	Question string
	Actor    string
}

// Listing preserves healthy projections while reporting unreadable runs.
type Listing struct {
	Jobs       []Projection
	Unreadable []error
}

// Event is a copied, provider-independent confirmed event.
type Event struct {
	Sequence      int64
	ID            string
	Time          string
	Type          string
	Scope         string
	Source        string
	Actor         string
	CorrelationID string
	CausedBy      []string
	Depth         int
	Payload       json.RawMessage
}

// EventFilter is OR within populated dimensions and AND across dimensions.
type EventFilter struct {
	TypePrefixes []string
	Sources      []string
	Actors       []string
}

// EventBatch advances AfterSequence through every confirmed event scanned,
// including events excluded by Filter.
type EventBatch struct {
	Events        []Event
	AfterSequence int64
}

// Subscription is a bounded pull stream. A pull never blocks the writer and
// never silently drops confirmed events: if the unread confirmed backlog exceeds
// the delivery bound, Next terminates with SlowConsumerError and the last
// successfully delivered sequence for an exact resubscription.
type Subscription interface {
	Next(context.Context) (EventBatch, error)
	Close() error
}
