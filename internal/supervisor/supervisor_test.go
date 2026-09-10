package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/runconfig"
)

type blockingExecutor struct {
	started chan struct{}
	release chan struct{}
	calls   int
	mu      sync.Mutex
}

func (b *blockingExecutor) SpawnTurn(context.Context, kernel.SpawnTurn) ([]kernel.Event, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-b.release
	return []kernel.Event{{Type: kernel.AgentTurnDone, Source: kernel.SourceRuntime}}, nil
}
func (b *blockingExecutor) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	return nil, nil
}
func (b *blockingExecutor) AskHuman(context.Context, kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}

type quietExecutor struct{}

func (quietExecutor) SpawnTurn(context.Context, kernel.SpawnTurn) ([]kernel.Event, error) {
	return nil, nil
}
func (quietExecutor) CallTool(context.Context, kernel.CallTool) ([]kernel.Event, error) {
	return nil, nil
}
func (quietExecutor) AskHuman(context.Context, kernel.AskHuman) ([]kernel.Event, error) {
	return nil, nil
}

type claimStub struct {
	mu            sync.Mutex
	checkpoints   [][2]int64
	heartbeats    int
	finishes      int
	checkpointErr error
}

func (c *claimStub) Checkpoint(cursor, revision int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkpoints = append(c.checkpoints, [2]int64{cursor, revision})
	return c.checkpointErr
}
func (c *claimStub) Heartbeat() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeats++
	return nil
}
func (c *claimStub) Finish(exec.Outcome, error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishes++
	return nil
}

func TestClaimCheckpointsConfirmedFrontierAndStopsAfterFenceLoss(t *testing.T) {
	_, root := seedRun(t, "r1", "live", []kernel.Event{{ID: "step", Type: kernel.ExecStepCompleted,
		Source: kernel.SourceRuntime, Payload: map[string]any{"source_seq": int64(1)}}, {ID: "cancel", Type: kernel.RunCancelled, Source: kernel.SourceHuman}})
	claim := &claimStub{checkpointErr: errors.New("stale fence")}
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }, Claim: claim})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	_, result, err := h.WaitCompletion(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "stale fence") {
		t.Fatalf("worker result error = %v, want checkpoint fence loss", result.Err)
	}
	claim.mu.Lock()
	defer claim.mu.Unlock()
	if len(claim.checkpoints) != 1 || claim.checkpoints[0][0] != 3 || claim.checkpoints[0][1] < 4 {
		t.Fatalf("checkpoints = %#v: the coordinator must receive the confirmed completed frontier and run revision", claim.checkpoints)
	}
	if claim.finishes != 1 {
		t.Fatalf("terminal finish calls = %d, want one fenced terminal attempt", claim.finishes)
	}
}

func TestShutdownStopsHeartbeatWithoutFinishingIdleJob(t *testing.T) {
	_, root := seedRun(t, "r1", "live", nil)
	claim := &claimStub{}
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }, Claim: claim, Heartbeat: time.Millisecond})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		claim.mu.Lock()
		beats := claim.heartbeats
		claim.mu.Unlock()
		if beats > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat never renewed the active claim")
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	claim.mu.Lock()
	beats, finishes := claim.heartbeats, claim.finishes
	claim.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	claim.mu.Lock()
	defer claim.mu.Unlock()
	if claim.heartbeats != beats {
		t.Fatalf("heartbeats advanced from %d to %d after shutdown: a stopped process must relinquish by expiry", beats, claim.heartbeats)
	}
	if finishes != 0 || claim.finishes != 0 {
		t.Fatalf("shutdown recorded %d terminal outcomes: stopping heartbeat is not terminal evidence", claim.finishes)
	}
}

