// Package agent implements the agent role: enrollment, the mTLS control
// connection to the server, running tests against the server's endpoint,
// continuous monitoring, and local policy enforcement.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/engine"
	"github.com/agoodley/pathgauge/internal/enrich"
	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/pki"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/shaping"
	"github.com/agoodley/pathgauge/internal/stats"
	"github.com/agoodley/pathgauge/internal/store"
	"github.com/agoodley/pathgauge/internal/version"
)

// Identity is the agent's enrolled identity (stored in DATA_DIR/agent).
type Identity struct {
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	ServerURL string `json:"server_url"`
	DataKey   []byte `json:"data_key"`
}

func identityDir(dataDir string) string { return filepath.Join(dataDir, "agent") }

// LoadIdentity reads the stored identity.
func LoadIdentity(dataDir string) (*Identity, error) {
	b, err := os.ReadFile(filepath.Join(identityDir(dataDir), "identity.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("this agent is not enrolled yet: run 'sudo pathgauge agent join https://SERVER:7443 --token TOKEN'")
		}
		return nil, err
	}
	var id Identity
	return &id, json.Unmarshal(b, &id)
}

// Join enrolls with a server using a join token.
func Join(ctx context.Context, serverURL, token, name, dataDir string) (*Identity, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the server URL must look like https://server:7443")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(config.DefaultControlPort))
	}
	caPrefix, _, err := pki.ParseJoinToken(token)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(name)
	if err != nil {
		return nil, err
	}
	host := hostinfo.Collect(hostinfo.SystemSources())
	hostJSON, _ := json.Marshal(host)
	body, _ := json.Marshal(agentproto.EnrollRequest{Token: token, Name: name, CSR: string(csrPEM), Version: version.Get().Version, Host: hostJSON})
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify:     true, //nolint:gosec // verified by VerifyConnection against the CA pinned by the token
		VerifyConnection:       pki.PinnedVerifier(caPrefix),
		SessionTicketsDisabled: true,
		MinVersion:             tls.VersionTLS12,
	}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String()+"/api/agent/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enroll with %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.Unmarshal(rb, &e)
		return nil, fmt.Errorf("server refused enrollment: %s", firstNonEmpty(e.Error, resp.Status))
	}
	var er agentproto.EnrollResponse
	if err := json.Unmarshal(rb, &er); err != nil {
		return nil, err
	}
	dir := identityDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := &Identity{AgentID: er.AgentID, Name: name, ServerURL: u.String(), DataKey: er.DataKey}
	ib, _ := json.MarshalIndent(id, "", "  ")
	for name, data := range map[string][]byte{"identity.json": ib, "agent.key": keyPEM, "agent.crt": []byte(er.Cert), "ca.crt": []byte(er.CA)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	chownToService(dataDir, dir)
	return id, nil
}

