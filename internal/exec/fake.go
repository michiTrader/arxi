package exec

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/michiTrader/arxi/internal/kernel"
)

// Fake is the executor that `--sim` and every test use instead of calling a
// provider.
//
// It is production code and not test-only code, and that placement is the
// point: `--sim` is a user-facing feature, so if the fake lived in a _test.go
// file the shipped binary could not simulate anything. Users would then be left
// discovering how a blueprint behaves by paying for it.
//
// What the fake does NOT do is decide anything. It answers "the turn happened"
// and lets kernel.Decide draw every conclusion. If the fake started deciding
// that a submit means a stage advances, --sim would be predicting its own
// behaviour rather than the reducer's, and would agree with `run` only by
// coincidence.
type Fake struct {
	mu sync.Mutex

	// Calls records every effect received, in arrival order. Assertions read
	// this instead of counting log events, because an effect that produced no
	// event is exactly the bug worth catching.
	Calls []Call

	// TurnCostUSD is what each simulated turn "costs". Non-zero by default in
	// NewFake: a simulation where everything is free can never reach a budget
	// ceiling, so budget.warning and budget.exceeded would be dead code that
	// nobody exercises until a real run hits them.
	TurnCostUSD float64

	// ToolResults maps tool name to the output to return. A tool with no entry
	// still succeeds with empty output, because the common case in a test is
	// caring that the call happened, not what it printed.
	ToolResults map[string]string

	// FailTools maps tool name to a DOMAIN failure. The result is an event of
	// type tool.call_completed with ok=false, not a Go error: the tool ran and
	// refused, and that is a fact the log must keep.
	FailTools map[string]string

	// BreakTools maps tool name to a TRANSPORT failure, returned as a Go error
	// with no event. This is the different, worse case: nothing can be said
	// about what happened, so nothing is written.
	BreakTools map[string]error

	// BreakTurns maps agent name to a TRANSPORT failure when its turn is opened:
	// the provider was unreachable, rate-limited or returned a 500, so the turn
	// never happened and no event describes it.
	//
	// This is the most ordinary failure a real run meets and it was the one the
	// simulation could not produce. BreakTools covers a tool that could not be
	// reached, but a turn is where the money and the coordination live, and the
	// two fail differently: a broken tool leaves a member mid-turn that can still
	// finish, while a broken turn leaves a member the reducer believes is
	// thinking and that will never emit agent.turn_done. Without a way to
	// simulate it, the loop's promise not to abandon a run over one failed effect
	// was only ever documented, never exercised.
	BreakTurns map[string]error

	// HumanReplies maps inbox kind to an immediate answer. Empty means the
	// question stays open, which is the realistic default: AskHuman normally
	// blocks, and a fake that always answers instantly would make it
	// impossible to test what a run does while waiting.
	HumanReplies map[string]string

	// AskTools maps tool name to the policy that stops it, normally "ask". The
	// call produces tool.call_denied instead of running, which is how a
	// simulation reaches the human-in-the-loop path at all.
	//
	// It is separate from FailTools because a denial is not a failure. A failed
	// tool is a fact the agent reacts to and the run continues; a denied one
	// suspends the member until a person answers, and those two produce entirely
	// different runs. Without this knob the approval scenario the design
	// specifies was unreachable in --sim, so the state a run spends the most
	// wall-clock time in -- waiting for a human -- could only ever be tested by
	// hand-appending the event, which tests the reducer and not the path.
	AskTools map[string]string

	// DenyTurnTool makes a member's simulated turn call a tool before finishing,
	// keyed by agent name. Without it, a simulated turn calls nothing, so a
	// policy=ask denial could never arise DURING a turn -- which is the only
	// moment it arises in a real run, since an agent requests a tool while
	// thinking. Appending the denial from outside instead puts it before the
	// stage that requested it was even entered, a shape no provider produces.
	DenyTurnTool map[string]string

	// Submits decides whether a simulated turn ends by submitting to its stage.
	// True in NewFake, and that default is what makes --sim able to reach the end
	// of a staged blueprint at all.
	//
	// A real agent submits by calling a tool during its turn, so the event exists
	// in a real run and the reducer advances stages on nothing else. A fake that
	// never emitted it left every staged blueprint stuck in stage one: the turns
	// happened, the quorum was never met, and --sim reported a perfectly good
	// blueprint as silent. That is worse than not simulating, because it teaches
	// the user to distrust a correct answer.
	//
	// It is a knob rather than a constant because the run that does NOT submit is
	// what quiescence detection has to catch, and that path needs simulating too.
	// Emitting the event is not a decision: whether a submit advances the stage
	// remains entirely the reducer's call, which is what keeps --sim predicting
	// `run` rather than predicting itself.
	Submits bool

	// SubmitAgents restricts which members submit, when non-empty. A stage whose
	// rule needs everybody and where one member never submits is the realistic
	// shape of a stuck run, and reproducing it is how the diagnosis gets tested.
	SubmitAgents map[string]bool

	// counters tracks occurrences per (scope, kind) rather than one global
	// number. See the comment on id for why the difference matters.
	counters map[string]int
}

