package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
)

// asAPIError reports whether err is a provider refusal, and binds it.
//
// errors.As and not a type assertion, because Complete wraps: a refusal that
// travelled through one fmt.Errorf would fail a bare assertion, and the failure
// would silently reclassify a DOMAIN fact as a TRANSPORT error. The turn would
// then vanish from the log instead of being recorded as refused, which is the
// exact confusion the Executor contract exists to prevent.
func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

// Resolver answers "which endpoint, which credential, which model id" for a ref.
//
// Declared here as an interface rather than taking a *modelstore.Store, for the
// reason internal/exec declares exec.Log: it keeps this package testable without
// a providers directory on disk, and it keeps file layout out of the run path.
// modelstore.Store satisfies it; so does a map in a test.
type Resolver interface {
	Resolve(ref string) (model.Resolution, error)
}

// ToolRunner does the work a permitted tool describes.
//
// Declared here for the same reason as Resolver, and with a sharper motive: the
// implementation (internal/toolrun) starts child processes and writes to disk,
// and importing it directly would put the package that holds provider API keys
// in the same dependency closure as the code that runs `bash`. Keeping the
// boundary an interface is what lets those two stay apart.
//
// It takes the member name rather than a pre-opened workspace, because WHICH
// workspace is a per-member decision (`workspace: worktree` gives each member
// its own) and the wiring site owns that mapping. An executor that held one
// workspace would silently give every member the same one, which is the exact
// overwrite the worktree default exists to prevent.
type ToolRunner interface {
	// RunTool performs name with args for member, and reports what happened.
	//
	// The result is a string because that is what becomes the `result` field of
	// tool.call_completed, and therefore what the next turn reads. Returning a
	// struct would tempt a caller to record fields the spec does not carry.
	//
	// An error means the RUNNER failed, not that the tool reported failure. A
	// command exiting non-zero is a successful run with a bad outcome, and the
	// two must not arrive by the same channel: one needs a fix to arxi, the
	// other is the answer the agent asked for.
	RunTool(ctx context.Context, member, name string, args map[string]any) (string, error)
}

// Executor is the live executor. It satisfies exec.Executor structurally.
//
// SpawnTurn calls a model. CallTool and AskHuman deliberately do not do their
// real work yet, and they say so rather than pretending -- see their comments.
// Shipping a CallTool that silently succeeded would be worse than one that
// refuses: the reducer would advance a stage on the strength of a tool result
// that never happened.
type Executor struct {
	// Resolver maps a member's model ref to an endpoint and a key variable.
	Resolver Resolver

	// DefaultModel is used by a member that names no model of its own.
	// MemberConfig.Model empty means "the run decides", and this is that
	// decision, taken explicitly at the wiring site by `--model` rather than
	// invented in here.
	DefaultModel string

	// Members is the frozen roster, so a turn can find the model its member
	// declared. Taken from kernel.Config at wiring time.
	Members []kernel.MemberConfig

	// ToolPolicy holds per-agent policy overrides, keyed by agent then tool.
	//
	// Nil is the normal case and means "the defaults decide", which is the
	// safe direction: no entry resolves to deny for an ungranted tool and ask
	// for a granted mutating one. Entries exist because `arxi agent tool
	// policy --agent backend --allow bash` is a written decision, and the
	// remedy `run why` prints has to actually change something.
	ToolPolicy map[string]map[string]surface.Policy

	// Tools performs the tools policy has allowed. Nil means this build has no
	// runner, and CallTool then refuses instead of pretending -- a faked result
	// would advance the stage on work nobody did.
	//
	// Optional on purpose. `--sim` runs must not carry a real runner, and a
	// field that had to be filled would force every test to supply one.
	Tools ToolRunner

	// Prompt is the run's opening instruction. It joins the volatile half of
	// every context, because it is what the run was started to do.
	Prompt string

	// NewClient builds the caller for a resolution. Injectable so tests point at
	// an httptest server without reaching the network.
	NewClient func(model.Resolution) *Client

	// Temperature, when set, is sent on every call.
	Temperature *float64

	// mu guards ids. The runner executes independent effects in PARALLEL, so
	// two turns can mint an id at the same moment.
	mu  sync.Mutex
	ids map[string]int
}

