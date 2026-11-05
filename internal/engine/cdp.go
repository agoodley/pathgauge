package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// CDPDefaults fills unset CDP settings.
func CDPDefaults(s results.CDPSettings) results.CDPSettings {
	m := flow.CDPModel{RateBps: s.RateMbps * 1e6, BurstFactor: s.BurstFactor, BurstS: s.BurstS, PeriodS: s.PeriodS}.WithDefaults()
	s.BurstFactor, s.BurstS, s.PeriodS = m.BurstFactor, m.BurstS, m.PeriodS
	if s.Streams <= 0 {
		s.Streams = 4
	}
	if s.RPOs <= 0 {
		s.RPOs = 15
	}
	if s.DurationS <= 0 {
		s.DurationS = 60
	}
	return s
}

// CDPParams controls a CDP replication test.
type CDPParams struct {
	Direction  results.Direction // up: this side is the CDP source
	Settings   results.CDPSettings
	OnInterval func(dir results.Direction, t, mbps float64)
}

// RunCDP replicates simulated changed data across the path and measures
// the lag (the RPO the replica would have) and its effect on latency.
func RunCDP(ctx context.Context, c *Client, p CDPParams) (*results.CDP, error) {
	set := CDPDefaults(p.Settings)
	if set.RateMbps <= 0 {
		return nil, errors.New("CDP: set the change rate (Mbit/s on the wire)")
	}
	model := flow.CDPModel{RateBps: set.RateMbps * 1e6, BurstFactor: set.BurstFactor, BurstS: set.BurstS, PeriodS: set.PeriodS}
	base, peak := model.Rates()
	res := &results.CDP{Direction: p.Direction, Settings: set, BaseMbps: stats.Round(base*8/1e6, 2), PeakMbps: stats.Round(peak*8/1e6, 2)}

	// Latency before and during, from probes on the main socket.
	idle := newProber(c, 0)
	ictx, icancel := context.WithTimeout(ctx, 2*time.Second)
	idle.run(ictx, 10)
	icancel()
	idle.close()
	res.IdleRTT = roundSum(stats.Summarize(gotRTTs(idle)))

	dur := time.Duration(set.DurationS * float64(time.Second))
	loaded := newProber(c, 0)
	defer loaded.close()
	lctx, lcancel := context.WithCancel(ctx)
	probed := make(chan struct{})
	go func() { loaded.run(lctx, 5); close(probed) }()
	r, err := startTCP(ctx, c, TCPParams{Direction: p.Direction, Streams: set.Streams, Duration: dur,
		CDP: &model, CDPLimitBps: set.LimitMbps * 1e6, OnInterval: p.OnInterval})
	if err != nil {
		lcancel()
		<-probed
		return nil, err
	}
	tr, err := r.wait(ctx)
	lcancel()
	<-probed
	if err != nil {
		return nil, err
	}
	res.LoadedRTT = roundSum(stats.Summarize(gotRTTs(loaded)))
	if r.cdp == nil {
		return nil, errors.New("the endpoint's PathGauge is too old for the CDP test; upgrade it")
	}
	cr := r.cdp
	res.RetransPct = stats.Round(tr.RetransPct, 3)
	res.GeneratedMB = stats.Round(float64(cr.Generated)/1e6, 1)
	res.SentMB = stats.Round(float64(cr.Sent)/1e6, 1)
	if cr.Seconds > 0 {
		res.ThroughputMbps = stats.Round(float64(cr.Sent)*8/cr.Seconds/1e6, 2)
	}
	for _, pt := range cr.Points {
		res.Points = append(res.Points, results.CDPPoint(pt))
	}
	evalCDP(res, cr.Lag)
	if p.Direction == results.Down {
		res.Notes = append(res.Notes, "The other end was the CDP source (replication towards this host).")
	}
	return res, nil
}

// evalCDP turns the lag samples (every 100 ms) into the summary.
func evalCDP(res *results.CDP, lag []float64) {
	rpo := res.Settings.RPOs
	if len(lag) == 0 {
		res.Verdict = "poor"
		return
	}
	s := stats.Summarize(lag)
	res.MaxLagS, res.P95LagS, res.MeanLagS = stats.Round(s.Max, 2), stats.Round(s.P95, 2), stats.Round(s.Mean, 2)
	res.EndLagS = stats.Round(lag[len(lag)-1], 2)
	within := 0
	for _, l := range lag {
		if l <= rpo {
			within++
		}
	}
	res.WithinRPOPct = stats.Round(100*float64(within)/float64(len(lag)), 1)
	res.OverRPOS = stats.Round(float64(len(lag)-within)*0.1, 1)
	// What the path carried while changes were waiting.
	var busy []float64
	for _, pt := range res.Points {
		if pt.LagS > 0.5 {
			busy = append(busy, pt.SentMbps)
		}
	}
	if len(busy) > 0 {
		res.Backlogged = true
		sort.Float64s(busy)
		res.SustainedMbps = stats.Round(busy[len(busy)/2], 2)
	}
	switch {
	case res.MaxLagS <= rpo/2 && res.EndLagS <= rpo/2:
		res.Verdict = "good"
	case res.MaxLagS <= rpo:
		res.Verdict = "acceptable"
	default:
		res.Verdict = "poor"
	}
	if res.EndLagS > rpo && res.EndLagS >= res.MaxLagS*0.9 {
		res.Notes = append(res.Notes, fmt.Sprintf("The lag was still growing when the test ended (%.1f s): the path can't keep up with this change rate.", res.EndLagS))
	}
	if res.Settings.LimitMbps > 0 && res.Backlogged && res.SustainedMbps >= res.Settings.LimitMbps*0.9 {
		res.Notes = append(res.Notes, fmt.Sprintf("The %.0f Mbit/s throttle limited replication; the path itself may carry more.", res.Settings.LimitMbps))
	}
}

func gotRTTs(p *prober) []float64 {
	var out []float64
	for _, s := range p.samples() {
		if s.got {
			out = append(out, s.rttMs)
		}
	}
	return out
}

// cdpPeakBps is the highest rate a CDP test will offer (bit/s).
func cdpPeakBps(s results.CDPSettings) float64 {
	set := CDPDefaults(s)
	_, peak := flow.CDPModel{RateBps: set.RateMbps * 1e6, BurstFactor: set.BurstFactor, BurstS: set.BurstS, PeriodS: set.PeriodS}.Rates()
	b := peak * 8
	if set.LimitMbps > 0 {
		b = math.Min(b, set.LimitMbps*1e6)
	}
	return b
}
