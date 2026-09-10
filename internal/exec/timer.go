package exec

import (
	"fmt"
	"sort"

	"github.com/michiTrader/arxi/internal/kernel"
)

// TimerState is the clock state reconstructed exclusively from durable timer
// lifecycle records. Deadlines use the timeline recorded by timer.scheduled:
// Unix milliseconds for live runs and logical milliseconds for simulations.
type TimerState struct {
	NowMs   int64
	Pending map[string]int64
}

// RecoverTimers folds timer lifecycle records in log order. A later schedule
// replaces an earlier schedule for the same id, while cancellation, firing, or
// a legacy timer.tick removes it. timer.tick is accepted as a terminal record
// so runs written during the timer-ledger migration do not redeliver a timeout.
func RecoverTimers(events []kernel.Event) (TimerState, error) {
	out := TimerState{Pending: map[string]int64{}}
	for _, event := range events {
		switch event.Type {
		case kernel.TimerScheduled:
			id := event.Str("timer_id")
			if id == "" {
				return out, fmt.Errorf("%s at seq %d has no timer_id", event.Type, event.Seq)
			}
			deadline, err := timerPayloadInt64(event, "deadline_ms", false)
			if err != nil {
				return out, err
			}
			after, err := timerPayloadInt64(event, "after_ms", true)
			if err != nil {
				return out, err
			}
			if deadline < after {
				return out, fmt.Errorf("%s at seq %d has deadline_ms %d before its after_ms %d", event.Type, event.Seq, deadline, after)
			}
			if base := deadline - after; base > out.NowMs {
				out.NowMs = base
			}
			out.Pending[id] = deadline
		case kernel.TimerCancelled, kernel.TimerTick:
			id := event.Str("timer_id")
			if id == "" {
				return out, fmt.Errorf("%s at seq %d has no timer_id", event.Type, event.Seq)
			}
			delete(out.Pending, id)
		case kernel.TimerFired:
			id := event.Str("timer_id")
			if id == "" {
				return out, fmt.Errorf("%s at seq %d has no timer_id", event.Type, event.Seq)
			}
			firedAt, err := timerPayloadInt64(event, "fired_at_ms", false)
			if err != nil {
				return out, err
			}
			if firedAt > out.NowMs {
				out.NowMs = firedAt
			}
			delete(out.Pending, id)
		}
	}
	return out, nil
}

func timerPayloadInt64(event kernel.Event, key string, positive bool) (int64, error) {
	value, err := payloadInt64(event, key)
	if err != nil {
		return 0, err
	}
	if positive && value <= 0 {
		return 0, fmt.Errorf("%s at seq %d has non-positive %s %d", event.Type, event.Seq, key, value)
	}
	return value, nil
}

func sortedTimerIDs(pending map[string]int64) []string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
