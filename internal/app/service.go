package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runread"
)

const (
	defaultPollInterval = 25 * time.Millisecond
	defaultBatchSize    = 256
	// A subscription may decode at most this much newly confirmed history in one
	// pull. It either delivers that bounded prefix or fails with a resumable cursor.
	defaultBacklogBytes = int64(4 << 20)
)

// Service reads runs rooted below one filesystem location. Filesystem details
// stay inside the application boundary; callers address jobs only by id.
type Service struct {
	root         string
	pollInterval time.Duration
	batchSize    int
	backlogBytes int64
}

// NewReadService constructs the filesystem-backed Phase 1 read application.
func NewReadService(root string) *Service {
	return &Service{
		root: root, pollInterval: defaultPollInterval,
		batchSize: defaultBatchSize, backlogBytes: defaultBacklogBytes,
	}
}

// Inspect folds one confirmed prefix and validates durable execution progress.
func (s *Service) Inspect(ctx context.Context, id string) (Projection, error) {
	if err := ctx.Err(); err != nil {
		return Projection{}, err
	}
	dir, err := s.resolve(id)
	if err != nil {
		return Projection{}, err
	}
	return inspectDir(dir)
}

// List folds every discovered run. One damaged run does not hide healthy jobs.
func (s *Service) List(ctx context.Context) (Listing, error) {
	if err := ctx.Err(); err != nil {
		return Listing{}, err
	}
	dirs, err := s.discover()
	if err != nil {
		return Listing{}, err
	}
	out := Listing{Jobs: make([]Projection, 0, len(dirs))}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return Listing{}, err
		}
		job, err := inspectDir(dir)
		if err != nil {
			out.Unreadable = append(out.Unreadable, fmt.Errorf("%s: %w", filepath.Base(dir), err))
			continue
		}
		out.Jobs = append(out.Jobs, job)
	}
	sort.SliceStable(out.Jobs, func(i, j int) bool {
		li, lj := attentionRank(out.Jobs[i].Status), attentionRank(out.Jobs[j].Status)
		if li != lj {
			return li < lj
		}
		return out.Jobs[i].ID < out.Jobs[j].ID
	})
	return out, nil
}

// Subscribe creates a bounded pull subscription after an exclusive sequence.
func (s *Service) Subscribe(ctx context.Context, id string, after int64, filter EventFilter) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if after < 0 {
		return nil, fmt.Errorf("%w: after sequence must not be negative", ErrInvalidArgument)
	}
	dir, err := s.resolve(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(logstore.EventsPath(dir)); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("inspect job %s: %w", id, err)
	}
	return &subscription{
		dir: dir, after: after, filter: cloneFilter(filter),
		pollInterval: s.pollInterval, batchSize: s.batchSize,
		backlogBytes: s.backlogBytes, closed: make(chan struct{}),
	}, nil
}

func inspectDir(dir string) (Projection, error) {
	run, err := runread.Open(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Projection{}, fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(dir))
		}
		return Projection{}, err
	}
	recovery, err := exec.Recover(run.Events)
	if err != nil {
		return Projection{}, fmt.Errorf("invalid durable execution progress: %w", err)
	}
	return projectJob(run.State, run.Simulated, recovery.Unknown, filepath.Base(dir)), nil
}

func projectJob(state kernel.State, simulated bool, unknown []string, fallback string) Projection {
	id := state.RunID
	if id == "" {
		id = fallback
	}
	out := Projection{
		ID: id, Actor: state.Actor, Status: string(state.Status), Terminal: state.Status.Terminal(),
		Sequence: state.Seq, Stage: state.Stage, StageIndex: state.StageIndex,
		Turns: state.Turns, MaxTurns: state.MaxTurns, SpentUSD: state.SpentUSD,
		TreeSpentUSD: state.TreeSpentUSD, BudgetUSD: state.BudgetUSD,
		Simulated: simulated, UnknownWork: append([]string(nil), unknown...), Result: state.Result,
	}
	for _, member := range state.Members {
		out.Members = append(out.Members, Member{
			Name: member.Name, Role: member.Role, State: string(member.State), Detail: member.Detail,
			Submitted: member.Submitted, Busy: member.Busy(), Runnable: member.Runnable(),
			SpentUSD: member.SpentUSD, Turns: member.Turns,
		})
	}
	for _, item := range state.Inbox {
		if item.Replied {
			continue
		}
		out.Pending = append(out.Pending, PendingDecision{
			ID: item.ID, Kind: decisionKind(item.Kind), Question: item.Question, Actor: item.Agent,
		})
	}
	return out
}

