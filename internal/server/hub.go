package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/analysis"
	"github.com/agoodley/pathgauge/internal/engine"
	"github.com/agoodley/pathgauge/internal/guard"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
	"github.com/agoodley/pathgauge/internal/version"
)

// Hub tracks connected agents, active runs and dashboard subscribers.
type Hub struct {
	s      *Server
	mu     sync.Mutex
	agents map[string]*agentConn
	runs   map[string]*activeRun
	subs   map[chan []byte]struct{}
}

type agentConn struct {
	id      string
	send    chan agentproto.Msg
	busy    string
	cancel  context.CancelFunc
	started time.Time
}

type activeRun struct {
	id, agentID string
	started     time.Time
	timeout     time.Duration
	seq         int
	events      []RunEvent // step events kept in full, progress events capped
	progress    int
	// away is when the agent's connection was lost; zero while it is
	// connected. The run carries on running on the agent, which uploads
	// the result (or reclaims the run) when it is back.
	away time.Time
	// unclaimed is when the agent came back without this run. A result it
	// finished while away may still follow; if none does, the run is lost.
	unclaimed time.Time
}

// unclaimedGrace is how long a run waits for its result after the agent
// reconnected without it.
var unclaimedGrace = time.Minute

// RunEvent is one buffered engine event of an active run.
type RunEvent struct {
	Seq   int             `json:"seq"`
	Event json.RawMessage `json:"event"`
}

// maxProgressEvents caps buffered live-chart events per run.
const maxProgressEvents = 5000

func newHub(s *Server) *Hub {
	return &Hub{s: s, agents: map[string]*agentConn{}, runs: map[string]*activeRun{}, subs: map[chan []byte]struct{}{}}
}

// Online reports whether an agent is connected, and its active run.
func (h *Hub) Online(id string) (online bool, busy string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.agents[id]
	if c == nil {
		return false, ""
	}
	return true, c.busy
}

// broadcast sends a live update to every dashboard subscriber.
func (h *Hub) broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow subscriber: drop rather than block agents
		}
	}
}

