package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/michiTrader/arxi/internal/authorization"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

const (
	maxNativeTurnRounds           = 64
	authorizationSuspensionSchema = "arxi.authorization-suspension/v1"
)

// AuthorizationConfig freezes every execution-context version included in an
// exact action grant. Empty values disable live exact authorization so legacy
// artifacts can replay without gaining authority they never recorded.
type AuthorizationConfig struct {
	ToolSchemaVersion  string
	PolicyVersion      string
	WorkspaceProfileID string
	TTLMS              int64
}

// TurnToolPolicyResolver separates a policy decision from external dispatch.
// The runner must know ask before it writes exec.work_started for the tool.
type TurnToolPolicyResolver interface {
	ResolveTurnToolPolicy(kernel.SpawnTurn, turn.ToolCall) string
}

// AuthorizedTurnToolExecutor dispatches only after the runner has atomically
// consumed an exact grant. It must not resolve policy again: the grant, rather
// than a mutable lookup, is the authority for this call.
type AuthorizedTurnToolExecutor interface {
	ExecuteAuthorizedTurnTool(context.Context, kernel.SpawnTurn, turn.ToolCall) (TurnToolOutcome, error)
}

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
	ParentWorkID string
	Kind         string
	Slot         string
	PreparedJSON string
	Started      bool
	Status       string
	ResultJSON   string
}