func decisionKind(kind string) string {
	if kind == "tool_approval" {
		return "approval"
	}
	return "question"
}

func (s *Service) resolve(id string) (string, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return "", fmt.Errorf("%w: no run root configured", ErrInvalidArgument)
	}
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return "", fmt.Errorf("%w: invalid job id", ErrInvalidArgument)
	}
	dir := filepath.Join(s.root, id)
	if _, err := os.Stat(logstore.EventsPath(dir)); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return "", fmt.Errorf("inspect job %s: %w", id, err)
	}
	return dir, nil
}

func (s *Service) discover() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		if _, err := os.Stat(logstore.EventsPath(dir)); err == nil {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out, nil
}

func attentionRank(status string) int {
	switch status {
	case string(kernel.StatusBlocked):
		return 0
	case string(kernel.StatusRunning):
		return 1
	case string(kernel.StatusPaused):
		return 2
	case string(kernel.StatusQueued):
		return 3
	default:
		return 4
	}
}

type subscription struct {
	dir          string
	after        int64
	byteCursor   int64
	readSequence int64
	filter       EventFilter
	pollInterval time.Duration
	batchSize    int
	backlogBytes int64

	nextMu    sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}
	queue     []kernel.Event
}

func (s *subscription) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *subscription) Next(ctx context.Context) (EventBatch, error) {
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	for {
		select {
		case <-s.closed:
			return EventBatch{}, ErrClosed
		default:
		}
		if err := ctx.Err(); err != nil {
			return EventBatch{}, err
		}
		batch, advanced, err := s.scan()
		if err != nil {
			return EventBatch{}, err
		}
		if advanced {
			return batch, nil
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return EventBatch{}, ctx.Err()
		case <-s.closed:
			if !timer.Stop() {
				<-timer.C
			}
			return EventBatch{}, ErrClosed
		case <-timer.C:
		}
	}
}

func (s *subscription) scan() (EventBatch, bool, error) {
	if len(s.queue) == 0 {
		read, overflow, err := runread.ReadConfirmedLimit(s.dir, s.byteCursor, s.backlogBytes)
		if err != nil {
			return EventBatch{}, false, err
		}
		if overflow {
			_ = s.Close()
			return EventBatch{}, false, &SlowConsumerError{AfterSequence: s.after}
		}
		events, err := runread.Decode(s.dir, read.Bytes)
		if err != nil {
			return EventBatch{}, false, err
		}
		if err := runread.ValidateSequence(s.dir, events, s.readSequence); err != nil {
			return EventBatch{}, false, err
		}
		if len(events) > 0 {
			s.readSequence = events[len(events)-1].Seq
		}
		s.byteCursor = read.NextOffset
		for _, event := range events {
			if event.Seq > s.after {
				s.queue = append(s.queue, event)
			}
		}
	}
	if len(s.queue) == 0 {
		return EventBatch{}, false, nil
	}
	limit := s.batchSize
	if limit <= 0 || limit > len(s.queue) {
		limit = len(s.queue)
	}
	scanned := s.queue[:limit]
	s.queue = s.queue[limit:]
	out := EventBatch{Events: make([]Event, 0, len(scanned)), AfterSequence: s.after}
	for _, event := range scanned {
		if event.Seq <= s.after {
			continue
		}
		if event.Seq <= out.AfterSequence {
			return EventBatch{}, false, fmt.Errorf("confirmed event sequence %d is not ordered after %d", event.Seq, out.AfterSequence)
		}
		out.AfterSequence = event.Seq
		if matches(event, s.filter) {
			projected, err := projectEvent(event)
			if err != nil {
				return EventBatch{}, false, err
			}
			out.Events = append(out.Events, projected)
		}
	}
	if out.AfterSequence == s.after {
		return EventBatch{}, false, nil
	}
	s.after = out.AfterSequence
	return out, true, nil
}