func TestWaitCompletionRetainsFastPassForMultipleObservers(t *testing.T) {
	_, root := seedRun(t, "r1", "live", nil)
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())

	deadline := time.Now().Add(time.Second)
	for {
		if generation, _, ok := h.Completion(); ok {
			for observer := 0; observer < 2; observer++ {
				gotGeneration, result, err := h.WaitCompletion(context.Background(), 0)
				if err != nil || gotGeneration != generation || result.Outcome.StoppedBy != exec.StopIdle {
					t.Fatalf("observer %d = (%d, %+v, %v), want retained generation %d", observer, gotGeneration, result, err, generation)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not publish a completion")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOpenAtKeepsLocationsDistinct(t *testing.T) {
	dir1, _ := seedRun(t, "r1", "live", nil)
	dir2, _ := seedRun(t, "r1", "live", nil)
	s := New("unused", Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }})
	h1, err := s.OpenAt(context.Background(), "r1", dir1)
	if err != nil {
		t.Fatal(err)
	}
	defer h1.Close(context.Background())
	h2, err := s.OpenAt(context.Background(), "r1", dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close(context.Background())
	if h1.w == h2.w {
		t.Fatal("different run locations shared one worker")
	}
}

func TestResidentReturnsOnlyExistingWorker(t *testing.T) {
	_, root := seedRun(t, "r1", "live", nil)
	built := 0
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) {
		built++
		return quietExecutor{}, nil
	}})
	if h, ok := s.Resident("r1"); ok || h != nil {
		t.Fatalf("dormant resident = (%v, %v), want (nil, false)", h, ok)
	}
	if built != 0 {
		t.Fatalf("dormant lookup built executor %d times", built)
	}
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Resident("r1")
	if !ok || got == nil || got.w != h.w {
		t.Fatalf("active resident = (%v, %v), want existing worker", got, ok)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Resident("r1"); ok || got != nil {
		t.Fatalf("closed resident = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestResidentCommandDoesNotInterruptExternalCall(t *testing.T) {
	dir, root := seedRun(t, "r1", "live", nil)
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]kernel.Event{{ID: "prompt", Type: kernel.RunPrompt, Source: kernel.SourceHuman,
		Payload: map[string]any{"text": "work"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	blocking := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return blocking, nil }})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	<-blocking.started
	resident, ok := s.Resident("r1")
	if !ok {
		t.Fatal("active worker not found")
	}
	commanded := make(chan error, 1)
	go func() {
		commanded <- resident.Command(context.Background(), func(*logstore.Store) error { return nil })
	}()
	select {
	case err := <-commanded:
		t.Fatalf("command completed during external call: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-commanded; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerOwnsWriterAndSerializesBoundedCommands(t *testing.T) {
	dir, root := seedRun(t, "r1", "live", nil)
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }, CommandLimit: 1})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	if _, err := logstore.Open(dir); err == nil {
		t.Fatal("resident worker did not retain writer ownership")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- h.Command(context.Background(), func(*logstore.Store) error { close(entered); <-release; return nil })
	}()
	<-entered
	queued := make(chan error, 1)
	go func() { queued <- h.Command(context.Background(), func(*logstore.Store) error { return nil }) }()
	time.Sleep(10 * time.Millisecond)
	if err := h.Command(context.Background(), func(*logstore.Store) error { return nil }); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third command error = %v, want queue full", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForDispatchedCallThenReopenRestoresProgress(t *testing.T) {
	dir, root := seedRun(t, "r1", "live", nil)
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append([]kernel.Event{{ID: "prompt", Type: kernel.RunPrompt, Source: kernel.SourceHuman,
		Payload: map[string]any{"text": "work"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	blocking := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return blocking, nil }})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	closed := make(chan error, 1)
	go func() { closed <- h.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("close returned during dispatched call: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	s2 := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }})
	h2, err := s2.Open(context.Background(), "r1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close(context.Background())
	blocking.mu.Lock()
	calls := blocking.calls
	blocking.mu.Unlock()
	if calls != 1 {
		t.Fatalf("dispatched calls = %d, want one", calls)
	}
}

func TestRestoreRefusesModifiedImmutableArtifactsBeforeBuild(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) error
		want   string
	}{
		{"effective config", func(dir string) error {
			path := filepath.Join(dir, runconfig.FileName)
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(path, append(body, ' '), 0o600)
		}, "effective config digest"},
		{"blueprint snapshot", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), []byte("changed\n"), 0o644)
		}, "frozen blueprint digest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, root := seedRun(t, "r1", "live", nil)
			if err := tc.mutate(dir); err != nil {
				t.Fatal(err)
			}
			built := false
			s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) {
				built = true
				return quietExecutor{}, nil
			}})
			_, err := s.Open(context.Background(), "r1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("open error = %v, want %q", err, tc.want)
			}
			if built {
				t.Fatal("executor built before immutable artifacts were verified")
			}
		})
	}
}