type authorizationSuspension struct {
	Schema             string           `json:"schema"`
	AuthorizationID    string           `json:"authorization_id"`
	SuspensionID       string           `json:"suspension_id"`
	JobID              string           `json:"job_id"`
	RunID              string           `json:"run_id"`
	RequesterPrincipal string           `json:"requester_principal"`
	ParentWorkID       string           `json:"parent_work_id"`
	SourceSeq          int64            `json:"source_seq"`
	Round              int              `json:"round"`
	CallIndex          int              `json:"call_index"`
	Effect             kernel.SpawnTurn `json:"effect"`
	Request            turn.Request     `json:"request"`
	Trace              []TurnEntry      `json:"trace"`
	Seen               []turn.ToolCall  `json:"seen"`
	PendingCalls       []turn.ToolCall  `json:"pending_calls"`
	Call               turn.ToolCall    `json:"call"`
	ChildID            string           `json:"child_id"`
	ChildSlot          string           `json:"child_slot"`
	ToolSchemaVersion  string           `json:"tool_schema_version"`
	PolicyVersion      string           `json:"policy_version"`
	WorkspaceProfileID string           `json:"workspace_profile_id"`
	ActionDigest       string           `json:"action_digest"`
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
		for callIndex, call := range calls {
			policy := "allow"
			if resolver, ok := x.(TurnToolPolicyResolver); ok {
				policy = resolver.ResolveTurnToolPolicy(e, call)
			}
			if policy == "ask" {
				if err := r.suspendAuthorization(w, e, round, callIndex, req, trace, seen, calls, call, &progress); err != nil {
					return nil, err
				}
				return nil, nil
			}
			var outcome TurnToolOutcome
			if policy == "deny" {
				outcome, err = r.runStoppedToolChild(ctx, w, e, round, call, x, &progress)
			} else {
				outcome, err = r.runToolChild(ctx, w, e, round, call, x, &progress)
			}
			if err != nil {
				return nil, err
			}
			entry := TurnToolEntry{Call: call, Outcome: outcome}
			trace = append(trace, TurnEntry{Tool: &entry})
			result := outcome.Result
			resultBlocks = append(resultBlocks, turn.ContentBlock{Type: turn.BlockToolResult, ToolResult: &result})
			seen[call.ID] = call
			if !outcome.Continue {
				events, err := x.FinishTurn(e, trace)
				if err != nil {
					return nil, NotDispatched(fmt.Errorf("finish interrupted native turn for %s: %w", e.Agent, err))
				}
				return events, nil
			}
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

	if child.Status == "unknown" {
		return turn.Response{}, fmt.Errorf("%w: child work %s has no committed model outcome", ErrUnknownWork, child.ID)
	}
	provider, class, honors := req.Provider, WorkNonIdempotent, false
	if classifier, ok := x.(TurnDispatchClassifier); ok {
		provider, class, honors = classifier.ClassifyModelDispatch(req)
	}
	meta := childMetadata(r, child, provider, class, honors)
	if err := r.register(meta); err != nil {
		return turn.Response{}, fmt.Errorf("register model dispatch %s: %w", child.ID, err)
	}
	if child.Started && r.Dispatches != nil {
		receipt, found, lookupErr := r.Dispatches.Receipt(meta)
		if lookupErr != nil {
			return turn.Response{}, lookupErr
		}
		if found {
			var resp turn.Response
			if err := json.Unmarshal(receipt.CanonicalOutcome, &resp); err != nil {
				return turn.Response{}, fmt.Errorf("decode receipt outcome of %s: %w", child.ID, err)
			}
			if err := validateTurnResponse(resp); err != nil {
				return turn.Response{}, fmt.Errorf("validate receipt outcome of %s: %w", child.ID, err)
			}
			if err := r.finishTurnChild(parent, child, "completed", receipt.CanonicalOutcome, nil); err != nil {
				return turn.Response{}, err
			}
			return resp, nil
		}
	}
	if child.Started && (meta.WorkClass != WorkIdempotent || !meta.SupportsIdempotency) {
		return turn.Response{}, fmt.Errorf("%w: child work %s has no committed model outcome", ErrUnknownWork, child.ID)
	}
	if !child.Started {
		if err := r.startTurnChild(parent, child); err != nil {
			return turn.Response{}, err
		}
	}
	var resp turn.Response
	var receipt *DispatchReceipt
	var callErr error
	if dispatch, ok := x.(MetadataTurnExecutor); ok {
		resp, receipt, callErr = dispatch.CompleteTurnDispatch(ctx, req, meta)
	} else {
		resp, callErr = x.CompleteTurn(ctx, req)
	}
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
	if receipt != nil {
		receipt.CanonicalOutcome = body
		if err := r.recordReceipt(meta, receipt); err != nil {
			return turn.Response{}, fmt.Errorf("record model receipt %s: %w", child.ID, err)
		}
	}
	if err := r.finishTurnChild(parent, child, "completed", body, nil); err != nil {
		return turn.Response{}, err
	}
	return resp, nil
}

func (r *Runner) resumeAuthorization(ctx context.Context, resumeWork Work, effect kernel.ResumeAuthorization) ([]kernel.Event, error) {
	events, err := r.Log.Read(1, 0)
	if err != nil {
		return nil, NotDispatched(fmt.Errorf("read exact authorization history: %w", err))
	}
	state, _ := kernel.Fold(kernel.State{}, events, r.Config)
	a := state.Authorization(effect.AuthorizationID)
	if a == nil || a.Schema != "arxi.authorization/v1" || a.SuspensionID != effect.SuspensionID ||
		a.ActionDigest != effect.ActionDigest || a.Decision != "granted" || a.GrantEventID == "" ||
		a.ConsumingWorkID != "" {
		return nil, NotDispatched(fmt.Errorf("authorization %s is not a current unconsumed exact grant", effect.AuthorizationID))
	}
	var suspensionJSON, suspensionDigest string
	for _, event := range events {
		if event.Type == kernel.ExecWorkPrepared && event.Str("child_kind") == "authorization" {
			var candidate authorizationSuspension
			if json.Unmarshal([]byte(event.Str("request_json")), &candidate) == nil && candidate.SuspensionID == effect.SuspensionID {
				if suspensionJSON != "" {
					return nil, NotDispatched(fmt.Errorf("authorization %s has duplicate suspension records", effect.AuthorizationID))
				}
				suspensionJSON, suspensionDigest = event.Str("request_json"), event.Str("request_digest")
			}
		}
	}
	if suspensionJSON == "" {
		return nil, NotDispatched(fmt.Errorf("authorization %s has no exact suspension bytes", effect.AuthorizationID))
	}
	if requestDigest([]byte(suspensionJSON)) != suspensionDigest {
		return nil, NotDispatched(fmt.Errorf("authorization %s suspension bytes do not match their persisted digest", effect.AuthorizationID))
	}
	var suspension authorizationSuspension
	if err := json.Unmarshal([]byte(suspensionJSON), &suspension); err != nil {
		return nil, NotDispatched(fmt.Errorf("decode authorization suspension: %w", err))
	}
	if err := r.validateAuthorizationSuspension(*a, suspension); err != nil {
		return nil, NotDispatched(err)
	}
	var child turnChild
	foundChild := false
	for _, event := range events {
		if event.Str("work_id") != suspension.ChildID {
			continue
		}
		switch event.Type {
		case kernel.ExecWorkPrepared:
			child = turnChild{ID: suspension.ChildID, ParentWorkID: suspension.ParentWorkID,
				Kind: event.Str("child_kind"), Slot: event.Str("child_slot"), PreparedJSON: event.Str("request_json")}
			foundChild = true
		case kernel.ExecWorkStarted:
			child.Started = true
		case kernel.ExecWorkFinished:
			child.Status, child.ResultJSON = event.Str("status"), event.Str("result_json")
		}
	}
	prepared, _ := json.Marshal(suspension.Call)
	if !foundChild || child.Kind != "tool" || child.Slot != suspension.ChildSlot || child.PreparedJSON != string(prepared) {
		return nil, NotDispatched(fmt.Errorf("authorization %s exact child bytes or identity changed", a.ID))
	}
	if child.Status == "completed" {
		return r.continueAuthorization(ctx, resumeWork, suspension, child)
	}
	if child.Started {
		if child.Status == "unknown" {
			return nil, fmt.Errorf("%w: authorized child %s has a durable unknown outcome", ErrUnknownWork, child.ID)
		}
		return nil, fmt.Errorf("%w: authorized child %s started without a committed outcome", ErrUnknownWork, child.ID)
	}
	authorized, ok := r.Executor.(AuthorizedTurnToolExecutor)
	if !ok {
		return nil, NotDispatched(fmt.Errorf("executor cannot dispatch exact authorized native tools"))
	}
	head := r.Log.Head()
	if r.Clock.NowMs() > 0 {
		expires, parseErr := time.Parse(time.RFC3339Nano, a.ExpiresAt)
		if parseErr != nil {
			return nil, NotDispatched(fmt.Errorf("authorization %s has invalid expiry: %w", a.ID, parseErr))
		}
		if r.Now == nil {
			return nil, NotDispatched(fmt.Errorf("authorization %s cannot verify live expiry without an injected clock", a.ID))
		}
		now, parseErr := time.Parse(time.RFC3339Nano, r.Now())
		if parseErr != nil {
			return nil, NotDispatched(fmt.Errorf("authorization %s current time is invalid: %w", a.ID, parseErr))
		}
		if !now.Before(expires) {
			expired := kernel.Event{ID: "authorization-expired-" + a.ID, Type: kernel.AuthorizationExpired,
				Ts: now.UTC().Format(time.RFC3339Nano), Source: kernel.SourceRuntime, Payload: map[string]any{
					"schema": a.Schema, "authorization_id": a.ID, "action_digest": a.ActionDigest,
					"expired_at": a.ExpiresAt,
				}}
			if _, err := r.Log.AppendIfSeq(head, r.stamp([]kernel.Event{expired})); err != nil {
				return nil, NotDispatched(fmt.Errorf("materialize expired authorization %s: %w", a.ID, err))
			}
			return nil, NotDispatched(fmt.Errorf("authorization %s expired before consumption", a.ID))
		}
	}
	consumed := kernel.Event{ID: "authorization-consumed-" + a.ID, Type: kernel.AuthorizationConsumed,
		Source: kernel.SourceRuntime, Actor: suspension.Effect.Agent, Payload: map[string]any{
			"schema": a.Schema, "authorization_id": a.ID, "action_digest": a.ActionDigest,
			"grant_event_id": a.GrantEventID, "work_id": child.ID,
		}}
	started := r.progressEvent(kernel.ExecWorkStarted, map[string]any{
		"work_id": child.ID, "parent_work_id": suspension.ParentWorkID, "work_scope": "turn_child",
	}, resumeWork.Source)
	if _, err := r.Log.AppendIfSeq(head, r.stamp([]kernel.Event{consumed, started})); err != nil {
		return nil, NotDispatched(fmt.Errorf("consume authorization %s: %w", a.ID, err))
	}
	child.Started = true
	outcome, callErr := authorized.ExecuteAuthorizedTurnTool(ctx, suspension.Effect, suspension.Call)
	if callErr == nil {
		callErr = validateToolOutcome(suspension.Call, outcome)
	}
	if callErr != nil {
		status := "unknown"
		if errors.Is(callErr, ErrNotDispatched) {
			status = "failed"
		}
		if err := r.finishTurnChild(Work{ID: suspension.ParentWorkID, Source: resumeWork.Source}, &child, status, nil, callErr); err != nil {
			return nil, err
		}
		if status == "unknown" {
			return nil, fmt.Errorf("%w: authorized tool call %s: %v", ErrUnknownWork, child.ID, callErr)
		}
		return nil, callErr
	}
	body, err := json.Marshal(outcome)
	if err != nil {
		return nil, err
	}
	if err := r.finishTurnChild(Work{ID: suspension.ParentWorkID, Source: resumeWork.Source}, &child, "completed", body, nil); err != nil {
		return nil, err
	}
	child.Status, child.ResultJSON = "completed", string(body)
	return r.continueAuthorization(ctx, resumeWork, suspension, child)
}

func (r *Runner) validateAuthorizationSuspension(a kernel.Authorization, s authorizationSuspension) error {
	if s.Schema != authorizationSuspensionSchema || s.AuthorizationID != a.ID || s.SuspensionID != a.SuspensionID ||
		s.JobID != r.JobID || s.RunID != r.RunID || s.RequesterPrincipal != a.RequesterPrincipal ||
		s.ParentWorkID != a.ParentWorkID || s.Call.ID != a.ProviderCallID || s.Call.Name != a.Tool ||
		s.Call.ArgumentDigest != a.ArgumentDigest || s.ToolSchemaVersion != a.ToolSchemaVersion ||
		s.PolicyVersion != a.PolicyVersion || s.WorkspaceProfileID != a.WorkspaceProfileID ||
		s.ActionDigest != a.ActionDigest || r.Authorization.ToolSchemaVersion != a.ToolSchemaVersion ||
		r.Authorization.PolicyVersion != a.PolicyVersion || r.Authorization.WorkspaceProfileID != a.WorkspaceProfileID {
		return fmt.Errorf("authorization %s bindings changed from the persisted exact suspension", a.ID)
	}
	if len(s.PendingCalls) == 0 || s.CallIndex < 0 || s.CallIndex >= len(s.PendingCalls) ||
		s.PendingCalls[s.CallIndex].ID != s.Call.ID || s.PendingCalls[s.CallIndex].Name != s.Call.Name ||
		s.PendingCalls[s.CallIndex].ArgumentDigest != s.Call.ArgumentDigest {
		return fmt.Errorf("authorization %s provider call order changed in the exact suspension", a.ID)
	}
	if err := turn.ValidateToolCall(s.Call); err != nil {
		return fmt.Errorf("authorization %s call is invalid: %w", a.ID, err)
	}
	action, err := authorization.NewAction(authorization.ActionInput{JobID: s.JobID, RunID: s.RunID,
		RequesterPrincipal: s.RequesterPrincipal, SuspendedParentWorkID: s.ParentWorkID,
		ProviderCallID: s.Call.ID, ToolName: s.Call.Name, ArgumentDigest: s.Call.ArgumentDigest,
		ToolSchemaVersion: s.ToolSchemaVersion, PolicyVersion: s.PolicyVersion, WorkspaceProfileID: s.WorkspaceProfileID})
	if err != nil || action.Digest() != a.ActionDigest {
		return fmt.Errorf("authorization %s action digest does not match exact suspension bytes", a.ID)
	}
	return nil
}

func (r *Runner) continueAuthorization(ctx context.Context, resumeWork Work, s authorizationSuspension, child turnChild) ([]kernel.Event, error) {
	var outcome TurnToolOutcome
	if err := json.Unmarshal([]byte(child.ResultJSON), &outcome); err != nil {
		return nil, NotDispatched(fmt.Errorf("decode authorized tool outcome %s: %w", child.ID, err))
	}
	if err := validateToolOutcome(s.Call, outcome); err != nil {
		return nil, NotDispatched(err)
	}
	trace := append([]TurnEntry(nil), s.Trace...)
	seen := make(map[string]turn.ToolCall, len(s.Seen)+1)
	for _, call := range s.Seen {
		seen[call.ID] = call
	}
	entry := TurnToolEntry{Call: s.Call, Outcome: outcome}
	trace = append(trace, TurnEntry{Tool: &entry})
	seen[s.Call.ID] = s.Call
	results := []turn.ContentBlock{{Type: turn.BlockToolResult, ToolResult: &outcome.Result}}
	x, ok := r.Executor.(TurnExecutor)
	if !ok {
		return nil, NotDispatched(fmt.Errorf("executor cannot continue exact native turn"))
	}
	progress, err := r.loadTurnProgress(Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source})
	if err != nil {
		return nil, err
	}
	for i := s.CallIndex + 1; i < len(s.PendingCalls); i++ {
		call := s.PendingCalls[i]
		policy := "allow"
		if resolver, ok := x.(TurnToolPolicyResolver); ok {
			policy = resolver.ResolveTurnToolPolicy(s.Effect, call)
		}
		if policy == "ask" {
			if err := r.suspendAuthorization(Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, s.Effect,
				s.Round, i, s.Request, trace, seen, s.PendingCalls, call, &progress); err != nil {
				return nil, err
			}
			return nil, nil
		}
		var next TurnToolOutcome
		if policy == "deny" {
			next, err = r.runStoppedToolChild(ctx, Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, s.Effect, s.Round, call, x, &progress)
		} else {
			next, err = r.runToolChild(ctx, Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, s.Effect, s.Round, call, x, &progress)
		}
		if err != nil {
			return nil, err
		}
		nextEntry := TurnToolEntry{Call: call, Outcome: next}
		trace = append(trace, TurnEntry{Tool: &nextEntry})
		seen[call.ID] = call
		results = append(results, turn.ContentBlock{Type: turn.BlockToolResult, ToolResult: &next.Result})
		if !next.Continue {
			return x.FinishTurn(s.Effect, trace)
		}
	}
	req := s.Request
	req.Messages = append(req.Messages, turn.Message{Role: turn.RoleTool, Content: results})
	for round := s.Round + 1; round < maxNativeTurnRounds; round++ {
		resp, err := r.runModelChild(ctx, Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, round, req, x, &progress)
		if err != nil {
			return nil, err
		}
		trace = append(trace, TurnEntry{Response: &resp})
		calls, err := responseToolCalls(resp)
		if err != nil {
			return nil, NotDispatched(err)
		}
		if len(calls) == 0 {
			return x.FinishTurn(s.Effect, trace)
		}
		if err := validateRoundCalls(calls, seen); err != nil {
			return nil, NotDispatched(err)
		}
		req.Messages = append(req.Messages, turn.Message{Role: turn.RoleAssistant, Content: resp.Content})
		resultBlocks := make([]turn.ContentBlock, 0, len(calls))
		for i, call := range calls {
			policy := "allow"
			if resolver, ok := x.(TurnToolPolicyResolver); ok {
				policy = resolver.ResolveTurnToolPolicy(s.Effect, call)
			}
			if policy == "ask" {
				if err := r.suspendAuthorization(Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, s.Effect,
					round, i, req, trace, seen, calls, call, &progress); err != nil {
					return nil, err
				}
				return nil, nil
			}
			var next TurnToolOutcome
			if policy == "deny" {
				next, err = r.runStoppedToolChild(ctx, Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, s.Effect, round, call, x, &progress)
			} else {
				next, err = r.runToolChild(ctx, Work{ID: s.ParentWorkID, SourceSeq: s.SourceSeq, Source: resumeWork.Source}, s.Effect, round, call, x, &progress)
			}
			if err != nil {
				return nil, err
			}
			nextEntry := TurnToolEntry{Call: call, Outcome: next}
			trace = append(trace, TurnEntry{Tool: &nextEntry})
			seen[call.ID] = call
			resultBlocks = append(resultBlocks, turn.ContentBlock{Type: turn.BlockToolResult, ToolResult: &next.Result})
			if !next.Continue {
				return x.FinishTurn(s.Effect, trace)
			}
		}
		req.Messages = append(req.Messages, turn.Message{Role: turn.RoleTool, Content: resultBlocks})
	}
	return nil, NotDispatched(fmt.Errorf("native turn exceeded %d model rounds after authorization", maxNativeTurnRounds))
}

