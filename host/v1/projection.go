package v1

import (
	"context"
	"errors"
	"sync"

	"github.com/michiTrader/arxi/internal/app"
)

func publicJob(job app.Projection) Job {
	out := Job{
		ID: JobID(job.ID), Actor: job.Actor, Status: JobStatus(job.Status), Terminal: job.Terminal,
		Sequence: job.Sequence, Stage: job.Stage, StageIndex: job.StageIndex,
		Turns: job.Turns, MaxTurns: job.MaxTurns, SpentUSD: job.SpentUSD,
		TreeSpentUSD: job.TreeSpentUSD, BudgetUSD: job.BudgetUSD,
		Simulated: job.Simulated, UnknownWork: len(job.UnknownWork), Result: job.Result,
	}
	for _, member := range job.Members {
		out.Members = append(out.Members, Member{
			Name: member.Name, Role: member.Role, State: member.State, Detail: member.Detail,
			Submitted: member.Submitted, Busy: member.Busy, Runnable: member.Runnable,
			SpentUSD: member.SpentUSD, Turns: member.Turns,
		})
	}
	for _, decision := range job.Pending {
		out.Pending = append(out.Pending, PendingDecision{
			ID: ItemID(decision.ID), Kind: DecisionKind(decision.Kind),
			Question: decision.Question, Actor: decision.Actor,
		})
	}
	return out
}

type publicSubscription struct {
	inner app.Subscription
	jobID JobID

	mu       sync.Mutex
	closed   bool
	afterSeq int64
}

func (s *publicSubscription) Next(ctx context.Context) (EventBatch, error) {
	s.mu.Lock()
	closed, after := s.closed, s.afterSeq
	s.mu.Unlock()
	if closed {
		return EventBatch{}, adaptReadError(string(CapabilitySubscribe), s.jobID, after, app.ErrClosed)
	}
	batch, err := s.inner.Next(ctx)
	if err != nil {
		var slow *app.SlowConsumerError
		if errors.As(err, &slow) {
			hostErr := newError(CodeSlowConsumer, string(CapabilitySubscribe),
				"subscription backlog exceeds its delivery bound", err)
			hostErr.JobID, hostErr.AfterSeq = s.jobID, slow.AfterSequence
			return EventBatch{}, hostErr
		}
		return EventBatch{}, adaptReadError(string(CapabilitySubscribe), s.jobID, after, err)
	}
	s.mu.Lock()
	s.afterSeq = batch.AfterSequence
	s.mu.Unlock()
	out := EventBatch{Events: make([]Event, 0, len(batch.Events)), AfterSeq: batch.AfterSequence}
	for _, event := range batch.Events {
		out.Events = append(out.Events, Event{
			Sequence: event.Sequence, ID: event.ID, Time: event.Time, Type: event.Type,
			Scope: event.Scope, Source: event.Source, Actor: event.Actor,
			CorrelationID: event.CorrelationID, CausedBy: append([]string(nil), event.CausedBy...),
			Depth: event.Depth, Payload: append([]byte(nil), event.Payload...),
		})
	}
	return out, nil
}

func (s *publicSubscription) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.inner.Close()
}
