// Package logstore owns the append-only event log of a run: durability,
// sequence-number assignment, and rebuilding State by folding the log.
//
// It exists because of ADR-0002: the log is the truth and snapshots are cache.
// That decision moves a specific responsibility here and nowhere else — if two
// components could write the log, or if a half-written batch could survive a
// crash, then `State = fold(Decide, State0, events)` would stop being a
// function of the log, and every guarantee built on top of it (replay,
// `run why`, `--sim` agreeing with `run`) would become a coincidence.
//
// The kernel must never import this package. The dependency runs one way:
// logstore knows about kernel.Event and kernel.Decide, and the kernel does not
// know that storage exists. internal/arch_test.go enforces that.
package logstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/michiTrader/iash/internal/kernel"
)

const (
	// eventsFileName is the log itself: NDJSON, one kernel.Event per line.
	eventsFileName = "events.ndjson"

	// lockFileName enforces the single-writer rule of ADR-0002. It is a file
	// and not a mutex because the constraint is between processes: two `iash`
	// invocations on the same run directory are the realistic way duplicate
	// `seq` gets created, and an in-process mutex cannot see them.
	lockFileName = "writer.lock"

	// pendingFileName records that a batch write is in flight. Its presence at
	// Open means the previous process died mid-Append, and the offset it holds
	// is where the log must be rolled back to. See the atomicity comment on
	// Append.
	pendingFileName = "pending.commit"

	// snapshotFileName holds the newest snapshot. Snapshots are a cache and
	// nothing reads this file yet on purpose (ADR-0002): shipping the cache
	// before the thing it caches is trusted is how a stale snapshot becomes a
	// state nobody can explain.
	snapshotFileName = "state.snapshot.json"
)

// Store owns one run's log directory: runs/<run-id>/.
//
// A Store holds the writer lock for as long as it is open, so it is a
// long-lived object owned by whichever process is driving the run, not
// something to create per operation.
type Store struct {
	dir string

	// mu serialises everything. It is not a performance compromise: seq
	// assignment, the write, and the fsync have to be one indivisible step, or
	// two goroutines can both read head==7, both decide their event is 8, and
	// produce the duplicate seq that breaks the CAS of ADR-0006. Serialising
	// the whole operation is the only version of this that is correct, and the
	// cost is irrelevant next to the fsync it already contains.
	mu sync.Mutex

	events *os.File
	lock   *os.File

	// head is the highest seq durably written. size is the log's committed
	// length in bytes, which is what a rolled-back batch is truncated to.
	head int64
	size int64

	closed bool
}

// Open acquires the run directory for writing and validates the existing log.
//
// Validation is not optional bookkeeping. Open is the only moment where damage
// can be reported before it has consequences: once appends resume, a log with a
// gap in it will fold into a state that looks plausible and is wrong, and
// nothing downstream can detect that. So Open reads the whole log, checks that
// `seq` runs 1..N with no gaps, and refuses to proceed if it does not.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logstore: create run directory: %w", err)
	}

	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}

	s := &Store{dir: dir, lock: lock}

	// Roll back before reading. A batch left in flight by a dead process must
	// not be visible to the scan, or the scan would validate bytes that are
	// about to be discarded and could report corruption for a log that is
	// merely mid-rollback.
	if err := s.rollbackPending(); err != nil {
		s.releaseLock()
		return nil, err
	}

	head, size, err := s.recoverAndScan()
	if err != nil {
		s.releaseLock()
		return nil, err
	}
	s.head, s.size = head, size

	f, err := os.OpenFile(s.eventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.releaseLock()
		return nil, fmt.Errorf("logstore: open log for append: %w", err)
	}
	s.events = f

	// fsync the directory so the log file and lock file entries themselves
	// survive a power loss. Without this, on a freshly created run directory
	// the file's *contents* can be durable while the directory entry naming it
	// is not, and the log comes back as if the run never started.
	if err := fsyncDir(dir); err != nil {
		s.events.Close()
		s.releaseLock()
		return nil, err
	}

	return s, nil
}

// Close releases the writer lock. A Store that is not closed keeps the run
// directory locked until the process exits, which is why the lock file records
// the pid: the error message can then name what to investigate.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var firstErr error
	if s.events != nil {
		if err := s.events.Close(); err != nil {
			firstErr = err
		}
	}
	if err := s.releaseLock(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Head returns the highest seq durably written, or 0 for an empty log.
//
// It is the version token of ADR-0006: State = fold(...up to Head()). A caller
// reads it, decides, and passes it back to AppendIfSeq.
func (s *Store) Head() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}

