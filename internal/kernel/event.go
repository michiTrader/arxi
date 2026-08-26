package kernel

// EventType es el tipo de un evento del log.
//
// El namespace es jerárquico y con punto (`stage.entered`, no `stage_entered`)
// porque los watchers hacen match por prefijo: `stage.*` tiene que poder
// significar "todo lo que le pase a las etapas" sin una lista a mano.
type EventType string

const (
	// --- ciclo de vida del run ---
	RunStarted   EventType = "run.started"
	RunPrompt    EventType = "run.prompt"
	RunPaused    EventType = "run.paused"
	RunUnpaused  EventType = "run.unpaused"
	RunCancelled EventType = "run.cancelled"
	RunExpired   EventType = "run.expired"
	RunResult    EventType = "run.result"

	// RunQuiescent es un EVENTO, no un estado terminal. Que el sistema se haya
	// quedado callado no significa que haya que matarlo: significa que hay que
	// avisarle a alguien. Ver decide.go:checkQuiescence.
	RunQuiescent EventType = "run.quiescent"

	// --- etapas ---
	StageEntered   EventType = "stage.entered"
	StageSubmitted EventType = "stage.submitted"
	StageAdvanced  EventType = "stage.advanced"
	StageTimeout   EventType = "stage.timeout"

	// --- agentes ---
	AgentActivated EventType = "agent.activated"
	AgentSteered   EventType = "agent.steered"
	AgentNotified  EventType = "agent.notified"
	AgentTurnDone  EventType = "agent.turn_done"
	AgentBlocked   EventType = "agent.blocked"
	AgentUnblocked EventType = "agent.unblocked"
	AgentFailed    EventType = "agent.failed"

	// --- herramientas ---
	ToolCall          EventType = "tool.call"
	ToolCallCompleted EventType = "tool.call_completed"
	ToolCallDenied    EventType = "tool.call_denied"

	// --- modelo ---
	LLMResponse EventType = "llm.response"

	// --- recursos ---
	LockAcquired     EventType = "lock.acquired"
	LockReleased     EventType = "lock.released"
	ResourceConflict EventType = "resource.conflict"

	// --- presupuesto ---
	BudgetWarning  EventType = "budget.warning"
	BudgetExceeded EventType = "budget.exceeded"

	// --- humano en el bucle ---
	InboxCreated EventType = "inbox.created"
	InboxReplied EventType = "inbox.replied"
	InboxTimeout EventType = "inbox.timeout"

	// --- reloj ---
	TimerTick EventType = "timer.tick"
)

// Source dice quién produjo el evento.
//
// Existe por una razón concreta: los eventos derivados (los que emite el propio
// reducer vía Emit) NO deben volver a disparar watchers, o un watcher sobre
// `stage.*` entra en bucle con los `stage.advanced` que él mismo causó.
// Ver decide.go, la guarda `e.Source != SourceRuntime`.
type Source string

const (
	SourceHuman   Source = "human"
	SourceAgent   Source = "agent"
	SourceRuntime Source = "runtime"
	SourceTrigger Source = "trigger"
)

// Event es una entrada del log. El log es la fuente de verdad: el estado se
// deriva con fold(Decide, State0, events) y el snapshot es solo una caché.
//
// Todos los campos son datos planos y serializables. No hay punteros a nada
// vivo, ni funciones, ni canales: un evento tiene que poder escribirse en
// NDJSON, leerse seis meses después y producir exactamente el mismo estado.
type Event struct {
	// Seq es el número de secuencia dentro del run. Lo asigna el escritor único
	// del log, NO el reducer: el reducer devuelve eventos con Seq 0 porque no
	// le corresponde decidir el orden global.
	Seq int64 `json:"seq"`

	ID    string    `json:"id"`
	Ts    string    `json:"ts"`
	Type  EventType `json:"type"`
	Scope string    `json:"scope"`

	Source Source `json:"source"`
	Actor  string `json:"actor,omitempty"`

	// CorrelationID agrupa toda la cadena causal que arrancó de una misma causa
	// raíz. CausedBy son los padres directos. Los dos juntos son lo que permite
	// que `event trace` reconstruya el árbol y que `run why` camine hacia atrás.
	CorrelationID string   `json:"correlation_id,omitempty"`
	CausedBy      []string `json:"caused_by,omitempty"`

	// Depth es la profundidad causal. Es el freno de la cascada de watchers:
	// sin esto, un watcher que reacciona a lo que otro watcher causó no tiene
	// fondo, y el fondo se paga en dólares. Ver Config.MaxDepth.
	Depth int `json:"depth"`

	Payload map[string]any `json:"payload,omitempty"`
}

// Str lee un string del payload. Devuelve "" si falta o si es de otro tipo.
//
// No devuelve error a propósito. El payload viene de JSON, donde todo puede
// faltar, y un reducer lleno de `if err != nil` por cada campo opcional sería
// ilegible. La ausencia de un campo es un caso normal, no una falla: la
// obligatoriedad se verifica en el schema (spec/events.md), no acá.
func (e Event) Str(key string) string {
	if e.Payload == nil {
		return ""
	}
	s, _ := e.Payload[key].(string)
	return s
}

// Num lee un número del payload.
//
// Acepta float64 (lo que da encoding/json), int e int64 (lo que dan los tests y
// el código que construye eventos a mano). Sin esta normalización, un evento
// construido en Go y el mismo evento después de un round-trip por JSON darían
// resultados distintos, y el replay dejaría de ser fiel.
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

// json_Number es la interfaz mínima de json.Number, declarada acá para no
// importar encoding/json en el kernel. No es purismo: el test de arquitectura
// (internal/arch_test.go) verifica el grafo de imports del kernel, y cuanto más
// chico es ese grafo, más fuerte es la garantía de pureza.
type json_Number interface {
	Float64() (float64, error)
}
