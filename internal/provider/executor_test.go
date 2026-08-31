package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
)

// TestTheLiveExecutorSatisfiesTheInterfaceTheRunnerWants is the test that makes
// the whole package legitimate.
//
// internal/exec may not import this package (an arch rule holds it to the
// kernel), so nothing in the compiler forces these two to agree unless somebody
// says so. Without this line the signatures could drift apart -- a renamed
// method, an added parameter -- and the failure would surface at the wiring site
// in cmd/arxi as an unreadable "does not implement" error, or worse, would be
// "fixed" there by dropping back to the Fake.
func TestTheLiveExecutorSatisfiesTheInterfaceTheRunnerWants(t *testing.T) {
	var _ exec.Executor = (*Executor)(nil)
}

// fixedResolver answers with one resolution, whatever it is asked.
type fixedResolver struct {
	res model.Resolution
	err error
}

func (f fixedResolver) Resolve(string) (model.Resolution, error) {
	return f.res, f.err
}

// serverReturning starts a provider that replies with the given status and body,
// and reports what it was asked.
func serverReturning(t *testing.T, status int, body any) (*httptest.Server, *chatRequest) {
	t.Helper()
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// executorFor wires an Executor against a test server, using a priced model.
func executorFor(t *testing.T, srv *httptest.Server, members ...kernel.MemberConfig) *Executor {
	t.Helper()
	return &Executor{
		Resolver: fixedResolver{res: model.Resolution{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6",
			BaseURL:  srv.URL,
		}},
		DefaultModel: "claude-sonnet-4-6",
		Members:      members,
		Prompt:       "fix the failing test",
	}
}

func okBody(in, out int, text string) map[string]any {
	return map[string]any{
		"id":      "cmpl-1",
		"model":   "claude-sonnet-4-6",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
	}
}

func types(evs []kernel.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = string(e.Type)
	}
	return out
}

// TestATurnEmitsActivatedThenTheResponseThenDone pins the order the reducer
// depends on. Every element of it is load-bearing and the reasons are in
// exec/fake.go: activated marks the member busy, and the budget is charged on
// llm.response, so a turn_done seen first would let a run look finished and
// under budget while the money was already spent.
func TestATurnEmitsActivatedThenTheResponseThenDone(t *testing.T) {
	srv, _ := serverReturning(t, 200, okBody(1000, 100, "done"))
	x := executorFor(t, srv)

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err != nil {
		t.Fatalf("a successful call returned an error: %v", err)
	}

	want := []string{"agent.activated", "llm.response", "agent.turn_done"}
	got := types(evs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, expected %v.\n"+
			"  activated must come first: it is what marks the member busy, so without "+
			"it --max-turns is unreachable and coalescing never engages.\n"+
			"  llm.response must precede turn_done: the reducer charges the budget "+
			"there.", got, want)
	}
}

// TestTheTurnReportsWhatItCost is the money test. The reducer reads exactly this
// key (decide.go applyCost reads e.Num("cost_usd")), so a wrong name here means
// a run that spends and never charges.
func TestTheTurnReportsWhatItCost(t *testing.T) {
	// 10k in, 1k out on sonnet: 10000*3/1e6 + 1000*15/1e6 = 0.03 + 0.015
	srv, _ := serverReturning(t, 200, okBody(10_000, 1_000, "done"))
	x := executorFor(t, srv)

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err != nil {
		t.Fatalf("SpawnTurn: %v", err)
	}

	var llm kernel.Event
	for _, e := range evs {
		if e.Type == kernel.LLMResponse {
			llm = e
		}
	}
	cost, ok := llm.Payload["cost_usd"].(float64)
	if !ok {
		t.Fatalf("cost_usd is %T, not a float64. The reducer reads this exact key to "+
			"charge the budget; anything else means the run spends and never charges",
			llm.Payload["cost_usd"])
	}
	if math.Abs(cost-0.045) > 1e-9 {
		t.Errorf("cost = %v, expected 0.045 (10k in at $3/Mtok + 1k out at $15/Mtok)", cost)
	}

	// The counts must travel too: a cost with no counts cannot be audited,
	// because an expensive model and a long prompt have opposite fixes.
	if llm.Payload["in_tokens"] != 10_000 && llm.Payload["in_tokens"] != float64(10_000) {
		t.Errorf("in_tokens = %v, expected 10000", llm.Payload["in_tokens"])
	}
	if llm.Payload["out_tokens"] != 1_000 && llm.Payload["out_tokens"] != float64(1_000) {
		t.Errorf("out_tokens = %v, expected 1000", llm.Payload["out_tokens"])
	}
}

// TestATransportFailureProducesNoEvents is one half of the Executor contract.
//
// Emitting agent.activated and then failing would leave a member the reducer
// believes is thinking, holding a turn that can never close -- a run wedged in a
// state no human command can clear.
func TestATransportFailureProducesNoEvents(t *testing.T) {
	x := &Executor{
		Resolver: fixedResolver{res: model.Resolution{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6",
			// A port nothing listens on.
			BaseURL: "http://127.0.0.1:1",
		}},
		DefaultModel: "claude-sonnet-4-6",
	}

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err == nil {
		t.Fatal("an unreachable provider reported success")
	}
	if len(evs) != 0 {
		t.Errorf("a transport failure emitted %d events (%v); the member would be left "+
			"thinking forever, holding a turn that can never close", len(evs), types(evs))
	}
}