func TestCloseRetryWaitsAfterEarlierTimeout(t *testing.T) {
	dir, root := seedRun(t, "r1", "live", nil)
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{{ID: "prompt", Type: kernel.RunPrompt, Source: kernel.SourceHuman,
		Payload: map[string]any{"text": "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	blocking := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return blocking, nil }})
	if _, err := s.Open(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want deadline exceeded", err)
	}
	retried := make(chan error, 1)
	go func() { retried <- s.Close(context.Background()) }()
	select {
	case err := <-retried:
		t.Fatalf("retry returned while worker remained active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-retried; err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRefusesUnknownWorkBeforeBuildingExecutor(t *testing.T) {
	dir, root := seedRun(t, "r1", "live", nil)
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		{Type: kernel.ExecWorkPrepared, Source: kernel.SourceRuntime, Payload: map[string]any{"work_id": "w1", "source_seq": int64(1)}},
		{Type: kernel.ExecWorkFinished, Source: kernel.SourceRuntime, Payload: map[string]any{"work_id": "w1", "status": "unknown"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	built := false
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { built = true; return quietExecutor{}, nil }})
	_, err = s.Open(context.Background(), "r1")
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("open error = %v, want unknown work", err)
	}
	if built {
		t.Fatal("executor was built despite unknown durable work")
	}
}

func TestDecisionCommandWakesIdleWorker(t *testing.T) {
	_, root := seedRun(t, "r1", "live", nil)
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }})
	h, err := s.Open(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	select {
	case <-h.Completions():
	case <-time.After(time.Second):
		t.Fatal("worker did not become idle")
	}
	err = h.Command(context.Background(), func(store *logstore.Store) error {
		_, err := store.Append([]kernel.Event{{ID: "cancel", Type: kernel.RunCancelled, Source: kernel.SourceHuman}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-h.Completions():
		if got.Err != nil || got.Outcome.State.Status != kernel.StatusCancelled {
			t.Fatalf("completion = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("decision did not wake worker")
	}
}

func TestLaunchAcceptsFreshRun(t *testing.T) {
	_, root := seedFreshRun(t, "r1", "sim")
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }})
	if err := s.Launch(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
}

func TestOpenRefusesLegacyRun(t *testing.T) {
	_, root := seedFreshRun(t, "r1", "sim")
	s := New(root, Options{Build: func(string, runconfig.Artifact) (exec.Executor, error) { return quietExecutor{}, nil }})
	if _, err := s.Open(context.Background(), "r1"); !errors.Is(err, ErrLegacy) {
		t.Fatalf("open error = %v, want legacy refusal", err)
	}
}

func seedFreshRun(t *testing.T, id, mode string) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := root + "/" + id
	cfg := kernel.Config{Blueprint: "test", Members: []kernel.MemberConfig{{Name: "worker"}}}.ResolveDefaults()
	snapshot := []byte("name: test\nmembers:\n  - name: worker\n")
	sum := sha256.Sum256(snapshot)
	artifact := runconfig.New(id, mode, hex.EncodeToString(sum[:]), "prompt", "", cfg, nil, nil)
	if _, err := runconfig.Publish(dir, artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), snapshot, 0o644); err != nil {
		t.Fatal(err)
	}
	_, digest, err := runconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{{ID: "start", Type: kernel.RunStarted, Source: kernel.SourceHuman,
		Payload: map[string]any{"run_id": id, "actor": "test", "blueprint_sha": artifact.BlueprintSHA,
			"effective_config_schema": runconfig.Schema, "effective_config_path": runconfig.FileName,
			"effective_config_sha": digest, "budget_usd": 1.0}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, root
}

func seedRun(t *testing.T, id, mode string, extra []kernel.Event) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := root + "/" + id
	cfg := kernel.Config{Blueprint: "test", Members: []kernel.MemberConfig{{Name: "worker"}}}.ResolveDefaults()
	snapshot := []byte("name: test\nmembers:\n  - name: worker\n")
	sum := sha256.Sum256(snapshot)
	artifact := runconfig.New(id, mode, hex.EncodeToString(sum[:]), "prompt", "", cfg, nil, nil)
	if mode == "live" {
		artifact.Routes = []runconfig.Route{{Ref: "m", Provider: "p", Protocol: model.ProtocolOpenAIChatCompletions, Model: "m", BaseURL: "https://example.test"}}
	}
	if _, err := runconfig.Publish(dir, artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), snapshot, 0o644); err != nil {
		t.Fatal(err)
	}
	_, digest, err := runconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	events := []kernel.Event{{ID: "start", Type: kernel.RunStarted, Source: kernel.SourceHuman,
		Payload: map[string]any{"run_id": id, "actor": "test", "blueprint_sha": artifact.BlueprintSHA,
			"effective_config_schema": runconfig.Schema, "effective_config_path": runconfig.FileName,
			"effective_config_sha": digest, "budget_usd": 1.0}}}
	if len(extra) == 0 {
		events = append(events, kernel.Event{ID: "ev-step", Type: kernel.ExecStepCompleted,
			Source: kernel.SourceRuntime, Payload: map[string]any{"source_seq": int64(1)}})
	} else {
		events = append(events, extra...)
	}
	if _, err := store.Append(events); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, root
}
