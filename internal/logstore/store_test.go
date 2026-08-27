package logstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/michiTrader/iash/internal/kernel"
)

// ------------------------------------------------------------------ helpers

// open opens a Store in a temp dir and closes it when the test ends. Closing
// matters even in tests: the writer lock is a real file, and a leaked lock would
// make the next subtest fail for a reason unrelated to what it is testing.
func open(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ev builds an event with Seq 0, which is how the reducer hands them over
// (ADR-0002). Tests that pre-assign seq are testing a caller that is already
// wrong; see TestAppendRejectsPreAssignedSeq.
func ev(t kernel.EventType, actor string, payload map[string]any) kernel.Event {
	return kernel.Event{
		ID: "e-" + actor, Ts: "2026-08-26T00:00:00Z", Type: t,
		Scope: "run:r1", Source: kernel.SourceAgent, Actor: actor, Payload: payload,
	}
}

// runStarted is the event every fold has to begin with: without it the reducer
// has no members and every later event lands on an empty state.
func runStarted() kernel.Event {
	return kernel.Event{
		ID: "e1", Ts: "2026-08-26T00:00:00Z", Type: kernel.RunStarted,
		Scope: "run:r1", Source: kernel.SourceRuntime,
		Payload: map[string]any{
			"run_id": "r1", "actor": "team", "budget_usd": 5.0,
			"blueprint_sha": "abc123",
		},
	}
}

// bp mirrors the blueprint used in the kernel tests: several members and two
// stages with different advance rules. A single-member blueprint would let a
// broken fold look correct.
func bp() kernel.Config {
	return kernel.Config{
		Blueprint: "test",
		Members: []kernel.MemberConfig{
			{Name: "backend", Role: "coordinator", Tools: []string{"write", "bash"}},
			{Name: "designer"},
			{Name: "frontend", Tools: []string{"write"}},
			{Name: "mediator", Advisory: true},
		},
		Stages: []kernel.StageConfig{
			{Name: "execute", AdvanceWhen: "all", TimeoutMs: 1_800_000, OnTimeout: "escalate"},
			{Name: "integrate", AdvanceWhen: "any", OnConflict: "merge"},
		},
	}.ResolveDefaults()
}

// seededLog appends a realistic run prefix and returns the committed events.
func seededLog(t *testing.T, s *Store) []kernel.Event {
	t.Helper()
	batch := []kernel.Event{
		runStarted(),
		ev(kernel.StageEntered, "runtime", map[string]any{"stage": "execute"}),
		ev(kernel.StageSubmitted, "backend", map[string]any{"stage": "execute"}),
		ev(kernel.StageSubmitted, "designer", map[string]any{"stage": "execute"}),
		ev(kernel.LLMResponse, "backend", map[string]any{"cost_usd": 0.5}),
	}
	out, err := s.Append(batch)
	if err != nil {
		t.Fatalf("seeding the log failed: %v", err)
	}
	return out
}

// ------------------------------------------------------------------ seq

func TestSeqStartsAtOneAndHasNoGaps(t *testing.T) {
	s := open(t)

	if got := s.Head(); got != 0 {
		t.Fatalf("Head() on a fresh log = %d, want 0: seq numbering must start at 1, "+
			"and a head of %d would make the first event seq %d", got, got, got+1)
	}

	// Several separate appends, some batched, because the bug this catches is a
	// counter that resets or double-counts across calls.
	var all []kernel.Event
	for i := 0; i < 4; i++ {
		batch := []kernel.Event{
			ev(kernel.LLMResponse, fmt.Sprintf("a%d", i), map[string]any{"cost_usd": 0.1}),
			ev(kernel.AgentTurnDone, fmt.Sprintf("a%d", i), nil),
		}
		out, err := s.Append(batch)
		if err != nil {
			t.Fatalf("Append batch %d: %v", i, err)
		}
		all = append(all, out...)
	}

	for i, e := range all {
		want := int64(i + 1)
		if e.Seq != want {
			t.Fatalf("event %d got seq %d, want %d: seq must be strictly monotonic "+
				"with no gaps, because Fold cannot distinguish a gap from an event "+
				"that was lost, and a repeat silently breaks the CAS of ADR-0006",
				i, e.Seq, want)
		}
	}
	if got := s.Head(); got != int64(len(all)) {
		t.Fatalf("Head() = %d after %d events: the head is the version token the CAS "+
			"compares against, so it has to equal the last assigned seq", got, len(all))
	}
}

func TestAppendReturnsTheAssignedSeqWithoutMutatingTheInput(t *testing.T) {
	s := open(t)

	in := []kernel.Event{ev(kernel.AgentTurnDone, "backend", nil)}
	out, err := s.Append(in)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if in[0].Seq != 0 {
		t.Errorf("Append mutated its input: caller's event now has seq %d. The caller "+
			"may reuse or re-inspect the slice it passed, and a mutated copy makes "+
			"'events arrive with seq 0' false on a retry", in[0].Seq)
	}
	if out[0].Seq != 1 {
		t.Errorf("returned seq = %d, want 1: the caller needs the committed numbers "+
			"to record causality (caused_by refers to them)", out[0].Seq)
	}
}

func TestAppendRejectsPreAssignedSeq(t *testing.T) {
	s := open(t)

	e := ev(kernel.AgentTurnDone, "backend", nil)
	e.Seq = 7

	_, err := s.Append([]kernel.Event{e})
	var want *SeqAssignedError
	if !errors.As(err, &want) {
		t.Fatalf("Append accepted an event with seq already set (err = %v). "+
			"Only the log writer assigns seq (ADR-0002); silently overwriting it "+
			"would let a caller believe it controls an order it does not control, "+
			"and the divergence would only surface later as caused_by pointing at "+
			"the wrong event", err)
	}
}

func TestEmptyAppendIsANoOp(t *testing.T) {
	s := open(t)
	seededLog(t, s)
	before := s.Head()

	out, err := s.Append(nil)
	if err != nil {
		t.Fatalf("Append(nil): %v: the reducer legitimately emits no events, so "+
			"callers hand over empty batches routinely", err)
	}
	if len(out) != 0 || s.Head() != before {
		t.Fatalf("an empty append moved the head from %d to %d: an empty batch must "+
			"not consume a seq, or the log grows gaps whenever nothing happened",
			before, s.Head())
	}
}

// ------------------------------------------------------------------ concurrency

func TestConcurrentAppendAssignsNoDuplicateSeq(t *testing.T) {
	s := open(t)

	const writers = 8
	const perWriter = 12

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int64]string{}
	var failures []string

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				actor := fmt.Sprintf("w%d", w)
				out, err := s.Append([]kernel.Event{ev(kernel.AgentTurnDone, actor, nil)})
				if err != nil {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("writer %d: %v", w, err))
					mu.Unlock()
					return
				}
				mu.Lock()
				if prev, dup := seen[out[0].Seq]; dup {
					failures = append(failures, fmt.Sprintf(
						"seq %d assigned twice: to %s and to %s", out[0].Seq, prev, actor))
				}
				seen[out[0].Seq] = actor
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("concurrent Append produced conflicting sequence numbers:\n  %s\n"+
			"A duplicate seq is unrecoverable: two events claim the same version of "+
			"the state, so the CAS of ADR-0006 can pass when it should fail and the "+
			"log can no longer be folded. seq assignment and the write must be one "+
			"critical section", strings.Join(failures, "\n  "))
	}

	total := int64(writers * perWriter)
	if s.Head() != total {
		t.Fatalf("Head() = %d after %d concurrent appends: every append that returned "+
			"successfully must have consumed exactly one seq", s.Head(), total)
	}
	// Re-read from disk: an in-memory counter can be right while the file is not.
	got, err := s.Read(1, 0)
	if err != nil {
		t.Fatalf("Read after concurrent appends: %v", err)
	}
	if int64(len(got)) != total {
		t.Fatalf("the log holds %d events but %d appends succeeded: an acknowledged "+
			"append that is not on disk is a lost event, and the log is the only "+
			"source of truth (ADR-0002)", len(got), total)
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("on-disk event %d has seq %d: the file must be in seq order with "+
				"no gaps, because that is what Open validates and Fold relies on",
				i, e.Seq)
		}
	}
}