// chownToService gives the agent's files to the service user when join
// runs as root, so pathgauge-agent.service (user pathgauge) can read them.
func chownToService(dataDir, dir string) {
	if os.Geteuid() != 0 {
		return
	}
	u, err := user.Lookup("pathgauge")
	if err != nil {
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = os.Chown(dataDir, uid, gid)
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error { //nolint:gosec // our own root-owned directory, created moments ago
		if err == nil {
			_ = os.Chown(p, uid, gid) //nolint:gosec // our own directory, just created by root
		}
		return nil
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Agent is a running agent.
type Agent struct {
	cfg    config.Agent
	policy config.Policy
	log    *slog.Logger
	id     *Identity
	tlsCfg *tls.Config

	mu        sync.Mutex
	out       chan agentproto.Msg // current connection's writer, nil when offline
	runCancel context.CancelFunc
	runID     string
	monOn     bool
	monEvery  time.Duration
	dataAddr  string
	pending   []store.Sample
	shaper    engine.SQMHooks
}

// New loads the identity and prepares an agent.
func New(cfg config.Agent, policy config.Policy, log *slog.Logger) (*Agent, error) {
	id, err := LoadIdentity(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	dir := identityDir(cfg.DataDir)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	if err != nil {
		return nil, fmt.Errorf("agent certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid CA certificate")
	}
	a := &Agent{cfg: cfg, policy: policy, log: log, id: id, monOn: true, monEvery: time.Minute}
	a.tlsCfg = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// The CA is private and pinned at enrollment, so verify the chain
		// against it and skip host-name checks (servers are often reached
		// by IP or by a name not in their certificate). VerifyConnection
		// also runs on resumed sessions.
		InsecureSkipVerify:     true, //nolint:gosec // verified by VerifyConnection against the pinned CA
		VerifyConnection:       pki.ChainVerifier(pool),
		SessionTicketsDisabled: true,
	}
	if policy.Shaping != config.ShapingNone {
		a.shaper = shaping.NewTuner(log, "")
	}
	return a, nil
}

// Run connects to the server and serves until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("agent starting", "name", a.id.Name, "id", a.id.AgentID, "server", a.id.ServerURL, "version", version.Get().Version)
	go a.monitorLoop(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("disconnected from server; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

func (a *Agent) session(ctx context.Context) error {
	u, _ := url.Parse(a.id.ServerURL)
	wsURL := "wss://" + u.Host + "/api/agent/connect"
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: a.tlsCfg}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	ws, resp, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	dcancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
			return fmt.Errorf("server rejected this agent (revoked or deleted?): %s", resp.Status)
		}
		return err
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(16 << 20)
	host := hostinfo.Collect(hostinfo.SystemSources())
	hb, _ := json.Marshal(host)
	pb, _ := json.Marshal(a.policy)
	if err := wsjson.Write(ctx, ws, agentproto.Msg{Type: agentproto.TypeHello, Data: mustJSON(agentproto.Hello{Version: version.Get().Version, Host: hb, Policy: pb})}); err != nil {
		return err
	}
	var welcome agentproto.Msg
	if err := wsjson.Read(ctx, ws, &welcome); err != nil || welcome.Type != agentproto.TypeWelcome {
		return fmt.Errorf("no welcome from server: %w", err)
	}
	var w agentproto.Welcome
	_ = json.Unmarshal(welcome.Data, &w)
	dataHost := w.DataHost
	if dataHost == "" {
		dataHost = u.Hostname()
	}
	out := make(chan agentproto.Msg, 256)
	a.mu.Lock()
	a.dataAddr = net.JoinHostPort(dataHost, strconv.Itoa(w.DataPort))
	a.monOn = w.MonitorEnabled
	if w.MonitorInterval >= 10 {
		a.monEvery = time.Duration(w.MonitorInterval) * time.Second
	}
	a.out = out
	pending := a.pending
	a.pending = nil
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.out = nil
		a.mu.Unlock()
	}()
	a.log.Info("connected to server", "server", u.Host, "data_plane", a.dataAddr, "monitoring", w.MonitorEnabled)
	if len(pending) > 0 {
		out <- agentproto.Msg{Type: agentproto.TypeMonitor, Data: mustJSON(agentproto.Monitor{Samples: pending})}
	}
	a.flushSpool(out)

	errc := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-out:
				wctx, c := context.WithTimeout(ctx, 30*time.Second)
				err := wsjson.Write(wctx, ws, m)
				c()
				if err != nil {
					errc <- err
					cancel()
					return
				}
			}
		}
	}()
	for {
		var m agentproto.Msg
		if err := wsjson.Read(ctx, ws, &m); err != nil {
			select {
			case werr := <-errc:
				return werr
			default:
			}
			return err
		}
		a.handle(ctx, m)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// send queues a message for the server, spooling results while offline.
func (a *Agent) send(m agentproto.Msg) {
	a.mu.Lock()
	out := a.out
	a.mu.Unlock()
	if out != nil {
		select {
		case out <- m:
			return
		case <-time.After(5 * time.Second):
		}
	}
	if m.Type == agentproto.TypeResult {
		a.spool(m)
	}
}

func (a *Agent) spoolDir() string { return filepath.Join(a.cfg.DataDir, "agent", "spool") }

func (a *Agent) spool(m agentproto.Msg) {
	_ = os.MkdirAll(a.spoolDir(), 0o700)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(a.spoolDir(), m.RunID+".json"), b, 0o600); err == nil {
		a.log.Info("server unreachable; result kept for later upload", "run", m.RunID)
	}
}

