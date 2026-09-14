package main

import (
	"context"
	"encoding/json"
	"sync"

	hostv1 "github.com/michiTrader/arxi/host/v1"
)

// Protocol event streaming (ADR-0016). The connection carries two message
// kinds: responses answer requests and keep their strict order; notifications
// are server-initiated, carry `type` and no `id` — the shape hello already
// established — and interleave with responses. Everything in this file exists
// to emit those notifications without weakening the response contract.

// eventNotification is one confirmed batch for one subscription, plus the
// terminal marker when the run has ended and the batch delivered everything
// through the sequence at which it ended.
type eventNotification struct {
	Type         string         `json:"type"`
	Subscription string         `json:"subscription"`
	AfterSeq     int64          `json:"after_seq"`
	Events       []hostv1.Event `json:"events"`
	Terminal     bool           `json:"terminal,omitempty"`
	Job          *hostv1.Job    `json:"job,omitempty"`
	Error        *protoError    `json:"error,omitempty"`
}

// attachAck is the response to run.attach. TerminalMarkers says whether
// terminal notifications will be emitted: they require the inspect capability
// on the same job, and a stream that ends silently when the client expected a
// marker would be a client blocked forever on a promise the wire never made.
type attachAck struct {
	Subscription    string `json:"subscription"`
	AfterSeq        int64  `json:"after_seq"`
	TerminalMarkers bool   `json:"terminal_markers"`
}

// connWriter serializes every write on the connection. The loop writes
// responses and the pumps write notifications, all under one mutex; there is
// no other arbitration because there is no other ordering requirement —
// responses are ordered by the loop, a subscription's notifications by its
// single pump, and any interleaving between the two is valid.
type connWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *connWriter) write(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(v)
}

// subscriptionPump delivers one subscription's batches as notifications.
type subscriptionPump struct {
	id        string // the attach request's id, doubling as the subscription identity
	jobID     hostv1.JobID
	after     int64
	sub       hostv1.Subscription
	host      lifecycleHost
	principal hostv1.Principal
	markers   bool
	// release is closed by the connection loop once the ack response is on
	// the wire. Waiting on it is what guarantees the ack precedes this
	// subscription's first event: the pump exists before the response is
	// written, and without the latch it could win the race.
	release chan struct{}
}

// run delivers batches until the stream ends: terminal marker, host error,
// connection cancellation or Close. It exits silently on cancellation and
// Close — the connection is ending, and a notification nobody can read is
// noise — and emits an error notification for any other failure, because a
// stream that stops delivering without saying why looks identical to a
// quiescent run.
func (p *subscriptionPump) run(ctx context.Context, w *connWriter) {
	defer p.sub.Close()
	<-p.release
	for {
		batch, err := p.sub.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				_ = w.write(eventNotification{Type: "events", Subscription: p.id,
					AfterSeq: p.after, Error: &protoError{Code: errFailed, Message: err.Error()}})
			}
			return
		}
		p.after = batch.AfterSeq
		notification := eventNotification{Type: "events", Subscription: p.id,
			AfterSeq: batch.AfterSeq, Events: batch.Events}
		if p.markers {
			// The projection is the same fold the CLI and Wait use. The
			// sequence check prevents announcing terminal before the batch
			// that reached it has been delivered.
			if job, ierr := p.host.Inspect(ctx, hostv1.InspectRequest{Principal: p.principal, JobID: p.jobID}); ierr == nil &&
				job.Terminal && batch.AfterSeq >= job.Sequence {
				notification.Terminal = true
				notification.Job = &job
				_ = w.write(notification)
				return
			}
		}
		if err := w.write(notification); err != nil {
			return
		}
	}
}

// connStreams owns the subscriptions opened on one connection: their release
// latches and their teardown. It hangs off the session by pointer so the
// value copies the loop makes all register into the same lifetime.
type connStreams struct {
	ctx context.Context
	w   *connWriter
	wg  sync.WaitGroup

	mu      sync.Mutex
	pending []*subscriptionPump // registered, waiting for their ack to be written
	live    []*subscriptionPump // released, delivering
}

func newConnStreams(ctx context.Context, w *connWriter) *connStreams {
	return &connStreams{ctx: ctx, w: w}
}

// register records a pump before its ack is written. The pump does not start
// here: releasePending starts it, and only the connection loop calls that,
// immediately after writing the response — which is the whole ordering
// guarantee.
func (c *connStreams) register(p *subscriptionPump) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, p)
}

// releasePending starts every pump registered by the response just written.
func (c *connStreams) releasePending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	c.live = append(c.live, pending...)
	c.mu.Unlock()
	for _, p := range pending {
		// Closing the latch is what releases the pump: it has been waiting
		// since registration, and only the loop reaches this point, after the
		// ack is written. Without the close the pump would block forever.
		close(p.release)
		c.wg.Add(1)
		go func(p *subscriptionPump) {
			defer c.wg.Done()
			p.run(c.ctx, c.w)
		}(p)
	}
}

// closeAll ends every subscription on connection teardown. Close is the pull
// stream's own cancellation contract: it makes a blocked Next return, so the
// pumps exit and Wait returns without deadlock. A pump mid-write finishes its
// write first — the mutex serializes it — and a notification written to a
// closing socket is harmless.
func (c *connStreams) closeAll() {
	c.mu.Lock()
	pumps := append(append([]*subscriptionPump{}, c.pending...), c.live...)
	c.pending, c.live = nil, nil
	c.mu.Unlock()
	for _, p := range pumps {
		_ = p.sub.Close()
	}
	c.wg.Wait()
}
