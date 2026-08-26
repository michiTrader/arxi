package kernel

// RunStatus es el estado del run entero.
//
// Nótese que "quiescent" NO está acá. Que el sistema se haya quedado callado no
// es un estado terminal, es un evento (run.quiescent) que despierta al
// coordinador con un diagnóstico. Convertirlo en estado terminal fue la
// tentación obvia y sería el peor error de diseño posible: el modo de falla más
// común de estos sistemas es quedarse mudo, y la respuesta correcta es
// intervenir, no morir.
type RunStatus string

const (
	StatusQueued    RunStatus = "queued"
	StatusRunning   RunStatus = "running"
	StatusBlocked   RunStatus = "blocked"
	StatusPaused    RunStatus = "paused"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
	StatusCancelled RunStatus = "cancelled"
	StatusExpired   RunStatus = "expired"
)

// Terminal indica si el run ya no acepta eventos que cambien su estado. Un
// evento que llega tarde a un run terminal no es un error: se registra y se
// ignora. Pasa siempre (un tool lento que responde después del cancel) y hacer
// que sea un error haría fallar replays perfectamente válidos.
func (s RunStatus) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// MemberState es el estado de un miembro dentro del run.
type MemberState string

const (
	MemberIdle      MemberState = "idle"
	MemberThinking  MemberState = "thinking"
	MemberTool      MemberState = "tool"
	MemberSubmitted MemberState = "submitted"
	MemberWaiting   MemberState = "waiting"
	MemberInactive  MemberState = "inactive"
	MemberFailed    MemberState = "failed"
)

// Member es un participante del run.
//
// Advisory es un rasgo genérico del rol, no un flag de una feature. Reemplaza
// al --counts-toward-advance del primer borrador: en vez de un booleano que
// solo sirve para las etapas, un miembro advisory es "alguien que opina pero no
// decide", y cada blueprint interpreta esa propiedad como corresponda (no
// cuenta para el quórum, no bloquea el avance, puede seguir hablando).
type Member struct {
	Name      string         `json:"name"`
	Role      string         `json:"role,omitempty"`
	Advisory  bool           `json:"advisory,omitempty"`
	State     MemberState    `json:"state"`
	Detail    string         `json:"detail,omitempty"`
	BlockedOn map[string]any `json:"blocked_on,omitempty"`
	SinceSeq  int64          `json:"since_seq"`
	SpentUSD  float64        `json:"spent_usd"`
	Turns     int            `json:"turns"`
	Submitted bool           `json:"submitted,omitempty"`

	// PendingCauses son eventos que llegaron mientras el miembro estaba
	// ocupado. No se pierden ni abren un turno nuevo: se acumulan y se drenan
	// todos juntos en el próximo turno. Esto ES `on_busy: queue`, y es la misma
	// máquina que implementa el follow-up.
	PendingCauses []string `json:"pending_causes,omitempty"`
}

// Runnable dice si vale la pena abrir un turno para este miembro.
//
// SUBTILEZA CARA: solo MemberIdle. La tentación es incluir MemberSubmitted
// ("ya contestó, podría contestar otra vez"), y eso rompe la detección de
// quiescencia de la peor manera: en un run por etapas donde todos hicieron
// submit pero la regla de avance no se cumple, el sistema se ve eternamente
// sano (hay miembros "runnable") cuando en realidad está trabado para siempre.
// Ese es exactamente el caso que run.quiescent existe para atrapar.
func (m Member) Runnable() bool { return m.State == MemberIdle }

// Busy dice si el miembro está gastando dinero ahora mismo.
func (m Member) Busy() bool { return m.State == MemberThinking || m.State == MemberTool }

// InboxItem es una pregunta pendiente a un humano.
type InboxItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Question  string `json:"question"`
	Agent     string `json:"agent,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty"`
	Replied   bool   `json:"replied,omitempty"`
}

