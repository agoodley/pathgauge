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
}

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

	h.mu.Lock()
	if old := h.agents[id]; old != nil {
		old.cancel()
	}
	h.agents[id] = conn
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.agents[id] == conn {
			delete(h.agents, id)
		}
		busy := conn.busy
		h.mu.Unlock()
		if busy != "" {
			h.failRun(busy, "agent disconnected during the run")
		}
		h.broadcast(map[string]any{"type": "agent_status", "agent_id": id, "online": false})
		h.s.log.Info("agent disconnected", "agent", a.Name)
	}()
	h.s.log.Info("agent connected", "agent", a.Name, "version", hp.Version, "remote", r.RemoteAddr)
	h.broadcast(map[string]any{"type": "agent_status", "agent_id": id, "online": true})

	advHost := h.s.cfg.DataPlane.Advertise
	conn.send <- agentproto.Msg{Type: agentproto.TypeWelcome, Data: mustJSON(agentproto.Welcome{
		DataPort: h.s.DataPort(), DataHost: advHost, MonitorEnabled: a.MonitorEnabled, MonitorInterval: a.MonitorInterval, ServerVersion: version.Get().Version,
	})}

	go func() {
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
		h.handleAgentMsg(ctx, id, m)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (h *Hub) handleAgentMsg(ctx context.Context, agentID string, m agentproto.Msg) {
	switch m.Type {
	case agentproto.TypeEvent:
		h.mu.Lock()
		seq := 0
		if ar := h.runs[m.RunID]; ar != nil {
			ar.seq++
			seq = ar.seq
			isProgress := strings.Contains(string(m.Data), `"type":"progress"`)
			if !isProgress || ar.progress < maxProgressEvents {
				ar.events = append(ar.events, RunEvent{Seq: seq, Event: m.Data})
				if isProgress {
					ar.progress++
				}
			}
		}
		h.mu.Unlock()
		h.broadcast(map[string]any{"type": "run_event", "run_id": m.RunID, "agent_id": agentID, "seq": seq, "event": m.Data})
	case agentproto.TypeResult:
		h.finishRun(ctx, agentID, m.RunID, m.Data)
	case agentproto.TypeFailed:
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
	if _, err := (engine.Options{Profile: req.Profile, Tests: req.Tests}).TestList(); err != nil {
		return "", err
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
	timeout := runTimeout(req)
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

// runTimeout bounds how long a run may take before it is failed.
func runTimeout(req agentproto.RunRequest) time.Duration {
	base := map[string]time.Duration{"quick": 5 * time.Minute, "standard": 15 * time.Minute, "full": 45 * time.Minute}[req.Profile]
	if base == 0 || len(req.Tests) > 0 {
		base = 45 * time.Minute
	}
	if req.Scale > 1 {
		base = time.Duration(float64(base) * req.Scale)
	}
	return base
}

// CancelRun asks the agent to stop.
func (h *Hub) CancelRun(runID string) error {
	h.mu.Lock()
	ar := h.runs[runID]
	var conn *agentConn
	if ar != nil {
		conn = h.agents[ar.agentID]
	}
	h.mu.Unlock()
	if ar == nil {
		return errors.New("run is not active")
	}
	if conn != nil {
		select {
		case conn.send <- agentproto.Msg{Type: agentproto.TypeCancel, RunID: runID}:
		default:
		}
	}
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
	defer h.release(agentID, runID)
	var rep results.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		h.failRun(runID, "invalid result from agent: "+err.Error())
		return
	}
	a, _ := h.s.store.Agent(ctx, agentID)
	analysis.Analyze(&rep, analysis.Expectations{DownMbps: a.ExpectedDown, UpMbps: a.ExpectedUp})
	rep.ID = runID
	b, _ := json.Marshal(&rep)
	fj, _ := json.Marshal(rep.Findings)
	if err := h.s.store.FinishRun(ctx, runID, b, store.RunSummary{
		HealthScore: rep.Summary.HealthScore, DownMbps: rep.Summary.DownMbps, UpMbps: rep.Summary.UpMbps,
		RTTms: rep.Summary.IdleRTTms, BloatGrade: rep.Summary.BloatGrade, Findings: fj,
	}); err != nil {
		h.s.log.Error("store run", "run", runID, "err", err)
		return
	}
	h.s.log.Info("run finished", "run", runID, "agent", a.Name, "health", rep.Summary.HealthScore)
	h.broadcast(map[string]any{"type": "run_status", "run_id": runID, "agent_id": agentID, "status": "done", "health_score": rep.Summary.HealthScore})
	h.s.alerts.onRun(ctx, agentID, &rep)
}

// watchdog fails runs that exceed their timeout.
func (h *Hub) watchdog(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		var expired []string
		for id, r := range h.runs {
			if time.Since(r.started) > r.timeout {
				expired = append(expired, id)
			}
		}
		h.mu.Unlock()
		for _, id := range expired {
			h.failRun(id, "run timed out")
		}
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

// disconnect drops an agent (after revocation).
func (h *Hub) disconnect(id string) {
	h.mu.Lock()
	conn := h.agents[id]
	h.mu.Unlock()
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
