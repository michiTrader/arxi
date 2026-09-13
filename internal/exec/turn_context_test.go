package exec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	artifact, err := contextprep.Prepare(req.ContextID, req.RunID, req.ParentWorkID, req.EffectiveConfigSHA, req.Effect, history)
	if err != nil {
		return PreparedContext{}, err
	}
	body, err := json.Marshal(artifact)
	if err != nil {
		return PreparedContext{}, err
	}
	return PreparedContext{JSON: string(body), Digest: byteDigest(body), Schema: artifact.Schema,
		PreparerVersion: artifact.PreparerVersion, ContextID: artifact.ContextID,
		ParentWorkID: artifact.ParentWorkID, Subject: artifact.Subject,
		SourceThroughSeq: artifact.SourceThroughSeq, ContentDigest: artifact.ContentDigest,
		PresentationDigest: artifact.PresentationDigest, TranscriptContentDigest: artifact.Transcript.ContentDigest,
		Messages: artifact.Messages}, nil
}

// contextTurnExecutor counts route preparation so a test can tell a recovered
// route build (cheap, in-memory, allowed on every attempt) from a redispatched
// model call (never allowed once children are committed).
type contextTurnExecutor struct {
	nativeLoopExecutor
	prepareCalls int
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
