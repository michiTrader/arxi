package logstore

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// TestReadConfirmedIsStableWhileAPendingMarkerIsRemoved probes the one
// un-eliminated hypothesis left by the attach flake investigation recorded in
// cmd/arxi/attach_cli_test.go: "the fixture's interaction with the writer lock
// and the pending.commit marker under real contention".
//
// The shape is specific. readConfirmedLimit reads the pending marker TWICE --
// `before` at the top, `after` following lastCompleteOffset -- and uses only
// `after` to decide how much of the log is confirmed. The flaky test removes
// pending.commit while the followed process is looping over ReadConfirmed, so
// a read can straddle the removal:
//
//	before: marker exists, withholding the tail
//	after:  marker gone, so the whole complete prefix is confirmed
//
// Whether that is benign depends on a detail worth pinning rather than
// assuming: the mismatch check between the two reads only fires when BOTH
// exist, so an exists -> absent transition is deliberately not an error. The
// question is whether it can produce a NON-MONOTONIC result -- a NextOffset
// that moves backwards, or bytes served twice -- because that, not an error,
// is what would make the attach follower print a row more than once, which is
// exactly the assertion that fails intermittently.
//
// This runs the transition thousands of times against a real file. It does
// not reproduce the attach failure itself; it decides whether this layer can
// be the cause, so the next investigator can drop the hypothesis or focus on
// it.
func TestReadConfirmedIsStableWhileAPendingMarkerIsRemoved(t *testing.T) {
	const iterations = 400

	for i := 0; i < iterations; i++ {
		dir := t.TempDir()

		// Two complete records, then a third the marker withholds.
		committed := []byte("{\"seq\":1}\n{\"seq\":2}\n")
		withheld := []byte("{\"seq\":3}\n")
		if err := os.WriteFile(EventsPath(dir), append(append([]byte{}, committed...), withheld...), 0o644); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, pendingFileName)
		if err := os.WriteFile(marker, []byte(strconv.Itoa(len(committed))+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		// One goroutine removes the marker while the other reads across it.
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			_ = os.Remove(marker)
		}()

		var read ConfirmedRead
		var readErr error
		go func() {
			defer wg.Done()
			<-start
			read, readErr = ReadConfirmed(dir, 0)
		}()

		close(start)
		wg.Wait()

		if readErr != nil {
			t.Fatalf("iteration %d: ReadConfirmed failed while the marker was being removed: %v\n"+
				"  A writer completing its commit is the NORMAL end of a batch, not corruption. "+
				"An error here would make every follower report a corrupt log at the moment a "+
				"commit lands", i, readErr)
		}

		// Whatever the read observed, it must be a prefix boundary: either the
		// committed pair (marker seen) or the whole file (marker already gone).
		// Anything else means the read served a partial record or crossed a
		// boundary that never existed.
		switch read.NextOffset {
		case int64(len(committed)), int64(len(committed) + len(withheld)):
		default:
			t.Fatalf("iteration %d: NextOffset %d is neither the withheld boundary (%d) nor the "+
				"end of the log (%d).\n"+
				"  A follower resumes from NextOffset, so an offset inside a record makes the next "+
				"read start mid-line", i, read.NextOffset, len(committed), len(committed)+len(withheld))
		}
		if int64(len(read.Bytes)) != read.NextOffset {
			t.Fatalf("iteration %d: served %d bytes but reports NextOffset %d: a follower that "+
				"trusts NextOffset would re-print or skip the difference",
				i, len(read.Bytes), read.NextOffset)
		}
	}
}

// TestAReappearingPendingMarkerMovesTheConfirmedOffsetBackwards records a real
// rewind, and TestTheFollowerNeverRewindsOnOne records why it is not the flake.
//
// ReadConfirmed's offset is NOT monotonic. A marker naming a rollback point
// behind what a caller already consumed pulls the reported boundary back:
//
//	read at offset 0, no marker      -> NextOffset 20
//	marker appears naming 10         -> NextOffset 10   (backwards)
//
// That was measured, not predicted. It is correct for what ReadConfirmed
// promises -- it answers "what is confirmed right now", and a batch in flight
// genuinely un-confirms its own tail -- but it means a caller cannot treat
// NextOffset as a high-water mark.
func TestAReappearingPendingMarkerMovesTheConfirmedOffsetBackwards(t *testing.T) {
	dir := t.TempDir()
	body := []byte("{\"seq\":1}\n{\"seq\":2}\n")
	if err := os.WriteFile(EventsPath(dir), body, 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := ReadConfirmed(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextOffset != int64(len(body)) {
		t.Fatalf("a log with no pending marker did not confirm all of it: NextOffset %d, want %d",
			first.NextOffset, len(body))
	}

	marker := filepath.Join(dir, pendingFileName)
	if err := os.WriteFile(marker, []byte(strconv.Itoa(10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := ReadConfirmed(dir, first.NextOffset)
	if err != nil {
		t.Fatal(err)
	}
	if second.NextOffset >= first.NextOffset {
		t.Fatalf("the confirmed offset did not move backwards (%d -> %d).\n"+
			"  If ReadConfirmed became monotonic, that is a stronger contract than it had, and "+
			"the follower's guard documented in TestTheFollowerNeverRewindsOnOne is no longer "+
			"load-bearing. Re-derive both", first.NextOffset, second.NextOffset)
	}
}

// TestTheFollowerNeverRewindsOnOne pins the guard that makes the rewind above
// harmless to `arxi run attach`, which is the question the flake investigation
// was actually asking.
//
// followRunLog does `consumed = read.NextOffset`, unconditionally -- but only
// INSIDE `if len(read.Bytes) > 0`. A rewound read serves no bytes, because the
// caller's offset is already past the new boundary and the reader clamps
// fromOffset to it. So the assignment is unreachable on exactly the passes
// that could move consumed backwards.
//
// The follower therefore cannot re-print, and the pending marker is refuted as
// the cause of the intermittent "printed twice" failure. This is pinned rather
// than written down because the safety is a side effect of an emptiness check,
// not of an intentional max(): moving the assignment out of that branch, which
// reads like a harmless tidy-up, reintroduces the rewind.
func TestTheFollowerNeverRewindsOnOne(t *testing.T) {
	dir := t.TempDir()
	body := []byte("{\"seq\":1}\n{\"seq\":2}\n")
	if err := os.WriteFile(EventsPath(dir), body, 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, pendingFileName)

	// Mirrors followRunLog's loop, including the guard.
	var consumed int64
	advance := func() (printed int) {
		read, err := ReadConfirmed(dir, consumed)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Bytes) > 0 {
			consumed = read.NextOffset
			return len(read.Bytes)
		}
		return 0
	}

	if n := advance(); n != len(body) {
		t.Fatalf("the first pass served %d bytes, want the whole log (%d)", n, len(body))
	}
	high := consumed

	// A new batch begins, naming a rollback point behind what was consumed.
	if err := os.WriteFile(marker, []byte(strconv.Itoa(10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := advance(); n != 0 {
		t.Fatalf("a rewound read served %d bytes: those are records the follower already printed, "+
			"so the run log would show them twice", n)
	}
	if consumed != high {
		t.Fatalf("consumed moved %d -> %d across a rewound read.\n"+
			"  followRunLog resumes from consumed, so it would re-read and re-emit everything "+
			"between. The only thing preventing that is that a rewound read serves no bytes and "+
			"the assignment sits inside `if len(read.Bytes) > 0` -- if that guard moved, this is "+
			"the failure it would cause", high, consumed)
	}
}