func (a *Agent) flushSpool(out chan agentproto.Msg) {
	entries, _ := os.ReadDir(a.spoolDir())
	for _, e := range entries {
		p := filepath.Join(a.spoolDir(), e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m agentproto.Msg
		if json.Unmarshal(b, &m) == nil {
			out <- m
			_ = os.Remove(p)
			a.log.Info("uploaded a spooled result", "run", m.RunID)
		}
	}
}

func (a *Agent) handle(ctx context.Context, m agentproto.Msg) {
	switch m.Type {
	case agentproto.TypeRun:
		var req agentproto.RunRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			a.send(agentproto.Msg{Type: agentproto.TypeFailed, RunID: m.RunID, Error: "invalid run request"})
			return
		}
		a.startRun(ctx, m.RunID, req)
	case agentproto.TypeCancel:
		a.mu.Lock()
		if a.runID == m.RunID && a.runCancel != nil {
			a.runCancel()
		}
		a.mu.Unlock()
	case agentproto.TypeConfig:
		var c agentproto.Config
		if json.Unmarshal(m.Data, &c) == nil {
			a.mu.Lock()
			a.monOn = c.MonitorEnabled
			if c.MonitorInterval >= 10 {
				a.monEvery = time.Duration(c.MonitorInterval) * time.Second
			}
			a.mu.Unlock()
			a.log.Info("monitoring settings updated", "enabled", c.MonitorEnabled, "interval_s", c.MonitorInterval)
		}
	}
}

// buildOptions applies the local policy to a run request.
func (a *Agent) buildOptions(req agentproto.RunRequest) (engine.Options, []string, error) {
	if !a.policy.AllowsTime(time.Now()) {
		return engine.Options{}, nil, errors.New("this agent's local policy does not allow tests at this time")
	}
	o := engine.Options{Profile: req.Profile, Tests: req.Tests, Skip: req.Skip, Scale: req.Scale, MaxRateMbps: req.MaxRateMbps,
		Streams: req.Streams, TraceCycles: req.TraceCycles, Resolve: true, Mix: req.Mix, NoDSCP: req.NoDSCP,
		Labels: results.LinkLabels{Client: req.ClientName, Endpoint: req.ServerName}}
	tests, err := o.TestList()
	if err != nil {
		return o, nil, err
	}
	var notes, allowed []string
	for _, t := range tests {
		switch {
		case !a.policy.AllowsTest(t):
			notes = append(notes, "skipped "+t+" (not allowed by the agent's local policy)")
		case t == "sqm" && a.shaper == nil:
			notes = append(notes, "skipped sqm (the agent's local policy does not allow shaping)")
		default:
			allowed = append(allowed, t)
		}
	}
	o.Tests = allowed
	pol := float64(a.policy.MaxRateMbps)
	if o.MaxRateMbps <= 0 || o.MaxRateMbps > pol {
		o.MaxRateMbps = pol
	}
	if a.shaper != nil {
		o.SQM = a.shaper
	}
	if db := enrich.OpenDefault(a.cfg.DataDir); db != nil {
		o.Annotate = db.Lookup
		notes = append(notes, enrich.Attribution)
	}
	return o, notes, nil
}