// TestAProviderRefusalIsRecordedAsAFact is the other half. The provider
// answered; that happened, and the log is the truth.
func TestAProviderRefusalIsRecordedAsAFact(t *testing.T) {
	srv, _ := serverReturning(t, 429, map[string]any{
		"error": map[string]any{"message": "slow down", "type": "rate_limit_error"},
	})
	x := executorFor(t, srv)

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err != nil {
		t.Fatalf("a refusal came back as a transport error (%v); it is a DOMAIN fact "+
			"and belongs in the log", err)
	}
	if got := types(evs); strings.Join(got, ",") != "agent.activated,llm.response,agent.turn_done" {
		t.Fatalf("events = %v; a refused turn still happened and must still close", got)
	}

	var llm kernel.Event
	for _, e := range evs {
		if e.Type == kernel.LLMResponse {
			llm = e
		}
	}
	if llm.Payload["ok"] != false {
		t.Errorf("ok = %v, expected false; a refusal recorded as success is a turn that "+
			"never happened appearing to have worked", llm.Payload["ok"])
	}
	if llm.Payload["retryable"] != true {
		t.Errorf("retryable = %v, expected true for 429. Without it a caller cannot "+
			"tell a rate limit that will pass from a bad key that never will",
			llm.Payload["retryable"])
	}
	if s, _ := llm.Payload["status"].(int); s != 429 {
		t.Errorf("status = %v, expected 429", llm.Payload["status"])
	}
}

// TestAnUnpricedModelIsRefusedBeforeSpending: charging zero would leave --budget
// enforced against a total that never moves, so the ceiling would never be
// reached. The safety mechanism would be off while appearing to be on.
func TestAnUnpricedModelIsRefusedBeforeSpending(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	x := &Executor{
		Resolver: fixedResolver{res: model.Resolution{
			Provider: "someone", Model: "a-model-nobody-published", BaseURL: srv.URL,
		}},
		DefaultModel: "a-model-nobody-published",
	}

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err == nil {
		t.Fatal("an unpriced model was called; the budget would be charged nothing " +
			"forever and --budget would be decorative")
	}
	var noPrice *model.ErrNoPrice
	if !errors.As(err, &noPrice) {
		t.Errorf("error is %v, not an *ErrNoPrice; the caller cannot tell an unpriced "+
			"model from a broken connection", err)
	}
	if called {
		t.Error("the provider was contacted before the price was checked; the refusal " +
			"must cost nothing")
	}
	if len(evs) != 0 {
		t.Errorf("a refused-before-spending turn emitted %v", types(evs))
	}
}

// TestAMemberModelBeatsTheRunDefault: the blueprint names a model per member
// precisely so a reviewer can run on something cheap while an implementer runs
// on something capable. Ignoring the field would bill every member at the
// default's rate while the blueprint said otherwise.
func TestAMemberModelBeatsTheRunDefault(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		asked = append(asked, req.Model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okBody(10, 1, "ok"))
	}))
	t.Cleanup(srv.Close)

	// The resolver echoes back whatever ref it was given, so the test can see
	// which ref the executor chose.
	x := &Executor{
		Resolver:     refEchoResolver{baseURL: srv.URL},
		DefaultModel: "claude-sonnet-4-6",
		Members: []kernel.MemberConfig{
			{Name: "reviewer", Model: "claude-haiku-4-5"},
		},
	}

	if _, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "reviewer"}); err != nil {
		t.Fatalf("SpawnTurn: %v", err)
	}
	if len(asked) != 1 || asked[0] != "claude-haiku-4-5" {
		t.Errorf("the provider was asked for %v, expected claude-haiku-4-5. The member "+
			"declared it; billing at the run default's rate contradicts the blueprint",
			asked)
	}
}

// refEchoResolver resolves any ref to itself, so a test can observe the choice.
type refEchoResolver struct{ baseURL string }

func (r refEchoResolver) Resolve(ref string) (model.Resolution, error) {
	_, id := model.ParseRef(ref)
	return model.Resolution{Provider: "anthropic", Model: id, BaseURL: r.baseURL}, nil
}

// TestAMemberWithNoModelUsesTheRunDefault is the additive half: MemberConfig.Model
// empty means "the run decides", which is what lets every pre-existing blueprint
// still run.
func TestAMemberWithNoModelUsesTheRunDefault(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		asked = append(asked, req.Model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okBody(10, 1, "ok"))
	}))
	t.Cleanup(srv.Close)

	x := &Executor{
		Resolver:     refEchoResolver{baseURL: srv.URL},
		DefaultModel: "gpt-5.1-mini",
		Members:      []kernel.MemberConfig{{Name: "backend"}},
	}
	if _, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"}); err != nil {
		t.Fatalf("SpawnTurn: %v", err)
	}
	if len(asked) != 1 || asked[0] != "gpt-5.1-mini" {
		t.Errorf("asked for %v, expected the run default gpt-5.1-mini", asked)
	}
}

