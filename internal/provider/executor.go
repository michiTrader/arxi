package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/michiTrader/iash/internal/kernel"
	"github.com/michiTrader/iash/internal/model"
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

// CallTool refuses, loudly.
//
// A tool runner is a separate piece of work: it needs the resolved per-tool
// policy from §20.2 (allow/ask/deny), a sandbox for `bash`, and the workspace
// isolation the blueprint already declares. None of that exists yet.
//
// Refusing is the honest answer and the safe one. Returning a successful
// tool.call_completed would make the reducer advance a stage on the strength of
// work nobody did, and the log would record a result that never existed -- which
// is unrecoverable, because the log is what `run why` and every replay trust.
func (x *Executor) CallTool(ctx context.Context, e kernel.CallTool) ([]kernel.Event, error) {
	return nil, fmt.Errorf("tool %q was requested by %s, but this build has no tool runner: "+
		"a real one needs the resolved per-tool policy (allow/ask/deny), a sandbox for "+
		"bash and the declared workspace isolation.\n"+
		"  what works today: iash run start ... --sim, which fakes tools deliberately",
		e.Tool, e.Agent)
}

// AskHuman refuses for the same reason, with a sharper consequence.
//
// The inbox is a real feature (§20.2 shows `iash inbox`), and it needs somewhere
// to persist a question that outlives the process. Faking it would emit
// inbox.created for a question nobody can ever answer: applyInboxReplied would
// find no item, and the run would wait forever while looking perfectly healthy.
func (x *Executor) AskHuman(ctx context.Context, e kernel.AskHuman) ([]kernel.Event, error) {
	return nil, fmt.Errorf("%s asked a human (%s: %q), but this build has no inbox: "+
		"a faked question cannot be answered, and the run would wait forever while "+
		"looking healthy.\n"+
		"  what works today: iash run start ... --sim, which can answer canned replies",
		e.Agent, e.Kind, e.Question)
}
