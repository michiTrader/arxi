package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	hostv1 "github.com/michiTrader/arxi/host/v1"
)

// scriptedSubscription hands out prepared batches in order, then blocks like a
// live poll until Close or the context ends. It is the host port's pull
// contract in miniature: Next returns a batch, then waits for more.
type scriptedSubscription struct {
	mu      sync.Mutex
	batches []hostv1.EventBatch
	closed  chan struct{}
	once    sync.Once
	nexts   int
}

func newScriptedSubscription(batches ...hostv1.EventBatch) *scriptedSubscription {
	return &scriptedSubscription{batches: batches, closed: make(chan struct{})}
}

func (s *scriptedSubscription) Next(ctx context.Context) (hostv1.EventBatch, error) {
	s.mu.Lock()
	if len(s.batches) > 0 {
		batch := s.batches[0]
		s.batches = s.batches[1:]
		s.nexts++
		s.mu.Unlock()
		return batch, nil
	}
	s.mu.Unlock()
	select {
	case <-s.closed:
		return hostv1.EventBatch{}, context.Canceled
	case <-ctx.Done():
		return hostv1.EventBatch{}, ctx.Err()
	}
}

func (s *scriptedSubscription) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *scriptedSubscription) nextCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nexts
}

// streamingHost wraps the recording fake with a scripted subscription and an
// inspect projection, which together drive the pump's terminal detection.
type streamingHost struct {
	recordingLifecycleHost
	sub     *scriptedSubscription
	job     hostv1.Job
	inspect int
}

func (h *streamingHost) Subscribe(ctx context.Context, req hostv1.SubscribeRequest) (hostv1.Subscription, error) {
	h.subscribe = req
	return h.sub, nil
}

func (h *streamingHost) Inspect(context.Context, hostv1.InspectRequest) (hostv1.Job, error) {
	h.inspect++
	return h.job, nil
}

// runConnection drives a real connection over in-memory pipes with the lines
// supplied, returning every decoded message. It closes the writer side after
// writing the lines, which is how a client disconnect is simulated.
func runConnection(t *testing.T, session protoSession, lines ...string) []map[string]any {
	t.Helper()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- serveConnSessionContext(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, session)
	}()
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "write") {
			t.Fatalf("serveConnSessionContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not finish: a subscription must not keep the loop alive after the reader ends")
	}
	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		messages = append(messages, msg)
	}
	return messages
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func subscribeCapabilities() hostv1.CapabilitySet {
	return hostv1.CapabilitySet{Capabilities: []hostv1.Capability{
		hostv1.CapabilitySubscribe, hostv1.CapabilityInspect,
	}}
}

// The ack must be on the wire before the subscription's first event. That is
// the ordering guarantee the release latch exists for: the pump is created
// during dispatch, before the response is written, so without the latch it
// could win the race and a client would see an event before its own ack.
func TestAttachAckPrecedesFirstEvent(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription(
		hostv1.EventBatch{Events: []hostv1.Event{{Sequence: 1, Type: "run.started"}}, AfterSeq: 1},
		hostv1.EventBatch{Events: []hostv1.Event{{Sequence: 2, Type: "agent.activated"}}, AfterSeq: 2},
	)}
	host.capabilities = subscribeCapabilities()
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	messages := runConnection(t, session, `{"id":"s1","type":"run.attach","params":{"run":"r1"}}`)
	if len(messages) < 2 {
		t.Fatalf("messages = %#v: attach must produce an ack and then events", messages)
	}
	if messages[0]["type"] != "hello" {
		t.Fatalf("first message type = %v, want hello", messages[0]["type"])
	}
	ack := messages[1]
	if ack["id"] != "s1" || ack["ok"] != true {
		t.Fatalf("first response = %#v: the ack must be the attach request's response", ack)
	}
	result, _ := ack["result"].(map[string]any)
	if result == nil || result["subscription"] != "s1" || result["terminal_markers"] != true {
		t.Fatalf("ack result = %#v: it must identify the subscription and whether terminal markers will come", ack["result"])
	}
	if len(messages) < 3 {
		t.Fatalf("messages = %#v: the ack was not followed by events", messages)
	}
	first := messages[2]
	if first["type"] != "events" || first["subscription"] != "s1" {
		t.Fatalf("first notification = %#v: events must carry the type and the subscription identity", first)
	}
	if _, hasID := first["id"]; hasID {
		t.Fatalf("notification carries an id: a notification answers no request, and an id would make it look like a response to a client correlating by id")
	}
	events, _ := first["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("first notification events = %#v, want one", first["events"])
	}
}

// Requests still get exactly one response each while notifications interleave.
// This is the framing property the streaming design must not weaken.
func TestRequestsGetOneResponseEachWhileStreaming(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription(
		hostv1.EventBatch{Events: []hostv1.Event{{Sequence: 1, Type: "run.started"}}, AfterSeq: 1},
	)}
	host.capabilities = subscribeCapabilities()
	host.response = hostv1.Job{ID: "r1", Status: hostv1.JobRunning}
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	messages := runConnection(t, session,
		`{"id":"s1","type":"run.attach","params":{"run":"r1"}}`,
		`{"id":"c1","type":"run.show","params":{"run":"r1"}}`,
		`{"id":"c2","type":"run.cancel","params":{"run":"r1"}}`)
	responses := map[string]int{}
	for _, msg := range messages {
		if id, ok := msg["id"].(string); ok && id != "" {
			responses[id]++
		}
	}
	for _, id := range []string{"s1", "c1", "c2"} {
		if responses[id] != 1 {
			t.Fatalf("request %q got %d responses, want exactly 1: a subscription must not disturb the one-request/one-response contract", id, responses[id])
		}
	}
	if responses["s1"] != 1 {
		t.Fatalf("attach got %d responses", responses["s1"])
	}
}