// Call is one effect the Fake received.
type Call struct {
	Kind   string // "spawn_turn", "call_tool" or "ask_human"
	Agent  string
	Tool   string
	Detail string
}

// NewFake returns a Fake with defaults chosen so that a simulation exercises
// the interesting paths rather than the empty ones.
func NewFake() *Fake {
	return &Fake{
		TurnCostUSD:  0.01,
		ToolResults:  map[string]string{},
		FailTools:    map[string]string{},
		BreakTools:   map[string]error{},
		HumanReplies: map[string]string{},
		Submits:      true,
		SubmitAgents: map[string]bool{},
		AskTools:     map[string]string{},
		DenyTurnTool: map[string]string{},
		BreakTurns:   map[string]error{},
	}
}

// id returns a deterministic event id, derived from what the effect IS rather
// than from when it happened to be executed.
//
// Deterministic and not a UUID, because ids appear in caused_by chains and
// therefore in the log. With random ids, two --sim runs over identical input
// produce logs that differ on every line, and `run diff` becomes useless
// exactly when it would be most useful. The prefix marks them as simulated so
// nobody mistakes a simulated log for a real one.
//
// The id is built from (scope, kind, occurrence) rather than from one global
// counter, and that is a correction of a real bug rather than a stylistic
// preference. A global counter is deterministic in VALUE but not in
// ASSIGNMENT: the runner executes independent effects in parallel, so
// whichever goroutine reaches the mutex first takes the lower number. Spawning
// turns for `writer` and `editor` in the same step therefore produced
// sim-0001/0002 for the writer on some runs and sim-0003/0004 on others. Every
// number was stable; who received it was a coin flip. Keying by effect content
// instead means the writer's first turn is sim-writer-llm-1 no matter who wins
// the race, so the log a simulation produces depends only on its input.
func (f *Fake) id(scope, kind string) string {
	if f.counters == nil {
		f.counters = map[string]int{}
	}
	key := scope + "/" + kind
	f.counters[key]++
	return fmt.Sprintf("sim-%s-%s-%d", scope, kind, f.counters[key])
}

// record appends to Calls. Callers must hold the mutex.
//
// The mutex exists because the runner executes independent effects in parallel,
// so real concurrent access happens here in normal operation, not only under
// stress. Without it, `go test -race` fails and, worse, Calls silently loses
// entries.
func (f *Fake) record(c Call) {
	f.Calls = append(f.Calls, c)
}

