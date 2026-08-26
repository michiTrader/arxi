package kernel

// Config es el blueprint ya resuelto: la copia congelada que el run fijó al
// arrancar (ver State.BlueprintSHA). El reducer la trata como inmutable.
type Config struct {
	Blueprint  string         `json:"blueprint,omitempty"`
	Stages     []StageConfig  `json:"stages,omitempty"`
	Members    []MemberConfig `json:"members,omitempty"`
	Watchers   []Watcher      `json:"watchers,omitempty"`
	Inter      Interaction    `json:"interaction"`
	Workspace  string         `json:"workspace,omitempty"`
	Context    ContextSpec    `json:"context_policy"`
	ResultFrom string         `json:"result_from,omitempty"`

	BudgetWarnPct float64 `json:"budget_warn_pct,omitempty"`
	MaxDepth      int     `json:"max_depth,omitempty"`
}

// Interaction ya solo tiene SteerTarget.
//
// El campo turn_source del primer borrador está RETIRADO (ADR-0006). La idea era
// declarar quién puede abrir turnos para evitar carreras, y la corrección de
// IA B es que eso no resuelve nada: la carrera real es "dos escritores
// modifican el mismo estado" y se resuelve con CAS sobre `seq`
// (if_seq + on_busy), no con una política declarativa sobre quién habla.
type Interaction struct {
	SteerTarget string `json:"steer_target,omitempty"`
}

// StageConfig es una etapa del blueprint.
//
// OnTimeout por defecto es "escalate", no "fail". Un timeout de etapa casi
// nunca significa "el trabajo es imposible", significa "algo se trabó y hay que
// mirarlo". Fallar por defecto entrena al usuario a poner timeouts absurdamente
// largos, que es peor que no tenerlos.
type StageConfig struct {
	Name        string `json:"name"`
	AdvanceWhen string `json:"advance_when,omitempty"`
	TimeoutMs   int64  `json:"timeout_ms,omitempty"`
	OnTimeout   string `json:"on_timeout,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	OnConflict  string `json:"on_conflict,omitempty"`
}

// MemberConfig declara un participante.
//
// Activation por defecto es "coalesce": si llegan cinco razones para despertar
// al mismo agente, se abre un turno con las cinco. La alternativa ("cada
// evento, un turno") multiplica la factura por cinco a cambio de nada.
type MemberConfig struct {
	Name       string   `json:"name"`
	Role       string   `json:"role,omitempty"`
	Advisory   bool     `json:"advisory,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	Activation string   `json:"activation,omitempty"`
	Stages     []string `json:"stages,omitempty"`
}

// Watcher es una reacción declarada a un patrón de evento.
//
// IncludeSelf por defecto es false y eso es una decisión de seguridad, no de
// comodidad: un watcher sobre `agent.*` que se despierta con sus propios
// eventos es un bucle infinito que se cobra en dólares. La auto-exclusión y el
// límite de profundidad son los dos filtros baratos que corren ANTES de gastar
// un solo token.
type Watcher struct {
	Agent       string `json:"agent"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action,omitempty"`
	Tool        string `json:"tool,omitempty"`
	IncludeSelf bool   `json:"include_self,omitempty"`
}

// ContextSpec describe qué contexto se le arma a un turno.
//
// El ORDEN de las capas importa y no es alfabético: identity → situation →
// memory → shared → cause. Va de lo más estable a lo más volátil para que el
// prefix cache del proveedor pegue en las primeras capas. Invertir el orden
// funciona igual y cuesta varias veces más.
type ContextSpec struct {
	Identity   string   `json:"identity,omitempty"`
	Situation  []string `json:"situation,omitempty"`
	Memory     string   `json:"memory,omitempty"`
	Shared     []string `json:"shared,omitempty"`
	Cause      []string `json:"cause,omitempty"`
	MaxTokens  int      `json:"max_tokens,omitempty"`
	OnOverflow string   `json:"on_overflow,omitempty"`
}

// ResolveDefaults completa los valores por defecto. Es una función pura sobre la
// Config y se corre una vez al fijar el blueprint, no en cada Decide: si los
// defaults se aplicaran en cada paso, cambiar un default en una versión nueva
// del binario cambiaría el resultado de un replay viejo.
func (c Config) ResolveDefaults() Config {
	out := c

	if out.Inter.SteerTarget == "" {
		out.Inter.SteerTarget = "coordinator"
	}
	if out.BudgetWarnPct == 0 {
		out.BudgetWarnPct = 0.8
	}
	if out.MaxDepth == 0 {
		out.MaxDepth = 12
	}
	if out.ResultFrom == "" {
		out.ResultFrom = "last_submit"
	}

	// EL AGUJERO FATAL: si algún miembro puede escribir archivos y comparten un
	// solo directorio, dos agentes van a pisarse mutuamente y el lock del KV
	// store no lo va a impedir. El default seguro es worktree, no shared.
	if out.Workspace == "" {
		out.Workspace = "none"
		for _, m := range out.Members {
			for _, t := range m.Tools {
				if t == "write" || t == "bash" || t == "edit" {
					out.Workspace = "worktree"
				}
			}
		}
	}

	if out.Stages != nil {
		out.Stages = append([]StageConfig(nil), out.Stages...)
		for i := range out.Stages {
			if out.Stages[i].AdvanceWhen == "" {
				out.Stages[i].AdvanceWhen = "all"
			}
			if out.Stages[i].OnTimeout == "" {
				out.Stages[i].OnTimeout = "escalate"
			}
		}
	}
	if out.Members != nil {
		out.Members = append([]MemberConfig(nil), out.Members...)
		for i := range out.Members {
			if out.Members[i].Activation == "" {
				out.Members[i].Activation = "coalesce"
			}
		}
	}
	return out
}

// StageAt devuelve la etapa en el índice dado, o nil si está fuera de rango.
func (c Config) StageAt(i int) *StageConfig {
	if i < 0 || i >= len(c.Stages) {
		return nil
	}
	return &c.Stages[i]
}

// MemberCfg busca la declaración de un miembro por nombre.
func (c Config) MemberCfg(name string) *MemberConfig {
	for i := range c.Members {
		if c.Members[i].Name == name {
			return &c.Members[i]
		}
	}
	return nil
}