// SpawnTurn calls the model and reports what it cost.
//
// The event order is copied from exec.Fake and every step of it is load-bearing
// for the same reasons named there:
//
//   - agent.activated FIRST, because it is what marks the member busy. Without
//     it State.Turns never moves, so --max-turns is unreachable, coalescing never
//     engages, and a steer arriving mid-turn takes the wrong branch.
//   - llm.response BEFORE agent.turn_done, because the reducer charges the
//     budget on llm.response (decide.go applyCost). A turn_done seen first would
//     let a run look finished and under budget while the money was already spent.
//
// The failure split is the contract in exec.Executor:
//
//   - Could not turn the attempt into a fact (no connection, a timeout, an
//     unparseable body): return an error and NO events. Emitting agent.activated
//     and then failing would leave a member the reducer believes is thinking,
//     holding a turn that can never close.
//   - The provider answered and refused: that is a fact. It comes back as
//     events, with the cost the provider reported for it, because a prompt that
//     was charged for and then declined still costs money.
func (x *Executor) SpawnTurn(ctx context.Context, e kernel.SpawnTurn) ([]kernel.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("spawn turn for %s: %w", e.Agent, err)
	}

	// Resolution happens before anything is emitted, so a misconfigured model
	// costs nothing and produces no half-open turn.
	res, price, err := x.resolve(e.Agent)
	if err != nil {
		return nil, fmt.Errorf("spawn turn for %s: %w", e.Agent, err)
	}

	client := x.newClient(res)
	req := chatRequest{
		Model:       res.Model,
		Messages:    buildMessages(e.Context, x.Prompt),
		MaxTokens:   maxTokensFor(e.Context),
		Temperature: x.Temperature,
	}

	resp, callErr := client.Complete(ctx, req)

	// A transport failure: no events. See the contract above.
	var apiErr *APIError
	if callErr != nil && !asAPIError(callErr, &apiErr) {
		return nil, fmt.Errorf("spawn turn for %s: %w", e.Agent, callErr)
	}

	// From here the provider answered, so a turn happened and the log must show
	// it -- whether the answer was a completion or a refusal.
	in, out := 0, 0
	if resp != nil {
		in, out = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}
	cost := price.Cost(in, out)

	events := []kernel.Event{{
		ID:      x.id(e.Agent, "act"),
		Type:    kernel.AgentActivated,
		Source:  kernel.SourceRuntime,
		Actor:   e.Agent,
		Payload: map[string]any{"agent": e.Agent},
	}}

	llm := map[string]any{
		"agent":     e.Agent,
		"cost_usd":  cost,
		"coalesced": e.Coalesced,
		"model":     res.Provider + "/" + res.Model,
		"in_tokens": in,
		// The token counts travel into the log next to the dollars they produced.
		// A cost with no counts cannot be audited: nobody can tell an expensive
		// model from a long prompt, and those have opposite fixes.
		"out_tokens": out,
	}
	if apiErr != nil {
		// The refusal is recorded as an outcome of the turn, with its status, so
		// `run why` can distinguish a rate limit from a bad key.
		llm["ok"] = false
		llm["error"] = apiErr.Message
		llm["status"] = apiErr.Status
		llm["retryable"] = apiErr.Retryable()
	} else {
		llm["ok"] = true
		if text, present := resp.text(); present {
			llm["text"] = text
		}
		// finish_reason is recorded because "length" is not success: the reply
		// was cut off at max_tokens, and a truncated answer is indistinguishable
		// from a complete one unless the log says so.
		if fr := resp.finishReason(); fr != "" {
			llm["finish_reason"] = fr
		}
	}

	events = append(events, kernel.Event{
		ID:      x.id(e.Agent, "llm"),
		Type:    kernel.LLMResponse,
		Source:  kernel.SourceAgent,
		Actor:   e.Agent,
		Payload: llm,
	})

	// turn_done is emitted even for a refusal, because the turn is over either
	// way and the member must not be left thinking. The reducer decides what a
	// failed turn means; the executor's job is to report what happened.
	events = append(events, kernel.Event{
		ID:      x.id(e.Agent, "turn"),
		Type:    kernel.AgentTurnDone,
		Source:  kernel.SourceAgent,
		Actor:   e.Agent,
		Payload: map[string]any{"agent": e.Agent},
	})
	return events, nil
}

