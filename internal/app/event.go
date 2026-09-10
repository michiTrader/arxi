package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
)

func cloneFilter(filter EventFilter) EventFilter {
	return EventFilter{
		TypePrefixes: append([]string(nil), filter.TypePrefixes...),
		Sources:      append([]string(nil), filter.Sources...),
		Actors:       append([]string(nil), filter.Actors...),
	}
}

func matches(event kernel.Event, filter EventFilter) bool {
	return matchAny(filter.TypePrefixes, func(want string) bool {
		return strings.HasPrefix(string(event.Type), want)
	}) && matchAny(filter.Sources, func(want string) bool {
		return string(event.Source) == want
	}) && matchAny(filter.Actors, func(want string) bool {
		return event.Actor == want
	})
}

func matchAny(values []string, match func(string) bool) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if match(value) {
			return true
		}
	}
	return false
}

func projectEvent(event kernel.Event) (Event, error) {
	var payload json.RawMessage
	if event.Payload != nil {
		raw, err := json.Marshal(event.Payload)
		if err != nil {
			return Event{}, fmt.Errorf("encode payload for confirmed event %s at seq %d: %w", event.ID, event.Seq, err)
		}
		payload = append(json.RawMessage(nil), raw...)
	}
	return Event{
		Sequence: event.Seq, ID: event.ID, Time: event.Ts, Type: string(event.Type),
		Scope: event.Scope, Source: string(event.Source), Actor: event.Actor,
		CorrelationID: event.CorrelationID, CausedBy: append([]string(nil), event.CausedBy...),
		Depth: event.Depth, Payload: payload,
	}, nil
}
