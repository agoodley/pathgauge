package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/results"
)

// testAgent is an agent with a test endpoint to run against and no server.
func testAgent(t *testing.T) (*Agent, context.CancelFunc) {
	t.Helper()
	key := []byte("agent-test-key")
	ep := endpoint.New(endpoint.Options{Listen: "127.0.0.1:0", Keys: endpoint.StaticKeys{"agt_1": key}, Policy: config.DefaultPolicy()})
	if err := ep.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = ep.Serve(ctx) }()
	a := &Agent{
		cfg:      config.Agent{DataDir: t.TempDir()},
		policy:   config.DefaultPolicy(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		id:       &Identity{AgentID: "agt_1", Name: "test", DataKey: key},
		base:     ctx,
		dataAddr: ep.Addr().String(),
	}
	return a, stop
}

// connect stands in for a control session: messages the agent sends arrive
// on the returned channel until disconnect is called.
func (a *Agent) connectForTest() (out chan agentproto.Msg, disconnect func()) {
	out = make(chan agentproto.Msg, 256)
	a.mu.Lock()
	a.out = out
	a.mu.Unlock()
	return out, func() {
		a.mu.Lock()
		a.out = nil
		a.mu.Unlock()
	}
}

func waitIdle(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		idle := a.runCancel == nil
		a.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the run did not end")
}

func spooled(t *testing.T, a *Agent) []agentproto.Msg {
	t.Helper()
	var out []agentproto.Msg
	entries, _ := os.ReadDir(a.spoolDir())
	for _, e := range entries {
		b, err := os.ReadFile(a.spoolDir() + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var m agentproto.Msg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

var shortRun = agentproto.RunRequest{Profile: "quick", Tests: []string{"latency"}, Scale: 0.2}

// Losing the control connection must not stop the test or cut its result
// short: the run finishes, and its complete result waits on disk for the
// next connection. (The run used to be cancelled with the session, and
// what it had by then was uploaded as if it were the whole result.)
func TestRunOutlivesTheControlSession(t *testing.T) {
	a, _ := testAgent(t)
	out, disconnect := a.connectForTest()
	a.startRun("run-1", shortRun)

	// Wait for the test to be under way, then lose the connection.
	deadline := time.After(20 * time.Second)
	for started := false; !started; {
		select {
		case m := <-out:
			started = m.Type == agentproto.TypeEvent
		case <-deadline:
			t.Fatal("the run never started")
		}
	}
	disconnect()
	a.mu.Lock()
	still := a.runID
	a.mu.Unlock()
	if still != "run-1" {
		t.Fatalf("the run should still be going after the disconnect, agent says it is running %q", still)
	}
	waitIdle(t, a)

	kept := spooled(t, a)
	if len(kept) != 1 || kept[0].Type != agentproto.TypeResult || kept[0].RunID != "run-1" {
		t.Fatalf("want the result of run-1 kept on disk, got %+v", kept)
	}
	var rep results.Report
	if err := json.Unmarshal(kept[0].Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Cancelled() {
		t.Error("the result is marked as cut short")
	}
	if len(rep.Steps) != 1 || rep.Steps[0].Status != "ok" {
		t.Errorf("want one finished latency step, got %+v", rep.Steps)
	}

	// The next session uploads it; the copy goes once it has been written.
	next, _ := a.connectForTest()
	a.flushSpool(context.Background(), next)
	select {
	case m := <-next:
		if m.Type != agentproto.TypeResult || m.RunID != "run-1" {
			t.Errorf("uploaded %+v", m)
		}
		a.unspool(m.RunID) // what the session's writer does after a successful write
	default:
		t.Fatal("the kept result was not uploaded on reconnect")
	}
	if left := spooled(t, a); len(left) != 0 {
		t.Errorf("the copy should be gone after the upload, %d left", len(left))
	}
}

// A run stopped before the end never produces a result.
func TestCancelledRunUploadsNothing(t *testing.T) {
	a, _ := testAgent(t)
	out, _ := a.connectForTest()
	req := shortRun
	req.Scale = 2 // long enough to cancel
	a.startRun("run-2", req)
	a.handle(context.Background(), agentproto.Msg{Type: agentproto.TypeCancel, RunID: "run-2"})
	waitIdle(t, a)
	close(out)
	for m := range out {
		if final(m) {
			t.Errorf("a cancelled run sent %s", m.Type)
		}
	}
	if kept := spooled(t, a); len(kept) != 0 {
		t.Errorf("a cancelled run kept %+v", kept)
	}
}

// When the agent itself is stopped mid-run, the server is told at the next
// start instead of being sent a partial result.
func TestRunInterruptedByAgentStop(t *testing.T) {
	a, stop := testAgent(t)
	req := shortRun
	req.Scale = 2
	a.startRun("run-3", req)
	time.Sleep(300 * time.Millisecond)
	stop()
	waitIdle(t, a)
	kept := spooled(t, a)
	if len(kept) != 1 || kept[0].Type != agentproto.TypeFailed || kept[0].RunID != "run-3" {
		t.Fatalf("want a failure for run-3 kept for the next start, got %+v", kept)
	}
}

func TestSecondRunRequestIsRefusedWhileBusy(t *testing.T) {
	a, _ := testAgent(t)
	out, _ := a.connectForTest()
	req := shortRun
	req.Scale = 2
	a.startRun("run-4", req)
	a.startRun("run-5", req)
	a.handle(context.Background(), agentproto.Msg{Type: agentproto.TypeCancel, RunID: "run-4"})
	waitIdle(t, a)
	close(out)
	refused := false
	for m := range out {
		if m.Type == agentproto.TypeFailed && m.RunID == "run-5" {
			refused = true
		}
		if m.RunID == "run-4" && final(m) {
			t.Errorf("the cancelled run sent %s", m.Type)
		}
	}
	if !refused {
		t.Error("the second request should have been refused as busy")
	}
}

// More kept outcomes than the send queue holds must not block a session.
func TestFlushSpoolStopsWithTheSession(t *testing.T) {
	a, _ := testAgent(t)
	for i := 0; i < 5; i++ {
		a.spool(agentproto.Msg{Type: agentproto.TypeFailed, RunID: "r" + string(rune('a'+i)), Error: "x"})
	}
	out := make(chan agentproto.Msg, 2) // smaller than the spool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.flushSpool(ctx, out); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel() // the session ends with the queue full
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flushSpool is stuck on a full queue after the session ended")
	}
	if left := spooled(t, a); len(left) != 5 {
		t.Errorf("nothing was written to the server, so all 5 must still be kept; %d are", len(left))
	}
}