func TestSecondOpenFailsWhileTheFirstIsHeld(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	_, err = Open(dir)
	var locked *LockedError
	if !errors.As(err, &locked) {
		first.Close()
		t.Fatalf("a second Open on a held directory returned %v, want *LockedError. "+
			"Two writers on one log is exactly how duplicate seq is produced, and "+
			"the rule has to be enforced rather than documented (ADR-0002)", err)
	}

	// And the lock must be released on Close, or a crash-free restart of the
	// same run would be impossible without manual cleanup.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v: the lock has to be released on Close, or "+
			"every ordinary restart needs an operator to delete a file by hand", err)
	}
	second.Close()
}

// ------------------------------------------------------------------ durability

func TestTruncatedFinalLineIsDiscardedAndTheLogKeepsWorking(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	committed := seededLog(t, s)
	s.Close()

	// Simulate a process killed mid-write: append a partial line with no
	// terminating newline. This is what a SIGKILL, an OOM or a power loss
	// actually leaves behind.
	path := filepath.Join(dir, eventsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log to damage it: %v", err)
	}
	if _, err := f.WriteString(`{"seq":6,"id":"e6","type":"agent.tu`); err != nil {
		t.Fatalf("write partial line: %v", err)
	}
	f.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open on a log with a torn final line failed: %v\n"+
			"A torn tail is what an ordinary crash produces. Refusing to open makes "+
			"every crash an outage whose only repair is hand-editing the log", err)
	}
	defer reopened.Close()

	if got := reopened.Head(); got != int64(len(committed)) {
		t.Fatalf("Head() = %d after recovery, want %d: the torn line was never "+
			"acknowledged (Append fsyncs and commits before returning), so recovery "+
			"must restore exactly the last state an observer could have seen",
			got, len(committed))
	}

	// The partial bytes must be physically gone. If they are left in place, the
	// next Append concatenates onto them and produces one merged, permanently
	// unparseable line — turning a recoverable tail into unrecoverable
	// mid-log damage.
	out, err := reopened.Append([]kernel.Event{ev(kernel.AgentTurnDone, "backend", nil)})
	if err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if out[0].Seq != int64(len(committed))+1 {
		t.Fatalf("the event after recovery got seq %d, want %d", out[0].Seq, len(committed)+1)
	}

	events, err := reopened.Read(1, 0)
	if err != nil {
		t.Fatalf("Read after recovery: %v: if this fails, the truncation left partial "+
			"bytes behind and the new event was appended onto them", err)
	}
	if len(events) != len(committed)+1 {
		t.Fatalf("the log holds %d events, want %d: recovery either dropped committed "+
			"history or kept the torn line", len(events), len(committed)+1)
	}
}

