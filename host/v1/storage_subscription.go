package v1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
)

type storageSubscription struct {
	storage JobStorage
	jobID   JobID
	filter  EventFilter

	mu           sync.Mutex
	after        int64
	continuation Continuation
	closed       chan struct{}
	closeOnce    sync.Once
}

func (s *storageSubscription) Next(ctx context.Context) (EventBatch, error) {
	for {
		s.mu.Lock()
		after, continuation := s.after, s.continuation
		s.mu.Unlock()
		select {
		case <-s.closed:
			return EventBatch{}, subscriptionError(CodeConflict, s.jobID, after, errors.New("subscription is closed"))
		default:
		}
		batch, err := s.storage.ReadConfirmed(ctx, s.jobID, ConfirmedRead{
			AfterSequence: after, Continuation: continuation, Limit: 256,
		})
		if err != nil {
			return EventBatch{}, adaptStorageError(CapabilitySubscribe, s.jobID, after, err)
		}
		if batch.AfterSequence < after {
			return EventBatch{}, adaptStorageError(CapabilitySubscribe, s.jobID, after,
				errors.New("storage confirmed sequence moved backwards"))
		}
		out := EventBatch{Events: make([]Event, 0, len(batch.Records)), AfterSeq: batch.AfterSequence}
		for _, record := range batch.Records {
			event, err := decodeStoredEvent(record)
			if err != nil {
				return EventBatch{}, adaptStorageError(CapabilitySubscribe, s.jobID, after, err)
			}
			if matchesPublicEvent(event, s.filter) {
				projected, err := publicStoredEvent(event)
				if err != nil {
					return EventBatch{}, adaptStorageError(CapabilitySubscribe, s.jobID, after, err)
				}
				out.Events = append(out.Events, projected)
			}
		}
		s.mu.Lock()
		s.after, s.continuation = batch.AfterSequence, batch.Continuation
		s.mu.Unlock()
		if batch.AfterSequence > after {
			return out, nil
		}
		if !batch.End || batch.Continuation != continuation {
			continue
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return EventBatch{}, ctx.Err()
		case <-s.closed:
			if !timer.Stop() {
				<-timer.C
			}
			return EventBatch{}, subscriptionError(CodeConflict, s.jobID, after, errors.New("subscription is closed"))
		case <-timer.C:
		}
	}
}

func (s *storageSubscription) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func publicStoredEvent(event kernel.Event) (Event, error) {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Sequence: event.Seq, ID: event.ID, Time: event.Ts, Type: string(event.Type),
		Scope: event.Scope, Source: string(event.Source), Actor: event.Actor,
		CorrelationID: event.CorrelationID, CausedBy: append([]string(nil), event.CausedBy...),
		Depth: event.Depth, Payload: payload}, nil
}

func matchesPublicEvent(event kernel.Event, filter EventFilter) bool {
	return matchString(string(event.Type), filter.TypePrefixes, true) &&
		matchString(string(event.Source), filter.Sources, false) && matchString(event.Actor, filter.Actors, false)
}

func matchString(value string, choices []string, prefix bool) bool {
	if len(choices) == 0 {
		return true
	}
	for _, choice := range choices {
		if (!prefix && value == choice) || (prefix && strings.HasPrefix(value, choice)) {
			return true
		}
	}
	return false
}

func cloneEventFilter(filter EventFilter) EventFilter {
	return EventFilter{TypePrefixes: append([]string(nil), filter.TypePrefixes...),
		Sources: append([]string(nil), filter.Sources...), Actors: append([]string(nil), filter.Actors...)}
}

func subscriptionError(code ErrorCode, id JobID, after int64, err error) error {
	out := newError(code, string(CapabilitySubscribe), err.Error(), err)
	out.JobID, out.AfterSeq = id, after
	return out
}
