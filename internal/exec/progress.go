package exec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/michiTrader/arxi/internal/kernel"
)

const canonicalEffectVersion = "canonical-effect-v1"

type Work struct {
	ID          string
	SourceSeq   int64
	SourceID    string
	EffectIndex int
	Kind        string
	Class       string
	Digest      string
	Effect      kernel.Effect
	Source      kernel.Event
}

// Recovery is the durable execution boundary reconstructed from the log.
// Cursor names the latest source event whose exec.step_completed is committed;
// HasProgress distinguishes a modern run from a legacy log with no records.
type Recovery struct {
	Cursor      int64
	HasProgress bool
	Unknown     []string
}

// Recover validates execution records and derives the safe source-event cursor.
// Step completions must advance one domain event at a time; operational records
// between them are skipped because they are bookkeeping, not reducer inputs.
func Recover(events []kernel.Event) (Recovery, error) {
	var out Recovery
	prepared := map[string]int64{}
	finished := map[string]string{}
	bySeq := make(map[int64]kernel.Event, len(events))
	for _, event := range events {
		bySeq[event.Seq] = event
	}
	firstDomain := int64(0)
	for _, event := range events {
		if !isProgressEvent(event.Type) {
			firstDomain = event.Seq
			break
		}
	}
	expected := firstDomain

	for _, event := range events {
		switch event.Type {
		case kernel.ExecWorkPrepared:
			out.HasProgress = true
			seq, err := payloadInt64(event, "source_seq")
			if err != nil {
				return out, err
			}
			id := event.Str("work_id")
			if id == "" {
				return out, fmt.Errorf("%s at seq %d has no work_id", event.Type, event.Seq)
			}
			if old, ok := prepared[id]; ok && old != seq {
				return out, fmt.Errorf("work %s is prepared for conflicting source sequences %d and %d", id, old, seq)
			}
			prepared[id] = seq
		case kernel.ExecWorkStarted:
			out.HasProgress = true
			if _, ok := prepared[event.Str("work_id")]; !ok {
				return out, fmt.Errorf("work %q starts before it is prepared", event.Str("work_id"))
			}
		case kernel.ExecWorkFinished:
			out.HasProgress = true
			id, status := event.Str("work_id"), event.Str("status")
			if _, ok := prepared[id]; !ok {
				return out, fmt.Errorf("work %q finishes before it is prepared", id)
			}
			if status != "completed" && status != "failed" && status != "unknown" {
				return out, fmt.Errorf("work %s has invalid terminal status %q", id, status)
			}
			if old, ok := finished[id]; ok && old != status {
				return out, fmt.Errorf("work %s has conflicting terminal statuses %q and %q", id, old, status)
			}
			finished[id] = status
		case kernel.ExecStepCompleted:
			out.HasProgress = true
			seq, err := payloadInt64(event, "source_seq")
			if err != nil {
				return out, err
			}
			for expected > 0 && expected < seq {
				candidate, ok := bySeq[expected]
				if !ok {
					return out, fmt.Errorf("event log is missing seq %d before completed source %d", expected, seq)
				}
				if !isProgressEvent(candidate.Type) {
					break
				}
				expected++
			}
			if seq != expected {
				return out, fmt.Errorf("exec.step_completed source_seq %d is not the next unfinished domain event %d", seq, expected)
			}
			if candidate, ok := bySeq[seq]; !ok || isProgressEvent(candidate.Type) {
				return out, fmt.Errorf("exec.step_completed source_seq %d does not name a domain event", seq)
			}
			out.Cursor = seq
			expected = seq + 1
		}
	}
	for id, status := range finished {
		if status == "unknown" {
			out.Unknown = append(out.Unknown, id)
		}
	}
	sort.Strings(out.Unknown)
	return out, nil
}

func payloadInt64(event kernel.Event, key string) (int64, error) {
	if event.Payload == nil {
		return 0, fmt.Errorf("%s at seq %d has no %s", event.Type, event.Seq, key)
	}
	switch value := event.Payload[key].(type) {
	case int:
		return int64(value), nil
	case int64:
		return value, nil
	case float64:
		if value < 1 || value > math.MaxInt64 || math.Trunc(value) != value {
			return 0, fmt.Errorf("%s at seq %d has invalid %s %v", event.Type, event.Seq, key, value)
		}
		return int64(value), nil
	case json.Number:
		got, err := value.Int64()
		if err == nil && got >= 1 {
			return got, nil
		}
	}
	return 0, fmt.Errorf("%s at seq %d has invalid %s", event.Type, event.Seq, key)
}

func manifest(runID string, source kernel.Event, fx []kernel.Effect) ([]Work, error) {
	out := make([]Work, len(fx))
	for i, effect := range fx {
		kind, value, err := effectValue(effect)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(struct {
			Version string `json:"version"`
			Kind    string `json:"kind"`
			Value   any    `json:"value"`
		}{canonicalEffectVersion, kind, normalize(value)})
		if err != nil {
			return nil, fmt.Errorf("canonicalize effect %d (%s): %w", i, kind, err)
		}
		digest := sha256.Sum256(body)
		digestHex := hex.EncodeToString(digest[:])
		class := className(effect.Class())
		identity, err := json.Marshal([]any{runID, source.Seq, source.ID, i, kind, class, digestHex})
		if err != nil {
			return nil, fmt.Errorf("encode work identity: %w", err)
		}
		id := sha256.Sum256(identity)
		out[i] = Work{
			ID: "work-" + hex.EncodeToString(id[:]), SourceSeq: source.Seq,
			SourceID: source.ID, EffectIndex: i, Kind: kind, Class: class,
			Digest: digestHex, Effect: effect, Source: source,
		}
	}
	return out, nil
}

func effectValue(effect kernel.Effect) (string, any, error) {
	switch v := effect.(type) {
	case kernel.SpawnTurn:
		return "spawn_turn", v, nil
	case kernel.CallTool:
		return "call_tool", v, nil
	case kernel.Emit:
		return "emit", v, nil
	case kernel.SetTimer:
		return "set_timer", v, nil
	case kernel.CancelTimer:
		return "cancel_timer", v, nil
	case kernel.AskHuman:
		return "ask_human", v, nil
	case kernel.Snapshot:
		return "snapshot", struct{}{}, nil
	default:
		return "", nil, fmt.Errorf("canonicalize unhandled effect %T", effect)
	}
}

func className(class kernel.EffectClass) string {
	if class == kernel.ClassControl {
		return "control"
	}
	return "independent"
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, []any{key, normalize(x[key])})
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normalize(x[i])
		}
		return out
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return x
		}
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			if _, ok := decoded.(map[string]any); ok {
				return normalize(decoded)
			}
			if _, ok := decoded.([]any); ok {
				return normalize(decoded)
			}
		}
		return decoded
	}
}