// SpawnTurn simulates an agent turn.
//
// It emits agent.activated, then llm.response carrying the cost, then
// agent.turn_done. That order is the order the real executor must use, and each
// step of it is load-bearing.
//
// agent.activated comes FIRST because it is what marks the member busy. Without
// it the simulated member went straight from idle to idle: State.Turns never
// moved, so --max-turns could never be reached in a simulation; m.Busy() was
// never true, so coalescing (the 5x saving that applyTurnDone exists for) never
// engaged and every queued cause opened its own turn; and a steer arriving
// mid-turn took the not-busy branch instead of queueing. A simulation that
// silently skips the turn lifecycle is worse than no simulation, because it
// predicts a run that cannot happen.
//
// llm.response precedes agent.turn_done because the reducer charges the budget
// on llm.response. A turn_done seen first would let a run look finished and
// under budget on paper while the money was already spent.
func (f *Fake) SpawnTurn(ctx context.Context, e kernel.SpawnTurn) ([]kernel.Event, error) {
	// Cancellation is checked before doing anything, not after, so a cancelled
	// run stops spending instead of paying for turns whose results will be
	// discarded.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("spawn turn for %s: %w", e.Agent, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.record(Call{
		Kind:   "spawn_turn",
		Agent:  e.Agent,
		Detail: fmt.Sprintf("coalesced=%d", e.Coalesced),
	})

	// A provider that could not be reached. Recorded above and then failed here,
	// deliberately in that order: the attempt is a fact worth keeping even though
	// it produced nothing, otherwise a run that failed to reach a provider four
	// times looks identical to one that never tried.
	//
	// No events are returned, which is the honest answer. Emitting agent.activated
	// before failing would leave a member the reducer believes is thinking, with a
	// turn that can never close.
	if err, ok := f.BreakTurns[e.Agent]; ok {
		return nil, fmt.Errorf("spawn turn for %s: %w", e.Agent, err)
	}

	events := []kernel.Event{
		{
			ID:      f.id(e.Agent, "act"),
			Type:    kernel.AgentActivated,
			Source:  kernel.SourceRuntime,
			Actor:   e.Agent,
			Payload: map[string]any{"agent": e.Agent, "simulated": true},
		},
		{
			ID:     f.id(e.Agent, "llm"),
			Type:   kernel.LLMResponse,
			Source: kernel.SourceAgent,
			Actor:  e.Agent,
			// Coalesced travels into the event because it is the audit trail
			// for the 5x billing multiplier that coalescing avoids: without it
			// in the log, nobody can ever show what unified injection saved.
			Payload: map[string]any{
				"agent":     e.Agent,
				"cost_usd":  f.TurnCostUSD,
				"coalesced": e.Coalesced,
				"simulated": true,
			},
		},
	}

	// A tool call during the turn, when the test asked for one. It goes here --
	// after llm.response and before turn_done -- because that is when an agent
	// calls a tool: while it is thinking, having been billed for the reasoning
	// that decided to call it.
	//
	// The denial it produces is what suspends the member mid-turn, and only from
	// inside the turn does that shape match a real run. The turn_done still
	// follows, because the provider closes the turn whether or not the tool was
	// allowed; the reducer is what must keep the member waiting across it.
	if tool, ok := f.DenyTurnTool[e.Agent]; ok {
		if policy, stopped := f.AskTools[tool]; stopped {
			events = append(events, kernel.Event{
				ID:     f.id(e.Agent, "denied-"+tool),
				Type:   kernel.ToolCallDenied,
				Source: kernel.SourceRuntime,
				Actor:  e.Agent,
				Payload: map[string]any{
					"agent": e.Agent, "tool": tool,
					"policy": policy, "simulated": true,
				},
			})
		}
	}

	// The submit goes BEFORE turn_done, because that is the order a real run
	// produces: an agent submits by calling a tool while its turn is open, and the
	// turn ends afterwards. Emitting it after would make the reducer see a member
	// that finished thinking and only then submitted, which is a sequence a
	// provider never generates, so --sim would be exercising a path `run` cannot
	// reach.
	if f.submits(e.Agent) {
		events = append(events, kernel.Event{
			ID:     f.id(e.Agent, "submit"),
			Type:   kernel.StageSubmitted,
			Source: kernel.SourceAgent,
			Actor:  e.Agent,
			Payload: map[string]any{
				"agent": e.Agent, "stage": stageOf(e.Context), "simulated": true,
			},
		})
	}

	return append(events, kernel.Event{
		ID:      f.id(e.Agent, "turn"),
		Type:    kernel.AgentTurnDone,
		Source:  kernel.SourceAgent,
		Actor:   e.Agent,
		Payload: map[string]any{"agent": e.Agent, "simulated": true},
	}), nil
}

// submits reports whether this agent's simulated turn ends in a submit. Called
// with f.mu already held.
func (f *Fake) submits(agent string) bool {
	if !f.Submits {
		return false
	}
	if len(f.SubmitAgents) == 0 {
		return true
	}
	return f.SubmitAgents[agent]
}

// stageOf recovers the stage name from the context the reducer built.
//
// It reads what buildContext wrote ("stage:<name>" in Situation) rather than
// taking a stage from the Fake's own memory. The executor must not hold an
// opinion about which stage the run is in: the reducer decides that, and a fake
// that tracked it separately would drift and then submit to the wrong stage.
func stageOf(cs kernel.ContextSpec) string {
	for _, s := range cs.Situation {
		if name, ok := afterPrefix(s, "stage:"); ok {
			return name
		}
	}
	return ""
}

