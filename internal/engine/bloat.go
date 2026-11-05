package engine

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// BloatParams controls the bufferbloat (latency under load) test.
type BloatParams struct {
	Idle     time.Duration
	Phase    time.Duration
	Streams  int
	ProbeHz  float64
	Phases   []string // any of down, up, bidir
	OnSample func(phase string, t, rttMs float64)
	// OnInterval receives live throughput during loaded phases.
	OnInterval func(dir results.Direction, t, mbps float64)
}

// RunBufferbloat measures latency idle and under load in each direction.
func RunBufferbloat(ctx context.Context, c *Client, bp BloatParams) (*results.Bufferbloat, error) {
	if bp.Idle <= 0 {
		bp.Idle = 5 * time.Second
	}
	if bp.Phase <= 0 {
		bp.Phase = 10 * time.Second
	}
	if bp.Streams <= 0 {
		bp.Streams = 4
	}
	if bp.ProbeHz <= 0 {
		bp.ProbeHz = 20
	}
	if len(bp.Phases) == 0 {
		bp.Phases = []string{"down", "up", "bidir"}
	}
	start := time.Now()
	var mu sync.Mutex
	phase := "idle"
	setPhase := func(p string) {
		mu.Lock()
		phase = p
		mu.Unlock()
	}
	getPhase := func() string {
		mu.Lock()
		defer mu.Unlock()
		return phase
	}

	type lab struct {
		phase string
		s     *probeSample
	}
	var probes []lab
	p := newProber(c, 0)
	defer p.close()
	p.onRTT = func(s *probeSample) {
		if bp.OnSample != nil {
			bp.OnSample(getPhase(), time.Since(start).Seconds(), s.rttMs)
		}
	}
	// Tag probes with the phase at send time.
	probeCtx, stopProbes := context.WithCancel(ctx)
	var pwg sync.WaitGroup
	pwg.Add(2)
	go func() {
		defer pwg.Done()
		t := time.NewTicker(time.Duration(float64(time.Second) / bp.ProbeHz))
		defer t.Stop()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-t.C:
				ph := getPhase()
				s := p.send()
				mu.Lock()
				probes = append(probes, lab{phase: ph, s: s})
				mu.Unlock()
			}
		}
	}()
	connects := map[string][]float64{}
	go func() {
		defer pwg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-t.C:
				ph := getPhase()
				if d, err := c.ConnectTime(probeCtx); err == nil {
					mu.Lock()
					connects[ph] = append(connects[ph], float64(d.Microseconds())/1000)
					mu.Unlock()
				}
			}
		}
	}()

	res := &results.Bufferbloat{IncreaseMs: map[string]float64{}, Grades: map[string]string{}, BufferMs: map[string]float64{}, BufferBytes: map[string]float64{}, Streams: bp.Streams}
	type load struct {
		down, up *results.TCP
	}
	loads := map[string]load{}
	sleepCtx(ctx, bp.Idle)
	var runErr error
	for _, ph := range bp.Phases {
		if ctx.Err() != nil {
			break
		}
		setPhase(ph)
		var l load
		var errDown, errUp error
		var wg sync.WaitGroup
		if ph == "down" || ph == "bidir" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.down, errDown = RunTCP(ctx, c, TCPParams{Direction: results.Down, Streams: bp.Streams, Duration: bp.Phase, Omit: time.Second, OnInterval: bp.OnInterval})
			}()
		}
		if ph == "up" || ph == "bidir" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.up, errUp = RunTCP(ctx, c, TCPParams{Direction: results.Up, Streams: bp.Streams, Duration: bp.Phase, Omit: time.Second, OnInterval: bp.OnInterval})
			}()
		}
		wg.Wait()
		if errDown != nil {
			runErr = errDown
		}
		if errUp != nil {
			runErr = errUp
		}
		loads[ph] = l
		setPhase("drain")
		sleepCtx(ctx, 2*time.Second) // let queues drain between phases
	}
	stopProbes()
	pwg.Wait()
	sleepCtx(ctx, 500*time.Millisecond)
	if runErr != nil && len(loads) == 0 {
		return nil, runErr
	}

	byPhase := map[string][]float64{}
	sent := map[string]int{}
	var series []results.BloatSample
	p.close()
	p.mu.Lock() // reply handlers write probe samples under p.mu
	mu.Lock()
	for _, pr := range probes {
		sent[pr.phase]++
		if pr.s.got {
			byPhase[pr.phase] = append(byPhase[pr.phase], pr.s.rttMs)
			series = append(series, results.BloatSample{T: stats.Round(pr.s.sent.Sub(start).Seconds(), 3), RTTms: stats.Round(pr.s.rttMs, 3), Phase: pr.phase})
		}
	}
	mu.Unlock()
	p.mu.Unlock()

	idle := stats.Summarize(byPhase["idle"])
	phases := []string{"idle"}
	phases = append(phases, bp.Phases...)
	var working []float64
	for _, ph := range phases {
		bpr := results.BloatPhase{Name: ph, RTT: roundSum(stats.Summarize(byPhase[ph])), Connect: roundSum(stats.Summarize(connects[ph]))}
		if n := sent[ph]; n > 0 {
			bpr.Loss = stats.Round(100*float64(n-len(byPhase[ph]))/float64(n), 2)
		}
		l := loads[ph]
		var inflow []float64
		if l.down != nil {
			bpr.DownMbps = l.down.Mbps
			for _, iv := range l.down.Intervals {
				if iv.RTTms > 0 && iv.T > 1 {
					inflow = append(inflow, iv.RTTms)
				}
			}
		}
		if l.up != nil {
			bpr.UpMbps = l.up.Mbps
			for _, iv := range l.up.Intervals {
				if iv.RTTms > 0 && iv.T > 1 {
					inflow = append(inflow, iv.RTTms)
				}
			}
		}
		bpr.InFlowRTT = roundSum(stats.Summarize(inflow))
		for _, s := range series {
			if s.Phase == ph {
				bpr.Series = append(bpr.Series, s)
			}
		}
		res.Phases = append(res.Phases, bpr)
		if ph != "idle" && bpr.RTT.N > 0 {
			inc := math.Max(0, bpr.RTT.P95-idle.Median)
			res.IncreaseMs[ph] = stats.Round(inc, 2)
			res.Grades[ph] = stats.Grade(inc)
			res.BufferMs[ph] = stats.Round(inc, 1)
			mbps := bpr.DownMbps
			if ph == "up" {
				mbps = bpr.UpMbps
			}
			res.BufferBytes[ph] = math.Round(inc / 1000 * mbps * 1e6 / 8)
			// Working latency for responsiveness: p90 of each latency kind.
			w := []float64{bpr.RTT.P90}
			if bpr.InFlowRTT.N > 0 {
				w = append(w, bpr.InFlowRTT.P90)
			}
			if bpr.Connect.N > 0 {
				w = append(w, bpr.Connect.P90)
			}
			working = append(working, mean(w))
		}
	}
	res.Grade = stats.WorstGrade(res.Grades["down"], res.Grades["up"])
	if res.Grade == "" {
		res.Grade = res.Grades["bidir"]
	}
	if len(working) > 0 {
		res.WorkingLatency = stats.Round(mean(working), 2)
		res.RPM = math.Round(stats.RPM(res.WorkingLatency))
	}
	res.Queueing = classifyQueue(res, idle.Median)
	return res, nil
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// classifyQueue compares latency of a separate flow with latency inside
// the loaded flows: a flow-queuing AQM keeps the separate flow fast even
// while the loaded flows queue; a single FIFO slows both.
func classifyQueue(b *results.Bufferbloat, idleMedian float64) string {
	var probeInc, flowInc float64
	n := 0
	for _, ph := range b.Phases {
		if ph.Name == "idle" || ph.InFlowRTT.N == 0 || ph.RTT.N == 0 {
			continue
		}
		probeInc += math.Max(0, ph.RTT.Median-idleMedian)
		flowInc += math.Max(0, ph.InFlowRTT.Median-idleMedian)
		n++
	}
	if n == 0 {
		return "unclear"
	}
	probeInc /= float64(n)
	flowInc /= float64(n)
	switch {
	case flowInc < 10 && probeInc < 10:
		return "low-latency"
	case probeInc < 0.35*flowInc:
		return "flow-queued"
	case probeInc >= 0.6*flowInc:
		return "fifo"
	}
	return "unclear"
}
