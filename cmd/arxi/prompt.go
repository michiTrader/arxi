package main

import "github.com/michiTrader/arxi/internal/kernel"

// cmdRunPrompt implements `arxi run prompt <run> <text> [--to X] [--if-seq N]`.
//
// # The remedy `run why` prints for the one state that has no other way out
//
// This was the fifth gap of the worst kind -- a remedy this project prints and
// the binary refuses -- and it was created by closing the fourth. Wiring
// `run why` in the previous step made kernel.Explain's quiescence remedy
// reachable for the first time, and the top line of it is:
//
//	$ arxi run prompt <run> "..."   # inject a new cause
//
// Typing that answered "arxi run prompt is declared in the surface but not
// implemented yet". The gap is sharper than the previous four, because
// quiescence is defined as the state where nothing is coming on its own: every
// member is idle, no cause is pending, and the advance rule cannot be met by
// anybody who is left. `run unpause` does not help -- the run is not paused.
// Answering the inbox does not help -- there is no question. The run needs a new
// cause from outside, and this is the command that supplies one. Until it
// existed, the tool could diagnose that state precisely and then offer nothing
// that worked.
//
// # Why this file holds almost nothing
//
// The mechanism lives in injectCause, because `run steer` is the same mechanism
// with a different provenance and ADR-0005 has exactly one of these. What is
// prompt's alone is the vocabulary: a prompt is a NEW cause, so an empty one is
// refused in terms of what the run was going to be asked to do, and the log
// records run.prompt so `event trace` can later distinguish "somebody wanted
// something new" from "somebody corrected the course".
func cmdRunPrompt(args []string) {
	injectCause(injection{
		verb:    "prompt",
		typ:     kernel.RunPrompt,
		past:    "prompted",
		noun:    "prompt",
		is:      "the new cause the run acts on",
		example: "what you want to happen",
	}, args)
}
