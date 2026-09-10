package v1

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
)

type storageWorker struct {
	id           JobID
	record       JobRecord
	writer       JobWriter
	provider     TextProvider
	now          func() time.Time
	coordination *workerCoordination
	heartbeat    time.Duration

	mu        sync.Mutex
	events    []kernel.Event
	changed   chan struct{}
	done      chan struct{}
	finished  bool
	commands  chan storageCommand
	wake      chan struct{}
	stop      chan struct{}
	closeOnce sync.Once
	err       error
}

type storageCommand struct {
	ctx       context.Context
	makeEvent func([]kernel.Event) (kernel.Event, error)
	reply     chan error
}

func newStorageWorker(id JobID, record JobRecord, writer JobWriter, provider TextProvider, now func() time.Time, start kernel.Event) *storageWorker {
	start.Seq = 1
	return &storageWorker{id: id, record: record, writer: writer, provider: provider, now: now,
		events: []kernel.Event{start}, changed: make(chan struct{}), done: make(chan struct{}),
		commands: make(chan storageCommand, 32), wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

func newRecoveredStorageWorker(id JobID, record JobRecord, writer JobWriter, provider TextProvider,
	now func() time.Time, events []kernel.Event, coordination *workerCoordination, heartbeat time.Duration) (*storageWorker, error) {
	recovery, err := exec.Recover(events)
	if err != nil {
		return nil, err
	}
	if !recovery.HasProgress {
		return nil, errors.New("job has no durable execution progress")
	}
	return &storageWorker{id: id, record: record, writer: writer, provider: provider, now: now,
		coordination: coordination, heartbeat: heartbeat, events: append([]kernel.Event(nil), events...),
		changed: make(chan struct{}), done: make(chan struct{}), commands: make(chan storageCommand, 32),
		wake: make(chan struct{}, 1), stop: make(chan struct{})}, nil
}

func (w *storageWorker) start() { go w.run() }

func (w *storageWorker) run() {
	defer close(w.done)
	defer func() { w.setErr(w.writer.Close()) }()
	metadata, err := decodeStoredMetadata(w.record)
	if err != nil {
		w.setErr(err)
		return
	}
	log := &workerLog{worker: w, revision: w.record.Revision, config: metadata.Effective.Config}
	var clock exec.Clock
	var timekeeper exec.Timekeeper
	if metadata.Simulated {
		virtual := exec.NewVirtualClock()
		clock, timekeeper = virtual, exec.VirtualTime{C: virtual}
	} else {
		real := exec.NewRealClock()
		if w.now != nil {
			real.Now = w.now
		}
		clock, timekeeper = real, exec.RealTime{C: real}
	}
	runner := &exec.Runner{Log: log, Clock: clock,
		Executor: &textExecutor{provider: w.provider, effective: metadata.Effective},
		Config:   metadata.Effective.Config, RunID: string(w.id), Now: func() string {
			if w.now != nil {
				return w.now().UTC().Format(time.RFC3339Nano)
			}
			return time.Now().UTC().Format(time.RFC3339Nano)
		}}
	loop := &exec.Loop{Runner: runner, Log: log, Time: timekeeper, Config: metadata.Effective.Config}
	if recovery, recoverErr := exec.Recover(w.events); recoverErr != nil {
		w.setErr(recoverErr)
		return
	} else if recovery.HasProgress {
		loop.Cursor = recovery.Cursor
	}
	var heartbeatStop chan struct{}
	var heartbeatDone chan struct{}
	if w.coordination != nil {
		loop.Progress = w.checkpoint
		heartbeatStop, heartbeatDone = make(chan struct{}), make(chan struct{})
		go w.renew(heartbeatStop, heartbeatDone)
		defer func() { close(heartbeatStop); <-heartbeatDone }()
	}
	for {
		if w.stopping() {
			return
		}
		w.drainCommands()
		if w.stopping() {
			return
		}
		boundary, passDone := make(chan struct{}), make(chan struct{})
		var once sync.Once
		go func() {
			select {
			case <-w.stop:
				once.Do(func() { close(boundary) })
			case <-w.wake:
				once.Do(func() { close(boundary) })
			case <-passDone:
			}
		}()
		out, runErr := loop.RunUntilBoundary(context.Background(), boundary)
		close(passDone)
		once.Do(func() { close(boundary) })
		loop.Cursor = out.Cursor
		if runErr != nil {
			w.setErr(runErr)
		}
		if w.coordination != nil && (runErr != nil || out.StoppedBy == exec.StopTerminal) {
			if err := w.coordination.complete(out, runErr); err != nil {
				w.setErr(err)
			}
		}
		w.drainCommands()
		if w.stopping() {
			return
		}
		if runErr != nil || out.StoppedBy == exec.StopTerminal {
			return
		}
		if out.StoppedBy == exec.StopIdle {
			select {
			case <-w.stop:
				return
			case <-w.wake:
			}
		}
	}
}

func (w *storageWorker) command(ctx context.Context, makeEvent func([]kernel.Event) (kernel.Event, error)) error {
	command := storageCommand{ctx: ctx, makeEvent: makeEvent, reply: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return w.completionError()
	case w.commands <- command:
	}
	w.signalWake()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return w.completionError()
	case err := <-command.reply:
		return err
	}
}

func (w *storageWorker) drainCommands() {
	for {
		select {
		case command := <-w.commands:
			command.reply <- w.applyCommand(command)
		default:
			return
		}
	}
}

func (w *storageWorker) applyCommand(command storageCommand) error {
	if err := command.ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	event, err := command.makeEvent(append([]kernel.Event(nil), w.events...))
	if err != nil {
		return err
	}
	body, err := encodeStoredEvent(event)
	if err != nil {
		return err
	}
	result, err := w.writer.Append(command.ctx, AppendBatch{Expected: w.record.Revision, Records: []StoredRecord{{Data: body}}})
	if err != nil {
		return err
	}
	w.record.Revision = result.Revision
	for _, record := range result.Records {
		decoded, decodeErr := decodeStoredEvent(record)
		if decodeErr != nil {
			return decodeErr
		}
		w.events = append(w.events, decoded)
	}
	w.signalChanged()
	return nil
}

func (w *storageWorker) checkpoint(cursor, revision int64) error {
	return w.coordination.port.Checkpoint(context.Background(), ExecutionCheckpoint{
		Claim: w.coordination.claim, RunRevision: revision, CompletedCursor: cursor,
	})
}

func (w *storageWorker) renew(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(w.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := w.coordination.port.Heartbeat(context.Background(), w.coordination.claim); err != nil {
				w.setErr(err)
				w.closeOnce.Do(func() { close(w.stop); w.signalWake() })
				return
			}
		}
	}
}