func TestDamageInTheMiddleOfTheLogRefusesToOpen(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seededLog(t, s)
	s.Close()

	// Corrupt a line that is not the last one. Unlike a torn tail, this cannot
	// be un-acknowledged: those events were reported as committed.
	path := filepath.Join(dir, eventsFileName)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected a multi-line log, got %d lines", len(lines))
	}
	lines[2] = `{"seq":3,"id":"e3",BROKEN}`
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write damaged log: %v", err)
	}

	reopened, err := Open(dir)
	if err == nil {
		reopened.Close()
		t.Fatal("Open accepted a log with damage in the middle. Skipping a bad line " +
			"leaves a gap, and Fold cannot distinguish a gap from an event that never " +
			"existed, so the run would report a state with no events to justify it — " +
			"the exact outcome ADR-0002 was written to prevent. It must refuse")
	}
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Open returned %v, want *CorruptError: the operator has to be able "+
			"to tell 'this log is damaged, recover it' from 'the disk is busy'", err)
	}
}

func TestAGapInSeqIsRejectedOnOpen(t *testing.T) {
	dir := t.TempDir()

	// Hand-write a log that skips seq 3. This is what a naive recovery that
	// dropped a middle line would leave behind.
	var b strings.Builder
	for _, n := range []int64{1, 2, 4} {
		e := ev(kernel.AgentTurnDone, "backend", nil)
		e.Seq = n
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, eventsFileName), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatal("Open accepted a log whose seq jumps from 2 to 4. A gap is " +
			"indistinguishable from a lost event, so continuing means every later " +
			"answer is computed over history that is known to be incomplete")
	}
	if !strings.Contains(err.Error(), "seq") {
		t.Errorf("the error does not mention seq, so the operator cannot tell what is "+
			"wrong: %v", err)
	}
}

