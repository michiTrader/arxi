# ADR-0015: One host lifecycle, projected across every surface

- Status: accepted
- Affects: `host/v1`, `cmd/arxi/serve.go`, `internal/app`, `internal/surface`, `spec/events.md`
- Depends on: ADR-0008, ADR-0009, ADR-0010

## Context

The roadmap's first milestone requires a text-only external client — Asha — to
submit a durable job, observe it, answer its questions and survive an Arxi
restart through the public host and protocol surfaces. Three facts made the
current state unable to do that:

1. `host/v1` implements the full lifecycle (submit, inspect, cancel, approve,
   reject, answer, wait, subscribe, capabilities), but the protocol transport
   in `cmd/arxi/serve.go` wires only inspect and cancel — four of the
   thirty-eight declared protocol types answer at all.
2. The decision operations (`inbox.approve/reject/reply`) were withheld because
   the protocol carried only an item ID, while host dispatch deliberately
   requires JobID and ItemID for resource authorization. The vocabulary
   existed; the authorization shape did not.
3. Submit and Wait were withheld because the host's `TextProvider` port had
   only test stubs — no production implementation existed to execute a
   submitted job with a real model.

Meanwhile `internal/surface` derives the protocol vocabulary from the same
registry as the CLI, and a test keeps every registry path answering one way or
the other. That design is right; this record completes it.

## Decision

**One capability has one implementation, and every surface projects it.** The
protocol transport gains no logic of its own: each lifecycle message type is a
thin descriptor that maps parameters to the same `host/v1` call the CLI
makes, under the same capability check, with the principal supplied by the
trusted listener and never by the request.

### Decision operations carry both identities

`inbox.approve`, `inbox.reject` and `inbox.reply` require both the `run` and
the `item` parameter. Job-scoped authorization was never negotiable: guessing a
job by searching every run would duplicate resource selection outside host
dispatch and make its reauthorization check run against an invented or
ambiguous resource. The protocol now carries what the host always demanded.

### Submitted jobs execute through the real provider stack

`arxi serve` installs a production `TextProvider`: a thin adapter in the
composition root that resolves the requested model through the modelstore and
completes the call through the same provider transports the CLI uses
(`internal/provider`). The adapter lives in `cmd/arxi`, not in `host/v1`,
because the public package must stay free of `internal/` imports. A submitted
protocol job therefore bills the same providers a CLI run bills.

`run.start` maps to Submit with the CLI's parameter names; `run.result` maps to
Wait and returns the terminal projection. This is the text-only Phase 1 shape:
native tool loops over the protocol arrive when the public turn contract does,
and not before.

### What stays deliberately unimplemented

`run.attach` (Subscribe) stays `not_implemented`: its multi-response stream
cannot preserve the protocol's one-request/one-response framing without
subscription IDs, event messages, cancellation and writer arbitration — a
transport change that deserves its own record when an external client needs
streaming, not a framing hack bolted onto this one.

## Discarded alternatives

**A protocol-specific command layer.** Rejected: a second implementation of
the same capabilities is the drift the surface registry exists to prevent. The
day a fix lands in the CLI and not the socket, the socket becomes a lie.

**Guess the job for decision operations.** Rejected at design time and
rejected again: authorization against an ambiguous resource is not
authorization.

**Make `host/v1` call `internal/provider` directly.** Rejected: the public
package's independence is guarded by architecture tests; the composition root
is where internal knowledge is allowed.

**Wait for the native turn contract before exposing anything.** Rejected:
Asha's first milestone is text-only by decision, and the protocol vocabulary is
already declared. An honest `implemented` list is how the handshake says what
works; an empty socket is how a project stalls.

## Consequences

- The protocol lifecycle answers submit, inspect, wait, cancel, approve,
  reject, answer and capabilities; every other declared type answers
  `not_implemented`, which the handshake reports truthfully.
- An external text-only client can now run the full first milestone without
  shelling out or importing `internal/`.
- The adapter in `cmd/arxi` binds serve to the operator's modelstore: no
  providers registered means submit fails with the resolver's remedy, which is
  the honest failure.
- Subscribe remains the known gap on the path to Asha's event stream;
  recovery-based observation (inspect/wait) is available meanwhile.

## How it is verified

Contract tests drive the protocol over an in-memory reader/writer pair and
require: a submitted job reaches a terminal state through the real provider
adapter; a decision message without its job parameter is refused as a client
error, not dispatched; approve/reject/answer change the run exactly as the CLI
does; the handshake's `implemented` list matches the wired handlers, and every
declared type not in it answers `not_implemented`. The existing test that keys
`protoHandlers` against the registry continues to guard against a handler whose
type nobody declared.