type workerCoordination struct {
	port  Coordination
	claim ExecutionClaim
}

func (c *workerCoordination) complete(out exec.Outcome, runErr error) error {
	outcome := ExecutionFailed
	if runErr != nil {
		outcome = ExecutionUnknown
	} else {
		switch out.State.Status {
		case kernel.StatusSucceeded:
			outcome = ExecutionSucceeded
		case kernel.StatusCancelled:
			outcome = ExecutionCancelled
		case kernel.StatusFailed, kernel.StatusExpired:
		default:
			return nil
		}
	}
	return c.port.Complete(context.Background(), c.claim, outcome)
}

func (w *storageWorker) signalWake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *storageWorker) signalChanged() { close(w.changed); w.changed = make(chan struct{}) }
func (w *storageWorker) wait(ctx context.Context) error {
	w.mu.Lock()
	changed, finished, err := w.changed, w.finished, w.err
	w.mu.Unlock()
	if finished {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-w.done:
		return w.completionError()
	}
}
func (w *storageWorker) completionError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	return errAlreadyTerminal
}

func (w *storageWorker) setErr(err error) {
	w.mu.Lock()
	if err != nil {
		w.err = err
	}
	w.finished = true
	w.signalChanged()
	w.mu.Unlock()
}
func (w *storageWorker) stopping() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}
func (w *storageWorker) close() error {
	w.closeOnce.Do(func() { close(w.stop); w.signalWake() })
	<-w.done
	return w.err
}

type workerLog struct {
	worker   *storageWorker
	revision Revision
	config   kernel.Config
}

func (l *workerLog) Append(events []kernel.Event) ([]kernel.Event, error) {
	l.worker.mu.Lock()
	defer l.worker.mu.Unlock()
	records := make([]StoredRecord, len(events))
	for i, event := range events {
		body, err := encodeStoredEvent(event)
		if err != nil {
			return nil, err
		}
		records[i] = StoredRecord{Data: body}
	}
	result, err := l.worker.writer.Append(context.Background(), AppendBatch{Expected: l.revision, Records: records})
	if err != nil {
		return nil, err
	}
	l.revision, l.worker.record.Revision = result.Revision, result.Revision
	written := make([]kernel.Event, len(result.Records))
	for i, record := range result.Records {
		written[i], err = decodeStoredEvent(record)
		if err != nil {
			return nil, err
		}
	}
	l.worker.events = append(l.worker.events, written...)
	l.worker.signalChanged()
	return written, nil
}

func (l *workerLog) Read(fromSeq, toSeq int64) ([]kernel.Event, error) {
	l.worker.mu.Lock()
	defer l.worker.mu.Unlock()
	var out []kernel.Event
	for _, event := range l.worker.events {
		if event.Seq >= fromSeq && (toSeq == 0 || event.Seq <= toSeq) {
			out = append(out, event)
		}
	}
	return out, nil
}
func (l *workerLog) Head() int64 {
	l.worker.mu.Lock()
	defer l.worker.mu.Unlock()
	if len(l.worker.events) == 0 {
		return 0
	}
	return l.worker.events[len(l.worker.events)-1].Seq
}
func (l *workerLog) Fold(config kernel.Config, untilSeq int64) (kernel.State, error) {
	events, err := l.Read(1, untilSeq)
	if err != nil {
		return kernel.State{}, err
	}
	state, _ := kernel.Fold(kernel.State{}, events, config)
	return state, nil
}
func (l *workerLog) WriteSnapshot(state kernel.State, atSeq int64) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return l.worker.writer.WriteSnapshot(context.Background(), Snapshot{AtSequence: atSeq, Data: body})
}

var _ exec.LoopLog = (*workerLog)(nil)
