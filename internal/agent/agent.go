// Package agent implements the agent role: enrollment, the mTLS control
// connection to the server, running tests against the server's endpoint,
// continuous monitoring, and local policy enforcement.
package agent

import (
	"bytes"
	"cmp"
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
	"net/http/httptrace"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/engine"
	"github.com/agoodley/pathgauge/internal/enrich"
	"github.com/agoodley/pathgauge/internal/guard"
	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/pki"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/shaping"
	"github.com/agoodley/pathgauge/internal/stats"
	"github.com/agoodley/pathgauge/internal/store"
	"github.com/agoodley/pathgauge/internal/tlsprofile"
	"github.com/agoodley/pathgauge/internal/version"
)

// Identity is the agent's enrolled identity (stored in DATA_DIR/agent).
type Identity struct {
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	ServerURL string `json:"server_url"`
	DataKey   []byte `json:"data_key"`
	// TLSProfile is the TLS profile enrolment found to work (empty: the
	// default); the agent service opens its connections the same way.
	TLSProfile string `json:"tls_profile,omitempty"`
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

// JoinOptions controls enrolment.
type JoinOptions struct {
	URL, Token, Name, DataDir string
	// TLSProfile is a tlsprofile name, or "auto" (or empty) to try each
	// profile in turn and keep the first that gets an answer.
	TLSProfile string
	// Debug, if set, receives a step-by-step account of the connection.
	Debug io.Writer
	// HandshakeTimeout is how long to wait for an answer to each TLS hello
	// (default 8 s).
	HandshakeTimeout time.Duration
}

// Join enrolls with a server using a join token.
func Join(ctx context.Context, serverURL, token, name, dataDir string) (*Identity, error) {
	return JoinWith(ctx, JoinOptions{URL: serverURL, Token: token, Name: name, DataDir: dataDir})
}

// JoinWith enrolls with a server. Before sending anything it checks which
// TLS profile the path lets through, so a firewall that silently drops one
// kind of TLS hello doesn't leave the join hanging, and every failure says
// at which stage it stopped.
func JoinWith(ctx context.Context, o JoinOptions) (*Identity, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the server URL must look like https://server:7443")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(config.DefaultControlPort))
	}
	caPrefix, _, err := pki.ParseJoinToken(o.Token)
	if err != nil {
		return nil, err
	}
	name := o.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	// The HTTP transport reports progress from its own goroutines.
	var dmu sync.Mutex
	debugf := func(format string, a ...any) {
		if o.Debug != nil {
			dmu.Lock()
			defer dmu.Unlock()
			fmt.Fprintf(o.Debug, format+"\n", a...)
		}
	}
	profiles := []string{o.TLSProfile}
	auto := o.TLSProfile == "" || o.TLSProfile == tlsprofile.Auto
	if auto {
		profiles = tlsprofile.Names()
	} else if _, err := tlsprofile.Get(o.TLSProfile); err != nil {
		return nil, err
	}
	base := &tls.Config{
		InsecureSkipVerify:     true, //nolint:gosec // verified by VerifyConnection against the CA pinned by the token
		VerifyConnection:       pki.PinnedVerifier(caPrefix),
		SessionTicketsDisabled: true,
		MinVersion:             tls.VersionTLS12,
	}
	// Find a TLS profile that gets an answer (no secrets are sent yet).
	chosen := ""
	var stalled []string
	for _, p := range profiles {
		r, conn := tlsprofile.Probe(ctx, u.Host, p, base, cmp.Or(o.HandshakeTimeout, 8*time.Second))
		if conn != nil {
			_ = conn.Close()
		}
		if r.Stage == tlsprofile.StageConnect {
			debugf("TCP connect to %s: %v", u.Host, r.Err)
			return nil, fmt.Errorf("could not connect to %s: %w (check the address, that the server is running, and firewalls for TCP port %s)", u.Host, r.Err, u.Port())
		}
		debugf("TCP connect to %s: ok in %.1f ms; TLS profile %q: %s", u.Host, float64(r.Connect.Microseconds())/1000, p, r.Summary())
		if r.OK() {
			chosen = p
			break
		}
		if !r.Stalled {
			// A real TLS failure (for example a certificate that doesn't
			// match the token's CA pin): another profile won't help.
			return nil, fmt.Errorf("TLS with %s failed: %w", u.Host, r.Err)
		}
		stalled = append(stalled, p)
	}
	if chosen == "" {
		return nil, fmt.Errorf("connected to %s, but the server never answered the TLS hello (TLS profiles tried: %s). "+
			"Something on the path, such as a firewall, IPS or endpoint-security product, is probably dropping it. "+
			"Run 'pathgauge agent diagnose %s' for details; peer mode (pathgauge endpoint / pathgauge test) doesn't use TLS and may still work",
			u.Host, strings.Join(stalled, ", "), u.String())
	}
	if len(stalled) > 0 {
		debugf("using TLS profile %q (the server never answered %s)", chosen, strings.Join(stalled, ", "))
	}
	cfg := base.Clone()
	_ = tlsprofile.Apply(chosen, cfg)

	keyPEM, csrPEM, err := pki.NewKeyAndCSR(name)
	if err != nil {
		return nil, err
	}
	host := hostinfo.Collect(hostinfo.SystemSources())
	hostJSON, _ := json.Marshal(host)
	body, _ := json.Marshal(agentproto.EnrollRequest{Token: o.Token, Name: name, CSR: string(csrPEM), Version: version.Get().Version, Host: hostJSON})
	var wrote, answered atomic.Bool
	start := time.Now()
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			wrote.Store(true)
			debugf("enrolment request sent (%d bytes) after %.1f ms", len(body), float64(time.Since(start).Microseconds())/1000)
		},
		GotFirstResponseByte: func() {
			answered.Store(true)
			debugf("server answered after %.1f ms", float64(time.Since(start).Microseconds())/1000)
		},
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, u.String()+"/api/agent/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case !wrote.Load():
			return nil, fmt.Errorf("enroll with %s: sending the request failed: %w", u.Host, err)
		case !answered.Load():
			return nil, fmt.Errorf("enroll with %s: the request was sent but the server never answered (%w). "+
				"Check the server's log (journalctl -u pathgauge-server); if the server is fine, run 'pathgauge agent diagnose %s'", u.Host, err, u.String())
		}
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
	dir := identityDir(o.DataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := &Identity{AgentID: er.AgentID, Name: name, ServerURL: u.String(), DataKey: er.DataKey}
	if chosen != "default" {
		id.TLSProfile = chosen
	}
	ib, _ := json.MarshalIndent(id, "", "  ")
	for name, data := range map[string][]byte{"identity.json": ib, "agent.key": keyPEM, "agent.crt": []byte(er.Cert), "ca.crt": []byte(er.CA)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	chownToService(o.DataDir, dir)
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
	// Open connections the way enrolment found works on this path.
	if err := tlsprofile.Apply(id.TLSProfile, a.tlsCfg); err != nil {
		return nil, err
	}
	if policy.Shaping != config.ShapingNone {
		a.shaper = shaping.NewTuner(log, "")
	}
	return a, nil
}

// Run connects to the server and serves until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("agent starting", "name", a.id.Name, "id", a.id.AgentID, "server", a.id.ServerURL, "version", version.Get().Version)
	go guard.Loop(ctx, a.log, "monitoring", a.monitorLoop)
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
		GroupDurationS: req.GroupDurationS, NoBaseline: req.NoBaseline,
		Labels: results.LinkLabels{Client: req.ClientName, Endpoint: req.ServerName}}
	if req.CDP != nil {
		o.CDP = *req.CDP
	}
	tests, err := o.TestList()
	if err != nil {
		return o, nil, err
	}
	var notes, allowed []string
	for _, t := range tests {
		if engine.IsGroup(t) {
			// A group runs only if the policy allows every test in it.
			ok := true
			for _, m := range engine.GroupMembers(t) {
				if !a.policy.AllowsTest(m) {
					notes = append(notes, "skipped group "+t+" ("+m+" is not allowed by the agent's local policy)")
					ok = false
					break
				}
			}
			if ok {
				allowed = append(allowed, t)
			}
			continue
		}
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
		// A bug in one run fails that run; the agent stays up for the next.
		defer func() {
			if guard.Recovered(a.log, "run "+runID, recover()) {
				a.send(agentproto.Msg{Type: agentproto.TypeFailed, RunID: runID, Error: "internal error in the agent (see its log)"})
			}
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
