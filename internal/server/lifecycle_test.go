package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
)

// lifecycleServer is a server with one enrolled agent.
func lifecycleServer(t *testing.T) (*Server, *Hub) {
	t.Helper()
	srv := openTestServer(t)
	if err := srv.store.CreateAgent(context.Background(), store.Agent{ID: "agent-a", Name: "agent-a", DataKey: []byte("k"), CertSerial: "1"}); err != nil {
		t.Fatal(err)
	}
	return srv, srv.hub
}

// testConn is an agent's control connection as the hub sees it.
func testConn(id string) *agentConn {
	return &agentConn{id: id, send: make(chan agentproto.Msg, 64), cancel: func() {}, started: time.Now()}
}

// connected attaches an agent that is working on runID.
func connected(h *Hub, runID string) *agentConn {
	c := testConn("agent-a")
	h.attach(c, runID)
	return c
}

func runState(t *testing.T, srv *Server, id string) (status, errMsg string) {
	t.Helper()
	r, err := srv.store.GetRun(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status, r.Error
}

func sent(c *agentConn) []agentproto.Msg {
	var out []agentproto.Msg
	for {
		select {
		case m := <-c.send:
			out = append(out, m)
		default:
			return out
		}
	}
}

func cancelsFor(msgs []agentproto.Msg, runID string) int {
	n := 0
	for _, m := range msgs {
		if m.Type == agentproto.TypeCancel && m.RunID == runID {
			n++
		}
	}
	return n
}

// A run the server gives up on must be stopped on the agent too. It used
// to carry on there, loading the link and refusing the next run as busy.
func TestTimedOutRunIsCancelledOnTheAgent(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	c := connected(h, "run-1")

	h.sweep(time.Now().Add(30 * time.Minute)) // within the hour's timeout
	if st, _ := runState(t, srv, "run-1"); st != "running" {
		t.Fatalf("the run ended before its timeout: %s", st)
	}
	h.sweep(time.Now().Add(2 * time.Hour))
	if st, msg := runState(t, srv, "run-1"); st != "failed" || msg != "run timed out" {
		t.Errorf("want failed with a timeout, got %s %q", st, msg)
	}
	if n := cancelsFor(sent(c), "run-1"); n != 1 {
		t.Errorf("the agent was sent %d cancel(s) for the run, want 1", n)
	}
	if _, busy := h.Online("agent-a"); busy != "" {
		t.Errorf("the agent is still marked busy with %q", busy)
	}
}

// Losing the agent's control connection does not end the run: the test
// carries on at the agent, which picks the run up again when it is back.
func TestRunSurvivesAnAgentReconnect(t *testing.T) {
	srv, h := lifecycleServer(t)
	ctx := context.Background()
	activeTestRun(t, srv, "run-1", "agent-a")
	c := connected(h, "run-1")

	h.detach(c)
	if st, _ := runState(t, srv, "run-1"); st != "running" {
		t.Fatalf("the run was ended by the disconnect: %s", st)
	}
	if !h.AgentAway("run-1") {
		t.Error("the run should be shown as waiting for its agent")
	}
	if !h.ownsActive("agent-a", "run-1") {
		t.Fatal("the run is no longer active")
	}

	// The agent returns, still running it.
	c2 := testConn("agent-a")
	if abandoned := h.attach(c2, "run-1"); abandoned != "" {
		t.Errorf("the server asked to cancel a run it still has: %q", abandoned)
	}
	if h.AgentAway("run-1") {
		t.Error("the run should no longer be waiting for its agent")
	}
	if _, busy := h.Online("agent-a"); busy != "run-1" {
		t.Errorf("the agent should be busy with run-1 again, got %q", busy)
	}
	h.sweep(time.Now().Add(10 * time.Minute))
	if st, _ := runState(t, srv, "run-1"); st != "running" {
		t.Fatalf("a reclaimed run was ended: %s", st)
	}
	h.handleAgentMsg(ctx, "agent-a", agentproto.Msg{Type: agentproto.TypeEvent, RunID: "run-1", Data: json.RawMessage(`{"type":"step_start"}`)})
	if n := len(h.RunEvents("run-1")); n != 1 {
		t.Errorf("live events should flow again after the reconnect (%d recorded)", n)
	}
	h.handleAgentMsg(ctx, "agent-a", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-1", Data: reportJSON(t, "complete")})
	if st, _ := runState(t, srv, "run-1"); st != "done" {
		t.Errorf("want done, got %s", st)
	}
	if _, busy := h.Online("agent-a"); busy != "" {
		t.Errorf("the agent is still marked busy with %q", busy)
	}
}

// The run finished while the agent was away: it comes back without the
// run and uploads the result it kept.
func TestResultKeptWhileAwayIsAccepted(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	c := connected(h, "run-1")
	h.detach(c)

	c2 := testConn("agent-a")
	if abandoned := h.attach(c2, ""); abandoned != "" {
		t.Errorf("nothing to cancel, got %q", abandoned)
	}
	h.sweep(time.Now().Add(unclaimedGrace / 2))
	if st, _ := runState(t, srv, "run-1"); st != "running" {
		t.Fatalf("the run was failed before its result had a chance to arrive: %s", st)
	}
	h.handleAgentMsg(context.Background(), "agent-a", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-1", Data: reportJSON(t, "kept")})
	if st, _ := runState(t, srv, "run-1"); st != "done" {
		t.Errorf("want done, got %s", st)
	}
}

// The agent comes back without the run and nothing follows (it was
// restarted mid-run): the run fails soon, not at its full timeout.
func TestRunLostByTheAgentFailsSoon(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	c := connected(h, "run-1")
	h.detach(c)
	h.attach(testConn("agent-a"), "")

	h.sweep(time.Now().Add(unclaimedGrace + time.Second))
	st, msg := runState(t, srv, "run-1")
	if st != "failed" || !strings.Contains(msg, "sent no result") {
		t.Errorf("want failed because no result came, got %s %q", st, msg)
	}
	// A failure the agent kept for this run says why, if it arrives first.
	activeTestRun(t, srv, "run-2", "agent-a")
	c = connected(h, "run-2")
	h.detach(c)
	h.attach(testConn("agent-a"), "")
	h.handleAgentMsg(context.Background(), "agent-a", agentproto.Msg{Type: agentproto.TypeFailed, RunID: "run-2", Error: "the agent was stopped during the run"})
	if st, msg := runState(t, srv, "run-2"); st != "failed" || msg != "the agent was stopped during the run" {
		t.Errorf("want the agent's own reason, got %s %q", st, msg)
	}
}

// An agent that reconnects still working on a run the server has given up
// on (timed out, cancelled, server restarted) is told to stop.
func TestAgentIsToldToStopARunTheServerGaveUpOn(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	c := connected(h, "run-1")
	h.detach(c)
	if err := h.CancelRun("run-1"); err != nil { // cancelled by a user while the agent is away
		t.Fatal(err)
	}
	if abandoned := h.attach(testConn("agent-a"), "run-1"); abandoned != "run-1" {
		t.Errorf("the agent should be told to stop run-1, got %q", abandoned)
	}
	if st, _ := runState(t, srv, "run-1"); st != "cancelled" {
		t.Errorf("the run must stay cancelled, got %s", st)
	}
	if abandoned := h.attach(testConn("agent-a"), "run-unknown"); abandoned != "run-unknown" {
		t.Errorf("a run the server has never heard of should be stopped too, got %q", abandoned)
	}
}

// A newer connection from the same agent replaces the old one; the old
// one ending afterwards must not mark the run as waiting for its agent.
func TestReplacedConnectionDoesNotDetachTheRun(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	old := connected(h, "run-1")
	h.attach(testConn("agent-a"), "run-1")
	h.detach(old)
	if h.AgentAway("run-1") {
		t.Error("the run was marked as waiting although its agent is connected")
	}
	if on, busy := h.Online("agent-a"); !on || busy != "run-1" {
		t.Errorf("want the agent online and busy with run-1, got %v %q", on, busy)
	}
}

// A report from a run that was cut short is not stored as a finished run.
// (Agents before this change uploaded one when they lost the connection.)
func TestPartialResultIsNotStoredAsDone(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	connected(h, "run-1")
	partial, _ := json.Marshal(&results.Report{Schema: 1, Profile: "quick", Notes: []string{results.NoteCancelled},
		Steps: []results.Step{{ID: "s01", Test: "latency", Status: "ok", Result: &results.Latency{}}}})
	h.handleAgentMsg(context.Background(), "agent-a", agentproto.Msg{Type: agentproto.TypeResult, RunID: "run-1", Data: partial})
	st, msg := runState(t, srv, "run-1")
	if st != "failed" || !strings.Contains(msg, "cut short") {
		t.Errorf("want failed as cut short, got %s %q", st, msg)
	}
	r, _ := srv.store.GetRun(context.Background(), "run-1", true)
	if len(r.Report) != 0 {
		t.Error("the partial report was stored")
	}
}

func TestRunTimeoutCoversRequestedDurations(t *testing.T) {
	plain := runTimeout(agentproto.RunRequest{Profile: "quick"}, []string{"latency", "tcp-down"})
	if plain != 5*time.Minute {
		t.Errorf("quick profile: %v", plain)
	}
	// Three tests together for an hour, each alone first: four hours of
	// testing can't be given the 45 minutes of a custom run.
	group := agentproto.RunRequest{Tests: []string{"voip+tcp-down+tcp-up"}, GroupDurationS: 3600}
	if got := runTimeout(group, group.Tests); got < 4*time.Hour {
		t.Errorf("a 4 h group run gets %v", got)
	}
	group.NoBaseline = true
	if got := runTimeout(group, group.Tests); got < time.Hour || got > 3*time.Hour {
		t.Errorf("a 1 h group run without baselines gets %v", got)
	}
	cdp := agentproto.RunRequest{Tests: []string{"cdp-up", "cdp-down"}, CDP: &results.CDPSettings{DurationS: 7200}}
	if got := runTimeout(cdp, cdp.Tests); got < 4*time.Hour {
		t.Errorf("two 2 h CDP tests get %v", got)
	}
	if got := runTimeout(agentproto.RunRequest{Profile: "full", Scale: 2}, nil); got != 90*time.Minute {
		t.Errorf("full at scale 2: %v", got)
	}
}

// Revoking an agent ends its run at once: it will not be back for it.
func TestRevokedAgentsRunIsFailed(t *testing.T) {
	srv, h := lifecycleServer(t)
	activeTestRun(t, srv, "run-1", "agent-a")
	connected(h, "run-1")
	h.disconnect("agent-a")
	if st, msg := runState(t, srv, "run-1"); st != "failed" || !strings.Contains(msg, "revoked") {
		t.Errorf("want failed because the agent was revoked, got %s %q", st, msg)
	}
	if h.ownsActive("agent-a", "run-1") {
		t.Error("the run is still active")
	}
}
