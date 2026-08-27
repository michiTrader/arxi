package logstore

import "fmt"

// CASError reports that AppendIfSeq was rejected because the log had moved on.
//
// This is a distinct type, and it carries Actual, for a reason that ADR-0006
// states directly: a client whose compare-and-swap is rejected "has to re-read.
// That is a feature: the new state reaches it before it insists." If this were
// returned as a plain error string, the caller could only tell "the write did
// not happen" and would have to guess whether the right response is to re-read
// and retry or to stop and page a human. Those two responses are opposite: one
// is a normal, expected outcome of optimistic concurrency, the other means the
// disk is gone. Collapsing them is how a broken volume gets retried in a tight
// loop forever.
//
// Actual is the head the log was really at, so the caller can re-read exactly
// the range it is missing without a second round trip.
type CASError struct {
	Expected int64
	Actual   int64
}

func (e *CASError) Error() string {
	return fmt.Sprintf("append rejected: expected head seq %d, log is at %d "+
		"(re-read from seq %d and decide again; this is not an I/O failure)",
		e.Expected, e.Actual, e.Expected+1)
}

// LockedError reports that another writer already holds this run directory.
//
// It is a typed error because the caller's correct reaction is specific and not
// obvious: do not retry in a loop, and above all do not delete the lock. Two
// writers on one log produce duplicate `seq`, and a duplicate `seq` is what
// breaks the CAS of ADR-0006 — the failure does not appear at write time, it
// appears weeks later as a run whose history cannot be folded.
type LockedError struct {
	Dir   string
	Owner string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("run directory %s is already open for writing by %s: "+
		"exactly one writer per log is required, because two writers produce "+
		"duplicate seq and an unfoldable log. If that writer is certainly dead, "+
		"remove %s/%s by hand after confirming no process is running.",
		e.Dir, e.Owner, e.Dir, lockFileName)
}

// CorruptError reports damage in the middle of the log, not at its tail.
//
// A torn final line is recoverable and is recovered silently (see the recovery
// comment in store.go). Damage anywhere earlier is not: the log is the only
// source of truth (ADR-0002), so skipping a bad line in the middle would leave a
// gap, and `Fold` cannot distinguish a gap from an event that never existed. It
// would produce a state that is plausible and wrong, which is precisely the
// outcome ADR-0002 was written to prevent.
type CorruptError struct {
	Dir    string
	AtSeq  int64
	Offset int64
	Reason string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("log %s is corrupt at byte offset %d (after seq %d): %s. "+
		"Refusing to open: the log is the source of truth, and folding across "+
		"damaged history would yield a state with no events to justify it. "+
		"Recover from a backup or truncate deliberately to seq %d and fork.",
		e.Dir, e.Offset, e.AtSeq, e.Reason, e.AtSeq)
}

// SeqAssignedError reports that a caller handed Append an event that already had
// a sequence number.
//
// Rejecting instead of overwriting is deliberate. ADR-0002 says the reducer
// returns events with `seq: 0` because it cannot know what global order its
// output will land in. A caller that fills in `seq` believes it controls that
// order, and it does not. Silently overwriting the value would make the write
// succeed while the caller's model of the log stayed wrong, and the divergence
// would only surface later as an event referring to a `seq` that means something
// else. Failing here costs one loud error at the boundary instead.
type SeqAssignedError struct {
	Index int
	Seq   int64
}

func (e *SeqAssignedError) Error() string {
	return fmt.Sprintf("event at index %d already has seq %d: only the log writer "+
		"assigns seq (ADR-0002), so the reducer must hand over events with seq 0. "+
		"Clear the field; do not pre-assign it.", e.Index, e.Seq)
}
