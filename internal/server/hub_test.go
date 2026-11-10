package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
)

func openTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.Listen, cfg.DataDir = "127.0.0.1:0", t.TempDir()
	cfg.DataPlane.Listen = "127.0.0.1:0"
	srv, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// activeTestRun creates a running run for an agent, as StartRun does.
func activeTestRun(t *testing.T, srv *Server, runID, agentID string) {
	t.Helper()
	ctx := context.Background()
	if err := srv.store.CreateRun(ctx, store.Run{ID: runID, AgentID: agentID, Profile: "quick", Options: json.RawMessage(`{}`), Status: "running", Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	srv.hub.mu.Lock()
	srv.hub.runs[runID] = &activeRun{id: runID, agentID: agentID, started: time.Now(), timeout: time.Hour}
	srv.hub.mu.Unlock()
}

func reportJSON(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(&results.Report{Schema: 1, Profile: "quick", Client: results.Peer{Name: name},
		Steps: []results.Step{{ID: "s01", Test: "latency", Status: "ok", Result: &results.Latency{}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An agent may report only on runs that were given to it.
func TestAgentCannotTouchAnotherAgentsRun(t *testing.T) {
	srv := openTestServer(t)
	ctx := context.Background()
	h := srv.hub
	for _, id := range []string{"agent-a", "agent-b"} {
		if err := srv.store.CreateAgent(ctx, store.Agent{ID: id, Name: id, DataKey: []byte("k"), CertSerial: "1"}); err != nil {
			t.Fatal(err)
		}
	}
	activeTestRun(t, srv, "run-a", "agent-a")
	status := func() (string, string) {
		t.Helper()
		r, err := srv.store.GetRun(ctx, "run-a", true)
		if err != nil {
			t.Fatal(err)
		}
		var rep results.Report
		_ = json.Unmarshal(r.Report, &rep)
		return r.Status, rep.Client.Name
	}

	// agent-b tries each message type against agent-a's run.
	h.handleAgentMsg(ctx, "agent-b", agentproto.Msg{Type: agentproto.TypeEvent, RunID: "run-a", Data: json.RawMessage(`{"type":"step_start"}`)})
	if n := len(h.RunEvents("run-a")); n != 0 {
		t.Errorf("another agent injected %d event(s) into the run", n)
	}
	h.handleAgentMsg(ctx, "agent-b", agentproto.Msg{Type: agentproto.TypeFailed, RunID: "run-a", Error: "forged failure"})
	if st, _ := status(); st != "running" {
		t.Errorf("another agent failed the run: status %q", st)
	}
	h.handleAgentMsg(ctx, "agent-b", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-a", Data: reportJSON(t, "forged")})
	if st, name := status(); st != "running" || name != "" {
		t.Errorf("another agent finished the run: status %q, report from %q", st, name)
	}
	h.handleAgentMsg(ctx, "agent-b", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-a", Data: json.RawMessage(`not json`)})
	if st, _ := status(); st != "running" {
		t.Errorf("another agent's invalid result failed the run: status %q", st)
	}
	if !h.ownsActive("agent-a", "run-a") {
		t.Fatal("the run was released by another agent's messages")
	}

	// The owner's messages work as before.
	h.handleAgentMsg(ctx, "agent-a", agentproto.Msg{Type: agentproto.TypeEvent, RunID: "run-a", Data: json.RawMessage(`{"type":"step_start"}`)})
	if n := len(h.RunEvents("run-a")); n != 1 {
		t.Errorf("the owner's event was not recorded (%d)", n)
	}
	h.handleAgentMsg(ctx, "agent-a", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-a", Data: reportJSON(t, "genuine")})
	if st, name := status(); st != "done" || name != "genuine" {
		t.Errorf("the owner's result was not stored: status %q, report from %q", st, name)
	}

	// Once stored, a report can't be replaced, not even by its owner.
	h.handleAgentMsg(ctx, "agent-a", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-a", Data: reportJSON(t, "replacement")})
	h.handleAgentMsg(ctx, "agent-b", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-a", Data: reportJSON(t, "forged")})
	if st, name := status(); st != "done" || name != "genuine" {
		t.Errorf("a stored report was replaced: status %q, report from %q", st, name)
	}
}

// Reports carry text that agents and imported files supply, and share
// links serve them without a login, so they are sandboxed away from the
// dashboard's origin.
func TestReportIsServedSandboxed(t *testing.T) {
	srv := openTestServer(t)
	ctx := context.Background()
	if err := srv.store.CreateRun(ctx, store.Run{ID: "run-x", AgentID: "agent-a", Profile: "quick", Options: json.RawMessage(`{}`), Status: "running", Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := srv.store.FinishRun(ctx, "run-x", "agent-a", reportJSON(t, "client"), store.RunSummary{Findings: json.RawMessage(`[]`)}); err != nil || !ok {
		t.Fatalf("store report: %v %v", ok, err)
	}
	w := httptest.NewRecorder()
	srv.renderReport(w, httptest.NewRequest("GET", "/share/token", nil), "run-x", "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("report not rendered: %d", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	directives := map[string]string{}
	for _, d := range strings.Split(csp, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	sandbox, ok := directives["sandbox"]
	if !ok {
		t.Fatalf("report CSP has no sandbox directive: %q", csp)
	}
	for _, token := range strings.Fields(sandbox) {
		if token != "allow-scripts" {
			t.Errorf("sandbox token %q weakens the isolation of reports", token)
		}
	}
	if strings.Contains(sandbox, "allow-same-origin") || directives["default-src"] != "'none'" {
		t.Errorf("report CSP must keep an opaque origin and no network access: %q", csp)
	}
}
