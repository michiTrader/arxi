package exec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/compaction"
	"github.com/michiTrader/arxi/internal/contextprep"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

// domainPipeline mirrors the contextruntime adapter inside the package's own
// tests. The concrete adapter cannot be imported here (it imports exec), but
// the domain packages are pure leaves, so the same bytes are produced without
// a cycle.
type domainPipeline struct{}

func (domainPipeline) Project(req ContextProjection) (ContextTranscript, error) {
	history, err := transcript.Project(req.RunID, req.Subject, req.EffectiveConfigSHA, req.Events, req.Through)
	if err != nil {
		return ContextTranscript{}, err
	}
	body, err := json.Marshal(history)
	if err != nil {
		return ContextTranscript{}, err
	}
	return ContextTranscript{JSON: string(body), Digest: byteDigest(body), Schema: history.Schema,
		ProjectorVersion: history.ProjectorVersion, SourceFromSeq: history.SourceFromSeq,
		SourceThroughEventID: history.SourceThroughEventID, ContentDigest: history.ContentDigest}, nil
}

func (domainPipeline) Prepare(req ContextPreparation) (PreparedContext, error) {
	var history transcript.Artifact
	if err := json.Unmarshal([]byte(req.History.JSON), &history); err != nil {
		return PreparedContext{}, err
	}
	artifact, err := contextprep.Prepare(req.ContextID, req.RunID, req.ParentWorkID, req.EffectiveConfigSHA,
		req.Effect, history, compaction.Extractive{})
	if err != nil {
		return PreparedContext{}, err
	}
	body, err := json.Marshal(artifact)
	if err != nil {
		return PreparedContext{}, err
	}
	measurement, err := json.Marshal(artifact.Measurement)
	if err != nil {
		return PreparedContext{}, err
	}
	return PreparedContext{JSON: string(body), Digest: byteDigest(body), Schema: artifact.Schema,
		PreparerVersion: artifact.PreparerVersion, ContextID: artifact.ContextID,
		ParentWorkID: artifact.ParentWorkID, Subject: artifact.Subject,
		SourceThroughSeq: artifact.SourceThroughSeq, ContentDigest: artifact.ContentDigest,
		PresentationDigest: artifact.PresentationDigest, TranscriptContentDigest: artifact.Transcript.ContentDigest,
		Messages: artifact.Messages, MeasurementJSON: string(measurement),
		OverflowExceeded: artifact.Overflow.Exceeded, OverflowMode: artifact.Overflow.Mode,
		Compacted: artifact.Overflow.Compacted, CompactionDigest: artifact.Overflow.CompactionDigest}, nil
}

// contextTurnExecutor counts route preparation so a test can tell a recovered
// route build (cheap, in-memory, allowed on every attempt) from a redispatched
// model call (never allowed once children are committed).
type contextTurnExecutor struct {
	nativeLoopExecutor
	prepareCalls int
}

// FinishTurn projects the trace's tool evidence into domain events the way
// provider.Executor.FinishTurn does, so the canonical transcript sees the same
// committed facts a real turn would confirm.
func (x *contextTurnExecutor) FinishTurn(e kernel.SpawnTurn, trace []TurnEntry) ([]kernel.Event, error) {
	events := []kernel.Event{{Type: kernel.AgentActivated, Actor: e.Agent}}
	for _, entry := range trace {
		if entry.Tool == nil {
			continue
		}
		call, outcome := entry.Tool.Call, entry.Tool.Outcome
		events = append(events, kernel.Event{Type: kernel.ToolCall, Source: kernel.SourceAgent, Actor: e.Agent,
			Payload: map[string]any{"tool": call.Name, "call_id": call.ID, "args": json.RawMessage(call.Arguments)}})
		if outcome.Policy == "allow" {
			text := ""
			for _, block := range outcome.Result.Content {
				if block.Type == turn.BlockText {
					text = block.Text
				}
			}
			events = append(events, kernel.Event{Type: kernel.ToolCallCompleted, Source: kernel.SourceAgent, Actor: e.Agent,
				Payload: map[string]any{"tool": call.Name, "call_id": call.ID, "result": text}})
		} else {
			events = append(events, kernel.Event{Type: kernel.ToolCallDenied, Source: kernel.SourceAgent, Actor: e.Agent,
				Payload: map[string]any{"tool": call.Name, "call_id": call.ID, "policy": outcome.Policy}})
		}
	}
	events = append(events,
		kernel.Event{Type: kernel.LLMResponse, Actor: e.Agent, Payload: map[string]any{"ok": true, "text": "done"}},
		kernel.Event{Type: kernel.AgentTurnDone, Actor: e.Agent})
	return events, nil
}