// Lock es un lock cooperativo sobre una clave. Es cooperativo y no del sistema
// de archivos a propósito: sirve para coordinar intención entre agentes. El
// aislamiento real del filesystem lo da el workspace (ver Config.Workspace),
// porque un lock en el KV store no impide que dos procesos escriban el mismo
// archivo.
type Lock struct {
	Key    string `json:"key"`
	Holder string `json:"holder"`
}

// State es el estado completo del run, derivado del log. Nunca es la fuente de
// verdad: State = fold(Decide, State0, events). Si el snapshot y el log no
// coinciden, gana el log.
type State struct {
	RunID  string    `json:"run_id"`
	Actor  string    `json:"actor"`
	Status RunStatus `json:"status"`
	Seq    int64     `json:"seq"`

	// BlueprintSHA fija la copia resuelta del blueprint que usa este run.
	// Resuelve el segundo hueco de IA A: el reducer nunca lee el archivo vivo,
	// lee la copia congelada en runs/<id>/blueprint.snapshot.yaml. Sin esto, un
	// replay de la semana pasada usaría la config de hoy y daría otro
	// resultado, que es lo mismo que no tener replay.
	BlueprintSHA string `json:"blueprint_sha,omitempty"`

	Stage      string `json:"stage,omitempty"`
	StageIndex int    `json:"stage_index"`

	Members []Member `json:"members,omitempty"`

	SpentUSD  float64 `json:"spent_usd"`
	BudgetUSD float64 `json:"budget_usd"`
	Turns     int     `json:"turns"`
	MaxTurns  int     `json:"max_turns,omitempty"`

	// TreeSpentUSD es el gasto del subárbol completo. El presupuesto es una
	// sub-pool: un hijo consume del padre. Sin esto, N niveles de spawn
	// multiplican el costo por N y el --budget del run raíz es decorativo.
	TreeSpentUSD float64 `json:"tree_spent_usd"`
	ParentRunID  string  `json:"parent_run_id,omitempty"`
	SpawnDepth   int     `json:"spawn_depth"`

	Locks []Lock      `json:"locks,omitempty"`
	Inbox []InboxItem `json:"inbox,omitempty"`

	ActiveTimer string `json:"active_timer,omitempty"`
	Result      string `json:"result,omitempty"`

	// BudgetWarned y QuiescentEmitted evitan repetir avisos. Están en el estado
	// y no en el ejecutor porque el fold tiene que ser reproducible: si el aviso
	// dependiera de memoria del proceso, un replay emitiría avisos distintos.
	BudgetWarned     bool `json:"budget_warned,omitempty"`
	QuiescentEmitted bool `json:"quiescent_emitted,omitempty"`

	NextInboxID int `json:"next_inbox_id"`
}

// Member busca un miembro por nombre y devuelve un puntero para modificarlo.
func (s *State) Member(name string) *Member {
	for i := range s.Members {
		if s.Members[i].Name == name {
			return &s.Members[i]
		}
	}
	return nil
}

// Clone hace una copia profunda. Decide clona antes de tocar nada: la firma
// dice (State, Event) -> State y si mutara la entrada, el fold dejaría de ser
// reproducible y los tests de "no muta la entrada" existirían para nada.
func (s State) Clone() State {
	out := s
	if s.Members != nil {
		out.Members = make([]Member, len(s.Members))
		for i, m := range s.Members {
			out.Members[i] = m
			if m.BlockedOn != nil {
				out.Members[i].BlockedOn = make(map[string]any, len(m.BlockedOn))
				for k, v := range m.BlockedOn {
					out.Members[i].BlockedOn[k] = v
				}
			}
			if m.PendingCauses != nil {
				out.Members[i].PendingCauses = append([]string(nil), m.PendingCauses...)
			}
		}
	}
	if s.Locks != nil {
		out.Locks = append([]Lock(nil), s.Locks...)
	}
	if s.Inbox != nil {
		out.Inbox = append([]InboxItem(nil), s.Inbox...)
	}
	return out
}