// resolve finds the endpoint and the price for a member's model.
//
// An unpriced model is REFUSED rather than charged zero. Charging zero would
// leave --budget enforced against a total that never moves, so the ceiling would
// never be reached: the safety mechanism would be off while appearing to be on.
func (x *Executor) resolve(agent string) (model.Resolution, model.Price, error) {
	ref := x.DefaultModel
	for _, m := range x.Members {
		if m.Name == agent && m.Model != "" {
			ref = m.Model
			break
		}
	}
	if ref == "" {
		return model.Resolution{}, model.Price{}, fmt.Errorf(
			"member %s names no model and the run has no default.\n"+
				"  fix: give the member a `model:` in the blueprint, or start the run "+
				"with --model", agent)
	}
	if x.Resolver == nil {
		return model.Resolution{}, model.Price{}, fmt.Errorf(
			"no resolver is wired, so %q cannot be turned into an endpoint", ref)
	}
	res, err := x.Resolver.Resolve(ref)
	if err != nil {
		return model.Resolution{}, model.Price{}, err
	}
	price, ok := model.PriceOf(res.Model)
	if !ok {
		return model.Resolution{}, model.Price{}, &model.ErrNoPrice{Ref: res.Model}
	}
	return res, price, nil
}

func (x *Executor) newClient(res model.Resolution) *Client {
	if x.NewClient != nil {
		return x.NewClient(res)
	}
	return &Client{BaseURL: res.BaseURL, APIKeyEnv: res.APIKeyEnv}
}

// id mints an event id.
//
// Keyed by scope and kind rather than by one global counter, exactly as the fake
// does, because independent effects run in parallel: a shared counter would
// assign ids in whatever order the goroutines happened to reach it, so two runs
// of identical input would produce different ids and `run diff` would report a
// difference that is only scheduling luck.
func (x *Executor) id(scope, kind string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ids == nil {
		x.ids = map[string]int{}
	}
	key := scope + "/" + kind
	x.ids[key]++
	return fmt.Sprintf("ev-%s-%s-%d", scope, kind, x.ids[key])
}

// CallTool enforces the policy, and refuses only what it cannot yet do safely.
//
// The permission half of the tool runner is real now; the execution half is not.
// That split is not a halfway measure, it is the useful order: two of the three
// policy outcomes are decisions rather than work, and they are exactly the two
// where getting it wrong is unrecoverable.
//
//	deny  -> tool.call_denied. A decision. Nothing runs, and the log records
//	         why, so `run why` can name the tool and the member.
//	ask   -> tool.call_denied with policy=ask, which the reducer turns into an
//	         inbox item and a blocked_ref (applyToolDenied). Per spec this is
//	         NOT an error: it is a question.
//	allow -> still refused, loudly, because running it needs a sandbox for
//	         `bash` and the workspace isolation the blueprint declares.
//
// Refusing an ALLOW is the honest answer. Returning a successful
// tool.call_completed would make the reducer advance a stage on the strength of
// work nobody did, and the log would record a result that never existed -- which
// is unrecoverable, because the log is what `run why` and every replay trust.
//
// Emitting the denial as an EVENT rather than returning an error is the point of
// doing this half first. A denial is a domain fact: it happened, it is the
// correct outcome, and the run should continue knowing it. Returning an error
// would abort the effect and leave no trace of the decision, so the one case
// where the policy did its job would look identical to a broken executor.
func (x *Executor) CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error) {
	policy := x.policyFor(e.Agent, e.Tool)

	if policy == surface.PolicyAllow {
		if x.Tools == nil {
			return nil, fmt.Errorf("tool %q is allowed for %s, but this executor was "+
				"built without a tool runner.\n"+
				"  a faked result would advance the stage on work nobody did.\n"+
				"  what works today: arxi run start ... --sim, which fakes tools deliberately",
				e.Tool, e.Agent)
		}

		result, err := x.Tools.RunTool(ctx, e.Agent, e.Tool, e.Args)
		if err != nil {
			// Returned as an error, not as a completed call with the failure in
			// the result. The runner failing means arxi is broken -- bash is
			// missing, the workspace vanished, a path escaped confinement -- and
			// recording that as a tool that ran and reported trouble would put a
			// plausible-looking history of a broken build into the log the user is
			// asked to trust. The effect runner surfaces it; the run stops.
			return nil, err
		}

		// tool.call_completed, whose payload the spec fixes as {tool, result?}.
		// The result is what the next turn reads, so it carries the command's own
		// output verbatim -- including a non-zero exit, which is an ANSWER and not
		// an error: "the tests fail" is precisely what the agent asked to find out.
		return []kernel.Event{{
			ID:     x.id(e.Agent, "tool"),
			Type:   kernel.ToolCallCompleted,
			Source: kernel.SourceAgent,
			Actor:  e.Agent,
			Payload: map[string]any{
				"tool":   e.Tool,
				"result": result,
			},
		}}, nil
	}

	// One event, carrying the policy, because the reducer reads that field to
	// decide whether this is a dead end or a question. Losing it would collapse
	// "not allowed" and "not yet approved" into one outcome, and those have
	// different remedies -- one needs a policy change, the other an approval.
	return []kernel.Event{{
		ID:     x.id(e.Agent, "tool"),
		Type:   kernel.ToolCallDenied,
		Source: kernel.SourceAgent,
		Actor:  e.Agent,
		Payload: map[string]any{
			"tool":   e.Tool,
			"policy": string(policy),
		},
	}}, nil
}