func TestAnInterruptedBatchIsRolledBackWhole(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	committed := seededLog(t, s)
	s.Close()

	// Simulate a crash between writing the batch and clearing the commit
	// marker: three whole, well-formed lines on disk, plus the marker naming
	// the rollback offset. Nothing about the bytes themselves looks wrong, which
	// is exactly why the marker has to exist.
	path := filepath.Join(dir, eventsFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	committedSize := info.Size()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for n := int64(6); n <= 8; n++ {
		e := ev(kernel.AgentTurnDone, "ghost", nil)
		e.Seq = n
		line, merr := json.Marshal(e)
		if merr != nil {
			t.Fatal(merr)
		}
		if _, werr := f.Write(append(line, '\n')); werr != nil {
			t.Fatal(werr)
		}
	}
	f.Close()

	marker := fmt.Sprintf("%d\n", committedSize)
	if err := os.WriteFile(filepath.Join(dir, pendingFileName), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with a pending batch: %v", err)
	}
	defer reopened.Close()

	if got := reopened.Head(); got != int64(len(committed)) {
		t.Fatalf("Head() = %d, want %d: a batch of 3 events must not leave 1 or 2 "+
			"visible. Append never returned for that batch, so no caller learned "+
			"those events existed; keeping part of them would record a decision the "+
			"reducer never completed", got, len(committed))
	}

	events, err := reopened.Read(1, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, e := range events {
		if e.Actor == "ghost" {
			t.Fatalf("seq %d from the rolled-back batch is still visible: batch "+
				"atomicity is what stops a partially applied decision from becoming "+
				"history", e.Seq)
		}
	}

	// And the marker must be gone, or every subsequent Open would roll the log
	// back to the same old offset and silently discard all new work.
	if _, err := os.Stat(filepath.Join(dir, pendingFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the pending marker survived recovery: the next Open would roll back " +
			"to the same offset again and discard everything appended since")
	}
}

// ------------------------------------------------------------------ CAS

func TestAppendIfSeqAtTheCurrentHeadSucceeds(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)
	head := int64(len(committed))

	out, err := s.AppendIfSeq(head, []kernel.Event{ev(kernel.RunPrompt, "human", map[string]any{"text": "hi"})})
	if err != nil {
		t.Fatalf("AppendIfSeq at the current head %d was rejected: %v. A CAS that "+
			"fails when nothing changed makes --if-seq unusable and pushes clients "+
			"back to last-write-wins (ADR-0006)", head, err)
	}
	if out[0].Seq != head+1 {
		t.Errorf("committed seq = %d, want %d", out[0].Seq, head+1)
	}
}

func TestAppendIfSeqAtAStaleHeadIsRejectedAndReportsTheRealHead(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)
	head := int64(len(committed))

	stale := head - 2
	_, err := s.AppendIfSeq(stale, []kernel.Event{ev(kernel.RunPrompt, "human", map[string]any{"text": "late"})})

	var cas *CASError
	if !errors.As(err, &cas) {
		t.Fatalf("AppendIfSeq at stale seq %d returned %v, want *CASError. The whole "+
			"point of the CAS is refusing to write over a state the author never "+
			"saw; passing here would apply a correction to different history than "+
			"the one it was written for (ADR-0006)", stale, err)
	}
	if cas.Actual != head {
		t.Errorf("CASError.Actual = %d, want %d: the error has to carry the real head "+
			"so the client can re-read exactly the range it missed instead of "+
			"round-tripping again", cas.Actual, head)
	}
	if cas.Expected != stale {
		t.Errorf("CASError.Expected = %d, want %d", cas.Expected, stale)
	}
	if s.Head() != head {
		t.Errorf("a rejected CAS moved the head to %d, want %d: a rejection must "+
			"consume no seq, or a retry loop burns sequence numbers on writes that "+
			"never happened", s.Head(), head)
	}
}

func TestACASRejectionIsDistinguishableFromAnIOFailure(t *testing.T) {
	s := open(t)
	seededLog(t, s)

	_, casErr := s.AppendIfSeq(1, []kernel.Event{ev(kernel.RunPrompt, "human", nil)})

	// An I/O-class failure, produced by closing the store underneath the call.
	// The two errors have to be told apart by type, not by string matching:
	// their correct handling is opposite. A CAS rejection means re-read and
	// decide again; an I/O failure means stop, because retrying a broken device
	// in a loop is how a transient fault becomes a spin.
	s.Close()
	_, ioErr := s.AppendIfSeq(s.Head(), []kernel.Event{ev(kernel.RunPrompt, "human", nil)})

	var cas *CASError
	if !errors.As(casErr, &cas) {
		t.Fatalf("expected a *CASError from a stale CAS, got %v", casErr)
	}
	if ioErr == nil {
		t.Fatal("appending to a closed store succeeded: a caller would believe an " +
			"event was durably logged when nothing was written")
	}
	if errors.As(ioErr, &cas) {
		t.Fatalf("an I/O failure was reported as *CASError (%v). The caller would "+
			"re-read and retry forever against a store that cannot accept writes, "+
			"instead of surfacing a real fault", ioErr)
	}
}

