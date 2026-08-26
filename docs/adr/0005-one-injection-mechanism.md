# ADR-0005: A single injection mechanism gives queue, follow-up and coalescing

- Status: accepted
- Affects: `internal/kernel/decide.go` (`applyInjection`, `applyTurnDone`), `internal/kernel/state.go` (`PendingCauses`)

## Context

Three requirements that arrived separately and look like three features:

1. **`on_busy: queue`** — if you send something to an agent that is thinking, the
   text is neither lost nor does it interrupt them: it waits.
2. **Follow-up** — sending a second instruction to an agent who is already
   working on the first.
3. **Coalescing** — if five wake causes arrive while it is busy, do not open five
   turns.

The natural way to implement them is one at a time: a queue for (1), a
continuation field for (2), a counter with a window for (3). Three structures
that interact, and the interactions are where the bugs live: does the follow-up
go into the queue or skip it? does the coalescing count the ones already queued?

## Decision

A single state field: `PendingCauses`.

Three event types (`run.prompt`, `agent.steered`, `agent.notified`) go through
the **same** function, `applyInjection`, because they are the same fact with
different provenance: *text arrived for somebody*. If the addressee is busy, the
text accumulates in `PendingCauses`. When the current turn ends, `applyTurnDone`
drains the list into **one** `SpawnTurn`.

The three features fall out on their own:

- Accumulating instead of losing **is** `on_busy: queue`.
- `queue` on somebody who is already working **is** follow-up.
- Draining N causes into one `SpawnTurn` **is** coalescing.

There is no interaction between features that could have bugs, because there are
not three features. There is one mechanism.

### The coalescing is audited

`SpawnTurn.Coalesced` carries the causes that were merged.

This is not optional telemetry. N causes merged into 1 turn is a **direct
billing multiplier**: it is the difference between paying for five turns and
paying for one. If the number does not end up in the log, nobody can verify the
coalescing did what it claims, nor explain an invoice, nor notice it stopped
working after a refactor. A saving you cannot audit is a saving nobody is going
to believe.

### Broadcast does not wake the inactive

A bug found by a test. A `steer` addressed to `*` opened a turn for an `advisory`
member nobody had activated yet:

```go
if target == "*" && m.State == MemberInactive { continue }
```

A broadcast talks to whoever is participating, not to whoever has not been
invoked yet. Without this filter, every correction to the team pays for a turn
for each commenter nobody called — and advisory members exist precisely so they
cost no money until they are needed.

## Discarded alternative

**Interrupt the running turn and restart it with the new context.** That is what
looks more responsive. Discarded because it throws away work already paid for:
the interrupted turn already consumed tokens. With frequent injections, the agent
never finishes a turn and spending grows without producing anything. Waiting for
the end of the turn is slower and strictly cheaper.

## Consequences

- An injection to somebody busy takes until the end of the turn to have effect.
  That is the accepted cost of not burning paid work.
- `PendingCauses` is part of the state, so it is reconstructed by fold and
  `run why` can report "there are 3 causes waiting for the turn to finish".
- The three event types share a path, so a bug in the queueing logic shows up in
  all three at once — more visible, and with a single place to fix it.

## How it is verified

`decide_test.go` covers: that an injection onto somebody busy generates no
`SpawnTurn` at that moment, that when the turn ends exactly one is generated with
all the causes in `Coalesced`, and that a broadcast does not touch
`MemberInactive` members. That last test is the one that found the bug.
