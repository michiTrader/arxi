package kernel

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type EventType string

const (
	// Implementation note.
	RunStarted   EventType = "run.started"
	RunPrompt    EventType = "run.prompt"
	RunPaused    EventType = "run.paused"
	RunUnpaused  EventType = "run.unpaused"
	RunCancelled EventType = "run.cancelled"
	RunExpired   EventType = "run.expired"
	RunResult    EventType = "run.result"

	// Implementation note.
	// Implementation note.
	// Implementation note.
	RunQuiescent EventType = "run.quiescent"

	// Implementation note.
	StageEntered   EventType = "stage.entered"
	StageSubmitted EventType = "stage.submitted"
	StageAdvanced  EventType = "stage.advanced"
	StageTimeout   EventType = "stage.timeout"

	// Implementation note.
	AgentActivated EventType = "agent.activated"
	AgentSteered   EventType = "agent.steered"
	AgentNotified  EventType = "agent.notified"
	AgentTurnDone  EventType = "agent.turn_done"
	AgentBlocked   EventType = "agent.blocked"
	AgentUnblocked EventType = "agent.unblocked"
	AgentFailed    EventType = "agent.failed"

	// Implementation note.
	ToolCall          EventType = "tool.call"
	ToolCallCompleted EventType = "tool.call_completed"
	ToolCallDenied    EventType = "tool.call_denied"

	// Implementation note.
	LLMResponse EventType = "llm.response"

	// Implementation note.
	LockAcquired     EventType = "lock.acquired"
	LockReleased     EventType = "lock.released"
	ResourceConflict EventType = "resource.conflict"

	// Implementation note.
	BudgetWarning  EventType = "budget.warning"
	BudgetExceeded EventType = "budget.exceeded"

	// Implementation note.
	InboxCreated EventType = "inbox.created"
	InboxReplied EventType = "inbox.replied"
	InboxTimeout EventType = "inbox.timeout"

	// Implementation note.
	TimerTick EventType = "timer.tick"
)

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Source string

const (
	SourceHuman   Source = "human"
	SourceAgent   Source = "agent"
	SourceRuntime Source = "runtime"
	SourceTrigger Source = "trigger"
)

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Event struct {
	// Implementation note.
	// Implementation note.
	// Implementation note.
	Seq int64 `json:"seq"`

	ID    string    `json:"id"`
	Ts    string    `json:"ts"`
	Type  EventType `json:"type"`
	Scope string    `json:"scope"`

	Source Source `json:"source"`
	Actor  string `json:"actor,omitempty"`

	// Implementation note.
	// Implementation note.
	// Implementation note.
	CorrelationID string   `json:"correlation_id,omitempty"`
	CausedBy      []string `json:"caused_by,omitempty"`

	// Implementation note.
	// Implementation note.
	// Implementation note.
	Depth int `json:"depth"`

	Payload map[string]any `json:"payload,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func (e Event) Str(key string) string {
	if e.Payload == nil {
		return ""
	}
	s, _ := e.Payload[key].(string)
	return s
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
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

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type json_Number interface {
	Float64() (float64, error)
}