func (r *Runner) suspendAuthorization(parent Work, effect kernel.SpawnTurn, round, callIndex int, req turn.Request, trace []TurnEntry, seen map[string]turn.ToolCall, calls []turn.ToolCall, call turn.ToolCall, progress *durableTurnProgress) error {
	cfg := r.Authorization
	if r.JobID == "" || r.RunID == "" || cfg.ToolSchemaVersion == "" || cfg.PolicyVersion == "" || cfg.WorkspaceProfileID == "" || cfg.TTLMS <= 0 {
		return NotDispatched(fmt.Errorf("exact authorization bindings are absent; legacy asks cannot resume live"))
	}
	prepared, err := json.Marshal(call)
	if err != nil {
		return NotDispatched(err)
	}
	slot := fmt.Sprintf("tool/%d/%s", round, call.ID)
	childID := turnChildID(parent.ID, slot+"/"+call.Name+"/"+call.ArgumentDigest, string(prepared))
	if _, err := r.ensureTurnChild(parent, childID, "tool", slot, string(prepared), progress); err != nil {
		return err
	}
	authorizationID := "authorization-" + childID[len("work-"):]
	suspensionID := "suspension-" + childID[len("work-"):]
	action, err := authorization.NewAction(authorization.ActionInput{
		JobID: r.JobID, RunID: r.RunID, RequesterPrincipal: "agent:" + effect.Agent,
		SuspendedParentWorkID: parent.ID, ProviderCallID: call.ID, ToolName: call.Name,
		ArgumentDigest: call.ArgumentDigest, ToolSchemaVersion: cfg.ToolSchemaVersion,
		PolicyVersion: cfg.PolicyVersion, WorkspaceProfileID: cfg.WorkspaceProfileID,
	})
	if err != nil {
		return NotDispatched(err)
	}
	orderedSeen := make([]turn.ToolCall, 0, len(seen))
	for _, entry := range trace {
		if entry.Tool != nil {
			orderedSeen = append(orderedSeen, entry.Tool.Call)
		}
	}
	suspension := authorizationSuspension{
		Schema: authorizationSuspensionSchema, AuthorizationID: authorizationID, SuspensionID: suspensionID,
		JobID: r.JobID, RunID: r.RunID, RequesterPrincipal: "agent:" + effect.Agent,
		ParentWorkID: parent.ID, SourceSeq: parent.SourceSeq, Round: round, CallIndex: callIndex,
		Effect: effect, Request: req, Trace: trace, Seen: orderedSeen, PendingCalls: calls, Call: call,
		ChildID: childID, ChildSlot: slot, ToolSchemaVersion: cfg.ToolSchemaVersion,
		PolicyVersion: cfg.PolicyVersion, WorkspaceProfileID: cfg.WorkspaceProfileID, ActionDigest: action.Digest(),
	}
	body, err := json.Marshal(suspension)
	if err != nil {
		return NotDispatched(err)
	}
	suspensionWork := "authorization-suspension-" + childID[len("work-"):]
	if existing := progress.bySlot["authorization/"+call.ID]; existing != nil {
		if existing.ID != suspensionWork || existing.PreparedJSON != string(body) {
			return NotDispatched(fmt.Errorf("authorization suspension %s changed after it was prepared", suspensionID))
		}
		return nil
	}
	metaChild := &turnChild{ID: suspensionWork, ParentWorkID: parent.ID, Kind: "authorization", Slot: "authorization/" + call.ID, PreparedJSON: string(body)}
	meta := childMetadata(r, metaChild, "authorization", WorkNonIdempotent, false)
	preparedEvent := r.progressEvent(kernel.ExecWorkPrepared, map[string]any{
		"work_id": suspensionWork, "parent_work_id": parent.ID, "work_scope": "turn_child",
		"source_seq": parent.SourceSeq, "child_kind": "authorization", "child_slot": metaChild.Slot,
		"request_json": string(body), "work_class": string(meta.WorkClass), "dispatch_key": meta.DispatchKey,
		"request_digest": meta.RequestDigest, "provider": meta.Provider,
	}, parent.Source)
	now := r.Now
	if now == nil {
		return NotDispatched(fmt.Errorf("exact authorization requires an injected timestamp"))
	}
	requestedAt, err := time.Parse(time.RFC3339Nano, now())
	if err != nil {
		return NotDispatched(fmt.Errorf("parse authorization timestamp: %w", err))
	}
	expires := requestedAt.Add(time.Duration(cfg.TTLMS) * time.Millisecond).UTC().Format(time.RFC3339Nano)
	inboxID := "inbox-authorization-" + childID[len("work-"):12+len("work-")]
	requestEvent := kernel.Event{ID: "authorization-requested-" + authorizationID, Type: kernel.AuthorizationRequested,
		Ts: requestedAt.UTC().Format(time.RFC3339Nano), Source: kernel.SourceRuntime, Actor: effect.Agent, Payload: map[string]any{
			"schema": "arxi.authorization/v1", "authorization_id": authorizationID, "inbox_id": inboxID,
			"requester_principal": suspension.RequesterPrincipal, "suspension_id": suspensionID,
			"parent_work_id": parent.ID, "provider_call_id": call.ID, "tool": call.Name,
			"argument_digest": call.ArgumentDigest, "action_digest": action.Digest(),
			"tool_schema_version": cfg.ToolSchemaVersion, "policy_version": cfg.PolicyVersion,
			"workspace_profile_id": cfg.WorkspaceProfileID, "expires_at": expires, "after_ms": cfg.TTLMS,
		}}
	if _, err := r.Log.Append(r.stamp([]kernel.Event{preparedEvent, requestEvent})); err != nil {
		return fmt.Errorf("persist exact authorization suspension %s: %w", suspensionID, err)
	}
	progress.byID[suspensionWork], progress.bySlot[metaChild.Slot] = metaChild, metaChild
	return nil
}