func (x *contextTurnExecutor) PrepareTurnContext(ctx context.Context, e kernel.SpawnTurn, messages []turn.Message) (turn.Request, error) {
	x.prepareCalls++
	req, err := x.nativeLoopExecutor.PrepareTurn(ctx, e)
	if err != nil {
		return turn.Request{}, err
	}
	req.Messages = messages
	return req, nil
}

func contextTestWork(t *testing.T, r *Runner, effect kernel.SpawnTurn) Work {
	t.Helper()
	events, err := r.Log.Read(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	work, err := manifest(r.RunID, events[0], []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	return work[0]
}

// contextTestRunner enables the durable barrier over the shared exact runner.
func contextTestRunner(log *memLog, x Executor) *Runner {
	r := exactTestRunner(log, x)
	r.Context = ContextConfig{EffectiveConfigSHA: "cfg"}
	r.Pipeline = domainPipeline{}
	return r
}

func countContextEvents(events []kernel.Event) (requested, prepared int) {
	for _, event := range events {
		switch event.Type {
		case kernel.ContextPrepareRequested:
			requested++
		case kernel.ContextPrepared:
			prepared++
		}
	}
	return requested, prepared
}

func TestDurableTurnCommitsContextBarrierAndReusesItOnRecovery(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatalf("first durable turn failed: %v", err)
	}
	if x.prepareCalls != 1 {
		t.Fatalf("route prepared %d times, want 1: the first attempt builds its route exactly once", x.prepareCalls)
	}
	events, _ := log.Read(1, 0)
	requested, prepared := countContextEvents(events)
	if requested != 1 || prepared != 1 {
		t.Fatalf("context events = %d requested, %d prepared: the barrier is one request and one prepared record per turn", requested, prepared)
	}
	for _, event := range events {
		if event.Type == kernel.ContextPrepared &&
			(event.Str("transcript_json") == "" || event.Str("prepared_context_json") == "" ||
				event.Str("transcript_digest") == "" || event.Str("prepared_context_digest") == "") {
			t.Fatalf("context.prepared payload = %#v: exact bytes and digests are the whole proof of what was presented", event.Payload)
		}
	}

	// A restarted attempt reuses the committed record: the route is rebuilt
	// (stateless), but no new context record may appear and no model call may
	// dispatch again, because every child outcome is already committed.
	x2 := &contextTurnExecutor{}
	r2 := contextTestRunner(log, x2)
	if _, err := r2.runDurableTurn(context.Background(), work, effect, x2); err != nil {
		t.Fatalf("recovery refused to reuse the committed prepared context: %v", err)
	}
	events2, _ := log.Read(1, 0)
	requested2, prepared2 := countContextEvents(events2)
	if requested2 != 1 || prepared2 != 1 {
		t.Fatalf("after recovery context events = %d requested, %d prepared: re-preparing could observe newer events and silently change an already commissioned call", requested2, prepared2)
	}
	if len(x2.requests) != 0 {
		t.Fatalf("recovery dispatched %d model calls: committed child outcomes must be reused byte-for-byte", len(x2.requests))
	}
}

func TestDurableTurnModelChildBindsContextIdentity(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatal(err)
	}
	events, _ := log.Read(1, 0)
	bound := false
	for _, event := range events {
		if event.Type == kernel.ExecWorkPrepared && event.Str("child_kind") == "model" {
			if event.Str("context_id") == "" || event.Str("presentation_digest") == "" || event.Str("agent") == "" {
				t.Fatalf("model child bindings = %#v: a model request without its context binding could dispatch unverified input", event.Payload)
			}
			bound = true
		}
	}
	if !bound {
		t.Fatal("no model child was prepared: the turn never reached the provider loop")
	}
}

func TestDurableTurnRefusesTamperedPreparedContext(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatal(err)
	}
	for _, event := range log.events {
		if event.Type == kernel.ContextPrepared {
			event.Payload["transcript_json"] = `{"schema":"arxi.transcript/v1","tampered":true}`
		}
	}
	x2 := &contextTurnExecutor{}
	r2 := contextTestRunner(log, x2)
	_, err := r2.runDurableTurn(context.Background(), work, effect, x2)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered transcript error = %v: recovery must fail closed before model dispatch when bytes do not match their digest", err)
	}
}

