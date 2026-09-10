package exec

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
)

func timerEvent(seq int64, typ kernel.EventType, payload map[string]any) kernel.Event {
	return kernel.Event{Seq: seq, Type: typ, Source: kernel.SourceRuntime, Payload: payload}
}

func TestRecoverTimersFoldsLifecycle(t *testing.T) {
	events := []kernel.Event{
		timerEvent(1, kernel.TimerScheduled, map[string]any{"timer_id": "old", "after_ms": int64(50), "deadline_ms": int64(150)}),
		timerEvent(2, kernel.TimerScheduled, map[string]any{"timer_id": "keep", "after_ms": int64(100), "deadline_ms": int64(200)}),
		timerEvent(3, kernel.TimerScheduled, map[string]any{"timer_id": "old", "after_ms": int64(300), "deadline_ms": int64(400)}),
		timerEvent(4, kernel.TimerCancelled, map[string]any{"timer_id": "old"}),
		timerEvent(5, kernel.TimerScheduled, map[string]any{"timer_id": "fired", "after_ms": int64(10), "deadline_ms": int64(210)}),
		timerEvent(6, kernel.TimerFired, map[string]any{"timer_id": "fired", "fired_at_ms": int64(215)}),
		timerEvent(7, kernel.TimerScheduled, map[string]any{"timer_id": "legacy", "after_ms": int64(5), "deadline_ms": int64(220)}),
		timerEvent(8, kernel.TimerTick, map[string]any{"timer_id": "legacy"}),
	}
	got, err := RecoverTimers(events)
	if err != nil {
		t.Fatal(err)
	}
	if got.NowMs != 215 {
		t.Fatalf("logical now = %d, want 215", got.NowMs)
	}
	if !reflect.DeepEqual(got.Pending, map[string]int64{"keep": 200}) {
		t.Fatalf("pending = %v, want keep at 200", got.Pending)
	}
}

func TestVirtualClockRestorePreservesLogicalTimeAndOrder(t *testing.T) {
	v := NewVirtualClock()
	if err := v.Restore(100, map[string]int64{"later": 300, "b": 100, "a": 100}); err != nil {
		t.Fatal(err)
	}
	if got := v.TakeFired(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("overdue order = %v, want [a b]", got)
	}
	if v.NowMs() != 100 {
		t.Fatalf("restore advanced simulated downtime to %d", v.NowMs())
	}
	if delta, armed := v.NextDeadlineMs(); !armed || delta != 200 {
		t.Fatalf("next deadline = %d, %v; want 200, true", delta, armed)
	}
}

func TestRealClockRestoreKeepsAbsoluteDeadline(t *testing.T) {
	base := time.UnixMilli(1000)
	r := NewRealClock()
	r.Now = func() time.Time { return base }
	if err := r.Restore(map[string]int64{"future": 1200, "overdue": 900}); err != nil {
		t.Fatal(err)
	}
	if due := r.Due(); !reflect.DeepEqual(due, []string{"overdue"}) {
		t.Fatalf("initial due = %v, want [overdue]", due)
	}
	if delta, armed := r.NextDeadlineMs(); !armed || delta != 200 {
		t.Fatalf("remaining = %d, %v; want 200, true", delta, armed)
	}
}

func TestLoopAppendsFiredAndTickAtomically(t *testing.T) {
	log := newMemLog()
	clock := NewVirtualClock()
	loop := &Loop{
		Log:    log,
		Runner: &Runner{Log: log, Clock: clock, RunID: "r1"},
		Config: kernel.Config{Blueprint: "bp"},
	}
	if _, err := clock.SetTimer("stage:build", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := clock.Advance(10); err != nil {
		t.Fatal(err)
	}
	fired := clock.TakeFired()
	if err := loop.appendTicks(fired); err != nil {
		t.Fatal(err)
	}
	events, _ := log.Read(1, 0)
	if got := typesOf(events); !reflect.DeepEqual(got, []kernel.EventType{kernel.TimerFired, kernel.TimerTick}) {
		t.Fatalf("events = %v", got)
	}
	if events[0].Str("timer_id") != "stage:build" || int64(events[0].Num("fired_at_ms")) != 10 {
		t.Fatalf("firing payload = %#v", events[0].Payload)
	}

	log2 := newMemLog()
	log2.failAppend = context.DeadlineExceeded
	loop.Log, loop.Runner.Log = log2, log2
	if err := loop.appendTicks([]string{"retry"}); err == nil {
		t.Fatal("failed atomic append returned nil")
	}
	if log2.Head() != 0 {
		t.Fatalf("failed pair append exposed %d records", log2.Head())
	}
}