// policyFor resolves the effective policy for one tool on one member.
//
// A member absent from the roster gets deny, not allow. That case means the
// effect named someone the frozen blueprint does not contain, and the safe
// reading of "I cannot find out what you are permitted" is "nothing".
func (x *Executor) policyFor(agent, name string) surface.Policy {
	for _, m := range x.Members {
		if m.Name == agent {
			return tool.Resolve(m.Tools, x.ToolPolicy[agent], name)
		}
	}
	return surface.PolicyDeny
}

// AskHuman records the question so a human can find it.
//
// This used to refuse, on the grounds that an inbox "needs somewhere to persist
// a question that outlives the process". It does, and the answer turned out to
// be the place the events already go: the effect runner appends what this
// returns, kernel.Fold rebuilds State.Inbox from those events, and `arxi inbox`
// folds the same log. So the question outlives the process without a second
// store that could disagree with the first. There is no inbox database, and
// that absence is the design -- see internal/inbox's package doc.
//
// The one thing this must not do is invent the id. e.ID is minted by the reducer
// (nextInboxID, decide.go) precisely so an answer can be matched against it; a
// fresh id here would produce a question that looks entirely normal in the log
// and that applyInboxReplied can never match, so the run would wait forever
// while looking healthy -- which is the exact failure the old refusal was
// written to avoid. The refusal is gone; the property it protected is not.
//
// No inbox.replied is emitted, and the asymmetry is the point: this writes the
// question and only a human writes the answer. An executor that could answer
// its own questions would make the approval gate decorative.
func (x *Executor) AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error) {
	if err := ctx.Err(); err != nil {
		// Checked before emitting, not after. A cancelled run that silently
		// skipped writing the question would leave a member blocked on an inbox
		// item that does not exist, so `arxi inbox` would show nothing to answer
		// for a run that cannot proceed without an answer.
		return nil, fmt.Errorf("ask a human (%s: %q): %w", e.Kind, e.Question, err)
	}

	payload := map[string]any{
		"inbox_id":   e.ID,
		"kind":       e.Kind,
		"question":   e.Question,
		"on_timeout": e.OnTimeout,
	}
	// Agent is omitted when empty rather than written blank, because a budget
	// question belongs to nobody. An "agent":"" in the log reads as a member
	// whose name was lost, and a reader would go looking for it.
	if e.Agent != "" {
		payload["agent"] = e.Agent
	}
	if e.TimeoutMs > 0 {
		payload["timeout_ms"] = e.TimeoutMs
	}

	return []kernel.Event{{
		ID:      x.id(e.Agent, "inbox"),
		Type:    kernel.InboxCreated,
		Source:  kernel.SourceRuntime,
		Actor:   e.Agent,
		Payload: payload,
	}}, nil
}
