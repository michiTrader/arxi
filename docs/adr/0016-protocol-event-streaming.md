# ADR-0016: Protocol event streaming carries notifications beside responses

- Status: accepted
- Affects: `cmd/arxi/serve.go`, `cmd/arxi/serve_stream.go`, `internal/surface`, `docs/design/20-use-cases.md`
- Depends on: ADR-0015

## Context

ADR-0015 wired the protocol lifecycle but withheld `run.attach` (Subscribe),
naming the four missing pieces: subscription IDs, event messages, cancellation,
and writer arbitration. Without it, an external client can learn how a run ended
(`run.result`) but cannot observe it as it happens — the observation half of the
first milestone is polling, not streaming, and a client that wants the event
feed has no wire path to it.

The host port is not the obstacle: `Subscribe` returns a bounded pull stream of
confirmed event batches with a resumable cursor. The obstacle is the protocol's
framing: one request, one response, strictly in order, single-threaded loop.
A subscription emits messages nobody requested, while the loop is blocked
waiting for the next line.

## Decision

**The protocol carries two message kinds. Responses answer requests and keep
their strict order. Notifications are server-initiated, carry `type` and no
`id` — the shape hello already established — and interleave with responses.**

`run.attach` subscribes to a run's confirmed events. Its response is a normal
ack; from then on each confirmed batch arrives as one notification:

```json
{"type":"events","subscription":"<request-id>","after_seq":13,"events":[...]}
```

The subscription identity is the request's own `id`. No counter, no registry:
the client chose the id, the client can correlate it, and one attach request
produces exactly one subscription.

### Writer arbitration

One mutex owns the encoder. The connection loop writes responses under it;
each subscription runs one pump goroutine that pulls `Next` and writes
notifications under it. Responses stay strictly in order because the loop is
unchanged; a subscription's notifications stay in order because one goroutine
produces them; and the ack always precedes that subscription's first event
because the pump waits on a release latch the loop closes after writing the
ack. A response and a notification may interleave in either order — both are
valid, and no client can depend on one.

### Termination

The host's subscription has no end: `Next` polls forever. The transport owns
termination. After each batch the pump inspects the job; when the projection
is terminal and the batch has delivered everything through that sequence
(`job.Terminal && batch.AfterSeq >= job.Sequence`), the pump emits one final
notification carrying `"terminal": true` and the terminal job projection, and
closes. Attaching to an already-finished run therefore replays its history and
ends — the same contract `run attach` gives on the CLI.

Terminal markers require the inspect capability on the same job. When it is
not granted, the ack says so (`"terminal_markers": false`) and the stream runs
until the client disconnects — degraded visibly, never silently.

### Cancellation

There is no unsubscribe message type, and none is added: the frozen surface
rule is that a new command implements an already-declared promise, and no
detach was ever declared. A subscription ends when the run reaches terminal or
the client closes the connection; connection teardown closes every live
subscription, which is exactly the pull stream's own `Close` contract.

### Resumption

`run.attach` gains one optional wire parameter, `after_seq`, projecting the
host contract's resumable cursor. Every notification carries the cursor the
client has reached, so a disconnected client reattaches from its last
delivered sequence and loses nothing — this is what surviving an Arxi restart
means for the observation path.

## Discarded alternatives

**A separate event channel or socket per subscription.** Rejected: a second
transport doubles the framing, the handshake and the failure modes, and a
client must then correlate two connections to one run.

**Make the loop read requests and poll subscriptions in one select.** Rejected:
it buries request handling in a state machine and makes the strict response
order an emergent property of scheduling instead of a structural one. The
pump-per-subscription goroutine keeps each concern single-threaded.

**Detect terminal by event-type allowlist.** Rejected: terminal status is a
folded conclusion, not an event type — `run.quiescent` is terminal only when
nobody observes it. The projection (`projectStoredJob`) is the same fold the
CLI and `Wait` use; re-deriving a cheaper approximation of it would be a
second reducer that drifts.

**Fold reconstructed events in the adapter.** Considered: the public
projection is lossless, so the pump could rebuild kernel events and fold.
Rejected for now: it drags the frozen run config into the transport (the fold
needs it), which the storage port deliberately hides. Inspect already answers
the question over the port.

**An unsubscribe message type.** Rejected as a new promise; see Cancellation.

## Consequences

- An external client can now observe a run live: submit, attach, watch events
  arrive, receive the terminal projection, reattach after a disconnect from
  its last cursor.
- The connection loop is no longer single-goroutine; writer arbitration is a
  mutex and the response-order guarantee is unchanged.
- A client that never detaches and never sees terminal keeps a pump alive —
  bounded by connection lifetime, one goroutine and one 25 ms poll each.
- The `run attach` CLI is unchanged: it tails the log directly, which remains
  correct for a local process; the protocol projection is for remote clients.
- `hello.implemented` now includes `run.attach`; the framing contract is
  documented here rather than in a protocol spec file, which still does not
  exist and is not created by this record.

## How it is verified

Connection-level tests drive the real loop over in-memory pipes against a
scripted subscription: the ack precedes that subscription's first event;
every request still gets exactly one response while notifications interleave;
a terminal projection produces exactly one terminal notification and closes
the subscription; a non-terminal run streams on; connection close closes
every subscription without deadlocking the loop; `after_seq` reaches the host
request; and a decision request on the same connection answers normally while
a subscription is live. The hello advertisement tests pin `run.attach` in
`implemented` and `event.subscribe` in the advertised capabilities.
