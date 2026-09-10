package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

const maxNativeTurnRounds = 64

// TurnExecutor is the optional provider-neutral seam for native tool loops.
// Executor remains supported for text-only implementations; when this interface
// is present Runner durably coordinates each model and tool call itself.
type TurnExecutor interface {
	PrepareTurn(context.Context, kernel.SpawnTurn) (turn.Request, error)
	CompleteTurn(context.Context, turn.Request) (turn.Response, error)
	ExecuteTurnTool(context.Context, kernel.SpawnTurn, turn.ToolCall) (TurnToolOutcome, error)
	FinishTurn(kernel.SpawnTurn, []TurnEntry) ([]kernel.Event, error)
}

// NativeTurnEnabled lets an executor retain a legacy SpawnTurn path for
// simulations or compatibility while opting selected turns into the canonical
// durable loop. Executors without this optional gate use native turns always.
type NativeTurnGate interface {
	NativeTurnEnabled(kernel.SpawnTurn) bool
}

// TurnToolOutcome is the exact result of applying policy and, when permitted,
// invoking a native tool request. Continue is false for outcomes such as an
// approval question that end this model loop without reinjection.
type TurnToolOutcome struct {
	Result   turn.ToolResult `json:"result"`
	Policy   string          `json:"policy,omitempty"`
	Continue bool            `json:"continue"`
}

type TurnToolEntry struct {
	Call    turn.ToolCall
	Outcome TurnToolOutcome
}

// TurnEntry preserves model/tool ordering for the domain events emitted when the
// composite turn reaches a known terminal boundary.
type TurnEntry struct {
	Response *turn.Response
	Tool     *TurnToolEntry
}

type turnChild struct {
	ID           string
	Kind         string
	Slot         string
	PreparedJSON string
	Started      bool
	Status       string
	ResultJSON   string
}

type durableTurnProgress struct {
	byID   map[string]*turnChild
	bySlot map[string]*turnChild
}

func newDurableTurnProgress() durableTurnProgress {
	return durableTurnProgress{byID: map[string]*turnChild{}, bySlot: map[string]*turnChild{}}
}

// runDurableTurn reconstructs the transcript exclusively from committed child
// outcomes. A crash after a tool result therefore reinjects the same object and
// never calls the runner again.
func (r *Runner) runDurableTurn(ctx context.Context, w Work, e kernel.SpawnTurn, x TurnExecutor) ([]kernel.Event, error) {
	req, err := x.PrepareTurn(ctx, e)
	if err != nil {
		return nil, NotDispatched(fmt.Errorf("prepare native turn for %s: %w", e.Agent, err))
	}
	if req.Schema != turn.Schema {
		return nil, NotDispatched(fmt.Errorf("prepare native turn for %s: schema %q, want %q", e.Agent, req.Schema, turn.Schema))
	}
	progress, err := r.loadTurnProgress(w)
	if err != nil {
		return nil, NotDispatched(err)
	}

	var trace []TurnEntry
	seen := map[string]turn.ToolCall{}
	for round := 0; round < maxNativeTurnRounds; round++ {
		resp, err := r.runModelChild(ctx, w, round, req, x, &progress)
		if err != nil {
			return nil, err
		}
		trace = append(trace, TurnEntry{Response: &resp})
		calls, err := responseToolCalls(resp)
		if err != nil {
			return nil, NotDispatched(fmt.Errorf("model round %d for %s: %w", round, e.Agent, err))
		}
		if len(calls) == 0 {
			if resp.FinishReason == turn.FinishToolCalls {
				return nil, NotDispatched(fmt.Errorf("model round %d for %s stopped for tool calls but supplied none", round, e.Agent))
			}
			events, err := x.FinishTurn(e, trace)
			if err != nil {
				return nil, NotDispatched(fmt.Errorf("finish native turn for %s: %w", e.Agent, err))
			}
			return events, nil
		}

		if err := validateRoundCalls(calls, seen); err != nil {
			return nil, NotDispatched(fmt.Errorf("model round %d for %s: %w", round, e.Agent, err))
		}
		req.Messages = append(req.Messages, turn.Message{Role: turn.RoleAssistant, Content: resp.Content})
		resultBlocks := make([]turn.ContentBlock, 0, len(calls))
		continueLoop := true
		for _, call := range calls {
			outcome, err := r.runToolChild(ctx, w, e, round, call, x, &progress)
			if err != nil {
				return nil, err
			}
			entry := TurnToolEntry{Call: call, Outcome: outcome}
			trace = append(trace, TurnEntry{Tool: &entry})
			result := outcome.Result
			resultBlocks = append(resultBlocks, turn.ContentBlock{Type: turn.BlockToolResult, ToolResult: &result})
			seen[call.ID] = call
			if !outcome.Continue {
				continueLoop = false
				break
			}
		}
		if !continueLoop {

			events, err := x.FinishTurn(e, trace)
			if err != nil {
				return nil, NotDispatched(fmt.Errorf("finish interrupted native turn for %s: %w", e.Agent, err))
			}
			return events, nil
		}
		req.Messages = append(req.Messages, turn.Message{Role: turn.RoleTool, Content: resultBlocks})
	}
	return nil, NotDispatched(fmt.Errorf("native turn for %s exceeded %d model rounds", e.Agent, maxNativeTurnRounds))
}

