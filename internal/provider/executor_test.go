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
	"github.com/michiTrader/arxi/internal/surface"
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
	//
	// The keys are the catalogue's (spec/events.md:111). This test asserted
	// in_tokens/out_tokens until the two were read against each other, which is
	// the reason a wrong spelling survived: the only reader of these keys was the
	// test written from the same code it was checking.
	if llm.Payload["tokens_in"] != 10_000 && llm.Payload["tokens_in"] != float64(10_000) {
		t.Errorf("tokens_in = %v, expected 10000", llm.Payload["tokens_in"])
	}
	if llm.Payload["tokens_out"] != 1_000 && llm.Payload["tokens_out"] != float64(1_000) {
		t.Errorf("tokens_out = %v, expected 1000", llm.Payload["tokens_out"])
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
//
// The property is "never report work that did not happen", and it is unchanged.
// What changed is that not every outcome is an error any more: a DENIED call is
// a domain fact, so it is an event. This test used to assert that every CallTool
// returns an error, which was true only while nothing was implemented -- and the
// assertion would have blocked the policy layer while appearing to protect the
// log. The thing actually worth asserting is that no completion is ever
// fabricated, which is checked here in both directions.
func TestToolsAndTheInboxRefuseRatherThanFake(t *testing.T) {
	// An ALLOWED tool refuses when no runner was wired in. The sandbox now
	// exists, so this is no longer "nothing is implemented" — it is the case
	// where this particular Executor was built without one, which is what every
	// --sim run does. Left as an assertion because the failure mode is unchanged:
	// a nil runner must refuse rather than report a completion.
	allowed := &Executor{
		Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"read"}}},
	}
	evs, err := allowed.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "read"})
	if err == nil {
		t.Fatal("an allowed tool call succeeded; a stage would advance on work nobody did")
	}
	if len(evs) != 0 {
		t.Errorf("CallTool emitted %v; a result for a call nobody made is unrecoverable", types(evs))
	}
	if !strings.Contains(err.Error(), "--sim") {
		t.Errorf("error %q does not point at what works today", err)
	}

	// A DENIED tool is not an error and never runs, so there is nothing to
	// fake. It must not report a completion either.
	x := &Executor{}
	evs, err = x.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "bash"})
	if err != nil {
		t.Fatalf("a denied call returned an error (%v); the one case where the "+
			"policy did its job would be indistinguishable from a broken executor", err)
	}
	for _, e := range evs {
		if e.Type == kernel.ToolCallCompleted {
			t.Errorf("a denied call reported tool.call_completed; nothing ran")
		}
	}

	// AskHuman used to be asserted here, as a refusal. It is now implemented,
	// and the property the refusal protected -- never write a question that
	// cannot be matched to an answer -- is asserted by the three tests below.
}

// TestAskHumanRecordsTheQuestionUnderTheIdTheReducerMinted: the id in the event
// must be the reducer's, not one the executor invented.
//
// This is the whole of what the old refusal was defending. A fresh id would
// produce an inbox.created that looks entirely normal, and an inbox.replied
// carrying that id would match nothing in State.Inbox, so the member would stay
// blocked on a question that appears to have been asked and answered.
func TestAskHumanRecordsTheQuestionUnderTheIdTheReducerMinted(t *testing.T) {
	x := &Executor{}
	evs, err := x.AskHuman(context.Background(), kernel.AskHuman{
		ID:        "inbox-1",
		Kind:      "tool_approval",
		Question:  "backend wants to run bash. allow?",
		Agent:     "backend",
		OnTimeout: "deny",
	})
	if err != nil {
		t.Fatalf("AskHuman refused (%v); the inbox exists now, so a refusal "+
			"blocks a run for a reason that is no longer true", err)
	}
	if len(evs) != 1 {
		t.Fatalf("AskHuman emitted %v; want exactly one inbox.created", types(evs))
	}
	e := evs[0]
	if e.Type != kernel.InboxCreated {
		t.Fatalf("emitted %s, want %s", e.Type, kernel.InboxCreated)
	}
	if got := e.Payload["inbox_id"]; got != "inbox-1" {
		t.Errorf("inbox_id = %v, want inbox-1; an id the executor invented can "+
			"never be matched by applyInboxReplied, so the run waits forever "+
			"while the log looks healthy", got)
	}
	if got, _ := e.Payload["question"].(string); got == "" {
		t.Error("question is empty; a human is asked to authorise something " +
			"without being told what")
	}
	if got := e.Payload["on_timeout"]; got != "deny" {
		t.Errorf("on_timeout = %v, want deny; without it nothing records what "+
			"happens to an unanswered question", got)
	}
	if e.Source != kernel.SourceRuntime {
		t.Errorf("source = %q, want %q; the runtime asked, not a human",
			e.Source, kernel.SourceRuntime)
	}
	for _, ev := range evs {
		if ev.Type == kernel.InboxReplied {
			t.Error("AskHuman emitted inbox.replied; an executor that answers " +
				"its own questions makes the approval gate decorative")
		}
	}
}

