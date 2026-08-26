package logstore

import (
	"testing"

	"github.com/michiTrader/iash/internal/exec"
	"github.com/michiTrader/iash/internal/kernel"
)

// TestStoreSatisfiesTheExecutorsLogInterface is the seam test between the two
// halves of the executor, which were written in parallel by different authors
// against the same ADRs and never compiled together until now.
//
// It lives in the _test file of this package on purpose. internal/exec must not
// import internal/logstore — TestExecutorDependsOnlyOnTheKernel enforces that,
// so the runner stays testable without a filesystem and storage details cannot
// leak into the run loop. The dependency therefore has to be asserted from this
// side, and only in a test, so no production import is created in either
// direction.
//
// Without this assertion the two packages agree only by coincidence. The
// mismatch would not surface in either package's own test suite; it would
// surface at the wiring site in cmd/iash, which is the worst place to discover
// that the storage contract and the runner's expectation of it disagree.
func TestStoreSatisfiesTheExecutorsLogInterface(t *testing.T) {
	var _ exec.Log = (*Store)(nil)

	// A nil-pointer assertion only proves the method set matches. This proves
	// the methods actually work when reached through the interface, which is how
	// the runner will reach them: a Store whose Append satisfied the signature
	// but rejected the runner's events would still pass the line above.
	s := open(t)
	var logIface exec.Log = s

	out, err := logIface.Append([]kernel.Event{runStarted()})
	if err != nil {
		t.Fatalf("Append through exec.Log failed: %v.\n"+
			"The runner reaches the store only through this interface, so a failure "+
			"here means the run loop cannot record anything and every event the "+
			"reducer produces is lost", err)
	}
	if len(out) != 1 || out[0].Seq != 1 {
		t.Fatalf("Append through exec.Log returned %d events with first seq %d, want "+
			"1 event at seq 1: the runner relies on the returned seq to build "+
			"caused_by chains for the events it writes next", len(out), seqOf(out))
	}

	st, err := logIface.Fold(bp(), 0)
	if err != nil {
		t.Fatalf("Fold through exec.Log failed: %v.\n"+
			"The runner folds to honour the Snapshot effect; if this path is broken "+
			"the run cannot materialise its own state", err)
	}
	if st.RunID != "r1" {
		t.Fatalf("Fold through exec.Log gave run_id %q, want \"r1\": the runner would "+
			"be snapshotting a state that does not correspond to the run", st.RunID)
	}

	if err := logIface.WriteSnapshot(st, 1); err != nil {
		t.Fatalf("WriteSnapshot through exec.Log failed: %v", err)
	}
}

func seqOf(events []kernel.Event) int64 {
	if len(events) == 0 {
		return 0
	}
	return events[0].Seq
}
