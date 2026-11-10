package guard

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveredLogsAndReports(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if Recovered(log, "nothing", nil) {
		t.Error("no panic must report false")
	}
	if buf.Len() != 0 {
		t.Errorf("no panic must log nothing, got %q", buf.String())
	}
	closed := false
	func() {
		defer func() {
			if Recovered(log, "one connection", recover()) {
				closed = true
			}
		}()
		var xs []int
		_ = xs[3]
	}()
	if !closed {
		t.Fatal("the panic was not reported to the caller")
	}
	out := buf.String()
	for _, want := range []string{"one connection", "index out of range", "guard_test.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("log should contain %q: %s", want, out)
		}
	}
}

func TestGoSurvivesPanic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	done := make(chan struct{})
	Go(log, "worker", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not run")
	}
}

func TestLoopRestartsAfterPanicAndStopsOnReturn(t *testing.T) {
	old := restartDelay
	restartDelay = time.Millisecond
	defer func() { restartDelay = old }()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	var calls atomic.Int32
	Loop(context.Background(), log, "loop", func(context.Context) {
		if calls.Add(1) < 3 {
			panic("boom")
		}
	})
	if n := calls.Load(); n != 3 {
		t.Errorf("want 2 restarts then a normal return, got %d calls", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	calls.Store(0)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		Loop(ctx, log, "loop", func(context.Context) {
			if calls.Add(1) == 2 {
				cancel()
			}
			panic("always")
		})
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not stop when the context ended")
	}
}
