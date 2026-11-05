package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.DataDir = t.TempDir()
	s, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type hook struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	h.mu.Lock()
	h.msgs = append(h.msgs, m)
	h.mu.Unlock()
}

func (h *hook) wait(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		if len(h.msgs) >= n {
			out := append([]map[string]any(nil), h.msgs...)
			h.mu.Unlock()
			return out
		}
		h.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook received fewer than %d messages", n)
	return nil
}

func TestAlertFiresAndResolves(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	h := &hook{}
	ts := httptest.NewServer(h)
	defer ts.Close()

	if err := s.store.CreateAgent(ctx, store.Agent{ID: "agt_1", Name: "branch", DataKey: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SaveAlertRule(ctx, store.AlertRule{Name: "slow", Metric: "rtt_p95", Op: ">", Threshold: 100, ForMinutes: 5, Channel: "webhook", Target: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	// One healthy sample in the window: the condition must be sustained.
	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 120, RTTP95: 250}, {TS: now - 60, RTTP95: 50}})
	s.alerts.evaluate(ctx)
	if ev, _ := s.store.AlertEvents(ctx, 10); len(ev) != 0 {
		t.Fatalf("fired on a non-sustained breach: %+v", ev)
	}

	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 60, RTTP95: 220}, {TS: now - 30, RTTP95: 240}})
	s.alerts.evaluate(ctx)
	s.alerts.evaluate(ctx) // must not fire twice
	ev, _ := s.store.AlertEvents(ctx, 10)
	if len(ev) != 1 || ev[0].State != "firing" || ev[0].Value != 220 {
		t.Fatalf("expected one firing event at the least-breaching value 220: %+v", ev)
	}
	msgs := h.wait(t, 1)
	if msgs[0]["state"] != "firing" || msgs[0]["text"] == "" {
		t.Errorf("webhook payload %+v", msgs[0])
	}

	// Recovery resolves the event and notifies again.
	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 120, RTTP95: 20}, {TS: now - 60, RTTP95: 20}, {TS: now - 30, RTTP95: 20}})
	s.alerts.evaluate(ctx)
	ev, _ = s.store.AlertEvents(ctx, 10)
	if ev[0].State != "resolved" {
		t.Fatalf("expected resolved: %+v", ev)
	}
	msgs = h.wait(t, 2)
	if msgs[1]["state"] != "resolved" {
		t.Errorf("resolution payload %+v", msgs[1])
	}
}

func TestRunBasedAlert(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	_ = s.store.CreateAgent(ctx, store.Agent{ID: "agt_2", Name: "site", DataKey: []byte("k")})
	_, _ = s.store.SaveAlertRule(ctx, store.AlertRule{Name: "bad health", Metric: "health_score", Op: "<", Threshold: 70, Channel: "log", Enabled: true})
	s.alerts.onRun(ctx, "agt_2", &results.Report{Summary: results.Summary{HealthScore: 40}})
	ev, _ := s.store.AlertEvents(ctx, 10)
	if len(ev) != 1 || ev[0].State != "firing" {
		t.Fatalf("health alert: %+v", ev)
	}
	s.alerts.onRun(ctx, "agt_2", &results.Report{Summary: results.Summary{HealthScore: 95}})
	ev, _ = s.store.AlertEvents(ctx, 10)
	if ev[0].State != "resolved" {
		t.Fatalf("health alert should resolve: %+v", ev)
	}
}

func TestOfflineAlert(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	_ = s.store.CreateAgent(ctx, store.Agent{ID: "agt_3", Name: "gone", DataKey: []byte("k")})
	_, _ = s.store.DB.ExecContext(ctx, `UPDATE agents SET last_seen=? WHERE id='agt_3'`, store.Ts(time.Now().Add(-30*time.Minute)))
	_, _ = s.store.SaveAlertRule(ctx, store.AlertRule{Name: "offline", Metric: "offline", Op: ">", Threshold: 10, Channel: "log", Enabled: true})
	s.alerts.evaluate(ctx)
	ev, _ := s.store.AlertEvents(ctx, 10)
	if len(ev) != 1 || ev[0].Value < 29 {
		t.Fatalf("offline alert: %+v", ev)
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "wrong password!") || CheckPassword("garbage", "x") {
		t.Error("password check wrong")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Error("short password accepted")
	}
}

func TestMonitorRollupAndHeatmap(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) // a Monday, 09:00 UTC
	var ss []store.Sample
	for i := 0; i < 10; i++ {
		ss = append(ss, store.Sample{TS: base.Unix() + int64(i*60), RTTP50: float64(10 + i), RTTP95: float64(20 + i), LossPct: 1, Sent: 50})
	}
	_ = s.store.AddSamples(ctx, "agt", ss)
	agg, err := s.store.Samples(ctx, "agt", base, base.Add(time.Hour), 300)
	if err != nil || len(agg) != 2 {
		t.Fatalf("5-minute buckets: %v %+v", err, agg)
	}
	if agg[0].RTTP95 != 24 || agg[0].RTTP50 != 12 {
		t.Errorf("bucket aggregation (max p95, mean p50): %+v", agg[0])
	}
	cells, _ := s.store.Heatmap(ctx, "agt", base.Add(-time.Hour))
	if len(cells) != 1 || cells[0].Weekday != 1 || cells[0].Hour != 9 || cells[0].N != 10 {
		t.Errorf("heatmap: %+v", cells)
	}
}
