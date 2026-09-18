# ADR-0020: Retrieved memory arrives as data, not as instruction

- Status: accepted
- Affects: `internal/contextprep`, `internal/provider`, `spec/events.md`
- Depends on: ADR-0013
- Enables: Phase 7 (read-only governed memory)

## Context

`docs/design/30-vision.md` states a constraint that nothing currently enforces:

> Memory content is data, not trusted instructions.

Today `ContextSpec.Memory` is static prose from the frozen blueprint, and the
preparer concatenates it into the **system** message, between the agent's
identity and its shared instructions:

```text
You are backend.

Memory:
the operator prefers tabs
```

That is one message, not two. No structural boundary survives into the
presentation; the only marker is the literal word `Memory:`, which is itself
content a record could contain.

It is harmless today. The prose comes from a frozen blueprint, so there is no
store to leak from, no ranking to bypass, and no author the operator did not
choose. It stops being harmless the moment Phase 7 does what the roadmap asks
— return records carrying provenance, evidence class, authority and confidence
— through a channel that grants them the authority of "You are backend."
regardless of what the record says. **Authority would be decided by the
channel rather than by the record**, which is the exact property Phase 7
exists to establish.

A stored record reading *"ignore your previous instructions"* would arrive as
an instruction.

One fact constrains every option, and it was measured rather than assumed:
**the two provider adapters treat roles differently.** `internal/provider/openai.go`
maps each message to its own wire message, preserving separation. But
`internal/provider/anthropic.go` concatenates *every* system message into a
single `System` string, joined by blank lines. A second system message is
therefore not a boundary on Anthropic — it is the same string with more
whitespace.

## Decision

Retrieved memory is presented in a **user-role message**, separate from the
system message, and never merged into it.

The system message keeps what the operator authored: identity, situation and
shared instructions. Memory occupies its own message, in the role every
adapter already reserves for content the model should treat as input rather
than as its own directive.

The presentation records, for each memory message, the record identities and
version IDs it carries, so the existing `MemoryReceipt` continues to bind what
was presented. The receipt mechanism is unchanged and was never the gap.

This ADR governs the **channel**. It deliberately says nothing about
retrieval, ranking, scoping or deletion; those are Phase 7's own decisions and
belong in their own records.

## Consequences

The presentation shape changes: a member with memory now produces at least two
messages where it produced one. `presentation_digest` changes for any
configuration that supplies memory, so prepared contexts committed before this
decision do not match ones committed after. That is correct — they are
genuinely different presentations — and the barrier from ADR-0013 already
refuses a digest that disagrees with its bytes, so the change surfaces as a
refusal rather than as silent drift.

Both adapters carry the separation to the wire without modification, but not
for the same reason, and the difference is the point: OpenAI emits a distinct
`user` message; Anthropic emits it in `Messages` rather than folding it into
`System`. On Anthropic the guarantee holds *because* memory left the system
role — had we chosen a second system message, the adapter would have
concatenated it back and the guarantee would have been undone at the wire,
silently, with the preparer still looking correct.

A model may still choose to follow text in a user message. This decision does
not claim otherwise: it removes the *structural* grant of authority, which is
what the system channel is. What a model does with clearly-marked data is a
prompt-design and evaluation question, not a containment one.

## Discarded alternatives

**A second system message.** The obvious minimal change, and wrong for a
reason invisible from the preparer: Anthropic concatenates system messages
into one string. The code would look separated and the wire would not be —
worse than the current state, which is at least honest about being one
message.

**Keep memory in the system message and mark it with delimiters.** A fence a
record can contain is not a fence. This is the same error as trusting a
`Memory:` prefix, and the premise of Phase 7 is that record content is
untrusted.

**A dedicated `memory` role.** Cleanest in the abstract, unavailable in
practice: `turn.Role` has four values that every adapter maps to a real wire
concept. A fifth would force each adapter to decide what it degrades to, and
the honest answer for both current providers is "a user message" — which is
what this decision selects directly, without a vocabulary term no provider
implements.

**Defer the channel decision to Phase 7's retrieval ADR.** Tempting, since no
store exists yet. But the retrieval ADR would then be written against a
presentation shape it silently inherits, and the shape is the expensive part
to change once prepared contexts have been committed against it. Deciding the
channel first is what lets the retrieval decision be about retrieval.

## How it is verified

`internal/contextprep/memory_channel_test.go` currently pins the *existing*
behaviour, including the part this decision overturns. Those pins were written
before this ADR so the design would start from a measured fact; implementing
this decision must make them fail, and they say so in their own failure
messages. They are replaced, not weakened, when the change lands.

The replacement must assert three things:

- memory is not in the system message, and the system message still carries
  identity, situation and shared sections;
- the separation survives to the wire — asserted against both
  `openAIRequest` and `anthropicRequest`, not against `turn.Message` alone,
  because the Anthropic concatenation is invisible at the neutral layer and is
  precisely the trap this decision avoids;
- `MemoryReceipt` still binds the presented content, so the channel change
  does not quietly cost the evidence that already existed.