// subscribe registers a dashboard live connection.
func (h *Hub) subscribe() (chan []byte, func()) {
	ch := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// handleAgent serves the agent control WebSocket (mTLS required).
func (h *Hub) handleAgent(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	leaf := r.TLS.PeerCertificates[0]
	id := leaf.Subject.CommonName
	a, err := h.s.store.Agent(r.Context(), id)
	if err != nil || a.Revoked || a.CertSerial != leaf.SerialNumber.Text(16) {
		http.Error(w, "agent unknown or revoked", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn := &agentConn{id: id, send: make(chan agentproto.Msg, 64), cancel: cancel, started: time.Now()}

	var hello agentproto.Msg
	rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
	err = wsjson.Read(rctx, ws, &hello)
	rcancel()
	if err != nil || hello.Type != agentproto.TypeHello {
		ws.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	var hp agentproto.Hello
	_ = json.Unmarshal(hello.Data, &hp)
	h.s.store.AgentSeen(ctx, id, hp.Version, r.RemoteAddr, hp.Host, hp.Policy)

	abandoned := h.attach(conn, hp.RunID)
	defer func() {
		h.detach(conn)
		h.broadcast(map[string]any{"type": "agent_status", "agent_id": id, "online": false})
		h.s.log.Info("agent disconnected", "agent", a.Name)
	}()
	h.s.log.Info("agent connected", "agent", a.Name, "version", hp.Version, "remote", r.RemoteAddr)
	h.broadcast(map[string]any{"type": "agent_status", "agent_id": id, "online": true})

	advHost := h.s.cfg.DataPlane.Advertise
	conn.send <- agentproto.Msg{Type: agentproto.TypeWelcome, Data: mustJSON(agentproto.Welcome{
		DataPort: h.s.DataPort(), DataHost: advHost, MonitorEnabled: a.MonitorEnabled, MonitorInterval: a.MonitorInterval, ServerVersion: version.Get().Version,
	})}
	if abandoned != "" {
		// The agent is still working on a run the server has given up on
		// (timed out, cancelled, or the server restarted): stop it, or it
		// keeps loading the link and refuses the next run as busy.
		h.s.log.Warn("agent is working on a run that is no longer active; cancelling it there", "agent", a.Name, "run", abandoned)
		conn.send <- agentproto.Msg{Type: agentproto.TypeCancel, RunID: abandoned}
	}

	go func() {
		defer func() {
			if guard.Recovered(h.s.log, "agent connection writer", recover()) {
				cancel()
			}
		}()
		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-conn.send:
				wctx, c := context.WithTimeout(ctx, 30*time.Second)
				err := wsjson.Write(wctx, ws, m)
				c()
				if err != nil {
					cancel()
					return
				}
			case <-ping.C:
				if err := ws.Ping(ctx); err != nil {
					cancel()
					return
				}
				h.s.store.AgentSeen(ctx, id, "", "", nil, nil)
			}
		}
	}()
	for {
		var m agentproto.Msg
		if err := wsjson.Read(ctx, ws, &m); err != nil {
			ws.Close(websocket.StatusNormalClosure, "")
			return
		}
		h.safeAgentMsg(ctx, id, m)
	}
}

// attach makes conn the agent's connection. claimed is the run the agent
// says it is still working on (it kept going while disconnected). attach
// returns that run's ID if the server no longer has it active, so the
// caller can tell the agent to stop.
func (h *Hub) attach(conn *agentConn, claimed string) (abandoned string) {
	var resumed, orphaned []string
	h.mu.Lock()
	if old := h.agents[conn.id]; old != nil {
		old.cancel()
	}
	h.agents[conn.id] = conn
	for _, ar := range h.runs {
		if ar.agentID != conn.id {
			continue
		}
		if ar.id == claimed {
			ar.away, ar.unclaimed = time.Time{}, time.Time{}
			conn.busy = ar.id
			resumed = append(resumed, ar.id)
		} else if ar.unclaimed.IsZero() {
			// The agent is back and not working on this run. It may have
			// finished it while away (the result follows) or lost it.
			ar.unclaimed = time.Now()
			orphaned = append(orphaned, ar.id)
		}
	}
	h.mu.Unlock()
	for _, id := range resumed {
		h.s.log.Info("agent is back and still running its test", "agent", conn.id, "run", id)
		h.broadcast(map[string]any{"type": "run_status", "run_id": id, "agent_id": conn.id, "status": "running"})
	}
	for _, id := range orphaned {
		h.s.log.Info("agent is back without its run; waiting for a result it may have kept", "agent", conn.id, "run", id)
	}
	if claimed != "" && len(resumed) == 0 {
		return claimed
	}
	return ""
}

// detach is called when conn ends. A run the agent was working on stays
// active: losing the control connection does not stop the test, and the
// agent uploads the result or reclaims the run when it reconnects. The
// run's own timeout still applies.
func (h *Hub) detach(conn *agentConn) {
	var away string
	h.mu.Lock()
	if h.agents[conn.id] == conn { // not already replaced by a newer connection
		delete(h.agents, conn.id)
		if ar := h.runs[conn.busy]; ar != nil && ar.agentID == conn.id {
			ar.away = time.Now()
			away = ar.id
		}
	}
	h.mu.Unlock()
	if away != "" {
		h.s.log.Warn("agent disconnected during a run; the run continues on the agent", "agent", conn.id, "run", away)
		h.broadcast(map[string]any{"type": "run_status", "run_id": away, "agent_id": conn.id, "status": "running", "agent_away": true})
	}
}

// AgentAway reports whether runID is active while its agent is disconnected.
func (h *Hub) AgentAway(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ar := h.runs[runID]
	return ar != nil && !ar.away.IsZero()
}

// safeAgentMsg handles one message; a panic while handling it costs that
// message, not the agent's connection.
func (h *Hub) safeAgentMsg(ctx context.Context, agentID string, m agentproto.Msg) {
	defer func() { guard.Recovered(h.s.log, "agent message", recover()) }()
	h.handleAgentMsg(ctx, agentID, m)
}

// ownsActive reports whether runID is an active run given to this agent.
// An agent may only report on its own runs.
func (h *Hub) ownsActive(agentID, runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ar := h.runs[runID]
	return ar != nil && ar.agentID == agentID
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (h *Hub) handleAgentMsg(ctx context.Context, agentID string, m agentproto.Msg) {
	switch m.Type {
	case agentproto.TypeEvent:
		h.mu.Lock()
		ar := h.runs[m.RunID]
		if ar == nil || ar.agentID != agentID {
			// Not an active run of this agent: nothing to show it on.
			h.mu.Unlock()
			return
		}
		ar.seq++
		seq := ar.seq
		isProgress := strings.Contains(string(m.Data), `"type":"progress"`)
		if !isProgress || ar.progress < maxProgressEvents {
			ar.events = append(ar.events, RunEvent{Seq: seq, Event: m.Data})
			if isProgress {
				ar.progress++
			}
		}
		h.mu.Unlock()
		h.broadcast(map[string]any{"type": "run_event", "run_id": m.RunID, "agent_id": agentID, "seq": seq, "event": m.Data})
	case agentproto.TypeResult:
		h.finishRun(ctx, agentID, m.RunID, m.Data)
	case agentproto.TypeFailed:
		if !h.ownsActive(agentID, m.RunID) {
			h.s.log.Warn("ignored a failure report for a run that is not an active run of this agent", "agent", agentID, "run", m.RunID)
			return
		}
		h.failRun(m.RunID, m.Error)
	case agentproto.TypeMonitor:
		var mon agentproto.Monitor
		if json.Unmarshal(m.Data, &mon) == nil && len(mon.Samples) > 0 {
			if err := h.s.store.AddSamples(ctx, agentID, mon.Samples); err != nil {
				h.s.log.Warn("store monitor samples", "err", err)
			}
			h.broadcast(map[string]any{"type": "monitor", "agent_id": agentID, "samples": mon.Samples})
		}
	}
}

// ErrAgentOffline and ErrAgentBusy are returned by StartRun.
var (
	ErrAgentOffline = errors.New("agent is not connected")
	ErrAgentBusy    = errors.New("agent is already running a test")
)

// StartRun asks an agent to run a profile against the server.
func (h *Hub) StartRun(ctx context.Context, agentID string, req agentproto.RunRequest, trigger, by string) (string, error) {
	tests, err := (engine.Options{Profile: req.Profile, Tests: req.Tests}).TestList()
	if err != nil {
		return "", err
	}
	if req.Mix != "" {
		if _, _, err := engine.ParseMix(req.Mix); err != nil {
			return "", err
		}
	}
	a, err := h.s.store.Agent(ctx, agentID)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	conn := h.agents[agentID]
	if conn == nil {
		h.mu.Unlock()
		return "", ErrAgentOffline
	}
	if conn.busy != "" {
		h.mu.Unlock()
		return "", ErrAgentBusy
	}
	id := engine.NewRunID()
	conn.busy = id
	timeout := runTimeout(req, tests)
	h.runs[id] = &activeRun{id: id, agentID: agentID, started: time.Now(), timeout: timeout}
	h.mu.Unlock()

	if req.ClientName == "" {
		req.ClientName = a.Name
	}
	if req.ServerName == "" {
		req.ServerName = h.s.serverName(ctx)
	}
	opts, _ := json.Marshal(req)
	profile := req.Profile
	if len(req.Tests) > 0 {
		profile = "custom"
	}
	if err := h.s.store.CreateRun(ctx, store.Run{ID: id, AgentID: agentID, Profile: profile, Options: opts, Status: "running", Trigger: trigger, RequestedBy: by}); err != nil {
		h.release(agentID, id)
		return "", err
	}
	_ = h.s.store.SetRunStatus(ctx, id, "running", "")
	select {
	case conn.send <- agentproto.Msg{Type: agentproto.TypeRun, RunID: id, Data: opts}:
	case <-time.After(5 * time.Second):
		h.failRun(id, "could not send the run request to the agent")
		return "", errors.New("agent did not accept the run request")
	}
	h.s.store.Audit(ctx, by, "run.start", id, fmt.Sprintf("agent=%s profile=%s trigger=%s", a.Name, profile, trigger))
	h.broadcast(map[string]any{"type": "run_status", "run_id": id, "agent_id": agentID, "status": "running"})
	return id, nil
}

// runTimeout bounds how long a run may take before it is failed and
// cancelled on the agent. Tests whose length the request sets (groups and
// CDP replication) add their time on top of the profile's allowance, which
// would otherwise be shorter than the run that was asked for.
func runTimeout(req agentproto.RunRequest, tests []string) time.Duration {
	base := map[string]time.Duration{"quick": 5 * time.Minute, "standard": 15 * time.Minute, "full": 45 * time.Minute}[req.Profile]
	if base == 0 || len(req.Tests) > 0 {
		base = 45 * time.Minute
	}
	scale := max(req.Scale, 1)
	base = time.Duration(float64(base) * scale)
	seconds := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	var extra time.Duration
	for _, t := range tests {
		switch {
		case engine.IsGroup(t):
			d := max(15*time.Second, time.Duration(30*scale*float64(time.Second)))
			if req.GroupDurationS > 0 {
				d = seconds(req.GroupDurationS)
			}
			runs := 1 // together
			if !req.NoBaseline {
				runs += len(engine.GroupMembers(t)) // and each on its own first
			}
			extra += time.Duration(runs) * d
		case strings.HasPrefix(t, "cdp-") && req.CDP != nil && req.CDP.DurationS > 0:
			extra += seconds(req.CDP.DurationS)
		}
	}
	// Half as much again for setup, draining and result collection.
	return base + extra*3/2
}

// CancelRun asks the agent to stop.
func (h *Hub) CancelRun(runID string) error {
	h.mu.Lock()
	ar := h.runs[runID]
	h.mu.Unlock()
	if ar == nil {
		return errors.New("run is not active")
	}
	h.cancelOnAgent(ar.agentID, runID)
	_ = h.s.store.SetRunStatus(context.Background(), runID, "cancelled", "cancelled by user")
	h.release(ar.agentID, runID)
	h.broadcast(map[string]any{"type": "run_status", "run_id": runID, "status": "cancelled"})
	return nil
}

func (h *Hub) release(agentID, runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.runs, runID)
	if c := h.agents[agentID]; c != nil && c.busy == runID {
		c.busy = ""
	}
}

func (h *Hub) failRun(runID, msg string) {
	h.mu.Lock()
	ar := h.runs[runID]
	h.mu.Unlock()
	_ = h.s.store.SetRunStatus(context.Background(), runID, "failed", msg)
	if ar != nil {
		h.release(ar.agentID, runID)
	}
	h.broadcast(map[string]any{"type": "run_status", "run_id": runID, "status": "failed", "error": msg})
}

func (h *Hub) finishRun(ctx context.Context, agentID, runID string, data json.RawMessage) {
	// An agent reports only on its own runs. A run that is still active
	// must be this agent's; one that is no longer active (a result held
	// back by a disconnect) is checked against its owner in the database.
	h.mu.Lock()
	ar := h.runs[runID]
	h.mu.Unlock()
	if ar != nil && ar.agentID != agentID {
		h.s.log.Warn("ignored a result for another agent's run", "agent", agentID, "run", runID)
		return
	}
	defer h.release(agentID, runID)
	// The analysis runs on whatever the agent sent. If it panics, the run
	// is failed rather than left "running" for ever.
	defer func() {
		if guard.Recovered(h.s.log, "result of run "+runID, recover()) && ar != nil {
			h.failRun(runID, "the server could not process the result (see the server log)")
		}
	}()
	var rep results.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		if ar != nil {
			h.failRun(runID, "invalid result from agent: "+err.Error())
		}
		return
	}
	if rep.Cancelled() {
		// Agents up to 0.1.0-rc.6 cancelled the test when they lost the control
		// connection and uploaded what they had. A run cut short is not a
		// result: the tests that never ran would read as "no problems".
		h.s.log.Warn("ignored the partial result of a run that was cut short on the agent", "agent", agentID, "run", runID)
		if ar != nil {
			h.failRun(runID, "the run was cut short on the agent (it lost its connection or was stopped), so its partial result was discarded")
		}
		return
	}
	a, _ := h.s.store.Agent(ctx, agentID)
	analysis.Analyze(&rep, analysis.Expectations{DownMbps: a.ExpectedDown, UpMbps: a.ExpectedUp})
	rep.ID = runID
	b, _ := json.Marshal(&rep)
	fj, _ := json.Marshal(rep.Findings)
	stored, err := h.s.store.FinishRun(ctx, runID, agentID, b, store.RunSummary{
		HealthScore: rep.Summary.HealthScore, DownMbps: rep.Summary.DownMbps, UpMbps: rep.Summary.UpMbps,
		RTTms: rep.Summary.IdleRTTms, BloatGrade: rep.Summary.BloatGrade, Findings: fj,
	})
	if err != nil {
		h.s.log.Error("store run", "run", runID, "err", err)
		return
	}
	if !stored {
		// Not this agent's run, or already done or cancelled (which
		// includes a result sent twice after a reconnect).
		h.s.log.Info("ignored a result: the run is not this agent's, or is already finished or cancelled", "agent", agentID, "run", runID)
		return
	}
	h.s.log.Info("run finished", "run", runID, "agent", a.Name, "health", rep.Summary.HealthScore)
	h.broadcast(map[string]any{"type": "run_status", "run_id": runID, "agent_id": agentID, "status": "done", "health_score": rep.Summary.HealthScore})
	h.s.alerts.onRun(ctx, agentID, &rep)
}

// watchdog ends runs the server gives up on.
func (h *Hub) watchdog(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.sweep(time.Now())
	}
}