func (r *Runner) runStoppedToolChild(ctx context.Context, parent Work, effect kernel.SpawnTurn, round int, call turn.ToolCall, x TurnExecutor, progress *durableTurnProgress) (TurnToolOutcome, error) {
	if err := turn.ValidateToolCall(call); err != nil {
		return TurnToolOutcome{}, NotDispatched(err)
	}
	prepared, err := json.Marshal(call)
	if err != nil {
		return TurnToolOutcome{}, NotDispatched(err)
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
			return TurnToolOutcome{}, err
		}
		return outcome, validateToolOutcome(call, outcome)
	}
	outcome, err := x.ExecuteTurnTool(ctx, effect, call)
	if err != nil {
		return TurnToolOutcome{}, err
	}
	if err := validateToolOutcome(call, outcome); err != nil {
		return TurnToolOutcome{}, NotDispatched(err)
	}
	body, err := json.Marshal(outcome)
	if err != nil {
		return TurnToolOutcome{}, err
	}
	if err := r.finishTurnChild(parent, child, "completed", body, nil); err != nil {
		return TurnToolOutcome{}, err
	}
	return outcome, nil
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
	if child.Status == "unknown" {
		return TurnToolOutcome{}, fmt.Errorf("%w: child work %s has no committed tool outcome", ErrUnknownWork, child.ID)
	}
	provider, class, honors := "tool", WorkNonIdempotent, false
	if classifier, ok := x.(TurnDispatchClassifier); ok {
		provider, class, honors = classifier.ClassifyToolDispatch(effect, call)
	}
	meta := childMetadata(r, child, provider, class, honors)
	if err := r.register(meta); err != nil {
		return TurnToolOutcome{}, fmt.Errorf("register tool dispatch %s: %w", child.ID, err)
	}
	if child.Started && r.Dispatches != nil {
		receipt, found, lookupErr := r.Dispatches.Receipt(meta)
		if lookupErr != nil {
			return TurnToolOutcome{}, lookupErr
		}
		if found {
			var outcome TurnToolOutcome
			if err := json.Unmarshal(receipt.CanonicalOutcome, &outcome); err != nil {
				return TurnToolOutcome{}, fmt.Errorf("decode receipt outcome of %s: %w", child.ID, err)
			}
			if err := validateToolOutcome(call, outcome); err != nil {
				return TurnToolOutcome{}, fmt.Errorf("validate receipt outcome of %s: %w", child.ID, err)
			}
			if err := r.finishTurnChild(parent, child, "completed", receipt.CanonicalOutcome, nil); err != nil {
				return TurnToolOutcome{}, err
			}
			return outcome, nil
		}
	}
	if child.Started && (meta.WorkClass != WorkIdempotent || !meta.SupportsIdempotency) {
		return TurnToolOutcome{}, fmt.Errorf("%w: child work %s has no committed tool outcome", ErrUnknownWork, child.ID)
	}
	if !child.Started {
		if err := r.startTurnChild(parent, child); err != nil {
			return TurnToolOutcome{}, err
		}
	}
	var outcome TurnToolOutcome
	var receipt *DispatchReceipt
	var callErr error
	if dispatch, ok := x.(MetadataTurnExecutor); ok {
		outcome, receipt, callErr = dispatch.ExecuteTurnToolDispatch(ctx, effect, call, meta)
	} else {
		outcome, callErr = x.ExecuteTurnTool(ctx, effect, call)
	}
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
	if receipt != nil {
		receipt.CanonicalOutcome = body
		if err := r.recordReceipt(meta, receipt); err != nil {
			return TurnToolOutcome{}, fmt.Errorf("record tool receipt %s: %w", child.ID, err)
		}
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
			child := &turnChild{ID: id, ParentWorkID: event.Str("parent_work_id"), Kind: event.Str("child_kind"), Slot: event.Str("child_slot"), PreparedJSON: event.Str("request_json")}
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
	child := &turnChild{ID: id, ParentWorkID: parent.ID, Kind: kind, Slot: slot, PreparedJSON: prepared}
	provider, class, honors := "external", WorkNonIdempotent, false
	if classifier, ok := r.Executor.(TurnDispatchClassifier); ok {
		if kind == "model" {
			var request turn.Request
			if json.Unmarshal([]byte(prepared), &request) == nil {
				provider, class, honors = classifier.ClassifyModelDispatch(request)
			}
		}
	}
	meta := childMetadata(r, child, provider, class, honors)
	event := r.progressEvent(kernel.ExecWorkPrepared, map[string]any{
		"work_id": id, "parent_work_id": parent.ID, "work_scope": "turn_child",
		"source_seq": parent.SourceSeq, "child_kind": kind, "child_slot": slot,
		"request_json": prepared, "work_class": string(meta.WorkClass),
		"dispatch_key": string(meta.DispatchKey), "request_digest": string(meta.RequestDigest),
		"provider": meta.Provider,
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
