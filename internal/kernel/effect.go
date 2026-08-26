package kernel

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Effect interface {
	isEffect()
	Class() EffectClass
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type EffectClass int

const (
	// Implementation note.
	// Implementation note.
	ClassControl EffectClass = iota
	// Implementation note.
	// Implementation note.
	ClassIndependent
)

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type SpawnTurn struct {
	Agent       string
	Context     ContextSpec
	CauseEvents []string
	BudgetSlice float64
	Coalesced   int
}

func (SpawnTurn) isEffect()          {}
func (SpawnTurn) Class() EffectClass { return ClassIndependent }

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type CallTool struct {
	Agent      string
	Tool       string
	Args       map[string]any
	Idempotent bool
}

func (CallTool) isEffect()          {}
func (CallTool) Class() EffectClass { return ClassIndependent }

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Emit struct{ Event Event }

func (Emit) isEffect()          {}
func (Emit) Class() EffectClass { return ClassControl }

// Implementation note.
// Implementation note.
// Implementation note.
type SetTimer struct {
	ID        string
	FiresAtMs int64
}

func (SetTimer) isEffect()          {}
func (SetTimer) Class() EffectClass { return ClassControl }

// Implementation note.
// Implementation note.
// Implementation note.
type CancelTimer struct{ ID string }

func (CancelTimer) isEffect()          {}
func (CancelTimer) Class() EffectClass { return ClassControl }

// Implementation note.
// Implementation note.
// Implementation note.
type AskHuman struct {
	Kind      string
	Question  string
	Agent     string
	OnTimeout string
	TimeoutMs int64
}

func (AskHuman) isEffect()          {}
func (AskHuman) Class() EffectClass { return ClassIndependent }

// Implementation note.
// Implementation note.
// Implementation note.
type Snapshot struct{ AtSeq int64 }

func (Snapshot) isEffect()          {}
func (Snapshot) Class() EffectClass { return ClassControl }

// Implementation note.
// Implementation note.
// Implementation note.
var allEffectVariants = []Effect{
	SpawnTurn{},
	CallTool{},
	Emit{},
	SetTimer{},
	CancelTimer{},
	AskHuman{},
	Snapshot{},
}

// Implementation note.
// Implementation note.
func EffectVariants() []Effect {
	out := make([]Effect, len(allEffectVariants))
	copy(out, allEffectVariants)
	return out
}