// Dir returns the run directory this Store owns.
func (s *Store) Dir() string { return s.dir }

// Append assigns seq to each event and writes them durably, in order.
//
// Events must arrive with Seq == 0: assigning the sequence number is this
// package's job and only this package's job (ADR-0002). The returned slice is a
// copy with seq filled in, so the caller can record the numbers the log
// actually committed.
func (s *Store) Append(events []kernel.Event) ([]kernel.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(events)
}

// AppendIfSeq is Append with a compare-and-swap precondition: it applies only
// if the log's current head is exactly expectedSeq.
//
// The CAS is on `seq` and never on a turn, per ADR-0006: `seq` identifies a
// version of the state, and a turn spans many events and therefore many states,
// so a CAS on a turn would sometimes pass when it should fail — the worst thing
// a CAS can do.
//
// A rejection returns *CASError, which carries the real head. That is what lets
// the caller re-read the exact range it missed instead of treating a normal
// concurrency outcome as a storage failure.
func (s *Store) AppendIfSeq(expectedSeq int64, events []kernel.Event) ([]kernel.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.head != expectedSeq {
		return nil, &CASError{Expected: expectedSeq, Actual: s.head}
	}
	return s.appendLocked(events)
}

// appendLocked does the work. The caller must hold s.mu.
//
// HOW A BATCH IS ATOMIC. A single write() of the whole buffer is not enough:
// POSIX does not promise that a write to a regular file lands all-or-nothing
// across a crash, so a batch of three events could come back as two whole lines
// plus a torn one. Two of three events surviving is worse than none surviving,
// because the two that survived describe a decision the reducer never
// completed.
//
// So the write is bracketed by an intent marker:
//
//  1. write pendingFileName containing the log's current committed size, fsync
//     it, fsync the directory — now the rollback point is durable;
//  2. write the whole batch, fsync the log;
//  3. remove pendingFileName, fsync the directory — this is the commit point.
//
// Open rolls the log back to the recorded size whenever the marker is present.
// The batch is therefore visible only if step 3 completed, and step 3 happens
// before Append returns. If a crash lands between 2 and 3, a fully written
// batch is rolled back — that is correct, not a loss: Append never returned, so
// no caller ever learned those events existed and nothing downstream depends on
// them.
//
// FSYNC POLICY. Two fsyncs of the log-side data plus two directory fsyncs per
// batch, all before Append returns. The cost is a few disk round trips per
// batch. It is worth paying because of what an event costs to produce here: an
// event usually records an LLM turn that was already billed, so losing an
// acknowledged append does not merely lose data, it makes the log disagree with
// money that was actually spent, and replay then reconstructs a state that
// never happened. Batching is the amortisation: a turn's events are appended
// together, so the fsync cost is per turn, not per event.
func (s *Store) appendLocked(events []kernel.Event) ([]kernel.Event, error) {
	if s.closed {
		return nil, errors.New("logstore: append on a closed store")
	}
	if len(events) == 0 {
		// No marker, no write, no fsync. An empty append is a no-op rather than
		// an error because callers hand over whatever the reducer emitted, and
		// the reducer legitimately emits nothing.
		return nil, nil
	}

	out := make([]kernel.Event, len(events))
	var buf []byte
	next := s.head

	for i, e := range events {
		if e.Seq != 0 {
			return nil, &SeqAssignedError{Index: i, Seq: e.Seq}
		}
		next++
		e.Seq = next
		out[i] = e

		line, err := json.Marshal(e)
		if err != nil {
			// Fail before touching the file. A batch that is half serialisable
			// must not become a batch that is half written.
			return nil, fmt.Errorf("logstore: encode event %d (seq %d): %w", i, next, err)
		}
		// A literal newline inside a serialised event would split one event
		// across two NDJSON lines and desynchronise every later read.
		// encoding/json escapes newlines, so this is a guard against a future
		// change of encoder, not against current behaviour.
		if containsNewline(line) {
			return nil, fmt.Errorf("logstore: encoded event %d (seq %d) contains a newline: "+
				"one event per line is what makes the log readable without a framing layer", i, next)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}

	if err := s.writePending(s.size); err != nil {
		return nil, err
	}

	n, err := s.events.Write(buf)
	if err != nil {
		// Leave the marker in place. Rolling back here would be a second I/O
		// operation on a device that just failed; the marker means the next
		// Open cleans up, which is the path that is already tested.
		return nil, fmt.Errorf("logstore: write batch of %d events at offset %d: %w",
			len(events), s.size, err)
	}
	if n != len(buf) {
		return nil, fmt.Errorf("logstore: short write (%d of %d bytes): "+
			"the batch will be rolled back on the next Open", n, len(buf))
	}
	if err := s.events.Sync(); err != nil {
		return nil, fmt.Errorf("logstore: fsync log after batch: %w", err)
	}

	if err := s.clearPending(); err != nil {
		return nil, err
	}

	s.head = next
	s.size += int64(len(buf))
	return out, nil
}

// Read returns events in [fromSeq, toSeq]. toSeq == 0 means "to the head".
//
// It re-reads the file every time instead of serving from an in-memory copy.
// That is deliberate and it is ADR-0002 applied to this package: a cache of the
// log is one more thing that can disagree with the log, and the whole point of
// the decision is that there is exactly one answer to "what happened". A
// per-run log is small enough that parsing it is not the expensive part of
// anything this system does — the expensive part is the model calls it records.
func (s *Store) Read(fromSeq, toSeq int64) ([]kernel.Event, error) {
	s.mu.Lock()
	limit := s.size
	head := s.head
	s.mu.Unlock()

	if fromSeq < 1 {
		fromSeq = 1
	}
	if toSeq == 0 || toSeq > head {
		toSeq = head
	}
	if fromSeq > toSeq {
		return nil, nil
	}

	var out []kernel.Event
	err := s.eachEvent(limit, func(e kernel.Event, _ int64) error {
		if e.Seq >= fromSeq && e.Seq <= toSeq {
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Fold rebuilds state by replaying the log through kernel.Decide.
// untilSeq == 0 means "the whole log".
//
// This is the function the rest of the system reads state through, and it is
// the reason the log exists at all (ADR-0002). Folding to untilSeq gives
// exactly the state at that point, which is what makes `run replay --until-seq`
// honest rather than approximate.
//
// The effects kernel.Fold returns are discarded, and that is the definition of
// a replay: the effects of these events were already performed (or deliberately
// never will be, when replaying an old run). Executing them here would re-send
// prompts and re-spend money to answer a question about the past.
func (s *Store) Fold(c kernel.Config, untilSeq int64) (kernel.State, error) {
	events, err := s.Read(1, untilSeq)
	if err != nil {
		return kernel.State{}, err
	}
	st, _ := kernel.Fold(kernel.State{}, events, c)
	return st, nil
}

// snapshot is the on-disk shape of a snapshot.
//
// Seq is mandatory and stored with the state because a snapshot without the
// point it corresponds to cannot be validated against the log. An unlabelled
// snapshot can only be trusted, and ADR-0002 exists precisely because trusting
// a snapshot is how you get a state with no events to justify it.
type snapshot struct {
	Seq   int64        `json:"seq"`
	State kernel.State `json:"state"`
}

// WriteSnapshot stores a snapshot of the state at atSeq.
//
// Nothing reads it yet. Snapshots are a read optimisation and only that
// (ADR-0002), so `Fold` deliberately still replays the log from the beginning;
// deleting every snapshot file must cost startup speed and no information, and
// there is a test that deletes the file and asserts the fold is unchanged.
//
// The write is temp-file-plus-rename rather than in-place. An in-place snapshot
// write that is interrupted leaves a file that parses as JSON often enough to be
// dangerous, and the moment snapshot loading is implemented that becomes a
// corrupt state loaded in preference to a perfectly good log.
func (s *Store) WriteSnapshot(st kernel.State, atSeq int64) error {
	if atSeq < 0 {
		return fmt.Errorf("logstore: snapshot at negative seq %d", atSeq)
	}

	body, err := json.MarshalIndent(snapshot{Seq: atSeq, State: st}, "", "  ")
	if err != nil {
		return fmt.Errorf("logstore: encode snapshot at seq %d: %w", atSeq, err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(s.dir, snapshotFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("logstore: create snapshot temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("logstore: write snapshot: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("logstore: fsync snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("logstore: close snapshot temp file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, snapshotFileName)); err != nil {
		return fmt.Errorf("logstore: publish snapshot: %w", err)
	}
	// The rename is a directory operation, so the directory needs the fsync for
	// it to survive a power loss.
	return fsyncDir(s.dir)
}

// SnapshotPath is where WriteSnapshot puts the snapshot. Exported so a test can
// delete it and prove the log stands alone.
func (s *Store) SnapshotPath() string { return filepath.Join(s.dir, snapshotFileName) }

func (s *Store) eventsPath() string  { return filepath.Join(s.dir, eventsFileName) }
func (s *Store) pendingPath() string { return filepath.Join(s.dir, pendingFileName) }
func (s *Store) lockPath() string    { return filepath.Join(s.dir, lockFileName) }

// ------------------------------------------------------------------ recovery

// recoverAndScan reads the whole log, validates it, and returns the head and
// the committed size.
//
// TORN-WRITE RECOVERY: an unterminated final line is discarded (and physically
// truncated); damage anywhere else refuses to open.
//
// Why discarding the tail is right: a line with no terminating newline was, by
// construction, never acknowledged. Append fsyncs and clears the intent marker
// before returning, so if the tail is torn then Append did not return, so no
// caller was ever told that event exists and no decision was taken on it.
// Truncating restores the log to the last state anybody could have observed.
//
// Why refusing to open instead would be worse: a torn tail is what a routine
// SIGKILL, OOM or power loss produces. Making that a fatal condition turns
// every crash into an outage that needs manual repair, and the only repair
// available is hand-editing the log — a far more dangerous operation, performed
// under time pressure, on the one file in the system that has no backup by
// design. The failure mode of refusing is "operators learn to edit the log";
// the failure mode of discarding is "an event nobody saw is gone". The second
// is strictly smaller.
//
// Why the same argument does NOT extend to damage in the middle: a bad line
// before the end cannot be un-acknowledged, and skipping it leaves a gap. Fold
// cannot tell a gap from an event that never existed, so it would return a
// state that is plausible and wrong — exactly what ADR-0002 was written to
// prevent. There is no safe automatic recovery there, so it refuses loudly.
//
// The physical truncation matters as much as the detection: leaving the partial
// bytes in place would make the next Append concatenate onto them and produce
// one merged, permanently unparseable line — turning a recoverable tail into
// the unrecoverable middle-of-log case.
func (s *Store) recoverAndScan() (head int64, size int64, err error) {
	f, err := os.Open(s.eventsPath())
	if errors.Is(err, os.ErrNotExist) {
		// A fresh run. head 0, size 0: the first event will be seq 1.
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("logstore: open log for scan: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var offset int64
	var expected int64

	for {
		// ReadBytes rather than bufio.Scanner: Scanner caps a token at 64 KiB by
		// default, and an event carrying an LLM response goes past that
		// routinely. A size limit here would report a healthy log as corrupt.
		line, readErr := r.ReadBytes('\n')

		if len(line) > 0 && line[len(line)-1] != '\n' {
			// Unterminated final chunk: the torn tail. Truncate to where it
			// started.
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return 0, 0, fmt.Errorf("logstore: read log at offset %d: %w", offset, readErr)
			}
			if terr := truncateAndSync(s.eventsPath(), offset); terr != nil {
				return 0, 0, terr
			}
			return expected, offset, nil
		}

		if len(line) > 0 {
			var e kernel.Event
			if jerr := json.Unmarshal(line, &e); jerr != nil {
				return 0, 0, &CorruptError{
					Dir: s.dir, AtSeq: expected, Offset: offset,
					Reason: fmt.Sprintf("line is not a valid event: %v", jerr),
				}
			}
			// The gap check. This is the cheap test that makes every later
			// guarantee possible: seq must be 1, 2, 3, ... with nothing missing
			// and nothing repeated.
			if e.Seq != expected+1 {
				return 0, 0, &CorruptError{
					Dir: s.dir, AtSeq: expected, Offset: offset,
					Reason: fmt.Sprintf("expected seq %d, found seq %d "+
						"(a gap is indistinguishable from a lost event, and a repeat breaks the CAS)",
						expected+1, e.Seq),
				}
			}
			expected = e.Seq
			offset += int64(len(line))
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return expected, offset, nil
			}
			return 0, 0, fmt.Errorf("logstore: read log at offset %d: %w", offset, readErr)
		}
	}
}

// eachEvent walks the committed prefix of the log, up to limit bytes.
//
// The limit is what keeps a reader from seeing a batch that is still in flight:
// s.size only advances after the commit point, so a concurrent Append's bytes
// are on disk but outside the range any reader will look at.
func (s *Store) eachEvent(limit int64, fn func(kernel.Event, int64) error) error {
	f, err := os.Open(s.eventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logstore: open log for read: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(io.LimitReader(f, limit))
	var offset int64
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var e kernel.Event
			if jerr := json.Unmarshal(line, &e); jerr != nil {
				// Open validated this file, so reaching here means it changed
				// underneath us — which the writer lock is supposed to prevent.
				return &CorruptError{
					Dir: s.dir, AtSeq: 0, Offset: offset,
					Reason: fmt.Sprintf("line became unparseable after Open validated it "+
						"(%v); something wrote the log without holding the lock", jerr),
				}
			}
			if err := fn(e, offset); err != nil {
				return err
			}
			offset += int64(len(line))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("logstore: read log at offset %d: %w", offset, readErr)
		}
	}
}

// rollbackPending undoes a batch that was in flight when the previous writer
// died. See the atomicity comment on appendLocked for why the marker is the
// commit point.
func (s *Store) rollbackPending() error {
	body, err := os.ReadFile(s.pendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logstore: read pending marker: %w", err)
	}

	offset, perr := strconv.ParseInt(trimSpace(string(body)), 10, 64)
	if perr != nil || offset < 0 {
		// The marker itself is damaged, so the rollback point is unknown. This
		// refuses rather than guessing: guessing means either discarding good
		// history or keeping a partial batch, and there is no way to tell which
		// from here.
		return &CorruptError{
			Dir: s.dir, AtSeq: -1, Offset: -1,
			Reason: fmt.Sprintf("pending marker %q is unreadable, so the rollback point "+
				"of the interrupted batch is unknown", string(body)),
		}
	}

	if err := truncateAndSync(s.eventsPath(), offset); err != nil {
		return err
	}
	if err := os.Remove(s.pendingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("logstore: remove pending marker: %w", err)
	}
	return fsyncDir(s.dir)
}

func (s *Store) writePending(offset int64) error {
	f, err := os.OpenFile(s.pendingPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("logstore: create pending marker: %w", err)
	}
	if _, err := f.WriteString(strconv.FormatInt(offset, 10) + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("logstore: write pending marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("logstore: fsync pending marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("logstore: close pending marker: %w", err)
	}
	// The marker's directory entry has to be durable before the batch is
	// written, or a crash could leave the batch on disk with no marker naming
	// its rollback point — which is the one state this scheme cannot repair.
	return fsyncDir(s.dir)
}

func (s *Store) clearPending() error {
	if err := os.Remove(s.pendingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("logstore: clear pending marker: %w", err)
	}
	// The removal is the commit point, so it has to be durable before Append
	// returns. Without this fsync the marker can reappear after a power loss and
	// the next Open would roll back a batch the caller was already told was
	// committed — losing an acknowledged write, which is the one thing this
	// package must never do.
	return fsyncDir(s.dir)
}

// ------------------------------------------------------------------ locking

// acquireLock takes the single-writer lock with O_EXCL.
//
// O_EXCL and not advisory flock: the guarantee has to hold when the two writers
// are on different machines sharing the directory, and flock does not survive
// NFS reliably. The trade-off is that a hard-killed process leaves the lock
// behind, so the lock records its pid and the error tells the operator what to
// check. A stale lock is an annoyance; two live writers are duplicate `seq` and
// an unfoldable log, so the failure is biased in the right direction on purpose.
func acquireLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		owner := "an unknown process"
		if body, rerr := os.ReadFile(path); rerr == nil && len(trimSpace(string(body))) > 0 {
			owner = trimSpace(string(body))
		}
		return nil, &LockedError{Dir: dir, Owner: owner}
	}
	if err != nil {
		return nil, fmt.Errorf("logstore: acquire writer lock: %w", err)
	}
	if _, werr := f.WriteString(fmt.Sprintf("pid %d\n", os.Getpid())); werr == nil {
		_ = f.Sync()
	}
	return f, nil
}

func (s *Store) releaseLock() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	if rerr := os.Remove(s.lockPath()); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = fmt.Errorf("logstore: remove writer lock: %w", rerr)
	}
	return err
}

// ------------------------------------------------------------------ helpers

// fsyncDir makes a directory's own metadata durable. Creating, renaming and
// removing a file are directory operations, and fsyncing the file does not make
// the entry that names it durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("logstore: open directory for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("logstore: fsync directory %s: %w", dir, err)
	}
	return nil
}

func truncateAndSync(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logstore: open log to truncate: %w", err)
	}
	defer f.Close()

	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("logstore: truncate log to %d bytes: %w", size, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("logstore: fsync after truncate: %w", err)
	}
	return nil
}

func containsNewline(b []byte) bool {
	for _, c := range b {
		if c == '\n' || c == '\r' {
			return true
		}
	}
	return false
}

// trimSpace exists so this file does not import strings for one call. It is not
// a general trimmer: it handles the whitespace a marker or lock file can end up
// with.
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