func TestDurableTurnRefusesConflictingPreparedRecords(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatal(err)
	}
	var contextID string
	for _, event := range log.events {
		if event.Type == kernel.ContextPrepared {
			contextID = event.Str("context_id")
		}
	}
	duplicate := r.progressEvent(kernel.ContextPrepared, map[string]any{
		"schema": "arxi.context-prepare/v1", "context_id": contextID, "parent_work_id": work.ID,
		"agent": effect.Agent, "source_through_seq": work.SourceSeq,
		"transcript_json": "{}", "transcript_digest": "0", "prepared_context_json": "{}",
		"prepared_context_digest": "0", "content_digest": "0", "presentation_digest": "0",
	}, work.Source)
	if _, err := log.Append(r.stamp([]kernel.Event{duplicate})); err != nil {
		t.Fatal(err)
	}
	x2 := &contextTurnExecutor{}
	r2 := contextTestRunner(log, x2)
	_, err := r2.runDurableTurn(context.Background(), work, effect, x2)
	if err == nil || !strings.Contains(err.Error(), "conflicting prepared records") {
		t.Fatalf("duplicate context error = %v: one context identity must be one exact value or recovery could pick either", err)
	}
}

func TestDurableTurnRefusesTerminalPreparationFailure(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	events, _ := log.Read(1, 0)
	contextID := contextIdentity(r.RunID, work.ID)
	failed := r.progressEvent(kernel.ContextPrepareFailed, map[string]any{
		"schema": "arxi.context-prepare/v1", "context_id": contextID, "parent_work_id": work.ID,
		"agent": effect.Agent, "source_through_seq": work.SourceSeq,
		"failure_class": "preparation", "error": "context limit exceeded",
	}, events[0])
	if _, err := log.Append(r.stamp([]kernel.Event{failed})); err != nil {
		t.Fatal(err)
	}
	x2 := &contextTurnExecutor{}
	r2 := contextTestRunner(log, x2)
	_, err := r2.runDurableTurn(context.Background(), work, effect, x2)
	if err == nil || !strings.Contains(err.Error(), "failed terminally") {
		t.Fatalf("terminal failure error = %v: retrying a deterministic preparation failure would either repeat it or succeed differently", err)
	}
}

func TestDurableTurnLegacyPathRecordsNoContextEvents(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := exactTestRunner(log, x)
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatal(err)
	}
	events, _ := log.Read(1, 0)
	for _, event := range events {
		switch event.Type {
		case kernel.ContextPrepareRequested, kernel.ContextPrepared, kernel.ContextPrepareFailed:
			t.Fatalf("legacy turn recorded %s: runs accepted without the context contract must keep their historical behavior", event.Type)
		}
	}
	if len(x.requests) == 0 {
		t.Fatal("legacy turn never dispatched a model request")
	}
	// Sanity: the marshaled model request stays decodable, mirroring what the
	// child record persists for recovery.
	body, err := json.Marshal(x.requests[0])
	if err != nil || len(body) == 0 {
		t.Fatalf("marshal prepared request: %v", err)
	}
}

// compactedHistoryFixture commits a first turn's evidence — opening prompt,
// tool call/result pair and model output — so the second turn has inherited
// history a compaction can actually shed. It returns the log with the first
// turn's domain events appended.
func compactedHistoryFixture(t *testing.T) (*memLog, *Runner) {
	t.Helper()
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	log.events[0].Payload["prompt"] = strings.Repeat("keep the billing migration reversible ", 20)
	first := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, first)
	final, err := r.runDurableTurn(context.Background(), work, first, x)
	if err != nil {
		t.Fatalf("first turn failed: %v", err)
	}
	if _, err := log.Append(r.stamp(final)); err != nil {
		t.Fatal(err)
	}
	return log, r
}

