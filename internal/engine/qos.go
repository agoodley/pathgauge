package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// RunQoS measures whether DSCP markings get priority under load: probes
// marked EF (voice), AF41 (video) and best effort run side by side, first
// idle, then while bulk best-effort TCP fills both directions. If EF's
// latency rises much less than best effort's, priority queuing works on
// the path.
func RunQoS(ctx context.Context, c *Client, idle, load time.Duration, streams int) (*results.QoS, error) {
	classes := []struct {
		name string
		dscp int
	}{{"EF (voice)", 46}, {"AF41 (video)", 34}, {"Best effort", 0}}
	type phaseSamples struct{ idle, loaded []*probeSample }
	probers := make([]*prober, len(classes))
	samples := make([]phaseSamples, len(classes))
	for i, cl := range classes {
		probers[i] = newProber(c, 0)
		probers[i].tos = cl.dscp << 2
		defer probers[i].close()
	}
	runProbes := func(d time.Duration) [][]*probeSample {
		pctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		start := make([]int, len(probers))
		for i, p := range probers {
			start[i] = len(p.samples())
		}
		var wg sync.WaitGroup
		for _, p := range probers {
			wg.Add(1)
			go func(p *prober) { defer wg.Done(); p.run(pctx, 20) }(p)
		}
		wg.Wait()
		sleepCtx(ctx, 500*time.Millisecond)
		out := make([][]*probeSample, len(probers))
		for i, p := range probers {
			out[i] = p.samples()[start[i]:]
		}
		return out
	}
	idleS := runProbes(idle)
	for i := range samples {
		samples[i].idle = idleS[i]
	}
	res := &results.QoS{}
	var wg sync.WaitGroup
	var down, up *results.TCP
	var errDown, errUp error
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg.Add(2)
	go func() {
		defer wg.Done()
		down, errDown = RunTCP(lctx, c, TCPParams{Direction: results.Down, Streams: streams, Duration: load + 2*time.Second, Omit: time.Second})
	}()
	go func() {
		defer wg.Done()
		up, errUp = RunTCP(lctx, c, TCPParams{Direction: results.Up, Streams: streams, Duration: load + 2*time.Second, Omit: time.Second})
	}()
	sleepCtx(ctx, 2*time.Second) // let the load build before measuring
	loadS := runProbes(load)
	wg.Wait()
	if errDown != nil {
		return nil, errDown
	}
	if errUp != nil {
		return nil, errUp
	}
	res.DownMbps, res.UpMbps = down.Mbps, up.Mbps
	for i := range samples {
		samples[i].loaded = loadS[i]
	}
	summ := func(ss []*probeSample) (stats.Summary, float64) {
		var rtts []float64
		lost := 0
		for _, s := range ss {
			if s.got {
				rtts = append(rtts, s.rttMs)
			} else {
				lost++
			}
		}
		loss := 0.0
		if len(ss) > 0 {
			loss = 100 * float64(lost) / float64(len(ss))
		}
		return roundSum(stats.Summarize(rtts)), loss
	}
	for _, p := range probers {
		p.mu.Lock() // reply handlers write samples under p.mu
	}
	for i, cl := range classes {
		qc := results.QoSClass{Name: cl.name, DSCP: cl.dscp}
		qc.Idle, _ = summ(samples[i].idle)
		qc.Loaded, qc.LossPct = summ(samples[i].loaded)
		qc.LossPct = stats.Round(qc.LossPct, 2)
		qc.Increase = stats.Round(math.Max(0, qc.Loaded.P95-qc.Idle.Median), 2)
		res.Classes = append(res.Classes, qc)
	}
	for _, p := range probers {
		p.mu.Unlock()
	}
	ef, be := res.Classes[0], res.Classes[2]
	switch {
	case be.Increase < 10:
		res.Verdict = fmt.Sprintf("Latency barely rises under load for any class (best effort +%.1f ms), so priority is not needed on this path right now.", be.Increase)
		res.Effective = true
	case ef.Increase < 0.5*be.Increase:
		res.Effective = true
		res.Verdict = fmt.Sprintf("Priority works: under load EF latency rose %.0f ms while best effort rose %.0f ms.", ef.Increase, be.Increase)
	default:
		res.Verdict = fmt.Sprintf("No priority: EF latency rose %.0f ms under load, about as much as best effort (%.0f ms). Voice marked EF is not protected on this path.", ef.Increase, be.Increase)
	}
	return res, nil
}
