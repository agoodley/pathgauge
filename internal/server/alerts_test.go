package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 120, RTTP95: store.Ms(250)}, {TS: now - 60, RTTP95: store.Ms(50)}})
	s.alerts.evaluate(ctx)
	if ev, _ := s.store.AlertEvents(ctx, 10); len(ev) != 0 {
		t.Fatalf("fired on a non-sustained breach: %+v", ev)
	}

	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 60, RTTP95: store.Ms(220)}, {TS: now - 30, RTTP95: store.Ms(240)}})
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
	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 120, RTTP95: store.Ms(20)}, {TS: now - 60, RTTP95: store.Ms(20)}, {TS: now - 30, RTTP95: store.Ms(20)}})
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
		ss = append(ss, store.Sample{TS: base.Unix() + int64(i*60), RTTP50: store.Ms(float64(10 + i)), RTTP95: store.Ms(float64(20 + i)), LossPct: 1, Sent: 50})
	}
	_ = s.store.AddSamples(ctx, "agt", ss)
	agg, err := s.store.Samples(ctx, "agt", base, base.Add(time.Hour), 300)
	if err != nil || len(agg) != 2 {
		t.Fatalf("5-minute buckets: %v %+v", err, agg)
	}
	if *agg[0].RTTP95 != 24 || *agg[0].RTTP50 != 12 {
		t.Errorf("bucket aggregation (max p95, mean p50): %+v", agg[0])
	}
	cells, _ := s.store.Heatmap(ctx, "agt", base.Add(-time.Hour))
	if len(cells) != 1 || cells[0].Weekday != 1 || cells[0].Hour != 9 || cells[0].N != 10 {
		t.Errorf("heatmap: %+v", cells)
	}
}

// During a total outage no probe is answered, so there is no latency. An
// open latency alert must not resolve the moment the link dies (it used
// to: the outage was stored as RTT 0, the best value in the window).
func TestLatencyAlertSurvivesAnOutage(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.store.CreateAgent(ctx, store.Agent{ID: "agt_1", Name: "branch", DataKey: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SaveAlertRule(ctx, store.AlertRule{Name: "slow", Metric: "rtt_p95", Op: ">", Threshold: 100, ForMinutes: 5, Channel: "log", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SaveAlertRule(ctx, store.AlertRule{Name: "lossy", Metric: "loss_pct", Op: ">", Threshold: 50, ForMinutes: 1, Channel: "log", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{{TS: now - 200, RTTP95: store.Ms(300), Sent: 50}, {TS: now - 140, RTTP95: store.Ms(320), Sent: 50}})
	s.alerts.evaluate(ctx)
	state := func() map[string]string {
		out := map[string]string{}
		ev, _ := s.store.AlertEvents(ctx, 10)
		for _, e := range ev {
			out[e.Message[:4]] = e.State
		}
		return out
	}
	if got := state(); got["slow"] != "firing" {
		t.Fatalf("the latency alert should be firing before the outage: %v", got)
	}
	// The link dies. One sample is what an agent before this fix sent
	// (zeros), the other what it sends now (no latency fields).
	_ = s.store.AddSamples(ctx, "agt_1", []store.Sample{
		{TS: now - 50, RTTP95: store.Ms(0), RTTP50: store.Ms(0), Jitter: store.Ms(0), LossPct: 100, Sent: 50},
		{TS: now - 20, LossPct: 100, Sent: 50},
	})
	s.alerts.evaluate(ctx)
	got := state()
	if got["slow"] != "firing" {
		t.Errorf("the latency alert resolved when the link died: %v", got)
	}
	if got["loss"] != "firing" {
		t.Errorf("the loss alert should fire for the outage: %v", got)
	}
}

func TestOutageSamplesHaveNoLatency(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.store.CreateAgent(ctx, store.Agent{ID: "agt", Name: "x", DataKey: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-30 * time.Minute).Truncate(10 * time.Minute)
	ts := func(min int) int64 { return base.Unix() + int64(min*60) }
	_ = s.store.AddSamples(ctx, "agt", []store.Sample{
		// Bucket 1: two good intervals and an outage.
		{TS: ts(0), RTTMin: store.Ms(9), RTTP50: store.Ms(10), RTTP95: store.Ms(20), RTTMax: store.Ms(25), Jitter: store.Ms(2), LossPct: 0, Sent: 50},
		{TS: ts(1), RTTMin: store.Ms(11), RTTP50: store.Ms(14), RTTP95: store.Ms(30), RTTMax: store.Ms(40), Jitter: store.Ms(4), LossPct: 0, Sent: 50},
		{TS: ts(2), RTTMin: store.Ms(0), RTTP50: store.Ms(0), RTTP95: store.Ms(0), RTTMax: store.Ms(0), Jitter: store.Ms(0), LossPct: 100, Sent: 50},
		// Bucket 2: outage only.
		{TS: ts(10), LossPct: 100, Sent: 50},
		{TS: ts(11), LossPct: 100, Sent: 1},
	})
	agg, err := s.store.Samples(ctx, "agt", base, base.Add(20*time.Minute), 600)
	if err != nil || len(agg) != 2 {
		t.Fatalf("want two buckets: %v %+v", err, agg)
	}
	a := agg[0]
	if a.RTTMin == nil || *a.RTTMin != 9 || *a.RTTP50 != 12 || *a.RTTP95 != 30 || *a.RTTMax != 40 || *a.Jitter != 3 {
		t.Errorf("the outage must not pull the bucket's latency towards zero: min %v p50 %v p95 %v max %v jitter %v",
			deref(a.RTTMin), deref(a.RTTP50), deref(a.RTTP95), deref(a.RTTMax), deref(a.Jitter))
	}
	if a.LossPct < 33 || a.LossPct > 34 {
		t.Errorf("bucket loss %.2f%%, want a third", a.LossPct)
	}
	b := agg[1]
	if b.RTTMin != nil || b.RTTP50 != nil || b.RTTP95 != nil || b.RTTMax != nil || b.Jitter != nil {
		t.Errorf("a bucket with no answers has no latency: %+v", b)
	}
	if b.LossPct != 100 {
		t.Errorf("outage bucket loss %.2f%%, want 100", b.LossPct)
	}
	last, ok := s.store.LatestSample(ctx, "agt")
	if !ok || last.RTTP95 != nil || last.LossPct != 100 {
		t.Errorf("latest sample during an outage: %+v", last)
	}
	js, _ := json.Marshal(b)
	if want := `"rtt_p95":null`; !strings.Contains(string(js), want) {
		t.Errorf("the API should send %s, got %s", want, js)
	}
	cells, err := s.store.Heatmap(ctx, "agt", base.Add(-time.Hour))
	if err != nil || len(cells) == 0 {
		t.Fatalf("heatmap: %v %+v", err, cells)
	}
	for _, c := range cells {
		if c.RTTP95 != nil && *c.RTTP95 < 20 {
			t.Errorf("heatmap average %.1f ms includes the outage's zeros", *c.RTTP95)
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