func responseToolCalls(resp turn.Response) ([]turn.ToolCall, error) {
	if err := validateTurnResponse(resp); err != nil {
		return nil, err
	}
	var calls []turn.ToolCall
	for i, block := range resp.Content {
		if block.Type != turn.BlockToolCall {
			continue
		}
		if block.ToolCall == nil {
			return nil, fmt.Errorf("content block %d is tool_call without a call", i)
		}
		if err := turn.ValidateToolCall(*block.ToolCall); err != nil {
			return nil, err
		}
		calls = append(calls, *block.ToolCall)
	}
	return calls, nil
}

func validateTurnResponse(resp turn.Response) error {
	if resp.Schema != turn.Schema {
		return fmt.Errorf("response schema %q, want %q", resp.Schema, turn.Schema)
	}
	switch resp.FinishReason {
	case turn.FinishStop, turn.FinishLength, turn.FinishToolCalls, turn.FinishRefusal, turn.FinishCanceled, turn.FinishError:
	default:
		return fmt.Errorf("unsupported finish reason %q", resp.FinishReason)
	}
	calls := 0
	for i, block := range resp.Content {
		switch block.Type {
		case turn.BlockText:
			if block.ToolCall != nil || block.ToolResult != nil || block.Source != nil {
				return fmt.Errorf("text content block %d carries incompatible data", i)
			}
		case turn.BlockToolCall:
			if block.ToolCall == nil {
				return fmt.Errorf("content block %d is tool_call without a call", i)
			}
			if err := turn.ValidateToolCall(*block.ToolCall); err != nil {
				return err
			}
			calls++
		default:
			return fmt.Errorf("response content type %q is not supported by the durable turn loop", block.Type)
		}
	}
	if (calls > 0) != (resp.FinishReason == turn.FinishToolCalls) {
		return fmt.Errorf("finish reason %q is inconsistent with %d tool calls", resp.FinishReason, calls)
	}
	if resp.Refusal != nil && resp.FinishReason != turn.FinishRefusal {
		return fmt.Errorf("refusal requires finish reason %q, got %q", turn.FinishRefusal, resp.FinishReason)
	}
	if resp.FinishReason == turn.FinishRefusal && (resp.Refusal == nil || resp.Refusal.Message == "") {
		return fmt.Errorf("finish reason %q requires a refusal message", turn.FinishRefusal)
	}
	return nil
}

func validateToolOutcome(call turn.ToolCall, outcome TurnToolOutcome) error {
	if outcome.Result.CallID != call.ID {
		return fmt.Errorf("tool %s returned call_id %q, want %q", call.Name, outcome.Result.CallID, call.ID)
	}
	if outcome.Continue && outcome.Policy != "" && outcome.Policy != "allow" {
		return fmt.Errorf("tool %s policy %q cannot continue the native loop", call.Name, outcome.Policy)
	}
	for i, block := range outcome.Result.Content {
		if block.Type != turn.BlockText || block.ToolCall != nil || block.ToolResult != nil || block.Source != nil {
			return fmt.Errorf("tool %s result block %d has unsupported content type %q", call.Name, i, block.Type)
		}
	}
	return nil
}

func validateRoundCalls(calls []turn.ToolCall, seen map[string]turn.ToolCall) error {
	batch := map[string]turn.ToolCall{}
	for _, call := range calls {
		if prior, ok := batch[call.ID]; ok && (prior.Name != call.Name || prior.ArgumentDigest != call.ArgumentDigest) {
			return fmt.Errorf("tool call id %q is reused with conflicting identity", call.ID)
		}
		if prior, ok := seen[call.ID]; ok && (prior.Name != call.Name || prior.ArgumentDigest != call.ArgumentDigest) {
			return fmt.Errorf("tool call id %q is reused with conflicting identity", call.ID)
		}
		batch[call.ID] = call
	}
	return nil
}

