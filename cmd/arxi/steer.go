package main

import "github.com/michiTrader/arxi/internal/kernel"

// cmdRunSteer implements `arxi run steer <run> <text> [--to X] [--if-seq N]`.
//
// # What steering is, given that the reducer cannot tell it from a prompt
//
// Both verbs reach applyInjection and are handled identically, so the honest
// question is why there are two. The answer is not in the behaviour, it is in
// the log: `run prompt` records that somebody wanted something NEW, and
// `run steer` records that somebody found the current direction WRONG. Six
// weeks later, `event trace` can be asked which of those bought a turn, and the
// two are not the same finding -- one says the plan was incomplete, the other
// says the team was going the wrong way. Collapsing them would make the cheaper
// half of that answer unrecoverable, because a log cannot be re-derived.
//
// So the verbs differ in what they RECORD and not in what they do, and this file
// exists to say the second half of that out loud.
//
// # The one thing steering does not do, and the trap in its own surface entry
//
// It does not interrupt. `--on-busy` was declared with `steer` as its default,
// which names ADR-0005's discarded alternative -- "interrupt the running turn
// and restart it with the new context", discarded "because it throws away work
// already paid for". Since parseInvocation applies declared defaults (see the
// comment at trigger.go's default/required/enum ordering), that entry would have
// made this command refuse itself on every plain invocation: the shared body
// rejects any --on-busy the reducer does not implement, and the caller had not
// typed one. The default was corrected to `queue` in the same change that wired
// this verb, and injectCause explains the discarded alternative when somebody
// asks for it explicitly rather than pretending it is merely unbuilt.
//
// A busy member therefore accumulates the correction in pending_causes and gets
// it when its turn ends, coalesced with anything else waiting. That is slower to
// take effect than an interrupt and it is what keeps the paid-for work.
func cmdRunSteer(args []string) {
	injectCause(injection{
		verb:    "steer",
		typ:     kernel.AgentSteered,
		past:    "steered",
		noun:    "steer",
		is:      "the correction the run acts on",
		example: "what to do differently",
	}, args)
}