// afterPrefix returns the remainder after prefix, and whether it was present and
// non-empty.
func afterPrefix(s, prefix string) (string, bool) {
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

// CallTool simulates a tool call.
func (f *Fake) CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("call tool %s: %w", e.Tool, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.record(Call{Kind: "call_tool", Agent: e.Agent, Tool: e.Tool, Detail: argSummary(e.Args)})

	// Transport failure: no event. Checked before the domain failure because
	// "the call never landed" and "the call landed and failed" are different
	// facts, and only the second one belongs in the log.
	if err, ok := f.BreakTools[e.Tool]; ok {
		return nil, fmt.Errorf("call tool %s: %w", e.Tool, err)
	}

	// A policy stop comes before the failure knobs because the tool never ran:
	// deciding it failed would put a result in the log for a call nobody made,
	// and the reducer would react to an outcome instead of suspending the member.
	if policy, ok := f.AskTools[e.Tool]; ok {
		return []kernel.Event{{
			ID:     f.id(e.Agent, "denied-"+e.Tool),
			Type:   kernel.ToolCallDenied,
			Source: kernel.SourceRuntime,
			Actor:  e.Agent,
			Payload: map[string]any{
				"agent": e.Agent, "tool": e.Tool,
				"policy": policy, "simulated": true,
			},
		}}, nil
	}

	if reason, ok := f.FailTools[e.Tool]; ok {
		return []kernel.Event{{
			ID:     f.id(e.Agent, "tool-"+e.Tool),
			Type:   kernel.ToolCallCompleted,
			Source: kernel.SourceRuntime,
			Actor:  e.Agent,
			Payload: map[string]any{
				"agent": e.Agent, "tool": e.Tool,
				"ok": false, "error": reason, "simulated": true,
			},
		}}, nil
	}

	return []kernel.Event{{
		ID:     f.id(e.Agent, "tool-"+e.Tool),
		Type:   kernel.ToolCallCompleted,
		Source: kernel.SourceRuntime,
		Actor:  e.Agent,
		Payload: map[string]any{
			"agent": e.Agent, "tool": e.Tool,
			"ok": true, "output": f.ToolResults[e.Tool], "simulated": true,
		},
	}}, nil
}

// AskHuman simulates creating an inbox item.
//
// It always emits inbox.created, and only emits inbox.replied when a canned
// answer exists. Emitting both unconditionally would mean no test could ever
// observe a run in the waiting state, and the waiting state is precisely what
// `run why` was built to explain.
func (f *Fake) AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ask human (%s): %w", e.Kind, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.record(Call{Kind: "ask_human", Agent: e.Agent, Detail: e.Kind})

	// inbox_id is the reducer's, never the fake's. The answer is matched against
	// this id, so inventing one here would produce a question that looks perfectly
	// normal in the log and cannot be answered by anybody: applyInboxReplied would
	// find no item and the run would stay blocked forever. The realistic executor
	// has the same obligation, which is why the id travels on the effect.
	out := []kernel.Event{{
		ID:     f.id(e.Kind, "inbox"),
		Type:   kernel.InboxCreated,
		Source: kernel.SourceRuntime,
		Actor:  e.Agent,
		Payload: map[string]any{
			"inbox_id": e.ID,
			"kind":     e.Kind, "question": e.Question, "agent": e.Agent,
			"on_timeout": e.OnTimeout, "timeout_ms": e.TimeoutMs,
			"simulated": true,
		},
	}}

	if reply, ok := f.HumanReplies[e.Kind]; ok {
		out = append(out, kernel.Event{
			ID:     f.id(e.Kind, "reply"),
			Type:   kernel.InboxReplied,
			Source: kernel.SourceHuman,
			Actor:  e.Agent,
			Payload: map[string]any{
				"inbox_id": e.ID,
				"kind":     e.Kind, "reply": reply, "simulated": true,
			},
		})
	}
	return out, nil
}

// Kinds returns the Kind of every recorded call, in order. A convenience for
// assertions, so a test reads as one comparison instead of a loop.
func (f *Fake) Kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.Kind
	}
	return out
}

// argSummary renders args deterministically.
//
// Sorted keys, because Go randomizes map iteration on purpose. An unsorted
// rendering would put a value that varies per execution into Call.Detail, and
// a test asserting on it would fail roughly one run in two: the kind of
// flakiness that gets "fixed" by deleting the assertion.
func argSummary(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%v", k, args[k])
	}
	return out
}
