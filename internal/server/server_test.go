package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/agent"
	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/results"
)

type apiClient struct {
	t    *testing.T
	base string
	c    *http.Client
}

func (a *apiClient) do(method, path string, body any, out any) int {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, r)
	req.Header.Set("X-PathGauge", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s %s: decode %v: %s", method, path, err, b)
		}
	}
	if resp.StatusCode >= 300 {
		a.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

func TestServerAgentEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.Listen, cfg.DataDir = "127.0.0.1:0", dir
	cfg.DataPlane.Listen = "127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := Open(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	<-srv.Ready()
	base := "https://" + srv.Addr().String()

	jar, _ := cookiejar.New(nil)
	api := &apiClient{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: srv.ca.Pool(), ServerName: "localhost"}}}}

	// Unauthenticated access is refused; setup creates the first admin.
	if code := api.do("GET", "/api/agents", nil, nil); code != 401 {
		t.Fatalf("unauthenticated agents list: %d", code)
	}
	var st map[string]any
	api.do("GET", "/api/setup", nil, &st)
	if st["needs_setup"] != true {
		t.Fatal("expected setup to be needed")
	}
	if code := api.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "short"}, nil); code != 400 {
		t.Fatalf("weak password accepted: %d", code)
	}
	if code := api.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "correct horse battery"}, nil); code != 200 {
		t.Fatal("setup failed")
	}
	if code := api.do("POST", "/api/setup", map[string]string{"username": "evil", "password": "correct horse battery"}, nil); code != 409 {
		t.Fatalf("second setup allowed: %d", code)
	}
	// CSRF header required for cookie sessions.
	req, _ := http.NewRequest("POST", base+"/api/enroll-tokens", strings.NewReader("{}"))
	resp, err := api.c.Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("missing X-PathGauge should be refused, got %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	var tok struct{ Token string }
	api.do("POST", "/api/enroll-tokens", map[string]any{"hours": 1, "note": "test"}, &tok)
	if !strings.HasPrefix(tok.Token, "pgjoin_") {
		t.Fatalf("token %q", tok.Token)
	}

	// A wrong CA pin is refused before any secret is sent.
	agentDir := t.TempDir()
	bad := "pgjoin_0000000000000000_" + strings.Split(tok.Token, "_")[2]
	if _, err := agent.Join(ctx, base, bad, "evil", agentDir); err == nil {
		t.Fatal("join with a wrong CA pin succeeded")
	}
	id, err := agent.Join(ctx, base, tok.Token, "test-agent", agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Join(ctx, base, tok.Token, "again", t.TempDir()); err == nil {
		t.Fatal("join token reused")
	}
	acfg := config.DefaultAgent()
	acfg.DataDir = agentDir
	ag, err := agent.New(acfg, config.DefaultPolicy(), log)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ag.Run(ctx) }()

	// Wait for the agent to connect.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var agents []map[string]any
		api.do("GET", "/api/agents", nil, &agents)
		if len(agents) == 1 && agents[0]["online"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never came online: %v", agents)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Run a short custom profile through the API.
	var started struct {
		RunID string `json:"run_id"`
	}
	if code := api.do("POST", "/api/runs", map[string]any{"agent_id": id.AgentID, "profile": "quick", "tests": []string{"info", "latency", "tcp-down", "tcp-up"}, "scale": 0.2}, &started); code != 200 {
		t.Fatal("start run failed")
	}
	if code := api.do("POST", "/api/runs", map[string]any{"agent_id": id.AgentID, "profile": "quick"}, nil); code != 409 {
		t.Fatalf("second concurrent run should be refused, got %d", code)
	}
	var run struct {
		Status      string          `json:"status"`
		Error       string          `json:"error"`
		HealthScore *int            `json:"health_score"`
		DownMbps    *float64        `json:"down_mbps"`
		Report      json.RawMessage `json:"report"`
	}
	deadline = time.Now().Add(90 * time.Second)
	for {
		api.do("GET", "/api/runs/"+started.RunID, nil, &run)
		if run.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if run.Status != "done" {
		t.Fatalf("run %s: %s", run.Status, run.Error)
	}
	var rep results.Report
	if err := json.Unmarshal(run.Report, &rep); err != nil {
		t.Fatal(err)
	}
	if run.HealthScore == nil || run.DownMbps == nil || *run.DownMbps < 50 || len(rep.Steps) != 4 || len(rep.Findings) == 0 {
		t.Fatalf("unexpected run: score %v down %v steps %d findings %d", run.HealthScore, run.DownMbps, len(rep.Steps), len(rep.Findings))
	}
	for _, s := range rep.Steps {
		if s.Status != "ok" {
			t.Errorf("step %s: %s %s", s.ID, s.Status, s.Error)
		}
	}
	if rep.Client.Name != "test-agent" {
		t.Errorf("client label %q", rep.Client.Name)
	}

	// HTML report, share link, compare, overview, metrics.
	req, _ = http.NewRequest("GET", base+"/api/runs/"+started.RunID+"/report.html", nil)
	resp, err = api.c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("report: %v %v", resp.StatusCode, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("test-agent")) || !bytes.Contains(body, []byte("<svg")) {
		t.Error("report HTML lacks expected content")
	}
	var share struct{ URL string }
	api.do("POST", "/api/runs/"+started.RunID+"/share", map[string]any{"hours": 1}, &share)
	anon := &http.Client{Transport: api.c.Transport}
	resp, err = anon.Get(share.URL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("share link: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	var ov map[string]any
	api.do("GET", "/api/overview", nil, &ov)
	if links, _ := ov["links"].([]any); len(links) != 1 {
		t.Errorf("overview links: %v", ov["links"])
	}
	req, _ = http.NewRequest("GET", base+"/metrics", nil)
	resp, _ = api.c.Do(req)
	mb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(mb, []byte(`pathgauge_agent_up{agent="test-agent"`)) || !bytes.Contains(mb, []byte("pathgauge_last_run_health_score")) {
		t.Errorf("metrics missing series:\n%s", mb)
	}

	// Alert rule validation and schedules.
	if code := api.do("POST", "/api/alert-rules", map[string]any{"name": "x", "metric": "nope", "op": ">", "threshold": 1, "channel": "log"}, nil); code != 400 {
		t.Errorf("invalid metric accepted: %d", code)
	}
	if code := api.do("POST", "/api/alert-rules", map[string]any{"name": "low score", "metric": "health_score", "op": "<", "threshold": 101, "channel": "log", "enabled": true}, nil); code != 200 {
		t.Error("valid rule refused")
	}
	if code := api.do("POST", "/api/schedules", map[string]any{"agent_id": id.AgentID, "profile": "quick", "cron": "not cron", "enabled": true}, nil); code != 400 {
		t.Errorf("bad cron accepted: %d", code)
	}
	if code := api.do("POST", "/api/schedules", map[string]any{"agent_id": id.AgentID, "profile": "quick", "cron": "0 3 * * *", "enabled": true}, nil); code != 200 {
		t.Error("valid schedule refused")
	}

	// Revoking the agent disconnects it and blocks the data plane.
	if code := api.do("POST", "/api/agents/"+id.AgentID+"/revoke", nil, nil); code != 200 {
		t.Fatal("revoke failed")
	}
	if _, ok := srv.Lookup(id.AgentID); ok {
		t.Error("revoked agent still has a data-plane key")
	}
	fmt.Fprint(io.Discard, rep.Summary)
}