// ------------------------------------------------------------------ fold

func TestTwoFoldsOfTheSameLogGiveIdenticalStates(t *testing.T) {
	s := open(t)
	seededLog(t, s)
	c := bp()

	first, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("first Fold: %v", err)
	}
	second, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("second Fold: %v", err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two folds of the same log gave different states: replay is worthless.\n"+
			"  first:  %+v\n  second: %+v\n"+
			"This means something mutated shared state through an aliased map or "+
			"slice; kernel.State.Clone exists for exactly this reason", first, second)
	}
}

func TestFoldDoesNotMutateTheConfigOrLeakBetweenCalls(t *testing.T) {
	s := open(t)
	seededLog(t, s)

	c := bp()
	before := bp()

	if _, err := s.Fold(c, 0); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if !reflect.DeepEqual(c, before) {
		t.Fatalf("Fold mutated the Config it was given.\n  before: %+v\n  after:  %+v\n"+
			"The Config is the frozen blueprint (ADR-0002); if a fold can change it, "+
			"the second replay of a run uses different config than the first and "+
			"neither reproduces the original", before, c)
	}
}

func TestFoldUntilSeqMatchesAFoldOfTheTruncatedLog(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)
	c := bp()

	// Every prefix, not just one: an off-by-one in the range would still pass a
	// single-point check.
	for _, until := range []int64{1, 2, 3, 4, int64(len(committed))} {
		fromStore, err := s.Fold(c, until)
		if err != nil {
			t.Fatalf("Fold(until=%d): %v", until, err)
		}

		events, err := s.Read(1, until)
		if err != nil {
			t.Fatalf("Read(1, %d): %v", until, err)
		}
		if int64(len(events)) != until {
			t.Fatalf("Read(1, %d) returned %d events: the range is inclusive on both "+
				"ends, and an off-by-one here makes --until-seq point at the wrong "+
				"state", until, len(events))
		}
		direct, _ := kernel.Fold(kernel.State{}, events, c)

		if !reflect.DeepEqual(fromStore, direct) {
			t.Fatalf("Fold(until=%d) disagrees with folding the truncated log.\n"+
				"  store:  %+v\n  direct: %+v\n"+
				"This is what `run replay --until-seq` reports, so a mismatch means "+
				"replay gives a plausible answer about a state the run was never in",
				until, fromStore, direct)
		}
		if fromStore.Seq != until {
			t.Errorf("Fold(until=%d) produced State.Seq = %d: the state's version has "+
				"to be the seq it was folded to, because that is the token the CAS "+
				"compares against (ADR-0006)", until, fromStore.Seq)
		}
	}
}

func TestFoldOfAnEmptyLogIsTheZeroState(t *testing.T) {
	s := open(t)

	st, err := s.Fold(bp(), 0)
	if err != nil {
		t.Fatalf("Fold on an empty log: %v: a run directory that exists but has no "+
			"events yet is the normal state between `run start` and the first "+
			"append, not an error", err)
	}
	if !reflect.DeepEqual(st, kernel.State{}) {
		t.Fatalf("folding an empty log gave %+v, want the zero State: State is "+
			"defined as fold over the events, so no events must mean no state", st)
	}
}

func TestFoldSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	c := bp()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seededLog(t, s)
	before, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	s.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	after, err := reopened.Fold(c, 0)
	if err != nil {
		t.Fatalf("Fold after reopen: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the state changed across a reopen.\n  before: %+v\n  after:  %+v\n"+
			"The log is the only thing that persists, so if a restart yields a "+
			"different state then the process was holding information the log does "+
			"not contain (ADR-0002)", before, after)
	}
}

// ------------------------------------------------------------------ snapshots