// sweep fails runs that have exceeded their timeout, and runs whose agent
// came back without them and sent no result. A run that timed out is also
// cancelled on the agent: otherwise it would keep loading the link and
// refuse the next run as busy.
func (h *Hub) sweep(now time.Time) {
	type gone struct{ id, agentID, why string }
	var expired []gone
	h.mu.Lock()
	for id, r := range h.runs {
		switch {
		case now.Sub(r.started) > r.timeout:
			expired = append(expired, gone{id, r.agentID, "run timed out"})
		case !r.unclaimed.IsZero() && now.Sub(r.unclaimed) > unclaimedGrace:
			expired = append(expired, gone{id, r.agentID, "the agent stopped working on this run and sent no result (it was restarted, or the run failed while it was disconnected)"})
		}
	}
	h.mu.Unlock()
	for _, g := range expired {
		h.cancelOnAgent(g.agentID, g.id)
		h.failRun(g.id, g.why)
	}
}

// cancelOnAgent tells the agent to stop a run, if it is connected. If it
// is not, it is told when it reconnects still working on the run (attach).
func (h *Hub) cancelOnAgent(agentID, runID string) {
	h.mu.Lock()
	conn := h.agents[agentID]
	h.mu.Unlock()
	if conn == nil {
		return
	}
	select {
	case conn.send <- agentproto.Msg{Type: agentproto.TypeCancel, RunID: runID}:
	default:
	}
}