// TestABudgetQuestionBelongsToNobodyAndSaysSo: an empty agent is omitted, not
// written blank, because "agent":"" reads as a member whose name was lost.
func TestABudgetQuestionBelongsToNobodyAndSaysSo(t *testing.T) {
	x := &Executor{}
	evs, err := x.AskHuman(context.Background(), kernel.AskHuman{
		ID:       "inbox-1",
		Kind:     "budget",
		Question: "the run is at 90% of its budget. continue?",
	})
	if err != nil {
		t.Fatalf("AskHuman refused: %v", err)
	}
	if _, ok := evs[0].Payload["agent"]; ok {
		t.Errorf("payload carries agent = %#v for a question with no member; "+
			"a blank name reads as one that was lost, and a reader goes "+
			"looking for it", evs[0].Payload["agent"])
	}
}

// TestACancelledRunDoesNotSilentlySkipTheQuestion: cancellation must be an
// error, not a quiet no-op.
//
// Returning no events and no error would leave the member blocked on an inbox
// item that does not exist: `arxi inbox` would show nothing to answer for a run
// that cannot proceed without an answer.
func TestACancelledRunDoesNotSilentlySkipTheQuestion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	x := &Executor{}
	evs, err := x.AskHuman(ctx, kernel.AskHuman{
		ID: "inbox-1", Kind: "tool_approval", Question: "allow bash?", Agent: "backend",
	})
	if err == nil {
		t.Fatal("a cancelled AskHuman succeeded quietly; the member would be " +
			"blocked on a question that was never recorded")
	}
	if len(evs) != 0 {
		t.Errorf("emitted %v after cancellation", types(evs))
	}
	if !strings.Contains(err.Error(), "allow bash?") {
		t.Errorf("error %q does not name the question that was lost", err)
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

// TestAPolicyDecisionReachesTheReducerAsAQuestion is the end-to-end claim, and
// the reason the policy half was worth shipping before the sandbox.
//
// The design promises something specific and load-bearing: `tool.call_denied`
// with `policy: ask` is not an error, it is a question that creates an inbox
// item and leaves a `blocked_ref` so the remedy is derivable (spec/events.md,
// and §20.2). Three pieces have to agree for that to be true -- this executor
// choosing `ask`, the event carrying the policy, and the reducer reading it --
// and no compiler checks any of the three against the others.
//
// So the event goes through the real reducer here, rather than being inspected
// as a payload. Asserting the payload would only prove that this package
// believes what it just wrote down.
func TestAPolicyDecisionReachesTheReducerAsAQuestion(t *testing.T) {
	x := &Executor{
		Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"read", "bash"}}},
	}

	evs, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "bash"})
	if err != nil {
		t.Fatalf("a mutating tool needing approval returned an error (%v); per "+
			"spec/events.md policy=ask is not an error, it is a question", err)
	}
	if len(evs) != 1 || evs[0].Type != kernel.ToolCallDenied {
		t.Fatalf("got %v, want one tool.call_denied", types(evs))
	}
	if got := evs[0].Payload["policy"]; got != "ask" {
		t.Fatalf("policy = %v, want ask\n"+
			"  consequence: the reducer reads this field to tell a dead end from a "+
			"question. Wrong here and the member is parked as denied, with no inbox "+
			"item and no remedy -- a run that stopped for a reason nobody can act on.", got)
	}

	// Through the real reducer, not a fake.
	st := kernel.State{
		Members: []kernel.Member{{Name: "backend", State: kernel.MemberIdle}},
	}
	ev := evs[0]
	ev.Seq = 5
	out, _ := kernel.Decide(st, ev, kernel.Config{})

	if len(out.Inbox) != 1 {
		t.Fatalf("the reducer produced %d inbox items, want 1\n"+
			"  consequence: the agent waits for an approval that was never asked "+
			"for, and `arxi inbox` shows nothing to approve.", len(out.Inbox))
	}
	item := out.Inbox[0]
	if item.Agent != "backend" {
		t.Errorf("inbox item belongs to %q, want backend", item.Agent)
	}
	if !strings.Contains(item.Question, "bash") {
		t.Errorf("the question %q does not name the tool, so approving it is a "+
			"guess about what is being approved", item.Question)
	}

	m := out.Member("backend")
	if m == nil {
		t.Fatal("backend vanished from the state")
	}
	if m.State != kernel.MemberWaiting {
		t.Errorf("backend is %v, want waiting: a member whose tool needs approval "+
			"but who is not marked waiting looks idle, and quiescence would call "+
			"the run finished with the question unanswered", m.State)
	}
	// blocked_ref is what makes the remedy derivable rather than hard-coded:
	// `run why` walks it to print `arxi inbox approve <id>`.
	if m.BlockedOn == nil {
		t.Fatal("no blocked_ref; `run why` would report a block it cannot name")
	}
	if m.BlockedOn["inbox_id"] != item.ID {
		t.Errorf("blocked_ref points at %v but the item is %q; the remedy would "+
			"name the wrong id", m.BlockedOn["inbox_id"], item.ID)
	}
	if m.BlockedOn["tool"] != "bash" {
		t.Errorf("blocked_ref tool = %v, want bash", m.BlockedOn["tool"])
	}
}