// secondTurnWork opens the second cause the way the runner would after the
// follow-up input is confirmed.
func secondTurnWork(t *testing.T, r *Runner) (Work, kernel.SpawnTurn) {
	t.Helper()
	if _, err := r.Log.Append(r.stamp([]kernel.Event{{ID: "follow-up", Type: kernel.RunPrompt,
		Source: kernel.SourceHuman, Payload: map[string]any{"text": "continue"}}})); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read(1, 0)
	effect := kernel.SpawnTurn{Agent: "backend", Context: kernel.ContextSpec{MaxTokens: 600, OnOverflow: "summarize"}}
	works, err := manifest(r.RunID, events[len(events)-1], []kernel.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	return works[0], effect
}

// TestDurableTurnCommitsOverflowDecisionAndReusesIt is Phase 6's barrier
// evidence: a turn over a known limit commits the overflow decision, the
// measurement and the compaction digest in the same batch, and recovery
// reuses that record without re-running the generator.
func TestDurableTurnCommitsOverflowDecisionAndReusesIt(t *testing.T) {
	log, r := compactedHistoryFixture(t)
	x := &contextTurnExecutor{}
	work, effect := secondTurnWork(t, r)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatalf("compacted durable turn failed: %v", err)
	}
	var prepared kernel.Event
	for _, event := range log.events {
		if event.Type == kernel.ContextPrepared && event.Str("context_id") == contextIdentity(r.RunID, work.ID) {
			prepared = event
		}
	}
	if prepared.Str("context_id") == "" {
		t.Fatal("no committed context.prepared: the turn never reached the barrier")
	}
	if eventBool(prepared, "overflow_exceeded") != true || eventBool(prepared, "compacted") != true || prepared.Str("overflow_mode") != "summarize" {
		t.Fatalf("overflow payload = %#v: measured pressure must be recorded with the mode that governed it", prepared.Payload)
	}
	if prepared.Str("compaction_digest") == "" || prepared.Str("token_measurement") == "" {
		t.Fatalf("compaction digest %q, measurement %q: the committed record must carry the exact accounting beside the artifact bytes",
			prepared.Str("compaction_digest"), prepared.Str("token_measurement"))
	}
	var artifact struct {
		Compaction *struct {
			ContentDigest string `json:"content_digest"`
		} `json:"compaction"`
	}
	if err := json.Unmarshal([]byte(prepared.Str("prepared_context_json")), &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Compaction == nil || artifact.Compaction.ContentDigest != prepared.Str("compaction_digest") {
		t.Fatalf("embedded compaction = %#v: the overflow decision must bind the exact embedded artifact", artifact.Compaction)
	}

	// Recovery reuses the whole record — compaction included — without new
	// context events and without dispatching anything again.
	x2 := &contextTurnExecutor{}
	r2 := contextTestRunner(log, x2)
	if _, err := r2.runDurableTurn(context.Background(), work, effect, x2); err != nil {
		t.Fatalf("recovery refused the compacted record: %v", err)
	}
	requested, preparedCount := countContextEvents(log.events)
	if requested != 2 || preparedCount != 2 {
		t.Fatalf("context events after recovery = %d/%d: re-compacting could observe newer events and silently change an already commissioned call", requested, preparedCount)
	}
	if len(x2.requests) != 0 {
		t.Fatalf("recovery dispatched %d model calls: committed child outcomes must be reused byte-for-byte", len(x2.requests))
	}
}

func TestDurableTurnRefusesTamperedCompactionBinding(t *testing.T) {
	log, r := compactedHistoryFixture(t)
	x := &contextTurnExecutor{}
	work, effect := secondTurnWork(t, r)
	if _, err := r.runDurableTurn(context.Background(), work, effect, x); err != nil {
		t.Fatal(err)
	}
	tampered := false
	for _, event := range log.events {
		if event.Type == kernel.ContextPrepared && event.Str("context_id") == contextIdentity(r.RunID, work.ID) {
			event.Payload["compaction_digest"] = "deadbeef"
			tampered = true
		}
	}
	if !tampered {
		t.Fatal("no committed context.prepared to tamper with")
	}
	x2 := &contextTurnExecutor{}
	r2 := contextTestRunner(log, x2)
	_, err := r2.runDurableTurn(context.Background(), work, effect, x2)
	if err == nil || !strings.Contains(err.Error(), "compaction binding") {
		t.Fatalf("tampered compaction error = %v: recovery must fail closed before model dispatch when the compaction binding disagrees", err)
	}
}

// classifiedPrepareError mimics the overflow-path errors the domain packages
// return: the class travels on the error, not through an import.
type classifiedPrepareError struct{}

func (classifiedPrepareError) Error() string { return "the static layer alone exceeds the limit" }

func (classifiedPrepareError) PreparationClass() string { return "compaction" }

type failingPipeline struct{ domainPipeline }

func (failingPipeline) Prepare(ContextPreparation) (PreparedContext, error) {
	return PreparedContext{}, classifiedPrepareError{}
}

func TestDurableTurnPreparationFailureClassifiesCompaction(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	r.Pipeline = failingPipeline{}
	effect := kernel.SpawnTurn{Agent: "backend"}
	work := contextTestWork(t, r, effect)
	_, err := r.runDurableTurn(context.Background(), work, effect, x)
	if err == nil {
		t.Fatal("failing pipeline prepared anyway")
	}
	var failed *kernel.Event
	for i, event := range log.events {
		if event.Type == kernel.ContextPrepareFailed {
			failed = &log.events[i]
		}
	}
	if failed == nil {
		t.Fatal("no context.prepare_failed record: a failed preparation must be terminal and visible")
	}
	if failed.Str("failure_class") != "compaction" {
		t.Fatalf("failure class = %q: the overflow path must be distinguishable from projection failures without importing the artifact packages", failed.Str("failure_class"))
	}
}

// TestLaterTurnReceivesPriorConversationAndToolEvidence is Phase 5's exit
// evidence: a second turn for the same agent must be prepared from the exact
// committed history of the first — opening input, native model output and the
// tool call/result pair — exactly once, without re-reading live state.
func TestLaterTurnReceivesPriorConversationAndToolEvidence(t *testing.T) {
	log := newMemLog()
	x := &contextTurnExecutor{}
	r := contextTestRunner(log, x)
	// The shared harness writes run.started without a prompt; give the run its
	// confirmed opening instruction the way acceptance does.
	log.events[0].Payload["prompt"] = "build it"
	first := kernel.SpawnTurn{Agent: "backend"}
	workOne := contextTestWork(t, r, first)
	final, err := r.runDurableTurn(context.Background(), workOne, first, x)
	if err != nil {
		t.Fatalf("first turn failed: %v", err)
	}
	// The real runner appends the returned domain events in finishWork; the
	// transcript projects from those confirmed records.
	if _, err := log.Append(r.stamp(final)); err != nil {
		t.Fatal(err)
	}

	// A second cause arrives and is confirmed before the next turn opens.
	if _, err := log.Append(r.stamp([]kernel.Event{{ID: "follow-up", Type: kernel.RunPrompt,
		Source: kernel.SourceHuman, Payload: map[string]any{"text": "continue from the tool result"}}})); err != nil {
		t.Fatal(err)
	}
	second := kernel.SpawnTurn{Agent: "backend"}
	events, _ := log.Read(1, 0)
	workTwo, err := manifest(r.RunID, events[len(events)-1], []kernel.Effect{second})
	if err != nil {
		t.Fatal(err)
	}

	// The first turn's fake conversation is already committed; the second turn
	// presents history, so the provider fake answers with a terminal response.
	x2 := &contextTurnExecutor{}
	finalTwo, err := r.runDurableTurn(context.Background(), workTwo[0], second, x2)
	if err != nil {
		t.Fatalf("second turn failed: %v", err)
	}
	if _, err := log.Append(r.stamp(finalTwo)); err != nil {
		t.Fatal(err)
	}

	var secondContext kernel.Event
	contexts := 0
	for _, event := range log.events {
		if event.Type == kernel.ContextPrepared {
			contexts++
			if event.Str("context_id") == contextIdentity(r.RunID, workTwo[0].ID) {
				secondContext = event
			}
		}
	}
	if contexts != 2 {
		t.Fatalf("prepared contexts = %d, want one per turn: preparation must happen once per durable turn", contexts)
	}
	var artifact struct {
		Messages []turn.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(secondContext.Str("prepared_context_json")), &artifact); err != nil {
		t.Fatalf("decode second prepared context: %v", err)
	}
	var sawPrompt, sawFollowUp, sawToolCall, sawToolResult int
	for _, message := range artifact.Messages {
		for _, block := range message.Content {
			switch {
			case block.Text == "build it":
				sawPrompt++
			case block.Text == "continue from the tool result":
				sawFollowUp++
			case block.Type == turn.BlockToolCall && block.ToolCall != nil:
				sawToolCall++
			case block.Type == turn.BlockToolResult && block.ToolResult != nil:
				sawToolResult++
			}
		}
	}
	if sawPrompt != 1 || sawFollowUp != 1 {
		t.Fatalf("opening=%d follow-up=%d: a later turn must receive each confirmed user input exactly once", sawPrompt, sawFollowUp)
	}
	if sawToolCall != 1 || sawToolResult != 1 {
		t.Fatalf("tool calls=%d results=%d: the second turn must inherit the first turn's tool evidence under exact call identity", sawToolCall, sawToolResult)
	}
}
