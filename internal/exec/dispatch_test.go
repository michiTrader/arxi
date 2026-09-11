package exec

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
)

type dispatchCoordinatorFake struct {
	registered []DispatchMetadata
	receipts   map[string]DispatchReceipt
}

func (c *dispatchCoordinatorFake) RegisterDispatch(meta DispatchMetadata) error {
	c.registered = append(c.registered, meta)
	return nil
}
func (c *dispatchCoordinatorFake) RecordReceipt(meta DispatchMetadata, receipt DispatchReceipt) error {
	if c.receipts == nil {
		c.receipts = map[string]DispatchReceipt{}
	}
	c.receipts[meta.DispatchKey] = receipt
	return nil
}
func (c *dispatchCoordinatorFake) Receipt(meta DispatchMetadata) (DispatchReceipt, bool, error) {
	receipt, ok := c.receipts[meta.DispatchKey]
	return receipt, ok, nil
}

type keyedExecutorFake struct {
	invocations []DispatchMetadata
	logical     map[string][]kernel.Event
}

func (x *keyedExecutorFake) SpawnTurn(context.Context, kernel.SpawnTurn) ([]kernel.Event, error) {
	panic("metadata dispatch must be used")
}
func (x *keyedExecutorFake) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	panic("metadata dispatch must be used")
}
func (x *keyedExecutorFake) AskHuman(context.Context, kernel.AskHuman) ([]kernel.Event, error) {
	panic("metadata dispatch must be used")
}
func (x *keyedExecutorFake) ClassifyDispatch(kernel.Effect) (string, WorkClass, bool) {
	return "keyed-fake", WorkIdempotent, true
}
func (x *keyedExecutorFake) Dispatch(_ context.Context, _ kernel.Effect, meta DispatchMetadata) ([]kernel.Event, *DispatchReceipt, error) {
	x.invocations = append(x.invocations, meta)
	if x.logical == nil {
		x.logical = map[string][]kernel.Event{}
	}
	events, exists := x.logical[meta.DispatchKey]
	if !exists {
		events = []kernel.Event{{Type: kernel.ToolCallCompleted, Source: kernel.SourceAgent,
			Payload: map[string]any{"tool": "fake", "result": "exact"}}}
		x.logical[meta.DispatchKey] = events
	}
	return events, &DispatchReceipt{ExternalID: "fake-" + meta.DispatchKey, Status: OutcomeSucceeded}, nil
}

func seedStartedDispatch(t *testing.T, runner *Runner, source kernel.Event, effect kernel.Effect) Work {
	t.Helper()
	work, err := manifest(runner.RunID, source, []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.prepareStep(source, work); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Log.Append([]kernel.Event{runner.progressEvent(kernel.ExecWorkStarted,
		map[string]any{"work_id": work[0].ID}, source)}); err != nil {
		t.Fatal(err)
	}
	return work[0]
}

func TestPreparedExternalWorkPersistsStableDispatchIdentityAndClass(t *testing.T) {
	log := newMemLog()
	x := &keyedExecutorFake{}
	runner := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run", JobID: "job"}
	if _, err := runner.RunStep(context.Background(), testSource(30), []kernel.Effect{kernel.CallTool{Agent: "a", Tool: "fake"}}); err != nil {
		t.Fatal(err)
	}
	prepared := log.events[0]
	if prepared.Str("work_class") != string(WorkIdempotent) || prepared.Str("dispatch_key") == "" || prepared.Str("request_digest") == "" {
		t.Fatalf("prepared payload = %#v: external recovery needs its stable key, request binding, and honest adapter class before start", prepared.Payload)
	}
}

func TestStartedIdempotentDispatchReusesKeyWithoutDuplicatingLogicalAction(t *testing.T) {
	log := newMemLog()
	x := &keyedExecutorFake{logical: map[string][]kernel.Event{}}
	coord := &dispatchCoordinatorFake{}
	runner := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run", JobID: "job", Dispatches: coord}
	source, effect := testSource(31), kernel.CallTool{Agent: "a", Tool: "fake"}
	work := seedStartedDispatch(t, runner, source, effect)
	meta := runner.metadataFor(work)
	x.logical[meta.DispatchKey] = []kernel.Event{{Type: kernel.ToolCallCompleted, Payload: map[string]any{"result": "exact"}}}

	if _, err := runner.RunStep(context.Background(), source, []kernel.Effect{effect}); err != nil {
		t.Fatal(err)
	}
	if len(x.invocations) != 1 || x.invocations[0].DispatchKey != meta.DispatchKey || len(x.logical) != 1 {
		t.Fatalf("retry invocations=%#v logical actions=%d: an idempotent adapter must receive the original key and represent one external action", x.invocations, len(x.logical))
	}
}

type nonIdempotentExecutorFake struct{ calls int }

func (x *nonIdempotentExecutorFake) SpawnTurn(context.Context, kernel.SpawnTurn) ([]kernel.Event, error) {
	x.calls++
	return nil, nil
}
func (x *nonIdempotentExecutorFake) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	x.calls++
	return nil, nil
}
func (x *nonIdempotentExecutorFake) AskHuman(context.Context, kernel.AskHuman) ([]kernel.Event, error) {
	x.calls++
	return nil, nil
}