// TestADeniedToolDoesNotCreateAQuestionNobodyAsked is the other half, and it
// guards the more tempting mistake.
//
// If `deny` also produced an inbox item, every ungranted tool would become a
// permission prompt, and the safe default would quietly turn into "ask a human
// about everything" -- which is how approval fatigue gets built, and an approval
// nobody reads is an allow.
func TestADeniedToolDoesNotCreateAQuestionNobodyAsked(t *testing.T) {
	x := &Executor{
		Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"read"}}},
	}

	evs, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "bash"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := evs[0].Payload["policy"]; got != "deny" {
		t.Fatalf("an ungranted tool resolved to %v, want deny", got)
	}

	st := kernel.State{Members: []kernel.Member{{Name: "backend", State: kernel.MemberIdle}}}
	out, _ := kernel.Decide(st, evs[0], kernel.Config{})

	if len(out.Inbox) != 0 {
		t.Errorf("a denied tool created %d inbox items; approval fatigue is how a "+
			"safe default becomes an allow, because an approval nobody reads is "+
			"granted by default", len(out.Inbox))
	}
	if m := out.Member("backend"); m != nil && m.State == kernel.MemberWaiting {
		t.Error("backend is waiting on a denial; nothing is ever going to arrive, " +
			"so the run would hang instead of reporting the refusal")
	}
}

// fakeRunner records what it was asked and answers without touching a disk.
//
// A fake here rather than the real internal/toolrun, deliberately: this package
// is being tested on whether it TRANSLATES between the effect and the runner,
// and a real runner would make these tests depend on bash, a filesystem and a
// timeout. The confinement itself is tested where it lives.
type fakeRunner struct {
	member, tool string
	args         map[string]any
	result       string
	err          error
}

func (f *fakeRunner) RunTool(_ context.Context, member, name string, args map[string]any) (string, error) {
	f.member, f.tool, f.args = member, name, args
	return f.result, f.err
}

