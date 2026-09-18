package provider

import (
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
)

// buildMessages turns the context the reducer assembled into chat messages.
//
// The ORDER of the sections is copied from kernel.ContextSpec and must not be
// rearranged: identity, situation, shared, cause -- most stable first,
// most volatile last. config.go explains why, and the reason is money. Providers
// cache a prompt PREFIX; a section that changes every turn placed early
// invalidates everything after it, so putting the causes first would turn a
// cache hit into a full-price prompt on every single turn of a run.
//
// The system message carries the stable sections and the user message carries
// the volatile ones. That split is what makes the prefix an actual prefix: the
// system message is byte-identical across the turns of a run, which is the
// condition the cache is keyed on.
//
// Memory is the one section that does NOT follow that stability ordering, and
// ADR-0020 is why: it leaves the system message entirely and becomes its own
// user-role message. The system channel is a structural grant of authority, so
// a memory record placed in it is obeyed for where it sits rather than for what
// it says. ADR-0025 brought this assembler under that decision after a probe
// measured it still concatenating memory between "You are backend." and the
// shared instructions -- the exact text ADR-0020 quotes as the defect it exists
// to remove, in a package ADR-0020 lists as affected.
//
// Memory sits between the system message and the volatile user content, so the
// cacheable prefix keeps the stability it had: identity and situation are still
// byte-identical across the turns of a run, and memory still precedes the causes
// that change every turn.
func buildMessages(cs kernel.ContextSpec, prompt string) []chatMessage {
	var sys strings.Builder

	// Identity first. It is the only section guaranteed constant for the whole
	// life of a member, so it is the strongest possible prefix.
	if cs.Identity != "" {
		sys.WriteString("You are ")
		sys.WriteString(cs.Identity)
		sys.WriteString(".\n")
	}

	writeSection(&sys, "Situation", cs.Situation)
	writeSection(&sys, "Shared", cs.Shared)

	msgs := make([]chatMessage, 0, 3)
	if s := strings.TrimSpace(sys.String()); s != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: s})
	}

	// Memory is a single string, not a list -- it is prose the run carries
	// forward rather than a set of facts. Written as a paragraph so it does not
	// acquire a bullet it never had.
	//
	// The "Memory:" label is legibility for the model, never a boundary: a
	// record can contain the same word, which is precisely why ADR-0020
	// discarded delimiter marking and moved the channel instead. The guarantee
	// is the role.
	if m := strings.TrimSpace(cs.Memory); m != "" {
		msgs = append(msgs, chatMessage{Role: "user", Content: memoryContent(m)})
	}

	// Causes and the prompt go in the user message: they are what changed since
	// the last turn, and they are the reason this turn is happening at all.
	var user strings.Builder
	writeSection(&user, "Why you were activated", cs.Cause)
	if prompt != "" {
		if user.Len() > 0 {
			user.WriteString("\n")
		}
		user.WriteString(prompt)
	}

	// A turn with nothing to say still needs a user message: many providers
	// refuse a conversation that is system-only with a 400, and that refusal
	// would arrive as a domain error on a turn that was merely empty.
	content := strings.TrimSpace(user.String())
	if content == "" {
		content = "Proceed."
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: content})
	return msgs
}

// memoryContent frames one memory message.
//
// It matches contextprep's memoryMessageText by shape rather than by import.
// The two assemblers are deliberately separate -- one freezes a durable
// artifact, the other builds a legacy single-turn request -- and sharing a
// helper would not have prevented the divergence ADR-0025 found, because the
// divergence was the channel, not the framing.
func memoryContent(memory string) string {
	var b strings.Builder
	b.WriteString("Memory:\n")
	b.WriteString(memory)
	b.WriteString("\n")
	return b.String()
}

// writeSection appends a titled list, or nothing at all when the list is empty.
//
// An empty section is omitted rather than written as a bare heading, because a
// heading with nothing under it still consumes tokens and still differs from the
// same prompt without it -- so it costs money AND breaks the prefix match.
func writeSection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(title)
	b.WriteString(":\n")
	for _, it := range items {
		if it == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(it)
		b.WriteString("\n")
	}
}

// maxTokensFor decides the ceiling on the reply.
//
// ContextSpec.MaxTokens is a budget for the WHOLE context, and the reducer
// defaults it to 24000. Spending all of it on output would leave nothing for the
// prompt, so the reply gets a fraction. The fraction is deliberate and
// conservative: a reply is normally far shorter than the context that produced
// it, and the cost of guessing low is a truncated answer the log records as
// finish_reason "length", while the cost of guessing high is a bill.
func maxTokensFor(cs kernel.ContextSpec) int {
	const (
		fallback = 4096
		fraction = 4 // a quarter of the context budget
		floor    = 256
	)
	if cs.MaxTokens <= 0 {
		return fallback
	}
	n := cs.MaxTokens / fraction
	if n < floor {
		// A context budget so small that a quarter of it cannot hold a sentence.
		// Returning that quarter would produce replies cut off mid-word on every
		// turn, which reads as a broken model rather than a misconfigured limit.
		return floor
	}
	return n
}
