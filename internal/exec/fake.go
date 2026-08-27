package exec

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/michiTrader/iash/internal/kernel"
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

	// HumanReplies maps inbox kind to an immediate answer. Empty means the
	// question stays open, which is the realistic default: AskHuman normally
	// blocks, and a fake that always answers instantly would make it
	// impossible to test what a run does while waiting.
	HumanReplies map[string]string

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
// It emits llm.response carrying the cost and agent.turn_done, in that order,
// because that is the order the real executor must use: the reducer charges the
// budget on llm.response, and a turn_done seen first would let a run finish
// under budget on paper while having already spent the money.
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

	return []kernel.Event{
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
		{
			ID:      f.id(e.Agent, "turn"),
			Type:    kernel.AgentTurnDone,
			Source:  kernel.SourceAgent,
			Actor:   e.Agent,
			Payload: map[string]any{"agent": e.Agent, "simulated": true},
		},
	}, nil
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

	out := []kernel.Event{{
		ID:     f.id(e.Kind, "inbox"),
		Type:   kernel.InboxCreated,
		Source: kernel.SourceRuntime,
		Actor:  e.Agent,
		Payload: map[string]any{
			"kind": e.Kind, "question": e.Question, "agent": e.Agent,
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
				"kind": e.Kind, "reply": reply, "simulated": true,
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
