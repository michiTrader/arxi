# ADR-0011: Authorization binds and consumes one exact action

- Status: accepted
- Affects: `internal/authorization`, `internal/turn`, `internal/kernel`, `internal/exec`, `internal/inbox`, `internal/app`, `host/v1`, `spec/events.md`

## Context

A tool policy of `ask` currently records a denial and opens an inbox item that
names the tool. Approval replies to that item and opens another model turn. It
does not authorize the provider call that caused the question: the next turn may
repeat different arguments, and changing policy globally may allow a different
action. Preventing two replies to one inbox item is not single-use authorization
because no immutable action is bound to the reply.

Phase 2 already preserves provider call IDs and canonical argument digests. Phase
3 adds fenced work, durable dispatch boundaries and compare-and-swap storage.
Those are the identities and atomic boundary needed to authorize consequential
work without approximating it after a human decision.

## Decision

**An approval grants one immutable action, and dispatch atomically consumes that
grant. A reply that cannot prove every binding fails closed.**

### Action identity

An authorization request records a domain-separated digest over:

- job and run identity;
- requesting principal;
- suspended parent work and provider call identity;
- tool name and canonical argument digest;
- tool-schema version;
- frozen policy version;
- workspace guarantee profile.

The exact canonical call and continuation are persisted before approval is
requested. The digest is an index and integrity check, not a replacement for
those bytes. Changing any bound field produces a different action.

The requester is an execution principal derived by the runtime. The approver is
an authenticated operator principal supplied by the application adapter. They
must differ. Keeping approval commands out of the agent surface remains useful,
but it is not the security boundary; the mutation path enforces the distinction.

### Request, decision and expiry

Policy is resolved before `exec.work_started` for a tool. `allow` proceeds through
the ordinary durable work protocol, `deny` records a terminal denial, and `ask`
persists the exact continuation plus `authorization.requested`. Asking does not
start the tool and does not complete it as an external action.

A grant records the authorization ID, unchanged action digest, approver principal
and absolute expiry. Reject and expiry are terminal decisions. Approval, reject
and answer verbs remain distinct and must match the pending inbox kind.

Time remains injected. A requested authorization arms a durable timer; its tick
records `authorization.expired`. Replay folds that judgment and never compares an
old grant to the current clock. The timer remains relevant after approval until
the grant is consumed.

### Single-use consumption

Immediately before external dispatch, the active fenced worker verifies the
request, grant, digest, principal rule, expiry and exact suspended bytes. It then
uses the authoritative writer compare-and-swap to append one atomic batch:

1. `authorization.consumed` for the grant and consuming work;
2. `exec.work_started` for that exact tool child.

Only after the batch commits may the runner receive the call. A competing worker
reloads the confirmed history and sees the consumption; it cannot dispatch. A
crash after this boundary uses the existing receipt and unknown-outcome rules.
Consumption is never rolled back and the grant is never restored merely because
the local result is missing.

### Compatibility

The command and agent-tool surface remains version 1. Existing `inbox approve`,
`inbox reject` and host lifecycle methods are strengthened behind their current
shape. `host/v1` already carries an authenticated principal and remains source
compatible.

Historical logs keep their historical meaning for replay and inspection. A
legacy unanswered mutating approval has no action, schema, policy or principal
binding, so live execution cannot upgrade it honestly and refuses to resume it.
Completed legacy work remains authoritative and is never rehashed under the new
contract.

## Discarded alternatives

**Open another model turn after approval.** Rejected: the model can emit different
arguments or no call at all, so the approval authorizes an intention rather than
a mutation.

**Bind only tool name and arguments.** Rejected: a changed schema can reinterpret
the same JSON, a changed policy can alter the decision context, and a changed
workspace can change which resources the call reaches.

**Mark the inbox item replied and call that consumption.** Rejected: a replied
item prevents duplicate human decisions but does not prevent two workers from
starting the external effect.

**Consume after the tool returns.** Rejected: a crash during the external call
would leave an apparently unused grant that recovery could dispatch again.

**Read wall-clock time inside the reducer.** Rejected: the same log would accept
a grant today and reject it tomorrow, which destroys replay.

## Consequences

- Approval can resume the original canonical provider continuation without asking
the model to restate it.
- Exact grants add durable records and one compare-and-swap before a consequential
dispatch.
- A consumed call may remain `unknown`; safety takes precedence over automatic
retry when no trustworthy receipt exists.
- Adapters must provide authenticated approver identities. Empty or self-approving
identities are rejected.
- Old pending mutating approvals remain inspectable but cannot execute.

## How it is verified

Canonicalization tests change each bound field independently and require a new
action digest. Reducer and execution tests prove that `ask` starts no tool, wrong
verbs and self-approval fail, elapsed grants expire by event, and changed
arguments, schema, policy or workspace cannot reuse a grant. Storage race tests
run two consumers and require one consumption, one start and one runner call.
Crash tests cover both sides of consume/start and preserve explicit unknown
outcomes. Replay tests cover new records and historical logs, while public-host
and surface tests keep `host/v1` source compatible and the declared capability
counts unchanged.