// TestAnAllowedToolRunsAndReportsWhatHappened is the other half of
// TestToolsAndTheInboxRefuseRatherThanFake, and without it that test proves
// only that a nil runner refuses -- which a permanently broken CallTool would
// also satisfy.
func TestAnAllowedToolRunsAndReportsWhatHappened(t *testing.T) {
	fr := &fakeRunner{result: "exit 0 (success)\n\nok\n"}
	x := &Executor{
		Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"read"}}},
		Tools:   fr,
	}

	evs, err := x.CallTool(context.Background(), kernel.CallTool{
		Agent: "backend", Tool: "read", Args: map[string]any{"path": "main.go"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != kernel.ToolCallCompleted {
		t.Fatalf("events = %v, want one tool.call_completed", types(evs))
	}
	if got := evs[0].Payload["result"]; got != fr.result {
		t.Errorf("result = %q, want %q\n"+
			"  the result is what the next turn reads: a summary invented here would "+
			"mean the model reasons about output the tool never produced", got, fr.result)
	}
	if evs[0].Payload["tool"] != "read" {
		t.Errorf("tool = %v, want read", evs[0].Payload["tool"])
	}
	if evs[0].Actor != "backend" {
		t.Errorf("actor = %q, want backend; a completion attributed to the wrong "+
			"member sends run why to the wrong place", evs[0].Actor)
	}

	// The member name has to reach the runner, because the runner is what maps it
	// to a workspace. Dropping it would silently give every member the same
	// directory, which is the overwrite the worktree default exists to prevent.
	if fr.member != "backend" {
		t.Errorf("runner got member %q, want backend", fr.member)
	}
	if fr.tool != "read" {
		t.Errorf("runner got tool %q, want read", fr.tool)
	}
	if fr.args["path"] != "main.go" {
		t.Errorf("runner got args %v; arguments dropped here would make every tool "+
			"call operate on a default nobody asked for", fr.args)
	}
}

// TestAToolThatFailsIsStillAnEventWhileABrokenRunnerIsAnError separates the two
// things a returning RunTool can mean.
func TestAToolThatFailsIsStillAnEventWhileABrokenRunnerIsAnError(t *testing.T) {
	// A command that ran and exited non-zero: the runner returns no error, and
	// the failure is IN the result. That is an answer, not a malfunction --
	// "the tests fail" is what the agent asked to find out.
	ok := &Executor{
		Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"bash"}}},
		ToolPolicy: map[string]map[string]surface.Policy{
			"backend": {"bash": surface.PolicyAllow},
		},
		Tools: &fakeRunner{result: "exit 1 (failure)\n\nFAIL ./pkg/auth\n"},
	}
	evs, err := ok.CallTool(context.Background(), kernel.CallTool{
		Agent: "backend", Tool: "bash", Args: map[string]any{"command": "go test ./..."},
	})
	if err != nil {
		t.Fatalf("a command exiting non-zero became an executor error (%v)\n"+
			"  the agent asked whether the tests pass; \"no\" is the answer, and "+
			"turning it into an error stops the run instead of informing it", err)
	}
	if len(evs) != 1 || evs[0].Type != kernel.ToolCallCompleted {
		t.Fatalf("events = %v, want one tool.call_completed", types(evs))
	}
	if !strings.Contains(evs[0].Payload["result"].(string), "FAIL") {
		t.Error("the failure output did not reach the result, so the next turn " +
			"cannot see what went wrong")
	}

	// A runner that could not run at all: bash missing, workspace gone, a path
	// refused by confinement. That IS a malfunction, and must not be recorded as
	// a tool that ran.
	broken := &Executor{
		Members: []kernel.MemberConfig{{Name: "backend", Tools: []string{"read"}}},
		Tools:   &fakeRunner{err: errors.New("toolrun: backend tried to reach \"../x\"")},
	}
	evs, err = broken.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "read"})
	if err == nil {
		t.Fatal("a broken runner produced no error; the log would hold a " +
			"plausible-looking history of work that never happened")
	}
	if len(evs) != 0 {
		t.Errorf("events = %v; a failed runner must emit nothing", types(evs))
	}
}

// TestADeniedToolIsNeverHandedToTheRunner guards the ordering that makes the
// policy layer worth having.
func TestADeniedToolIsNeverHandedToTheRunner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy surface.Policy
		tools  []string
	}{
		{"denied", surface.PolicyDeny, nil},
		{"ask", surface.PolicyAsk, []string{"bash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{result: "SHOULD NEVER APPEAR"}
			x := &Executor{
				Members: []kernel.MemberConfig{{Name: "backend", Tools: tc.tools}},
				ToolPolicy: map[string]map[string]surface.Policy{
					"backend": {"bash": tc.policy},
				},
				Tools: fr,
			}
			evs, err := x.CallTool(context.Background(), kernel.CallTool{Agent: "backend", Tool: "bash"})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if fr.tool != "" {
				t.Errorf("the runner was called for a %s tool\n"+
					"  checking the policy after running the tool protects nothing: "+
					"the write already happened, and the refusal is a note about it",
					tc.name)
			}
			for _, e := range evs {
				if e.Type == kernel.ToolCallCompleted {
					t.Error("a refused call reported tool.call_completed")
				}
			}
		})
	}
}