// TestAMemberWithNoModelAndNoDefaultIsRefusedWithTheFix: guessing a model here
// is a spend decision taken behind the user's back.
func TestAMemberWithNoModelAndNoDefaultIsRefusedWithTheFix(t *testing.T) {
	x := &Executor{Resolver: fixedResolver{}, Members: []kernel.MemberConfig{{Name: "backend"}}}
	_, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err == nil {
		t.Fatal("a member with no model and no default was executed; a guessed model " +
			"is a spend decision taken behind the user's back")
	}
	if !strings.Contains(err.Error(), "--model") {
		t.Errorf("error %q does not name the fix", err)
	}
}

// TestTheReplyAndWhyItStoppedAreRecorded: "length" is not success. A reply cut
// off at max_tokens is indistinguishable from a complete one unless the log says
// so, and the run would treat a half-finished answer as the member's conclusion.
func TestTheReplyAndWhyItStoppedAreRecorded(t *testing.T) {
	body := okBody(10, 5, "partial answer")
	body["choices"].([]any)[0].(map[string]any)["finish_reason"] = "length"
	srv, _ := serverReturning(t, 200, body)
	x := executorFor(t, srv)

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err != nil {
		t.Fatalf("SpawnTurn: %v", err)
	}
	for _, e := range evs {
		if e.Type != kernel.LLMResponse {
			continue
		}
		if e.Payload["text"] != "partial answer" {
			t.Errorf("text = %v, expected the reply to reach the log", e.Payload["text"])
		}
		if e.Payload["finish_reason"] != "length" {
			t.Errorf("finish_reason = %v, expected length. Without it a truncated reply "+
				"looks like a complete one and the run adopts a half answer",
				e.Payload["finish_reason"])
		}
	}
}

// TestA200CarryingAnErrorIsStillARefusal: some OpenAI-compatible servers answer
// this way. Trusting the status line would record a turn that never happened as
// one that answered with nothing.
func TestA200CarryingAnErrorIsStillARefusal(t *testing.T) {
	srv, _ := serverReturning(t, 200, map[string]any{
		"error": map[string]any{"message": "context length exceeded", "type": "invalid_request_error"},
	})
	x := executorFor(t, srv)

	evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	for _, e := range evs {
		if e.Type == kernel.LLMResponse && e.Payload["ok"] != false {
			t.Errorf("a 200 carrying an error object was recorded as ok=%v; the turn "+
				"never happened but the log would claim it answered", e.Payload["ok"])
		}
	}
}

// TestToolsAndTheInboxRefuseRatherThanFake.
//
// A successful tool.call_completed would advance a stage on work nobody did, and
// the log -- which every replay and `run why` trust -- would record a result
// that never existed. A faked inbox.created is worse: applyInboxReplied would
// find no item, so the run waits forever while looking healthy.
func TestToolsAndTheInboxRefuseRatherThanFake(t *testing.T) {
	x := &Executor{}

	evs, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "bash"})
	if err == nil {
		t.Fatal("CallTool succeeded; a stage would advance on work nobody did")
	}
	if len(evs) != 0 {
		t.Errorf("CallTool emitted %v; a result for a call nobody made is unrecoverable", types(evs))
	}
	if !strings.Contains(err.Error(), "--sim") {
		t.Errorf("error %q does not point at what works today", err)
	}

	evs, err = x.AskHuman(context.Background(), kernel.AskHuman{Agent: "backend", Kind: "approval"})
	if err == nil {
		t.Fatal("AskHuman succeeded; the run would wait forever on a question nobody can answer")
	}
	if len(evs) != 0 {
		t.Errorf("AskHuman emitted %v", types(evs))
	}
}

// TestACancelledContextStopsBeforeSpending: a cancelled run must stop paying for
// turns whose results will be discarded.
func TestACancelledContextStopsBeforeSpending(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	x := executorFor(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := x.SpawnTurn(ctx, kernel.SpawnTurn{Agent: "backend"}); err == nil {
		t.Fatal("a cancelled context still ran a turn")
	}
	if called {
		t.Error("a cancelled run contacted the provider; it paid for a turn whose " +
			"result is discarded")
	}
}

// TestTwoTurnsDoNotShareAnEventID: ids are keyed by scope and kind, so
// independent effects running in parallel cannot collide. A collision makes
// caused_by identify two events instead of one, and the causal graph `run why`
// walks would contain forks that never happened.
func TestTwoTurnsDoNotShareAnEventID(t *testing.T) {
	srv, _ := serverReturning(t, 200, okBody(10, 1, "ok"))
	x := executorFor(t, srv)

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		evs, err := x.SpawnTurn(context.Background(), kernel.SpawnTurn{Agent: "backend"})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		for _, e := range evs {
			if seen[e.ID] {
				t.Fatalf("event id %q was issued twice; caused_by would identify two "+
					"events and `run why` would show a fork that never happened", e.ID)
			}
			seen[e.ID] = true
		}
	}
}