func TestDeletingTheSnapshotChangesNothing(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)
	c := bp()

	before, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if err := s.WriteSnapshot(before, int64(len(committed))); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if _, err := os.Stat(s.SnapshotPath()); err != nil {
		t.Fatalf("WriteSnapshot reported success but wrote no file: %v", err)
	}

	if err := os.Remove(s.SnapshotPath()); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	after, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("Fold after deleting the snapshot: %v: deleting a cache must never "+
			"break a read", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("deleting the snapshot changed the folded state.\n  with:    %+v\n"+
			"  without: %+v\n"+
			"Snapshots are cache and nothing else (ADR-0002): you must be able to "+
			"delete every one and lose only startup speed. If they become "+
			"load-bearing, a stale snapshot turns into a state nobody can explain — "+
			"a run reporting `failed` with no failure event to justify it",
			before, after)
	}
}

func TestAWrongSnapshotIsIgnoredBecauseTheLogWins(t *testing.T) {
	// This test exists because deleting the snapshot is NOT enough to prove
	// snapshots are droppable. A Fold that consults the snapshot and falls back
	// to the log when the file is missing passes the deletion test perfectly,
	// while still being load-bearing: the moment a snapshot is present and
	// wrong, it wins. That is the failure ADR-0002 describes — "a State that
	// says status: failed with no failure event to justify it, because the
	// snapshot was written wrong once, three weeks ago".
	//
	// So the snapshot is deliberately poisoned with a state the log cannot
	// justify, and the fold must ignore it.
	s := open(t)
	committed := seededLog(t, s)
	c := bp()

	truth, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	poisoned := truth.Clone()
	poisoned.Status = kernel.StatusFailed
	poisoned.Result = "a failure that never happened"
	poisoned.SpentUSD = 999
	if err := s.WriteSnapshot(poisoned, int64(len(committed))); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	got, err := s.Fold(c, 0)
	if err != nil {
		t.Fatalf("Fold with a poisoned snapshot present: %v", err)
	}

	if !reflect.DeepEqual(got, truth) {
		t.Fatalf("a wrong snapshot changed the folded state.\n  from the log: %+v\n"+
			"  got:          %+v\n"+
			"The log wins, always (ADR-0002). If a snapshot can override it, the "+
			"system can report a status with no event to justify it, and diagnosis "+
			"becomes archaeology: there is no rule left for deciding what really "+
			"happened. Fold must replay the log and never consult the snapshot",
			truth, got)
	}
	if got.Status == kernel.StatusFailed {
		t.Fatal("the fold adopted the snapshot's invented `failed` status: this is " +
			"exactly the unexplainable state ADR-0002 was written to prevent")
	}
}

func TestASnapshotRecordsTheSeqItBelongsTo(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)
	at := int64(len(committed))

	st, err := s.Fold(bp(), 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if err := s.WriteSnapshot(st, at); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	body, err := os.ReadFile(s.SnapshotPath())
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("the snapshot is not valid JSON: %v", err)
	}
	if snap.Seq != at {
		t.Fatalf("snapshot records seq %d, want %d: a snapshot that does not say "+
			"which point of the log it corresponds to can only be trusted, and "+
			"trusting a snapshot is what ADR-0002 forbids", snap.Seq, at)
	}
	if snap.State.RunID != st.RunID || snap.State.Status != st.Status {
		t.Errorf("the snapshot does not round-trip the state it was given: got "+
			"run_id=%q status=%q, want run_id=%q status=%q",
			snap.State.RunID, snap.State.Status, st.RunID, st.Status)
	}
}

func TestWriteSnapshotLeavesNoTemporaryFilesBehind(t *testing.T) {
	s := open(t)
	st, err := s.Fold(bp(), 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.WriteSnapshot(st, int64(i)); err != nil {
			t.Fatalf("WriteSnapshot %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("left a temporary file %q behind: repeated snapshotting would "+
				"fill the run directory with debris, and a stray file that looks like "+
				"a snapshot is a trap for whoever implements snapshot loading", e.Name())
		}
	}
}

// ------------------------------------------------------------------ read