func (a *Agent) startRun(ctx context.Context, runID string, req agentproto.RunRequest) {
	a.mu.Lock()
	if a.runCancel != nil {
		a.mu.Unlock()
		a.send(agentproto.Msg{Type: agentproto.TypeFailed, RunID: runID, Error: "agent is busy"})
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	a.runCancel, a.runID = cancel, runID
	dataAddr := a.dataAddr
	a.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			a.mu.Lock()
			a.runCancel, a.runID = nil, ""
			a.mu.Unlock()
		}()
		opts, notes, err := a.buildOptions(req)
		if err != nil {
			a.send(agentproto.Msg{Type: agentproto.TypeFailed, RunID: runID, Error: err.Error()})
			return
		}
		a.log.Info("run starting", "run", runID, "profile", req.Profile, "tests", len(opts.Tests))
		var lastProgress time.Time
		opts.Progress = func(e engine.Event) {
			if e.Type == "progress" {
				if time.Since(lastProgress) < 200*time.Millisecond {
					return
				}
				lastProgress = time.Now()
			}
			e.RunID = runID
			a.send(agentproto.Msg{Type: agentproto.TypeEvent, RunID: runID, Data: mustJSON(e)})
		}
		dctx, dcancel := context.WithTimeout(rctx, 20*time.Second)
		c, err := engine.Dial(dctx, dataAddr, a.id.AgentID, a.id.DataKey)
		dcancel()
		if err != nil {
			a.send(agentproto.Msg{Type: agentproto.TypeFailed, RunID: runID, Error: "cannot reach the server's test endpoint at " + dataAddr + ": " + err.Error()})
			return
		}
		defer c.Close()
		rep, err := engine.RunExclusive(rctx, c, opts)
		if err != nil {
			a.send(agentproto.Msg{Type: agentproto.TypeFailed, RunID: runID, Error: err.Error()})
			return
		}
		rep.Notes = append(rep.Notes, notes...)
		if rctx.Err() != nil && ctx.Err() == nil {
			a.log.Info("run cancelled", "run", runID)
			return
		}
		b, _ := json.Marshal(rep)
		a.send(agentproto.Msg{Type: agentproto.TypeResult, RunID: runID, Data: b})
		a.log.Info("run finished", "run", runID)
	}()
}

// monitorLoop measures latency to the server every interval and reports
// one sample per interval (10 s of probes at 5 per second).
func (a *Agent) monitorLoop(ctx context.Context) {
	var c *engine.Client
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	timer := time.NewTimer(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		a.mu.Lock()
		on, every, busy, addr, online := a.monOn, a.monEvery, a.runCancel != nil, a.dataAddr, a.out != nil
		a.mu.Unlock()
		timer.Reset(every)
		if !on || busy || addr == "" {
			continue
		}
		if c == nil || !c.Alive() {
			dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var err error
			c, err = engine.Dial(dctx, addr, a.id.AgentID, a.id.DataKey)
			cancel()
			if err != nil {
				a.log.Debug("monitor: endpoint unreachable", "err", err)
				a.queueSample(store.Sample{TS: time.Now().Unix(), LossPct: 100, Sent: 1}, online)
				c = nil
				continue
			}
		}
		res, err := engine.RunLatency(ctx, c, engine.LatencyParams{Duration: 10 * time.Second, RateHz: 5})
		if err != nil {
			c.Close()
			c = nil
			continue
		}
		s := store.Sample{TS: time.Now().Unix(), RTTMin: res.UDP.Min, RTTP50: res.UDP.Median, RTTP95: res.UDP.P95, RTTMax: res.UDP.Max,
			Jitter: res.UDPJitterMs, LossPct: stats.Round(res.Loss.LossPct, 3), Sent: int(res.Loss.Sent)}
		if res.UDP.N == 0 {
			s = store.Sample{TS: s.TS, LossPct: 100, Sent: s.Sent}
		}
		a.queueSample(s, online)
	}
}

func (a *Agent) queueSample(s store.Sample, online bool) {
	if online {
		a.send(agentproto.Msg{Type: agentproto.TypeMonitor, Data: mustJSON(agentproto.Monitor{Samples: []store.Sample{s}})})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = append(a.pending, s)
	if len(a.pending) > 1440 {
		a.pending = a.pending[len(a.pending)-1440:]
	}
}