func TestStartedNonIdempotentDispatchWithoutReceiptIsNeverRerun(t *testing.T) {
	log := newMemLog()
	x := &nonIdempotentExecutorFake{}
	coord := &dispatchCoordinatorFake{}
	runner := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run", JobID: "job", Dispatches: coord}
	source, effect := testSource(33), kernel.CallTool{Agent: "a", Tool: "mutate"}
	seedStartedDispatch(t, runner, source, effect)

	_, err := runner.RunStep(context.Background(), source, []kernel.Effect{effect})
	if !errors.Is(err, ErrUnknownWork) || x.calls != 0 {
		t.Fatalf("error=%v calls=%d: started non-idempotent work without receipt must become unknown and must never run again", err, x.calls)
	}
}

func TestPreparedButUnstartedDispatchRunsAfterRecovery(t *testing.T) {
	log := newMemLog()
	x := &nonIdempotentExecutorFake{}
	runner := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run", JobID: "job"}
	source, effect := testSource(34), kernel.CallTool{Agent: "a", Tool: "mutate"}
	work, _ := manifest(runner.RunID, source, []kernel.Effect{effect})
	if err := runner.prepareStep(source, work); err != nil {
		t.Fatal(err)
	}

	if _, err := runner.RunStep(context.Background(), source, []kernel.Effect{effect}); err != nil || x.calls != 1 {
		t.Fatalf("error=%v calls=%d: a crash after prepare is proven pre-dispatch and must remain executable", err, x.calls)
	}
}

func TestRegisteredButUnstartedDispatchRunsAfterRecovery(t *testing.T) {
	log := newMemLog()
	x := &nonIdempotentExecutorFake{}
	coord := &dispatchCoordinatorFake{}
	runner := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run", JobID: "job", Dispatches: coord}
	source, effect := testSource(35), kernel.CallTool{Agent: "a", Tool: "mutate"}
	work, _ := manifest(runner.RunID, source, []kernel.Effect{effect})
	if err := runner.prepareStep(source, work); err != nil {
		t.Fatal(err)
	}
	if err := coord.RegisterDispatch(runner.metadataFor(work[0])); err != nil {
		t.Fatal(err)
	}

	if _, err := runner.RunStep(context.Background(), source, []kernel.Effect{effect}); err != nil || x.calls != 1 {
		t.Fatalf("error=%v calls=%d: registration is not a started boundary and must not strand prepared work", err, x.calls)
	}
}

func TestCommittedReceiptReconcilesExactCanonicalOutcomeWithoutRedispatch(t *testing.T) {
	log := newMemLog()
	x := &keyedExecutorFake{}
	coord := &dispatchCoordinatorFake{receipts: map[string]DispatchReceipt{}}
	runner := &Runner{Log: log, Clock: NewVirtualClock(), Executor: x, RunID: "run", JobID: "job", Dispatches: coord}
	source, effect := testSource(32), kernel.CallTool{Agent: "a", Tool: "fake"}
	work := seedStartedDispatch(t, runner, source, effect)
	want := []kernel.Event{{Type: kernel.ToolCallCompleted, Source: kernel.SourceAgent, Payload: map[string]any{"result": "receipt-exact"}}}
	body, _ := json.Marshal(want)
	coord.receipts[runner.metadataFor(work).DispatchKey] = DispatchReceipt{ExternalID: "lookup-1", Status: OutcomeSucceeded, CanonicalOutcome: body}

	result, err := runner.RunStep(context.Background(), source, []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if len(x.invocations) != 0 || !reflect.DeepEqual(result.Events[0].Payload, want[0].Payload) {
		t.Fatalf("dispatches=%d outcome=%#v: committed lookup evidence must restore the exact canonical result without calling outside again", len(x.invocations), result.Events)
	}
}