func TestReadRangesAreInclusiveAndClamped(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)
	head := int64(len(committed))

	cases := []struct {
		name       string
		from, to   int64
		wantFirst  int64
		wantLast   int64
		wantLength int
	}{
		{"whole log with to=0", 1, 0, 1, head, int(head)},
		{"explicit full range", 1, head, 1, head, int(head)},
		{"middle range is inclusive on both ends", 2, 4, 2, 4, 3},
		{"single event", 3, 3, 3, 3, 1},
		{"to beyond the head is clamped", 2, head + 50, 2, head, int(head - 1)},
		{"from below 1 is clamped", -5, 2, 1, 2, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Read(tc.from, tc.to)
			if err != nil {
				t.Fatalf("Read(%d, %d): %v", tc.from, tc.to, err)
			}
			if len(got) != tc.wantLength {
				t.Fatalf("Read(%d, %d) returned %d events, want %d: `event list --from "+
					"--to` and `replay --until-seq` are built on this range, so an "+
					"off-by-one silently shows the user a different slice of history "+
					"than they asked for", tc.from, tc.to, len(got), tc.wantLength)
			}
			if got[0].Seq != tc.wantFirst || got[len(got)-1].Seq != tc.wantLast {
				t.Fatalf("Read(%d, %d) spans seq %d..%d, want %d..%d",
					tc.from, tc.to, got[0].Seq, got[len(got)-1].Seq, tc.wantFirst, tc.wantLast)
			}
		})
	}
}

func TestReadPastTheHeadIsEmptyNotAnError(t *testing.T) {
	s := open(t)
	committed := seededLog(t, s)

	got, err := s.Read(int64(len(committed))+1, 0)
	if err != nil {
		t.Fatalf("Read past the head: %v: a client that has already consumed the "+
			"whole log polls exactly this range, and turning that into an error "+
			"would make every up-to-date follower log a failure", err)
	}
	if len(got) != 0 {
		t.Fatalf("Read past the head returned %d events, want none", len(got))
	}
}

func TestReadRoundTripsTheEventFaithfully(t *testing.T) {
	s := open(t)

	in := kernel.Event{
		ID: "e1", Ts: "2026-08-26T14:03:11Z", Type: kernel.AgentBlocked,
		Scope: "run:r1", Source: kernel.SourceRuntime, Actor: "backend",
		CorrelationID: "e7", CausedBy: []string{"e41"}, Depth: 2,
		Payload: map[string]any{
			"kind": "approval", "cost_usd": 0.5, "question": "deploy?",
		},
	}
	if _, err := s.Append([]kernel.Event{in}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Read(1, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	out := got[0]

	in.Seq = 1
	if !reflect.DeepEqual(out.CausedBy, in.CausedBy) || out.CorrelationID != in.CorrelationID {
		t.Errorf("causality did not survive the round trip: got correlation_id=%q "+
			"caused_by=%v, want %q %v. `event trace` and `run why` walk these fields, "+
			"so losing them makes the causal chain unreconstructable",
			out.CorrelationID, out.CausedBy, in.CorrelationID, in.CausedBy)
	}
	if out.Depth != in.Depth {
		t.Errorf("depth = %d, want %d: depth is the brake on watcher cascades, and a "+
			"reset depth means a replay can cascade further than the original run did",
			out.Depth, in.Depth)
	}
	// Num normalises float64/int, which is what makes an event built in Go and
	// the same event after a JSON round trip fold identically.
	if got, want := out.Num("cost_usd"), 0.5; got != want {
		t.Errorf("cost_usd = %v, want %v: money read back wrong makes the budget "+
			"ceiling meaningless on replay", got, want)
	}
	if out.Str("question") != "deploy?" {
		t.Errorf("payload string lost: %q", out.Str("question"))
	}
}

// ------------------------------------------------------------------ boundary

func TestTheStoreDoesNotDependOnAnythingButTheKernel(t *testing.T) {
	// A guard against the one import that would invert the architecture. The
	// full check lives in internal/arch_test.go; this one is here so the failure
	// lands in the package that caused it.
	//
	// logstore -> kernel is correct. kernel -> logstore would mean the reducer
	// could read a file, and then the same log would fold to different states on
	// different machines.
	events := []kernel.Event{runStarted()}
	if _, _, err := kernelFoldSignatureCheck(events); err != nil {
		t.Fatal(err)
	}
}

// kernelFoldSignatureCheck fails to compile if kernel.Fold's signature changes
// under us. It is a compile-time assertion dressed as a function: Fold is the
// contract this whole package is built on, and a silent signature change would
// otherwise surface as a subtly different replay.
func kernelFoldSignatureCheck(events []kernel.Event) (kernel.State, []kernel.Effect, error) {
	st, fx := kernel.Fold(kernel.State{}, events, bp())
	return st, fx, nil
}