func (r *Runner) runModelChild(ctx context.Context, parent Work, round int, req turn.Request, x TurnExecutor, progress *durableTurnProgress) (turn.Response, error) {
	prepared, err := json.Marshal(req)
	if err != nil {
		return turn.Response{}, NotDispatched(fmt.Errorf("encode model round %d: %w", round, err))
	}
	slot := fmt.Sprintf("model/%d", round)
	id := turnChildID(parent.ID, slot, string(prepared))
	child, err := r.ensureTurnChild(parent, id, "model", slot, string(prepared), progress)
	if err != nil {
		return turn.Response{}, err
	}
	if child.Status == "completed" {
		var resp turn.Response
		if err := json.Unmarshal([]byte(child.ResultJSON), &resp); err != nil {
			return turn.Response{}, fmt.Errorf("decode committed outcome of %s: %w", child.ID, err)
		}
		if err := validateTurnResponse(resp); err != nil {
			return turn.Response{}, NotDispatched(fmt.Errorf("validate committed outcome of %s: %w", child.ID, err))
		}
		return resp, nil
	}

	if child.Status == "unknown" || child.Started {
		return turn.Response{}, fmt.Errorf("%w: child work %s has no committed model outcome", ErrUnknownWork, child.ID)
	}
	if err := r.startTurnChild(parent, child); err != nil {
		return turn.Response{}, err
	}
	resp, callErr := x.CompleteTurn(ctx, req)
	if callErr == nil {
		if err := validateTurnResponse(resp); err != nil {
			callErr = NotDispatched(err)
		}
	}
	if callErr != nil {
		status := "unknown"
		if errors.Is(callErr, ErrNotDispatched) {
			status = "failed"
		}
		if err := r.finishTurnChild(parent, child, status, nil, callErr); err != nil {
			return turn.Response{}, err
		}
		if status == "unknown" {
			return turn.Response{}, fmt.Errorf("%w: child model call %s: %v", ErrUnknownWork, child.ID, callErr)
		}
		return turn.Response{}, callErr
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return turn.Response{}, fmt.Errorf("encode model outcome of %s: %w", child.ID, err)
	}
	if err := r.finishTurnChild(parent, child, "completed", body, nil); err != nil {
		return turn.Response{}, err
	}
	return resp, nil
}

func (r *Runner) runToolChild(ctx context.Context, parent Work, effect kernel.SpawnTurn, round int, call turn.ToolCall, x TurnExecutor, progress *durableTurnProgress) (TurnToolOutcome, error) {
	if err := turn.ValidateToolCall(call); err != nil {
		return TurnToolOutcome{}, NotDispatched(err)
	}
	prepared, err := json.Marshal(call)
	if err != nil {
		return TurnToolOutcome{}, NotDispatched(fmt.Errorf("encode tool call %s: %w", call.ID, err))
	}
	slot := fmt.Sprintf("tool/%d/%s", round, call.ID)
	id := turnChildID(parent.ID, slot+"/"+call.Name+"/"+call.ArgumentDigest, string(prepared))
	child, err := r.ensureTurnChild(parent, id, "tool", slot, string(prepared), progress)
	if err != nil {
		return TurnToolOutcome{}, err
	}
	if child.Status == "completed" {
		var outcome TurnToolOutcome
		if err := json.Unmarshal([]byte(child.ResultJSON), &outcome); err != nil {
			return TurnToolOutcome{}, fmt.Errorf("decode committed outcome of %s: %w", child.ID, err)
		}
		if err := validateToolOutcome(call, outcome); err != nil {
			return TurnToolOutcome{}, NotDispatched(fmt.Errorf("validate committed outcome of %s: %w", child.ID, err))
		}
		return outcome, nil
	}
	if child.Status == "unknown" || child.Started {
		return TurnToolOutcome{}, fmt.Errorf("%w: child work %s has no committed tool outcome", ErrUnknownWork, child.ID)
	}
	if err := r.startTurnChild(parent, child); err != nil {
		return TurnToolOutcome{}, err
	}
	outcome, callErr := x.ExecuteTurnTool(ctx, effect, call)
	if callErr == nil {
		if err := validateToolOutcome(call, outcome); err != nil {
			callErr = NotDispatched(err)
		}
	}
	if callErr != nil {
		status := "unknown"
		if errors.Is(callErr, ErrNotDispatched) {
			status = "failed"
		}
		if err := r.finishTurnChild(parent, child, status, nil, callErr); err != nil {
			return TurnToolOutcome{}, err
		}
		if status == "unknown" {
			return TurnToolOutcome{}, fmt.Errorf("%w: child tool call %s: %v", ErrUnknownWork, child.ID, callErr)
		}
		return TurnToolOutcome{}, callErr
	}
	if outcome.Result.CallID != call.ID {
		return TurnToolOutcome{}, fmt.Errorf("tool %s returned call_id %q, want %q", call.Name, outcome.Result.CallID, call.ID)
	}
	body, err := json.Marshal(outcome)
	if err != nil {
		return TurnToolOutcome{}, fmt.Errorf("encode tool outcome of %s: %w", child.ID, err)
	}
	if err := r.finishTurnChild(parent, child, "completed", body, nil); err != nil {
		return TurnToolOutcome{}, err
	}
	return outcome, nil
}

