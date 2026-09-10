package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
)

func TestInspectAndListProjectConfirmedRuns(t *testing.T) {
	root := t.TempDir()
	writeEvents(t, filepath.Join(root, "b"), []kernel.Event{
		event(1, kernel.RunStarted, "runtime", "", map[string]any{"run_id": "b", "actor": "human", "simulated": true}),
		event(2, kernel.InboxCreated, "runtime", "backend", map[string]any{
			"inbox_id": "inbox-1", "kind": "tool_approval", "question": "allow?", "agent": "backend",
		}),
	})
	writeEvents(t, filepath.Join(root, "a"), []kernel.Event{
		event(1, kernel.RunStarted, "runtime", "", map[string]any{"run_id": "a"}),
		event(2, kernel.RunResult, "runtime", "", map[string]any{"result": "done"}),
	})

	service := NewReadService(root)
	job, err := service.Inspect(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "b" || job.Sequence != 2 || !job.Simulated || len(job.Pending) != 1 {
		t.Fatalf("projection = %#v", job)
	}
	listing, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Jobs) != 2 || listing.Jobs[0].ID != "b" || listing.Jobs[1].ID != "a" {
		t.Fatalf("attention-ordered listing = %#v", listing.Jobs)
	}
}

func TestSubscriptionFiltersAndAdvancesAcrossNonmatches(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	writeEvents(t, dir, []kernel.Event{
		event(1, kernel.RunStarted, "runtime", "", map[string]any{"run_id": "r1"}),
		event(2, kernel.AgentActivated, "agent", "alpha", nil),
		event(3, kernel.StageEntered, "runtime", "", nil),
		event(4, kernel.AgentTurnDone, "agent", "beta", nil),
	})
	service := NewReadService(root)
	sub, err := service.Subscribe(context.Background(), "r1", 1, EventFilter{
		TypePrefixes: []string{"agent."}, Sources: []string{"agent", "human"}, Actors: []string{"alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := sub.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if batch.AfterSequence != 4 || len(batch.Events) != 1 || batch.Events[0].Sequence != 2 {
		t.Fatalf("batch = %#v", batch)
	}
}

func TestSubscriptionWithholdsPendingBatchAndCanBeClosedWhileWaiting(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	writeEvents(t, dir, []kernel.Event{event(1, kernel.RunStarted, "runtime", "", map[string]any{"run_id": "r1"})})
	confirmedSize := fileSize(t, filepath.Join(dir, "events.ndjson"))
	appendEvents(t, dir, []kernel.Event{event(2, kernel.RunPaused, "human", "", nil)})
	marker := []byte(`{"version":1,"commit_id":"test","pre_append_size":` + strconv.FormatInt(confirmedSize, 10) + `}`)
	if err := os.WriteFile(filepath.Join(dir, "pending.commit"), marker, 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewReadService(root)
	sub, err := service.Subscribe(context.Background(), "r1", 1, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending Next error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := sub.Next(context.Background())
		done <- err
	}()
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("closed Next error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Next")
	}
}

func TestSubscriptionRejectsConfirmedSequenceGap(t *testing.T) {
	root := t.TempDir()
	writeEvents(t, filepath.Join(root, "r1"), []kernel.Event{
		event(1, kernel.RunStarted, "runtime", "", map[string]any{"run_id": "r1"}),
		event(3, kernel.RunPaused, "human", "", nil),
	})
	service := NewReadService(root)
	sub, err := service.Subscribe(context.Background(), "r1", 0, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Next(context.Background()); err == nil {
		t.Fatal("sequence gap was accepted")
	}
}

func TestSubscriptionBacklogOverflowPreservesLastDeliveredCursor(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	writeEvents(t, dir, []kernel.Event{
		event(1, kernel.RunStarted, "runtime", "", map[string]any{"run_id": "r1"}),
	})
	service := NewReadService(root)
	service.backlogBytes = fileSize(t, filepath.Join(dir, "events.ndjson"))
	sub, err := service.Subscribe(context.Background(), "r1", 0, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := sub.Next(context.Background())
	if err != nil || batch.AfterSequence != 1 {
		t.Fatalf("first batch = %#v / %v", batch, err)
	}
	appendEvents(t, dir, []kernel.Event{
		event(2, kernel.RunPaused, "human", "", nil),
		event(3, kernel.RunUnpaused, "human", "", nil),
	})
	_, err = sub.Next(context.Background())
	var slow *SlowConsumerError
	if !errors.As(err, &slow) || !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("overflow error = %v, want slow consumer", err)
	}
	if slow.AfterSequence != 1 {
		t.Fatalf("resume cursor = %d, want 1", slow.AfterSequence)
	}
	if _, err := sub.Next(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next after overflow = %v, want closed", err)
	}
}

func event(seq int64, typ kernel.EventType, source, actor string, payload map[string]any) kernel.Event {
	return kernel.Event{Seq: seq, ID: "e" + string(rune('0'+seq)), Type: typ, Source: kernel.Source(source), Actor: actor, Payload: payload}
}

func writeEvents(t *testing.T, dir string, events []kernel.Event) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := marshalEvents(t, events)
	if err := os.WriteFile(filepath.Join(dir, "events.ndjson"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendEvents(t *testing.T, dir string, events []kernel.Event) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dir, "events.ndjson"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(marshalEvents(t, events)); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func marshalEvents(t *testing.T, events []kernel.Event) []byte {
	t.Helper()
	var body []byte
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	return body
}
