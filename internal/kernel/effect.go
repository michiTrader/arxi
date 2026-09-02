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
//
// Provenance is on the interface for the same reason Class is: a variant that
// forgets to answer does not compile. The alternative was a type switch in exec,
// and a type switch that misses a new variant does not fail loudly -- it writes
// that variant's events with no cause at all, which is the exact defect Cause
// exists to fix.
type Effect interface {
	isEffect()
	Class() EffectClass
	Provenance() Cause
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

// Cause is the provenance an effect hands to the events it produces: who caused
// them, which causal thread they belong to, and how deep in that thread they sit.
//
// It exists because the executor cannot work this out for itself. Runner.Run is
// handed a []Effect and nothing else, so by the time a turn's four events come
// back from a provider, the event that opened the turn is not in scope and never
// was. What that cost was measurable rather than theoretical: `arxi event trace`
// over a real run printed a chain of five events out of twenty-one, because the
// sixteen the executor minted arrived with caused_by empty, correlation_id empty
// and depth 0 -- and depth 0 also meant each of them cleared the depth brake in
// wakeWatchers as if it were a root cause.
//
// The reducer is the only place that holds both the effect and the event that
// decided it, so the reducer resolves the triple and the executor copies it.
// That is the argument AskHuman.ID already makes about inbox ids, and for the
// same reason: a field the reducer owns cannot come out differently on a second
// fold of the same log.
//
// The triple is RESOLVED, not raw. Depth is already the produced event's depth,
// and CorrelationID has already fallen back to the cause's own id when the cause
// carried none. No reader re-derives the rule, so no reader can derive it
// differently: causeOf (decide.go) computes it once, and derived() -- the other
// thing in the tree that writes these three fields -- goes through the same
// function.
type Cause struct {
	// Events are the direct parents, and become the event's caused_by. Usually
	// one, the event being decided. More than one when a turn was coalesced,
	// where the reasons queued while the agent was busy join the event that
	// finally freed it.
	Events []string

	// CorrelationID names the whole causal thread these events belong to.
	CorrelationID string

	// Depth is the causal depth the produced events carry. It is what makes
	// Config.MaxDepth enforceable on anything the executor writes, rather than
	// only on what the reducer emits.
	Depth int
}

// Apply writes the triple onto an event that does not already carry one.
//
// A non-empty CausedBy is left alone, on the same rule Runner.stamp follows for
// an id it did not mint: a producer that named its own parents knows something
// this Cause does not. It also makes Apply idempotent and makes the zero Cause
// harmless, which matters because an event built by derived() arrives with the
// triple already filled and blanking it would undo the reducer's own work.
func (c Cause) Apply(e *Event) {
	if len(e.CausedBy) > 0 {
		return
	}
	e.CausedBy = c.Events
	e.CorrelationID = c.CorrelationID
	e.Depth = c.Depth
}

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
	Cause       Cause
	BudgetSlice float64
	Coalesced   int
}

func (SpawnTurn) isEffect()           {}
func (SpawnTurn) Class() EffectClass  { return ClassIndependent }
func (s SpawnTurn) Provenance() Cause { return s.Cause }

// CallTool executes a tool. Idempotent comes from the surface declaration, not
// from a heuristic: if the executor crashes after sending the call and before
// writing the result, it can only retry without asking those that are declared
// idempotent.
type CallTool struct {
	Agent      string
	Tool       string
	Args       map[string]any
	Idempotent bool
	Cause      Cause
}

func (CallTool) isEffect()           {}
func (CallTool) Class() EffectClass  { return ClassIndependent }
func (t CallTool) Provenance() Cause { return t.Cause }

// Emit asks to write a derived event to the log. It is control because the
// event it writes changes the state that the following effects are going to
// see.
//
// The event's Seq goes as 0: the reducer does not assign sequence numbers
// because it is not the single writer of the log. The executor sets the seq
// when writing.
//
// Its Provenance is the zero Cause, and that is not an omission: the event it
// carries was built by derived(), which already wrote the triple onto it. There
// is nothing left for the executor to fill in.
type Emit struct{ Event Event }

func (Emit) isEffect()          {}
func (Emit) Class() EffectClass { return ClassControl }
func (Emit) Provenance() Cause  { return Cause{} }

// SetTimer arms a timer. FiresAtMs is a relative offset in milliseconds, not an
// absolute timestamp, precisely so that the virtual clock of --sim can run the
// same fold without waiting half an hour for real.
//
// Its Provenance is the zero Cause, and here that IS a decision worth naming:
// the timer.tick this arms is written much later, by Loop.appendTicks, out of a
// clock that does not remember who asked. Carrying the cause across would mean
// holding a timer-to-cause map in the Runner, and that map is not in the log --
// so a resumed run would write uncaused ticks where a fresh one wrote caused
// ones, and run, --sim, resume and replay would stop being one fold over the
// same bytes. A tick is a root cause. See the note in Loop.appendTicks.
type SetTimer struct {
	ID        string
	FiresAtMs int64
}

func (SetTimer) isEffect()          {}
func (SetTimer) Class() EffectClass { return ClassControl }
func (SetTimer) Provenance() Cause  { return Cause{} }

// CancelTimer disarms a timer. It is control: if it ran in parallel with the
// Emit that makes it unnecessary, there would be a race between "the stage
// advanced" and "the stage expired", and the run would end in two different
// ways depending on luck.
type CancelTimer struct{ ID string }

func (CancelTimer) isEffect()          {}
func (CancelTimer) Class() EffectClass { return ClassControl }
func (CancelTimer) Provenance() Cause  { return Cause{} }

// AskHuman creates an inbox item and waits. OnTimeout is what has to happen if
// nobody answers: deciding it here and not at timeout time is mandatory,
// because at timeout time there is nobody left watching.
type AskHuman struct {
	// ID is the inbox id the answer will be matched against, and it is minted by
	// the reducer rather than by whoever executes the question.
	//
	// Without it the executor invents its own id, so inbox.replied carries a name
	// the reducer never issued and applyInboxReplied matches nothing: the run
	// stays blocked no matter who answers. That is worst exactly where it matters
	// most, on budget.exceeded, whose entire promise is that a human can pay for
	// more instead of losing the work. A question nobody can answer is a failure
	// wearing the costume of a choice.
	ID        string
	Kind      string
	Question  string
	Agent     string
	OnTimeout string
	TimeoutMs int64
	Cause     Cause
}

func (AskHuman) isEffect()           {}
func (AskHuman) Class() EffectClass  { return ClassIndependent }
func (a AskHuman) Provenance() Cause { return a.Cause }

// Snapshot materializes the state at the given seq so that `run show` does not
// have to replay the entire log. It is control because it must look consistent
// with the events already emitted in this same step.
type Snapshot struct{ AtSeq int64 }

func (Snapshot) isEffect()          {}
func (Snapshot) Class() EffectClass { return ClassControl }
func (Snapshot) Provenance() Cause  { return Cause{} }

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