// sendConfig pushes monitoring settings to a connected agent.
func (h *Hub) sendConfig(a store.Agent) {
	h.mu.Lock()
	conn := h.agents[a.ID]
	h.mu.Unlock()
	if conn == nil {
		return
	}
	select {
	case conn.send <- agentproto.Msg{Type: agentproto.TypeConfig, Data: mustJSON(agentproto.Config{MonitorEnabled: a.MonitorEnabled, MonitorInterval: a.MonitorInterval})}:
	default:
	}
}

// disconnect drops an agent (after revocation) and ends its runs: unlike
// an agent that lost its connection, it will not be back to finish them.
func (h *Hub) disconnect(id string) {
	h.mu.Lock()
	conn := h.agents[id]
	var runs []string
	for rid, ar := range h.runs {
		if ar.agentID == id {
			runs = append(runs, rid)
		}
	}
	h.mu.Unlock()
	for _, rid := range runs {
		h.cancelOnAgent(id, rid) // best effort: the connection closes next
		h.failRun(rid, "the agent was revoked or deleted during the run")
	}
	if conn != nil {
		conn.cancel()
	}
}

func (s *Server) serverName(ctx context.Context) string {
	if n := strings.TrimSpace(s.store.Setting(ctx, "server_name")); n != "" {
		return n
	}
	h, _ := hostname()
	return h
}

// RunEvents returns the buffered events of an active run (nil if the run
// is not active), so a live view opened mid-run can show earlier steps.
func (h *Hub) RunEvents(runID string) []RunEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	ar := h.runs[runID]
	if ar == nil {
		return nil
	}
	return append([]RunEvent(nil), ar.events...)
}
