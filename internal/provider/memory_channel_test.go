package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/turn"
)

// These tests pin the wire fact ADR-0020 rests on, measured rather than read
// off the source.
//
// The decision is that retrieved memory arrives in a USER-role message. The
// obvious cheaper alternative — a second SYSTEM message — is rejected because
// it is not a boundary on Anthropic: that adapter concatenates every system
// message into one `System` string. The preparer would look correct and the
// wire would not be, which is worse than today's honest single message.
//
// That claim is the load-bearing part of the ADR, so it is asserted against
// both wire requests. Asserting it at the `turn.Message` layer would prove
// nothing: the neutral layer shows two messages in both arrangements, and the
// collapse happens below it. That invisibility is exactly the trap the
// decision avoids, so the test has to look where the trap is.

func memoryChannelBlocks(text string) []turn.ContentBlock {
	return []turn.ContentBlock{{Type: turn.BlockText, Text: text}}
}

const (
	memoryChannelIdentity = "You are backend."
	memoryChannelRecord   = "Memory:\nuntrusted record"
)

// TestASecondSystemMessageIsNotABoundaryOnAnthropic records why the cheaper
// option was discarded.
//
// If this ever stops holding — because the adapter learns to send structured
// system blocks, or the API grows a second channel — then the discarded
// alternative becomes viable and ADR-0020 should be revisited rather than
// silently kept.
func TestASecondSystemMessageIsNotABoundaryOnAnthropic(t *testing.T) {
	request := turn.Request{Schema: turn.Schema, Model: "m", MaxTokens: 10, Messages: []turn.Message{
		{Role: turn.RoleSystem, Content: memoryChannelBlocks(memoryChannelIdentity)},
		{Role: turn.RoleSystem, Content: memoryChannelBlocks(memoryChannelRecord)},
		{Role: turn.RoleUser, Content: memoryChannelBlocks("Proceed.")},
	}}

	wire, err := anthropicTurnRequest(request)
	if err != nil {
		t.Fatalf("anthropicTurnRequest: %v", err)
	}

	if !strings.Contains(wire.System, memoryChannelIdentity) ||
		!strings.Contains(wire.System, "untrusted record") {
		t.Fatalf("anthropic System = %q: expected BOTH system messages folded into it, so this "+
			"test is no longer measuring the collapse it documents", wire.System)
	}

	// The finding as an assertion rather than a log: the operator's
	// instruction and the untrusted record are one string, and nothing
	// downstream can tell them apart.
	if strings.Count(wire.System, memoryChannelIdentity) != 1 {
		t.Fatalf("unexpected System shape: %q", wire.System)
	}
	t.Logf("two system messages became one string: %q", wire.System)
}

// TestADR0020KeepsMemoryOutOfTheSystemChannelOnBothAdapters is the positive
// half: the decision's arrangement survives to the wire on both providers.
//
// Anthropic is the one that matters — it is where the rejected option failed.
// OpenAI is included because a decision that only works on one provider is a
// provider workaround rather than a platform guarantee, and that difference
// should fail loudly if it ever appears.
func TestADR0020KeepsMemoryOutOfTheSystemChannelOnBothAdapters(t *testing.T) {
	request := turn.Request{Schema: turn.Schema, Model: "m", MaxTokens: 10, Messages: []turn.Message{
		{Role: turn.RoleSystem, Content: memoryChannelBlocks(memoryChannelIdentity)},
		{Role: turn.RoleUser, Content: memoryChannelBlocks(memoryChannelRecord)},
		{Role: turn.RoleUser, Content: memoryChannelBlocks("Proceed.")},
	}}

	anthropic, err := anthropicTurnRequest(request)
	if err != nil {
		t.Fatalf("anthropicTurnRequest: %v", err)
	}
	if strings.Contains(anthropic.System, "untrusted record") {
		t.Fatalf("anthropic System = %q: a memory record reached the system channel, which is the "+
			"structural grant of authority ADR-0020 exists to remove", anthropic.System)
	}
	if anthropic.System != memoryChannelIdentity {
		t.Fatalf("anthropic System = %q, want only the operator's identity: the system channel "+
			"must carry what the operator authored and nothing else", anthropic.System)
	}
	if len(anthropic.Messages) != 2 {
		t.Fatalf("anthropic Messages = %d, want 2: the memory record and the prompt must each be "+
			"their own wire message", len(anthropic.Messages))
	}

	openAI, err := openAIRequest(request)
	if err != nil {
		t.Fatalf("openAIRequest: %v", err)
	}
	if len(openAI.Messages) != 3 {
		t.Fatalf("openai messages = %d, want 3: system, memory and prompt must stay separate",
			len(openAI.Messages))
	}
	// chatMessage.Content is `any` because the wire allows structured parts,
	// so this goes through fmt rather than a type assertion: a record
	// smuggled into a structured system part would still be caught, where a
	// failed string assertion would silently skip the check.
	for _, m := range openAI.Messages {
		if m.Role == "system" && strings.Contains(fmt.Sprint(m.Content), "untrusted record") {
			t.Fatalf("a memory record reached an openai system message: %v", m.Content)
		}
	}
}
