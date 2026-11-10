package server

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// apiMetrics exposes Prometheus text-format metrics (needs a viewer
// session or API token).
func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b strings.Builder
	agents, _ := s.store.Agents(ctx)
	help := func(name, typ, text string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ) }
	esc := func(v string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(v) }

	help("pathgauge_agent_up", "gauge", "1 if the agent is connected")
	for _, a := range agents {
		if a.Revoked {
			continue
		}
		on, _ := s.hub.Online(a.ID)
		fmt.Fprintf(&b, "pathgauge_agent_up{agent=%q,id=%q} %d\n", esc(a.Name), a.ID, map[bool]int{true: 1, false: 0}[on])
	}
	type gauge struct{ name, help string }
	mons := []gauge{{"pathgauge_monitor_rtt_p50_ms", "Monitoring RTT median"}, {"pathgauge_monitor_rtt_p95_ms", "Monitoring RTT p95"},
		{"pathgauge_monitor_jitter_ms", "Monitoring jitter"}, {"pathgauge_monitor_loss_percent", "Monitoring loss"}}
	for _, g := range mons {
		help(g.name, "gauge", g.help)
		for _, a := range agents {
			x, ok := s.store.LatestSample(ctx, a.ID)
			if !ok || time.Since(time.Unix(x.TS, 0)) > 15*time.Minute || a.Revoked {
				continue
			}
			v := map[string]*float64{"pathgauge_monitor_rtt_p50_ms": x.RTTP50, "pathgauge_monitor_rtt_p95_ms": x.RTTP95, "pathgauge_monitor_jitter_ms": x.Jitter, "pathgauge_monitor_loss_percent": &x.LossPct}[g.name]
			if v == nil {
				continue // no probe was answered: there is no latency to export
			}
			fmt.Fprintf(&b, "%s{agent=%q,id=%q} %g\n", g.name, esc(a.Name), a.ID, *v)
		}
	}
	runGauges := []gauge{{"pathgauge_last_run_health_score", "Health score of the latest run"}, {"pathgauge_last_run_down_mbps", "Download of the latest run"},
		{"pathgauge_last_run_up_mbps", "Upload of the latest run"}, {"pathgauge_last_run_timestamp_seconds", "When the latest run finished"}}
	for _, g := range runGauges {
		help(g.name, "gauge", g.help)
		for _, a := range agents {
			runs, _ := s.store.Runs(ctx, a.ID, "done", 1)
			if len(runs) == 0 || a.Revoked {
				continue
			}
			r := runs[0]
			var v float64
			switch g.name {
			case "pathgauge_last_run_health_score":
				if r.HealthScore != nil {
					v = float64(*r.HealthScore)
				}
			case "pathgauge_last_run_down_mbps":
				if r.DownMbps != nil {
					v = *r.DownMbps
				}
			case "pathgauge_last_run_up_mbps":
				if r.UpMbps != nil {
					v = *r.UpMbps
				}
			case "pathgauge_last_run_timestamp_seconds":
				if t, err := time.Parse(time.RFC3339Nano, r.EndedAt); err == nil {
					v = float64(t.Unix())
				}
			}
			fmt.Fprintf(&b, "%s{agent=%q,id=%q} %g\n", g.name, esc(a.Name), a.ID, v)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// The dashboard is built into ui/dist (see web/) and embedded at compile time.
//
//go:embed all:ui
var uiFS embed.FS

func (s *Server) uiHandler() http.Handler {
	dist, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, 404, "no such API endpoint")
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if f, err := dist.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// Client-side routes get the app shell.
		b, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "dashboard not built (run: make ui)", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
}
