package shaping

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agoodley/pathgauge/internal/engine"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// Tuner is the SQM tuner: it shapes this host's interface at decreasing
// fractions of the measured capacity and measures bufferbloat at each,
// to find the best rate for smart queue management (docs/PLAN.md §8.4).
type Tuner struct {
	m       *Manager
	iface   string
	Percent []float64
}

// NewTuner creates a tuner shaping iface (empty: the route to the endpoint).
func NewTuner(log *slog.Logger, iface string) *Tuner {
	return &Tuner{m: NewManager(log), iface: iface, Percent: []float64{95, 90, 85, 80, 70}}
}

// Run implements engine.SQMHooks.
func (t *Tuner) Run(ctx context.Context, c *engine.Client, down, up float64, progress func(engine.Event)) (*results.SQMTuner, error) {
	defer func() { _ = t.m.Clear(context.Background()) }()
	if progress == nil {
		progress = func(engine.Event) {}
	}
	note := func(msg string, kv ...any) {
		progress(engine.Event{Type: "progress", Data: map[string]any{"kind": "sqm", "message": fmt.Sprintf(msg, kv...)}})
	}
	iface := t.iface
	if iface == "" {
		var err error
		if iface, err = t.m.RouteInterface(ctx, c.EndpointIP.String()); err != nil {
			return nil, err
		}
	}
	res := &results.SQMTuner{Interface: iface}
	if down <= 0 || up <= 0 {
		note("measuring capacity")
		d, err := engine.RunTCP(ctx, c, engine.TCPParams{Direction: results.Down, Streams: 4, Duration: 6 * time.Second, Omit: time.Second})
		if err != nil {
			return nil, err
		}
		u, err := engine.RunTCP(ctx, c, engine.TCPParams{Direction: results.Up, Streams: 4, Duration: 6 * time.Second, Omit: time.Second})
		if err != nil {
			return nil, err
		}
		down, up = d.Mbps, u.Mbps
	}
	res.BaselineDown, res.BaselineUp = stats.Round(down, 2), stats.Round(up, 2)
	lite := engine.BloatParams{Idle: 3 * time.Second, Phase: 6 * time.Second, Streams: 4, Phases: []string{"down", "up"}}
	note("baseline bufferbloat")
	base, err := engine.RunBufferbloat(ctx, c, lite)
	if err != nil {
		return nil, err
	}
	res.BaselineGrade = base.Grade
	for _, pct := range t.Percent {
		if ctx.Err() != nil {
			break
		}
		st := results.SQMStep{Percent: pct, DownMbps: stats.Round(down*pct/100, 2), UpMbps: stats.Round(up*pct/100, 2)}
		note("shaping to %.0f%%", pct)
		ap, err := t.m.ApplySpec(ctx, Spec{Interface: iface, Qdisc: "auto", EgressRateMbps: st.UpMbps, IngressRateMbps: st.DownMbps, LeaseSeconds: 120})
		if err != nil {
			st.Error = err.Error()
			res.Steps = append(res.Steps, st)
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "Operation not permitted") || strings.Contains(err.Error(), "refusing") || strings.Contains(err.Error(), "will not replace") {
				res.Note = "shaping could not be applied: " + err.Error()
				break
			}
			continue
		}
		st.Qdisc = ap.Qdisc
		b, err := engine.RunBufferbloat(ctx, c, lite)
		if cerr := t.m.Clear(ctx); cerr != nil {
			// Nothing measured from here on would mean anything, and the
			// manager keeps retrying the removal by itself.
			st.Error = cerr.Error()
			res.Steps = append(res.Steps, st)
			res.Note = "stopped: " + cerr.Error()
			break
		}
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Grade = b.Grade
			st.IncreaseMs = stats.Round(maxf(b.IncreaseMs["down"], b.IncreaseMs["up"]), 1)
			for _, ph := range b.Phases {
				if ph.Name == "down" {
					st.DownMeasured = ph.DownMbps
				}
				if ph.Name == "up" {
					st.UpMeasured = ph.UpMbps
				}
			}
		}
		res.Steps = append(res.Steps, st)
		sleepCtx(ctx, time.Second)
	}
	// Recommend the highest rate reaching grade A or better; otherwise the
	// lowest latency increase.
	var best *results.SQMStep
	for i := range res.Steps {
		s := &res.Steps[i]
		if s.Error != "" || s.Grade == "" {
			continue
		}
		if stats.GradeRank(s.Grade) <= 1 {
			if best == nil || stats.GradeRank(best.Grade) > 1 || s.Percent > best.Percent {
				best = s
			}
		} else if best == nil || (stats.GradeRank(best.Grade) > 1 && s.IncreaseMs < best.IncreaseMs) {
			best = s
		}
	}
	if best != nil {
		rec := *best
		res.Recommended = &rec
		res.TCCommands = permanentCommands(iface, rec)
	}
	return res, nil
}

func permanentCommands(iface string, s results.SQMStep) []string {
	if s.Qdisc == "cake" {
		return []string{
			fmt.Sprintf("tc qdisc replace dev %s root cake bandwidth %.1fmbit", iface, s.UpMbps),
			"ip link add ifb0 type ifb && ip link set ifb0 up",
			fmt.Sprintf("tc qdisc replace dev %s handle ffff: ingress", iface),
			fmt.Sprintf("tc filter replace dev %s parent ffff: matchall action mirred egress redirect dev ifb0", iface),
			fmt.Sprintf("tc qdisc replace dev ifb0 root cake bandwidth %.1fmbit ingress", s.DownMbps),
		}
	}
	return []string{
		fmt.Sprintf("tc qdisc replace dev %s root handle 1: htb default 1 && tc class replace dev %s parent 1: classid 1:1 htb rate %.1fmbit && tc qdisc replace dev %s parent 1:1 fq_codel", iface, iface, s.UpMbps, iface),
		fmt.Sprintf("(ingress: mirror %s to an ifb device and apply the same at %.1fmbit; or set SQM on your router to %.1f down / %.1f up Mbit/s)", iface, s.DownMbps, s.DownMbps, s.UpMbps),
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
