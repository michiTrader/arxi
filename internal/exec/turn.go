package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/turn"
)

const (
	maxNativeTurnRounds           = 64
	authorizationSuspensionSchema = "arxi.authorization-suspension/v1"
	// contextRequestSchema names the durable preparation request carried by the
	// context.* event payloads. The artifact schemas travel inside the pipeline
	// results so the event layer never parses the artifacts it stores.
	contextRequestSchema = "arxi.context-prepare/v1"
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

// ContextConfig gates durable context preparation (ADR-0013). The digest binds
// every prepared transcript to the exact accepted configuration, so a resume
// that loads a different effective configuration can never present that
// content to a model. Empty keeps the legacy path and records no context.*
// events, which is what keeps historical runs replaying under the behavior
// they were accepted with.
type ContextConfig struct {
	EffectiveConfigSHA string
	// PolicyVersion names the accepted context-preparation contract. It is
	// recorded in the artifact so an audit can tell which preparation rules
	// produced a presentation, rather than assuming today's rules applied.
	PolicyVersion string
}

// ContextPipeline is the seam for durable context preparation. Projection and
// preparation are provider-neutral domain work that exec must not implement
// itself: the runner orchestrates the durable barrier and verifies the exact
// bytes, while the pipeline supplies them from behind this interface.
type ContextPipeline interface {
	// Project renders the confirmed prefix into exact transcript artifact
	// bytes. It receives only confirmed events at or below Through.
	Project(req ContextProjection) (ContextTranscript, error)
	// Prepare freezes one presentation from a projected transcript. It must be
	// a pure function of its inputs so the same request always yields the same
	// bytes.
	Prepare(req ContextPreparation) (PreparedContext, error)
}

// ContextProjection names the confirmed inputs of one transcript projection.
type ContextProjection struct {
	RunID              string
	Subject            string
	EffectiveConfigSHA string
	Events             []kernel.Event
	Through            int64
}

// ContextTranscript is the projected artifact and the bindings the runner
// records beside it. JSON and Digest are the exact evidence.
type ContextTranscript struct {
	JSON                 string
	Digest               string
	Schema               string
	ProjectorVersion     string
	SourceFromSeq        int64
	SourceThroughEventID string
	ContentDigest        string
}

// ContextRoute is the non-secret destination a presentation is prepared for.
// The barrier records it and re-proves it before reuse: a committed
// presentation that gets dispatched to a different model is a different call
// than the one the run commissioned, and nothing else in the record would
// catch that.
type ContextRoute struct {
	Provider             string
	Protocol             string
	Model                string
	BaseURL              string
	ToolSchemaVersion    string
	ContextPolicyVersion string
}

// ContextPreparation freezes one presentation request for one parent work.
type ContextPreparation struct {
	ContextID          string
	RunID              string
	ParentWorkID       string
	EffectiveConfigSHA string
	Effect             kernel.SpawnTurn
	History            ContextTranscript
	Route              ContextRoute
}

// PreparedContext is the frozen presentation and every binding the barrier
// must verify before a model child may start.
type PreparedContext struct {
	JSON                    string
	Digest                  string
	Schema                  string
	PreparerVersion         string
	ContextID               string
	ParentWorkID            string
	Subject                 string
	SourceThroughSeq        int64
	ContentDigest           string
	PresentationDigest      string
	TranscriptContentDigest string
	Messages                []turn.Message
	// MeasurementJSON is the canonical per-layer token measurement the event
	// commits beside the artifact bytes, so the pressure record is durable
	// evidence rather than a recomputation.
	MeasurementJSON  string
	OverflowExceeded bool
	OverflowMode     string
	Compacted        bool
	CompactionDigest string
}

// ContextPreparedTurnExecutor lets the runner prepare a turn from the durable
// canonical transcript instead of the SpawnTurn context alone. Implementations
// receive the exact verified presentation and must not reorder, drop or
// reconstruct it. Executors without this optional method keep the legacy
// PrepareTurn path and record no context.* events.
type ContextPreparedTurnExecutor interface {
	PrepareTurnContext(ctx context.Context, e kernel.SpawnTurn, messages []turn.Message) (turn.Request, error)
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
	// Agent attributes this child's exact outcome to a subject in the
	// canonical transcript. The runtime, not the member, appends the record,
	// so without it a later turn cannot prove whose model output this was.
	Agent        string
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
	// agent, contextID and presentationDigest are bindings added to every model
	// child's exec.work_prepared while this turn runs. agent attributes exact
	// child results to a subject in the canonical transcript; contextID and
	// presentationDigest tie the dispatched request to one verified
	// context.prepared so a model call cannot start from unverified input.
	agent              string
	contextID          string
	presentationDigest string
}

func newDurableTurnProgress() durableTurnProgress {
	return durableTurnProgress{byID: map[string]*turnChild{}, bySlot: map[string]*turnChild{}}
}

// runDurableTurn reconstructs the transcript exclusively from committed child
// outcomes. A crash after a tool result therefore reinjects the same object and
// never calls the runner again.
//
// Progress is loaded before any request is built: recovery must inspect what is
// already committed before rebuilding input, or a changed environment could
// produce different request bytes for work that already has durable identity.
func (r *Runner) runDurableTurn(ctx context.Context, w Work, e kernel.SpawnTurn, x TurnExecutor) ([]kernel.Event, error) {
	progress, err := r.loadTurnProgress(w)
	if err != nil {
		return nil, NotDispatched(err)
	}
	req, err := r.prepareDurableTurn(ctx, w, e, x, &progress)
	if err != nil {
		return nil, err
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
				if !r.exactAuthorizationEnabled() {
					outcome, err := r.runStoppedToolChild(ctx, w, e, round, call, x, &progress)
					if err != nil {
						return nil, err
					}
					entry := TurnToolEntry{Call: call, Outcome: outcome}
					trace = append(trace, TurnEntry{Tool: &entry})
					return x.FinishTurn(e, trace)
				}
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

// prepareDurableTurn applies the ADR-0013 barrier: the confirmed prefix is
// projected once, the exact presentation is committed as context.prepared, and
// only then is a model request built from it. Recovery loads the committed
// artifact instead of preparing again, so a restart can never observe newer
// events, memory or tokenizer behavior and silently change an already
// commissioned call. Without the context gate or the optional executor method
// the legacy single-turn preparation runs unchanged.
func (r *Runner) prepareDurableTurn(ctx context.Context, w Work, e kernel.SpawnTurn, x TurnExecutor, progress *durableTurnProgress) (turn.Request, error) {
	progress.agent = e.Agent
	if r.Context.EffectiveConfigSHA == "" || r.Pipeline == nil {
		return x.PrepareTurn(ctx, e)
	}
	preparedExecutor, ok := x.(ContextPreparedTurnExecutor)
	if !ok {
		return x.PrepareTurn(ctx, e)
	}
	base, err := preparedExecutor.PrepareTurnContext(ctx, e, nil)
	if err != nil {
		return turn.Request{}, NotDispatched(fmt.Errorf("prepare native turn for %s: %w", e.Agent, err))
	}
	if base.Schema != turn.Schema {
		return turn.Request{}, NotDispatched(fmt.Errorf("prepare native turn for %s: schema %q, want %q", e.Agent, base.Schema, turn.Schema))
	}
	contextID := contextIdentity(r.RunID, w.ID)
	req, found, err := r.loadPreparedContext(contextID, w, e, base, progress)
	if err != nil || found {
		return req, err
	}
	events, err := r.Log.Read(1, 0)
	if err != nil {
		return turn.Request{}, NotDispatched(fmt.Errorf("read confirmed history for context %s: %w", contextID, err))
	}
	if len(events) == 0 || events[len(events)-1].Seq < w.SourceSeq {
		return turn.Request{}, NotDispatched(fmt.Errorf("context %s confirmed prefix does not contain source seq %d", contextID, w.SourceSeq))
	}
	history, err := r.Pipeline.Project(ContextProjection{RunID: r.RunID, Subject: e.Agent,
		EffectiveConfigSHA: r.Context.EffectiveConfigSHA, Events: events, Through: w.SourceSeq})
	if err != nil {
		return turn.Request{}, r.failContextPreparation(contextID, w, e, err)
	}
	artifact, err := r.Pipeline.Prepare(ContextPreparation{ContextID: contextID, RunID: r.RunID,
		ParentWorkID: w.ID, EffectiveConfigSHA: r.Context.EffectiveConfigSHA, Effect: e, History: history,
		Route: r.contextRoute(base)})
	if err != nil {
		return turn.Request{}, r.failContextPreparation(contextID, w, e, err)
	}
	if err := r.commitContextPreparation(contextID, w, e, history, artifact); err != nil {
		return turn.Request{}, err
	}
	progress.contextID, progress.presentationDigest = contextID, artifact.PresentationDigest
	base.Messages = artifact.Messages
	return base, nil
}

// loadPreparedContext reuses a committed context.prepared byte-for-byte and
// refuses anything that is not exactly the recorded value for this identity:
// two values for one context ID, digests that do not match their bytes, or a
// changed boundary would each let a restart present different input for work
// that already had one durable identity. A terminal prepare_failed is also
// final here: retrying a deterministically failed preparation would either
// repeat the same failure or, worse, succeed differently after the
// environment moved.
func (r *Runner) loadPreparedContext(contextID string, w Work, e kernel.SpawnTurn, base turn.Request, progress *durableTurnProgress) (turn.Request, bool, error) {
	events, err := r.Log.Read(1, 0)
	if err != nil {
		return turn.Request{}, false, NotDispatched(fmt.Errorf("read confirmed history for context %s: %w", contextID, err))
	}
	found := false
	var req turn.Request
	for _, event := range events {
		if event.Str("context_id") != contextID {
			continue
		}
		switch event.Type {
		case kernel.ContextPrepared:
			if found {
				return turn.Request{}, false, NotDispatched(fmt.Errorf("context %s has conflicting prepared records: one context identity is one exact value", contextID))
			}
			artifact, verifyErr := verifyPreparedContext(event, contextID, w, e, base)
			if verifyErr != nil {
				return turn.Request{}, false, verifyErr
			}
			progress.contextID, progress.presentationDigest = contextID, artifact.PresentationDigest
			req = base
			req.Messages = artifact.Messages
			found = true
		case kernel.ContextPrepareFailed:
			return turn.Request{}, false, NotDispatched(fmt.Errorf("context %s preparation failed terminally: %s", contextID, event.Str("error")))
		}
	}
	return req, found, nil
}

// preparedContextRecord decodes exactly the fields the barrier must verify.
// It is declared locally so the runner never imports the artifact packages:
// the wire shape is the runner's contract, not the projector's type.
type preparedContextRecord struct {
	Schema             string `json:"schema"`
	ContextID          string `json:"context_id"`
	ParentWorkID       string `json:"parent_work_id"`
	Subject            string `json:"subject_agent"`
	SourceThroughSeq   int64  `json:"source_through_seq"`
	ContentDigest      string `json:"content_digest"`
	PresentationDigest string `json:"presentation_digest"`
	Transcript         struct {
		ContentDigest string `json:"content_digest"`
	} `json:"transcript"`
	Route struct {
		Provider string `json:"provider,omitempty"`
		Protocol string `json:"protocol,omitempty"`
		Model    string `json:"model,omitempty"`
	} `json:"route"`
	Messages    []turn.Message  `json:"messages"`
	Measurement json.RawMessage `json:"token_measurement"`
	Overflow    struct {
		Exceeded         bool   `json:"exceeded"`
		Mode             string `json:"mode,omitempty"`
		Compacted        bool   `json:"compacted"`
		CompactionDigest string `json:"compaction_digest,omitempty"`
	} `json:"overflow_decision"`
	Compaction *struct {
		ContentDigest string `json:"content_digest"`
	} `json:"compaction,omitempty"`
}

// verifyPreparedContext proves the exact recorded bytes before anything built
// from them may dispatch. The byte digests are the integrity binding; the
// field checks reject a prepared record whose identity was reused for another
// boundary, subject or parent work.
func verifyPreparedContext(event kernel.Event, contextID string, w Work, e kernel.SpawnTurn, base turn.Request) (preparedContextRecord, error) {
	var artifact preparedContextRecord
	transcriptJSON, transcriptDigest := event.Str("transcript_json"), event.Str("transcript_digest")
	preparedJSON, preparedDigest := event.Str("prepared_context_json"), event.Str("prepared_context_digest")
	if byteDigest([]byte(transcriptJSON)) != transcriptDigest {
		return artifact, NotDispatched(fmt.Errorf("context %s transcript bytes do not match their persisted digest", contextID))
	}
	if byteDigest([]byte(preparedJSON)) != preparedDigest {
		return artifact, NotDispatched(fmt.Errorf("context %s prepared-context bytes do not match their persisted digest", contextID))
	}
	if err := json.Unmarshal([]byte(preparedJSON), &artifact); err != nil {
		return artifact, NotDispatched(fmt.Errorf("context %s prepared-context bytes are not decodable: %w", contextID, err))
	}
	switch {
	case artifact.Schema == "":
		return artifact, NotDispatched(fmt.Errorf("context %s prepared record carries no schema", contextID))
	case artifact.ContextID != contextID || artifact.ParentWorkID != w.ID || artifact.Subject != e.Agent:
		return artifact, NotDispatched(fmt.Errorf("context %s identity does not match this turn's parent work and subject", contextID))
	case artifact.SourceThroughSeq != w.SourceSeq:
		return artifact, NotDispatched(fmt.Errorf("context %s boundary seq %d does not match source seq %d", contextID, artifact.SourceThroughSeq, w.SourceSeq))
	case artifact.PresentationDigest != event.Str("presentation_digest") || artifact.ContentDigest != event.Str("content_digest"):
		return artifact, NotDispatched(fmt.Errorf("context %s internal digests disagree with the committed record", contextID))
	case artifact.Transcript.ContentDigest == "":
		return artifact, NotDispatched(fmt.Errorf("context %s transcript binding is absent", contextID))
	case len(artifact.Messages) == 0:
		return artifact, NotDispatched(fmt.Errorf("context %s presentation carries no messages", contextID))
	}
	var history struct {
		ContentDigest string `json:"content_digest"`
	}
	if err := json.Unmarshal([]byte(transcriptJSON), &history); err != nil {
		return artifact, NotDispatched(fmt.Errorf("context %s transcript bytes are not decodable: %w", contextID, err))
	}
	if history.ContentDigest != artifact.Transcript.ContentDigest {
		return artifact, NotDispatched(fmt.Errorf("context %s transcript digest disagrees with the prepared artifact", contextID))
	}
	// A presentation is prepared for one destination. Reusing it against a
	// route that now resolves elsewhere would send commissioned input to a
	// model that was never authorized to see it, and every other digest in the
	// record would still verify.
	if artifact.Route.Provider != base.Provider || artifact.Route.Protocol != base.Protocol || artifact.Route.Model != base.Model {
		return artifact, NotDispatched(fmt.Errorf("context %s was prepared for %s/%s/%s but this turn resolves to %s/%s/%s",
			contextID, artifact.Route.Provider, artifact.Route.Protocol, artifact.Route.Model,
			base.Provider, base.Protocol, base.Model))
	}
	if artifact.Overflow.Exceeded != eventBool(event, "overflow_exceeded") || artifact.Overflow.Mode != event.Str("overflow_mode") ||
		artifact.Overflow.Compacted != eventBool(event, "compacted") {
		return artifact, NotDispatched(fmt.Errorf("context %s overflow decision disagrees with the committed record", contextID))
	}
	if string(artifact.Measurement) != event.Str("token_measurement") {
		return artifact, NotDispatched(fmt.Errorf("context %s token measurement disagrees with the committed record", contextID))
	}
	if artifact.Overflow.Compacted {
		switch {
		case artifact.Overflow.CompactionDigest == "" || artifact.Overflow.CompactionDigest != event.Str("compaction_digest"):
			return artifact, NotDispatched(fmt.Errorf("context %s compaction binding disagrees with the committed record", contextID))
		case artifact.Compaction == nil || artifact.Compaction.ContentDigest != artifact.Overflow.CompactionDigest:
			return artifact, NotDispatched(fmt.Errorf("context %s embedded compaction digest disagrees with the overflow decision", contextID))
		}
	} else if artifact.Overflow.CompactionDigest != "" {
		return artifact, NotDispatched(fmt.Errorf("context %s names a compaction it never committed", contextID))
	}
	return artifact, nil
}

// eventBool reads a boolean payload field. The barrier commits these fields
// itself, so a missing or mistyped value is simply false — and the agreement
// check above refuses any artifact that claims otherwise.
func eventBool(event kernel.Event, key string) bool {
	value, _ := event.Payload[key].(bool)
	return value
}

// commitContextPreparation appends the request and the exact artifacts in one
// batch. One confirmed append is what makes the barrier real: there is no
// window in which a request exists whose inputs were never frozen, and no
// window in which an artifact is authoritative without its bytes.
func (r *Runner) commitContextPreparation(contextID string, w Work, e kernel.SpawnTurn, history ContextTranscript, artifact PreparedContext) error {
	requested := r.progressEvent(kernel.ContextPrepareRequested, map[string]any{
		"schema": contextRequestSchema, "context_id": contextID, "parent_work_id": w.ID,
		"agent": e.Agent, "source_from_seq": history.SourceFromSeq, "source_through_seq": w.SourceSeq,
		"source_through_event_id": history.SourceThroughEventID, "effective_config_sha": r.Context.EffectiveConfigSHA,
		"projector_version": history.ProjectorVersion, "preparer_version": artifact.PreparerVersion,
	}, w.Source)
	prepared := r.progressEvent(kernel.ContextPrepared, map[string]any{
		"schema": contextRequestSchema, "context_id": contextID, "parent_work_id": w.ID,
		"agent": e.Agent, "source_from_seq": history.SourceFromSeq, "source_through_seq": w.SourceSeq,
		"source_through_event_id": history.SourceThroughEventID, "effective_config_sha": r.Context.EffectiveConfigSHA,
		"projector_version": history.ProjectorVersion, "preparer_version": artifact.PreparerVersion,
		"transcript_schema": history.Schema, "transcript_json": history.JSON, "transcript_digest": history.Digest,
		"prepared_context_schema": artifact.Schema, "prepared_context_json": artifact.JSON, "prepared_context_digest": artifact.Digest,
		"content_digest": artifact.ContentDigest, "presentation_digest": artifact.PresentationDigest,
		"token_measurement": artifact.MeasurementJSON, "overflow_exceeded": artifact.OverflowExceeded,
		"overflow_mode": artifact.OverflowMode, "compacted": artifact.Compacted, "compaction_digest": artifact.CompactionDigest,
	}, w.Source)
	if _, err := r.Log.Append(r.stamp([]kernel.Event{requested, prepared})); err != nil {
		return fmt.Errorf("persist context preparation %s: %w", contextID, err)
	}
	return nil
}

// failContextPreparation records a terminal, deterministic preparation failure.
// It is written only for failures that would repeat identically on retry, so
// recovery can refuse the request instead of half-preparing a different call.
// The failure class comes from the error itself when it carries one (the
// overflow path reports "compaction"), so the runner never imports the
// artifact packages to learn it.
func (r *Runner) failContextPreparation(contextID string, w Work, e kernel.SpawnTurn, cause error) error {
	event := r.progressEvent(kernel.ContextPrepareFailed, map[string]any{
		"schema": contextRequestSchema, "context_id": contextID, "parent_work_id": w.ID,
		"agent": e.Agent, "source_through_seq": w.SourceSeq, "effective_config_sha": r.Context.EffectiveConfigSHA,
		"failure_class": preparationClass(cause), "error": cause.Error(),
	}, w.Source)
	if _, err := r.Log.Append(r.stamp([]kernel.Event{event})); err != nil {
		return fmt.Errorf("persist context preparation failure %s: %w", contextID, err)
	}
	return NotDispatched(fmt.Errorf("prepare context %s for %s: %w", contextID, e.Agent, cause))
}

// classifiedPreparationError is satisfied by pipeline errors that name their
// own durable failure class. Satisfaction is implicit: exec declares the
// method, the domain package's error carries it, and no import direction
// bends.
type classifiedPreparationError interface {
	PreparationClass() string
}

func preparationClass(err error) string {
	var classified classifiedPreparationError
	if errors.As(err, &classified) {
		return classified.PreparationClass()
	}
	return "preparation"
}

// contextRoute reads the destination from the route the executor already
// froze for this turn. Resolving it a second time here would let the artifact
// record a route the request never used.
func (r *Runner) contextRoute(base turn.Request) ContextRoute {
	return ContextRoute{Provider: base.Provider, Protocol: base.Protocol, Model: base.Model,
		BaseURL: base.BaseURL, ToolSchemaVersion: r.Authorization.ToolSchemaVersion,
		ContextPolicyVersion: r.Context.PolicyVersion}
}

// contextIdentity derives one stable identity per parent work. The parent work
// ID already binds run, source event, effect index and effect bytes, so the
// same preparation attempt always recomputes the same identity and a retry
// cannot fork it.
func contextIdentity(runID, parentWorkID string) string {
	return "context-" + byteDigest([]byte(runID+"\x00"+parentWorkID))
}

func byteDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
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

func (r *Runner) exactAuthorizationEnabled() bool {
	cfg := r.Authorization
	return r.JobID != "" && r.RunID != "" && cfg.ToolSchemaVersion != "" && cfg.PolicyVersion != "" &&
		cfg.WorkspaceProfileID != "" && cfg.TTLMS > 0 && r.Now != nil
}

func (r *Runner) resumeAuthorization(ctx context.Context, resumeWork Work, effect kernel.ResumeAuthorization) ([]kernel.Event, error) {
	events, err := r.Log.Read(1, 0)
	if err != nil {
		return nil, NotDispatched(fmt.Errorf("read exact authorization history: %w", err))
	}
	if len(events) == 0 || events[len(events)-1].Seq <= 0 {
		return nil, NotDispatched(fmt.Errorf("authorization %s has no confirmed history", effect.AuthorizationID))
	}
	verifiedSeq := events[len(events)-1].Seq
	state, _ := kernel.Fold(kernel.State{}, events, r.Config)
	a := state.Authorization(effect.AuthorizationID)
	if a == nil || a.Schema != "arxi.authorization/v1" || a.SuspensionID != effect.SuspensionID ||
		a.ActionDigest != effect.ActionDigest || a.Decision != "granted" || a.GrantEventID == "" {
		return nil, NotDispatched(fmt.Errorf("authorization %s is not a current exact grant", effect.AuthorizationID))
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
	if a.ConsumingWorkID != "" && a.ConsumingWorkID != child.ID {
		return nil, NotDispatched(fmt.Errorf("authorization %s was consumed by different work %s", a.ID, a.ConsumingWorkID))
	}
	if a.ConsumingWorkID != "" && !child.Started {
		return nil, NotDispatched(fmt.Errorf("authorization %s consumption has no matching started child", a.ID))
	}
	if child.Started {
		if child.Status == "unknown" {
			return nil, fmt.Errorf("%w: authorized child %s has a durable unknown outcome", ErrUnknownWork, child.ID)
		}
		meta := childMetadata(r, &child, "tool", WorkNonIdempotent, false)
		if r.Dispatches != nil {
			receipt, found, lookupErr := r.Dispatches.Receipt(meta)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if found {
				var outcome TurnToolOutcome
				if err := json.Unmarshal(receipt.CanonicalOutcome, &outcome); err != nil {
					return nil, NotDispatched(fmt.Errorf("decode authorized tool receipt %s: %w", child.ID, err))
				}
				if err := validateToolOutcome(suspension.Call, outcome); err != nil {
					return nil, NotDispatched(err)
				}
				if err := r.finishTurnChild(Work{ID: suspension.ParentWorkID, Source: resumeWork.Source}, &child, "completed", receipt.CanonicalOutcome, nil); err != nil {
					return nil, err
				}
				child.Status, child.ResultJSON = "completed", string(receipt.CanonicalOutcome)
				return r.continueAuthorization(ctx, resumeWork, suspension, child)
			}
		}
		if err := r.finishTurnChild(Work{ID: suspension.ParentWorkID, Source: resumeWork.Source}, &child, "unknown", nil,
			fmt.Errorf("process stopped after authorized dispatch began and before a terminal outcome was committed")); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: authorized child %s started without a committed outcome", ErrUnknownWork, child.ID)
	}
	authorized, ok := r.Executor.(AuthorizedTurnToolExecutor)
	if !ok {
		return nil, NotDispatched(fmt.Errorf("executor cannot dispatch exact authorized native tools"))
	}
	if a.ConsumingWorkID != "" {
		return nil, NotDispatched(fmt.Errorf("authorization %s was already consumed", a.ID))
	}
	if err := r.register(childMetadata(r, &child, "tool", WorkNonIdempotent, false)); err != nil {
		return nil, NotDispatched(fmt.Errorf("register authorized tool dispatch %s: %w", child.ID, err))
	}
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
			if _, err := r.Log.AppendIfSeq(verifiedSeq, r.stamp([]kernel.Event{expired})); err != nil {
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
	if _, err := r.Log.AppendIfSeq(verifiedSeq, r.stamp([]kernel.Event{consumed, started})); err != nil {
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
	meta := childMetadata(r, &child, "tool", WorkNonIdempotent, false)
	if err := r.recordReceipt(meta, &DispatchReceipt{Status: OutcomeSucceeded, CanonicalOutcome: body}); err != nil {
		return nil, fmt.Errorf("record authorized tool receipt %s: %w", child.ID, err)
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
	digest := exactActionDigest(s.JobID, s.RunID, s.RequesterPrincipal, s.ParentWorkID, s.Call.ID,
		s.Call.Name, s.Call.ArgumentDigest, s.ToolSchemaVersion, s.PolicyVersion, s.WorkspaceProfileID)
	if digest != a.ActionDigest {
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
	actionDigest := exactActionDigest(r.JobID, r.RunID, "agent:"+effect.Agent, parent.ID, call.ID, call.Name,
		call.ArgumentDigest, cfg.ToolSchemaVersion, cfg.PolicyVersion, cfg.WorkspaceProfileID)
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
		PolicyVersion: cfg.PolicyVersion, WorkspaceProfileID: cfg.WorkspaceProfileID, ActionDigest: actionDigest,
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
			"argument_digest": call.ArgumentDigest, "action_digest": actionDigest,
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
			child := &turnChild{ID: id, ParentWorkID: event.Str("parent_work_id"), Kind: event.Str("child_kind"), Slot: event.Str("child_slot"), Agent: event.Str("agent"), PreparedJSON: event.Str("request_json")}
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
	child := &turnChild{ID: id, ParentWorkID: parent.ID, Kind: kind, Slot: slot, Agent: progress.agent, PreparedJSON: prepared}
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
	payload := map[string]any{
		"work_id": id, "parent_work_id": parent.ID, "work_scope": "turn_child",
		"source_seq": parent.SourceSeq, "child_kind": kind, "child_slot": slot,
		"request_json": prepared, "work_class": string(meta.WorkClass),
		"dispatch_key": string(meta.DispatchKey), "request_digest": string(meta.RequestDigest),
		"provider": meta.Provider,
	}
	// agent attributes exact child results to a subject in the canonical
	// transcript; context bindings tie the model request to one verified
	// context.prepared. A model child without its context binding can only come
	// from a legacy turn, which is exactly when no such proof exists.
	if progress.agent != "" {
		payload["agent"] = progress.agent
	}
	if kind == "model" && progress.contextID != "" {
		payload["context_id"] = progress.contextID
		payload["presentation_digest"] = progress.presentationDigest
	}
	event := r.progressEvent(kernel.ExecWorkPrepared, payload, parent.Source)
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
	// The finish record is what the canonical transcript projects exact native
	// output from, so it carries the subject attribution the prepared record
	// froze.
	if child.Agent != "" {
		payload["agent"] = child.Agent
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
