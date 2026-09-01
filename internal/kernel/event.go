package kernel

// EventType is the type of a log event.
//
// The namespace is hierarchical and dot-separated (`stage.entered`, not
// `stage_entered`) because watchers match by prefix: `stage.*` has to be able to
// mean "everything that happens to stages" without a hand-maintained list.
type EventType string

const (
	// --- run lifecycle ---
	RunStarted   EventType = "run.started"
	RunPrompt    EventType = "run.prompt"
	RunPaused    EventType = "run.paused"
	RunUnpaused  EventType = "run.unpaused"
	RunCancelled EventType = "run.cancelled"
	RunExpired   EventType = "run.expired"
	RunResult    EventType = "run.result"

	// RunQuiescent is an EVENT, not a terminal state. The system having gone
	// silent does not mean it should be killed: it means someone has to be told.
	// See decide.go:checkQuiescence.
	RunQuiescent EventType = "run.quiescent"

	// --- stages ---
	StageEntered   EventType = "stage.entered"
	StageSubmitted EventType = "stage.submitted"
	StageAdvanced  EventType = "stage.advanced"
	StageTimeout   EventType = "stage.timeout"

	// --- agents ---
	AgentActivated EventType = "agent.activated"
	AgentSteered   EventType = "agent.steered"
	AgentNotified  EventType = "agent.notified"
	AgentTurnDone  EventType = "agent.turn_done"
	AgentBlocked   EventType = "agent.blocked"
	AgentUnblocked EventType = "agent.unblocked"
	AgentFailed    EventType = "agent.failed"

	// --- tools ---
	ToolCall          EventType = "tool.call"
	ToolCallCompleted EventType = "tool.call_completed"
	ToolCallDenied    EventType = "tool.call_denied"

	// --- model ---
	LLMResponse EventType = "llm.response"

	// --- resources ---
	LockAcquired     EventType = "lock.acquired"
	LockReleased     EventType = "lock.released"
	ResourceConflict EventType = "resource.conflict"

	// --- budget ---
	BudgetWarning  EventType = "budget.warning"
	BudgetExceeded EventType = "budget.exceeded"

	// --- human in the loop ---
	InboxCreated EventType = "inbox.created"
	InboxReplied EventType = "inbox.replied"
	InboxTimeout EventType = "inbox.timeout"

	// --- clock ---
	TimerTick EventType = "timer.tick"
)

// Source says who produced the event.
//
// It exists for one concrete reason: derived events (the ones the reducer itself
// emits via Emit) must NOT re-trigger watchers, or a watcher on `stage.*` loops
// forever on the `stage.advanced` events it caused itself. See the
// `e.Source != SourceRuntime` guard in decide.go.
type Source string

const (
	SourceHuman   Source = "human"
	SourceAgent   Source = "agent"
	SourceRuntime Source = "runtime"
	SourceTrigger Source = "trigger"
)

// Event is one log entry. The log is the source of truth: state is derived with
// fold(Decide, State0, events) and the snapshot is only a cache.
//
// Every field is plain serializable data. No pointers to anything live, no
// functions, no channels: an event has to be writable as NDJSON, readable six
// months later, and produce exactly the same state.
type Event struct {
	// Seq is the sequence number within the run. The single log writer assigns
	// it, NOT the reducer: the reducer returns events with Seq 0 because
	// deciding the global order is not its job.
	Seq int64 `json:"seq"`

	ID    string    `json:"id"`
	Ts    string    `json:"ts"`
	Type  EventType `json:"type"`
	Scope string    `json:"scope"`

	Source Source `json:"source"`
	Actor  string `json:"actor,omitempty"`

	// CorrelationID groups the whole causal chain that started from one root
	// cause. CausedBy holds the direct parents. Together they are what lets
	// `event trace` rebuild the tree and `run why` walk backwards.
	CorrelationID string   `json:"correlation_id,omitempty"`
	CausedBy      []string `json:"caused_by,omitempty"`

	// Depth is causal depth. It is the brake on watcher cascades: without it, a
	// watcher reacting to what another watcher caused has no bottom, and the
	// bottom is paid in dollars. See Config.MaxDepth.
	Depth int `json:"depth"`

	Payload map[string]any `json:"payload,omitempty"`
}

// MatchEventType reports whether an event type matches a pattern.
//
// Exact match, `*` for everything, and a single trailing segment wildcard
// (`stage.*`). There is no full glob on purpose: a pattern nobody can read at a
// glance is a pattern that is going to wake agents nobody expected.
//
// It is EXPORTED because the reducer is no longer the only thing that matches a
// type against a pattern. `arxi event log --type stage.*` asks the same question
// a watcher asks, and a CLI that reimplemented the rule would be a second
// dialect of it: the day one gained partial globs the other would answer
// differently about the same log, and the user would have no way to tell which
// spelling belonged to which. internal/blueprint's validator already refuses the
// shapes this does not support (load.go:pattern) by describing THIS function's
// semantics, so there is exactly one rule and three places that agree about it.
func MatchEventType(pattern, typ string) bool {
	if pattern == typ || pattern == "*" {
		return true
	}
	if strings_HasSuffix(pattern, ".*") {
		return strings_HasPrefix(typ, pattern[:len(pattern)-1])
	}
	return false
}

// strings_HasSuffix and strings_HasPrefix are spelled out rather than imported.
//
// Not purism, and not a preference: internal/arch_test.go asserts the kernel's
// import graph, and event.go is the file that already declines to import
// encoding/json for json_Number for the same reason. Two three-line comparisons
// are cheaper than widening the graph the architecture test is there to keep
// narrow -- and decide.go, which does import strings, is where the reducer's own
// uses live.
func strings_HasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func strings_HasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Str reads a string from the payload. Returns "" if it is missing or of another
// type.
//
// It deliberately returns no error. The payload comes from JSON, where anything
// can be absent, and a reducer littered with `if err != nil` for every optional
// field would be unreadable. A missing field is a normal case, not a failure:
// what is mandatory is verified by the schema (spec/events.md), not here.
func (e Event) Str(key string) string {
	if e.Payload == nil {
		return ""
	}
	s, _ := e.Payload[key].(string)
	return s
}

// Num reads a number from the payload.
//
// It accepts float64 (what encoding/json produces), int and int64 (what tests
// and hand-built events produce). Without this normalization, an event built in
// Go and the same event after a JSON round-trip would give different results,
// and replay would stop being faithful.
func (e Event) Num(key string) float64 {
	if e.Payload == nil {
		return 0
	}
	switch v := e.Payload[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json_Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

// json_Number is the minimal interface of json.Number, declared here to avoid
// importing encoding/json into the kernel. This is not purism: the architecture
// test (internal/arch_test.go) checks the kernel's import graph, and the smaller
// that graph is, the stronger the purity guarantee.
type json_Number interface {
	Float64() (float64, error)
}
