package kernel

// Effect is something the reducer wants to happen in the real world. The
// reducer does not do it: it describes it. The executor carries it out and
// returns the result as a new event.
//
// The interface has an unexported method (isEffect) so that it is sealed: no
// package outside kernel can add variants. This is Go's way of approximating a
// Rust enum, and the reason the approximation is acceptable is in ADR-0007:
// what Go does not give us in the compiler we give ourselves with
// TestEffectExhaustive, which fails if someone adds a variant and forgets to
// register it.
type Effect interface {
	isEffect()
	Class() EffectClass
}

// EffectClass resolves the gap AI A flagged: if Decide returns []Effect, does
// the executor run them in order or in parallel?
//
// The answer cannot be "all in parallel" because Emit and SetTimer change what
// the rest of the system is going to see, nor "all in order" because then three
// agent turns that know nothing about each other get serialized and we lose the
// whole reason this tool exists.
//
// So they split into two classes: control first and in list order, independent
// afterwards and in parallel. The reducer orders the list before returning it
// (see orderEffects), so the executor only needs this rule: run the control
// prefix sequentially, then the rest concurrently.
type EffectClass int

const (
	// ClassControl changes the observable state of the run or the clock. These
	// execute in the exact order of the list, one after another.
	ClassControl EffectClass = iota
	// ClassIndependent are effects that only affect themselves. They can be
	// executed in parallel with each other.
	ClassIndependent
)

// SpawnTurn opens an agent turn. This is the expensive effect: each one is a
// call to a provider with real money on the line.
//
// Coalesced says how many causes were merged into this turn. If three events
// woke the same agent while it was busy, ONE turn is opened with the three
// causes in the context, not three turns. That number appears in the event so
// that how much the coalescing saved can be audited.
type SpawnTurn struct {
	Agent       string
	Context     ContextSpec
	CauseEvents []string
	BudgetSlice float64
	Coalesced   int
}

func (SpawnTurn) isEffect()          {}
func (SpawnTurn) Class() EffectClass { return ClassIndependent }

// CallTool executes a tool. Idempotent comes from the surface declaration, not
// from a heuristic: if the executor crashes after sending the call and before
// writing the result, it can only retry without asking those that are declared
// idempotent.
type CallTool struct {
	Agent      string
	Tool       string
	Args       map[string]any
	Idempotent bool
}

func (CallTool) isEffect()          {}
func (CallTool) Class() EffectClass { return ClassIndependent }

// Emit asks to write a derived event to the log. It is control because the
// event it writes changes the state that the following effects are going to
// see.
//
// The event's Seq goes as 0: the reducer does not assign sequence numbers
// because it is not the single writer of the log. The executor sets the seq
// when writing.
type Emit struct{ Event Event }

func (Emit) isEffect()          {}
func (Emit) Class() EffectClass { return ClassControl }

// SetTimer arms a timer. FiresAtMs is a relative offset in milliseconds, not an
// absolute timestamp, precisely so that the virtual clock of --sim can run the
// same fold without waiting half an hour for real.
type SetTimer struct {
	ID        string
	FiresAtMs int64
}

func (SetTimer) isEffect()          {}
func (SetTimer) Class() EffectClass { return ClassControl }

// CancelTimer disarms a timer. It is control: if it ran in parallel with the
// Emit that makes it unnecessary, there would be a race between "the stage
// advanced" and "the stage expired", and the run would end in two different
// ways depending on luck.
type CancelTimer struct{ ID string }

func (CancelTimer) isEffect()          {}
func (CancelTimer) Class() EffectClass { return ClassControl }

// AskHuman creates an inbox item and waits. OnTimeout is what has to happen if
// nobody answers: deciding it here and not at timeout time is mandatory,
// because at timeout time there is nobody left watching.
type AskHuman struct {
	Kind      string
	Question  string
	Agent     string
	OnTimeout string
	TimeoutMs int64
}

func (AskHuman) isEffect()          {}
func (AskHuman) Class() EffectClass { return ClassIndependent }

// Snapshot materializes the state at the given seq so that `run show` does not
// have to replay the entire log. It is control because it must look consistent
// with the events already emitted in this same step.
type Snapshot struct{ AtSeq int64 }

func (Snapshot) isEffect()          {}
func (Snapshot) Class() EffectClass { return ClassControl }

// allEffectVariants is the registry that makes the exhaustiveness test
// possible. If you add an Effect variant and do not add it here,
// TestEffectExhaustive fails. It is the mechanical replacement for the
// exhaustive `match` that Rust gives for free.
var allEffectVariants = []Effect{
	SpawnTurn{},
	CallTool{},
	Emit{},
	SetTimer{},
	CancelTimer{},
	AskHuman{},
	Snapshot{},
}

// EffectVariants returns a copy of the variant registry. A copy and not the
// slice directly so that one test cannot corrupt another test's registry.
func EffectVariants() []Effect {
	out := make([]Effect, len(allEffectVariants))
	copy(out, allEffectVariants)
	return out
}