func (r *Runner) loadTurnProgress(parent Work) (durableTurnProgress, error) {
	out := newDurableTurnProgress()
	events, err := r.Log.Read(1, 0)
	if err != nil {
		return out, fmt.Errorf("read native turn progress: %w", err)
	}
	for _, event := range events {
		if event.Str("parent_work_id") != parent.ID {
			continue
		}
		id := event.Str("work_id")
		switch event.Type {
		case kernel.ExecWorkPrepared:
			if id == "" || event.Str("work_scope") != "turn_child" {
				continue
			}
			child := &turnChild{ID: id, Kind: event.Str("child_kind"), Slot: event.Str("child_slot"), PreparedJSON: event.Str("request_json")}
			if old := out.bySlot[child.Slot]; old != nil && (old.ID != child.ID || old.PreparedJSON != child.PreparedJSON) {
				return out, fmt.Errorf("native turn slot %s has conflicting prepared identities", child.Slot)
			}
			out.byID[id], out.bySlot[child.Slot] = child, child
		case kernel.ExecWorkStarted:
			if child := out.byID[id]; child != nil {
				child.Started = true
			}
		case kernel.ExecWorkFinished:
			if child := out.byID[id]; child != nil {
				child.Status = event.Str("status")
				child.ResultJSON = event.Str("result_json")
			}
		}
	}
	return out, nil
}

func (r *Runner) ensureTurnChild(parent Work, id, kind, slot, prepared string, progress *durableTurnProgress) (*turnChild, error) {
	if existing := progress.bySlot[slot]; existing != nil {
		if existing.ID != id || existing.PreparedJSON != prepared {
			return nil, fmt.Errorf("native turn slot %s changed after it was prepared", slot)
		}
		return existing, nil
	}
	child := &turnChild{ID: id, Kind: kind, Slot: slot, PreparedJSON: prepared}
	event := r.progressEvent(kernel.ExecWorkPrepared, map[string]any{
		"work_id": id, "parent_work_id": parent.ID, "work_scope": "turn_child",
		"source_seq": parent.SourceSeq, "child_kind": kind, "child_slot": slot,
		"request_json": prepared,
	}, parent.Source)
	if _, err := r.Log.Append(r.stamp([]kernel.Event{event})); err != nil {
		return nil, fmt.Errorf("append preparation of native turn child %s: %w", id, err)
	}
	progress.byID[id], progress.bySlot[slot] = child, child
	return child, nil
}

func (r *Runner) startTurnChild(parent Work, child *turnChild) error {
	event := r.progressEvent(kernel.ExecWorkStarted, map[string]any{
		"work_id": child.ID, "parent_work_id": parent.ID, "work_scope": "turn_child",
	}, parent.Source)
	if _, err := r.Log.Append(r.stamp([]kernel.Event{event})); err != nil {
		return fmt.Errorf("append start of native turn child %s: %w", child.ID, err)
	}
	child.Started = true
	return nil
}

func (r *Runner) finishTurnChild(parent Work, child *turnChild, status string, result []byte, cause error) error {
	payload := map[string]any{
		"work_id": child.ID, "parent_work_id": parent.ID, "work_scope": "turn_child", "status": status,
	}
	if result != nil {
		payload["result_json"] = string(result)
	}
	if cause != nil {
		payload["error"] = cause.Error()
	}
	event := r.progressEvent(kernel.ExecWorkFinished, payload, parent.Source)
	if _, err := r.Log.Append(r.stamp([]kernel.Event{event})); err != nil {
		return fmt.Errorf("append outcome of native turn child %s: %w", child.ID, err)
	}
	child.Status, child.ResultJSON = status, string(result)
	return nil
}

func turnChildID(parentID, identity, prepared string) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + identity + "\x00" + prepared))
	return "work-" + hex.EncodeToString(sum[:])
}