// A batch that reaches the run's terminal sequence produces one terminal
// notification carrying the job projection, and the subscription closes. The
// sequence check is what stops a terminal marker from racing ahead of the
// events that reached it.
func TestAttachEmitsTerminalMarkerAndCloses(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription(
		hostv1.EventBatch{Events: []hostv1.Event{{Sequence: 1, Type: "run.started"}}, AfterSeq: 1},
		hostv1.EventBatch{Events: []hostv1.Event{{Sequence: 2, Type: "run.result"}}, AfterSeq: 2},
	)}
	host.capabilities = subscribeCapabilities()
	host.job = hostv1.Job{ID: "r1", Status: hostv1.JobSucceeded, Terminal: true, Sequence: 2}
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	messages := runConnection(t, session, `{"id":"s1","type":"run.attach","params":{"run":"r1"}}`)
	var terminal int
	for _, msg := range messages {
		if msg["type"] != "events" {
			continue
		}
		if msg["terminal"] == true {
			terminal++
			if msg["job"] == nil {
				t.Fatalf("terminal notification carries no job projection: a client needs to know how it ended")
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal notifications = %d, want exactly 1", terminal)
	}
	// Once terminal is delivered the pump stops pulling.
	if host.sub.nextCount() > 2 {
		t.Fatalf("pump pulled %d batches after terminal: the stream must end", host.sub.nextCount())
	}
	if host.inspect == 0 {
		t.Fatal("terminal detection never inspected the job")
	}
}

// A batch that is not yet at the terminal sequence must not end the stream:
// the run is still running.
func TestAttachKeepsStreamingUntilTerminalSequence(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription(
		hostv1.EventBatch{Events: []hostv1.Event{{Sequence: 1, Type: "run.started"}}, AfterSeq: 1},
	)}
	host.capabilities = subscribeCapabilities()
	// Terminal status is already set, but the delivered sequence is behind the
	// sequence at which the run ended: the batch that reached it has not
	// arrived, so the stream must keep going.
	host.job = hostv1.Job{ID: "r1", Status: hostv1.JobSucceeded, Terminal: true, Sequence: 5}
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	messages := runConnection(t, session, `{"id":"s1","type":"run.attach","params":{"run":"r1"}}`)
	for _, msg := range messages {
		if msg["terminal"] == true {
			t.Fatalf("terminal marker emitted at sequence %v before the batch reached %d: a client would stop watching a running run", msg["after_seq"], 5)
		}
	}
}

// Closing the connection must end the subscription without deadlocking: a
// pump blocked in Next has to wake on Close.
func TestConnectionCloseEndsLiveSubscriptions(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription()}
	host.capabilities = subscribeCapabilities()
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveConnSessionContext(context.Background(), strings.NewReader(
			`{"id":"s1","type":"run.attach","params":{"run":"r1"}}`+"\n"), &syncBuffer{}, session)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not close: a blocked subscription Next must wake on Close or the loop deadlocks on teardown")
	}
	host.sub.mu.Lock()
	closed := host.sub.closed != nil
	host.sub.mu.Unlock()
	if !closed {
		t.Fatal("subscription was never closed on connection teardown")
	}
}

// after_seq reaches the host request untouched: resumption depends on it.
func TestAttachForwardsTheResumeCursor(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription()}
	host.capabilities = subscribeCapabilities()
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	runConnection(t, session, `{"id":"s1","type":"run.attach","params":{"run":"r1","after_seq":42}}`)
	if host.subscribe.AfterSeq != 42 || host.subscribe.JobID != "r1" || host.subscribe.Principal.ID != "p" {
		t.Fatalf("host subscribe request = %#v: the cursor, job and principal must reach the host", host.subscribe)
	}
}

// Attaching without terminal markers is visible in the ack: a client that
// expected a marker would otherwise wait forever on a promise never made.
func TestAttachHonestlyReportsMissingTerminalMarkers(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription()}
	// Subscribe but not inspect: the stream can deliver events but cannot tell
	// the client the run ended.
	host.capabilities = hostv1.CapabilitySet{Capabilities: []hostv1.Capability{hostv1.CapabilitySubscribe}}
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	messages := runConnection(t, session, `{"id":"s1","type":"run.attach","params":{"run":"r1"}}`)
	ack := messages[1]
	result, _ := ack["result"].(map[string]any)
	if result["terminal_markers"] != false {
		t.Fatalf("ack terminal_markers = %v, want false when inspect is not granted: a client waiting for a marker that was never promised blocks forever", result["terminal_markers"])
	}
}

// The id is the subscription identity, so an attach without one cannot be
// correlated and is refused rather than streamed anonymously.
func TestAttachRequiresARequestID(t *testing.T) {
	host := &streamingHost{sub: newScriptedSubscription()}
	host.capabilities = subscribeCapabilities()
	session := newProtoSession(hostv1.Principal{ID: "p"}, host)

	messages := runConnection(t, session, `{"type":"run.attach","params":{"run":"r1"}}`)
	last := messages[len(messages)-1]
	if last["ok"] == true {
		t.Fatalf("attach without an id was accepted: the id is the subscription identity its notifications carry")
	}
}
