package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// SweepParams controls a rate sweep.
type SweepParams struct {
	Direction results.Direction
	MinBps    float64
	MaxBps    float64
	Steps     int
	Log       bool
	StepTime  time.Duration
	Size      int
	// OnStep reports each completed step for progress displays.
	OnStep func(results.SweepStep)
}

// RunSweep offers UDP at stepped rates from below to beyond capacity and
// records delivery, loss and queueing delay: the network's response curve.
func RunSweep(ctx context.Context, c *Client, sp SweepParams) (*results.Sweep, error) {
	if sp.Steps < 2 {
		sp.Steps = 10
	}
	if sp.StepTime <= 0 {
		sp.StepTime = 3 * time.Second
	}
	if sp.Size <= 0 {
		sp.Size = 1200
	}
	if sp.MaxBps <= 0 {
		return nil, fmt.Errorf("sweep needs a maximum rate")
	}
	if sp.MinBps <= 0 || sp.MinBps >= sp.MaxBps {
		sp.MinBps = sp.MaxBps / float64(sp.Steps*2)
	}
	res := &results.Sweep{Direction: sp.Direction, SizeBytes: sp.Size, StepSeconds: sp.StepTime.Seconds()}
	type rawStep struct {
		offered float64
		raw     udpRaw
	}
	var raws []rawStep
	baseOWD := math.Inf(1)
	heavyLoss := 0
	for i := 0; i < sp.Steps && ctx.Err() == nil; i++ {
		var rate float64
		if sp.Log {
			rate = sp.MinBps * math.Pow(sp.MaxBps/sp.MinBps, float64(i)/float64(sp.Steps-1))
		} else {
			rate = sp.MinBps + (sp.MaxBps-sp.MinBps)*float64(i)/float64(sp.Steps-1)
		}
		raw, err := runUDPRaw(ctx, c, UDPParams{Direction: sp.Direction, RateBps: rate, Size: sp.Size, Duration: sp.StepTime, TOS: -1, BinMs: 100})
		if err != nil {
			return nil, err
		}
		raws = append(raws, rawStep{offered: rate, raw: raw})
		if raw.sink.OWDRaw.N > 0 {
			baseOWD = math.Min(baseOWD, raw.sink.OWDRaw.Min)
		}
		if sp.OnStep != nil {
			sp.OnStep(results.SweepStep{OfferedMbps: stats.Round(rate/1e6, 2), DeliveredMbps: stats.Round(float64(raw.sink.Bytes)*8/math.Max(raw.sent.Seconds, 0.001)/1e6, 2), LossPct: stats.Round(raw.sink.Loss.LossPct, 2)})
		}
		// Stop once well past capacity: saves time and data.
		if raw.sink.Loss.LossPct > 40 {
			heavyLoss++
			if heavyLoss >= 2 {
				break
			}
		}
		sleepCtx(ctx, 700*time.Millisecond) // drain queues between steps
	}
	for _, rs := range raws {
		u := buildUDP(UDPParams{Direction: sp.Direction, RateBps: rs.offered, Size: sp.Size, BinMs: 100}, rs.raw, false, baseOWD)
		st := results.SweepStep{
			OfferedMbps: stats.Round(rs.offered/1e6, 3), SentMbps: u.SentMbps, DeliveredMbps: u.Mbps,
			LossPct: stats.Round(u.Loss.LossPct, 3), NetLossPct: u.NetLossPct, QueueP50ms: u.QueueDelay.Median, QueueP95ms: u.QueueDelay.P95, JitterMs: u.JitterMs,
		}
		if u.HostDrops > 0 && u.Loss.Sent > 0 {
			st.HostDropPct = stats.Round(100*float64(u.HostDrops)/float64(u.Loss.Sent), 3)
		}
		// The sender could not reach the offered rate, or the receiver
		// itself dropped most of what was lost: the hosts, not the
		// network, limited this step.
		st.HostLimited = u.SentMbps < 0.9*st.OfferedMbps || (st.HostDropPct > 0.5 && st.HostDropPct >= 0.5*st.LossPct)
		if half := rs.raw.sent.Seconds / 2; half > 0 {
			st.FirstHalfMbps = stats.Round(float64(rs.raw.sink.FirstHalfB)*8/half/1e6, 3)
			st.SecondHalfMbps = stats.Round(float64(rs.raw.sink.SecondHalfB)*8/half/1e6, 3)
		}
		res.Steps = append(res.Steps, st)
	}
	ClassifySweep(res)
	return res, nil
}

// ClassifySweep finds capacity, the knee and queue onset, estimates the
// buffer, and names the bottleneck's signature (docs/PLAN.md §8.1).
func ClassifySweep(s *results.Sweep) {
	if len(s.Steps) == 0 {
		s.Signature = "unknown"
		return
	}
	// Judge the network on steps the hosts could keep up with, using loss
	// with the receiver's own drops removed.
	all := s.Steps
	var valid []results.SweepStep
	hostLimitedFrom := -1.0
	for _, st := range all {
		if st.HostLimited {
			if hostLimitedFrom < 0 {
				hostLimitedFrom = st.OfferedMbps
			}
			continue
		}
		if st.HostDropPct > 0 {
			st.LossPct = st.NetLossPct
		}
		valid = append(valid, st)
	}
	defer func() { s.Steps = all }()
	if len(valid) == 0 {
		s.Signature = "host-limited"
		s.SignatureDetail = "Every step was limited by the test hosts (sending or receiving), so the network's response could not be measured. Use faster hosts or lower rates."
		return
	}
	s.Steps = valid
	first := s.Steps[0]
	for _, st := range s.Steps {
		s.CapacityMbps = math.Max(s.CapacityMbps, st.DeliveredMbps)
		s.MaxQueueMs = math.Max(s.MaxQueueMs, st.QueueP95ms)
	}
	knee := -1
	for i, st := range s.Steps {
		if st.LossPct > 1 || st.DeliveredMbps < 0.95*st.SentMbps {
			knee = i
			break
		}
	}
	onset := -1
	for i, st := range s.Steps {
		if st.QueueP50ms > math.Max(5, first.QueueP50ms+3) {
			onset = i
			break
		}
	}
	if knee > 0 {
		s.KneeMbps = s.Steps[knee-1].DeliveredMbps
	} else if knee == 0 {
		s.KneeMbps = s.Steps[0].DeliveredMbps
	} else {
		s.KneeMbps = s.CapacityMbps
	}
	if onset >= 0 {
		s.QueueOnsetMbps = s.Steps[onset].OfferedMbps
	}
	s.BufferBytes = math.Round(s.MaxQueueMs / 1000 * s.CapacityMbps * 1e6 / 8)

	allLossy := true
	for _, st := range s.Steps {
		if st.LossPct < 0.5 {
			allLossy = false
		}
	}
	// Burst allowance: in over-capacity steps the first half delivered
	// clearly more than the second half (consistently, in 2+ steps).
	bursty := 0
	for _, st := range s.Steps {
		if st.FirstHalfMbps > 1.3*st.SecondHalfMbps && st.SecondHalfMbps > 0 && st.LossPct > 1 {
			bursty++
		}
	}
	burst := bursty >= 2 || (bursty == 1 && len(s.Steps) <= 3)
	switch {
	case allLossy && first.OfferedMbps < 0.5*s.CapacityMbps:
		s.Signature = "non-congestive-loss"
		s.SignatureDetail = fmt.Sprintf("Loss appears even at the lowest rate (%.2f%% at %.1f Mbit/s): it is not caused by congestion. Suspect the physical layer, radio, or interface errors.", first.LossPct, first.OfferedMbps)
	case knee < 0 && hostLimitedFrom > 0:
		s.Signature = "host-limited"
		s.SignatureDetail = fmt.Sprintf("The network delivered every rate the hosts could generate; from about %.0f Mbit/s the test hosts themselves (CPU or socket buffers) were the limit, so the path's capacity is higher than this sweep can show.", hostLimitedFrom)
	case knee < 0:
		s.Signature = "no-bottleneck-reached"
		s.SignatureDetail = fmt.Sprintf("Every offered rate up to %.1f Mbit/s was delivered; the sweep did not reach the path's capacity.", s.Steps[len(s.Steps)-1].OfferedMbps)
	case burst:
		s.Signature = "burst-allowance"
		s.SignatureDetail = fmt.Sprintf("Above %.1f Mbit/s the start of each step gets through faster than its end: a token bucket allows a burst, then holds traffic to about %.1f Mbit/s.", s.KneeMbps, s.CapacityMbps)
	case s.MaxQueueMs < 5:
		s.Signature = "policer"
		s.SignatureDetail = fmt.Sprintf("Loss starts sharply at about %.1f Mbit/s with almost no queueing delay (max %.1f ms): a policer is dropping excess traffic.", s.KneeMbps, s.MaxQueueMs)
	case s.MaxQueueMs > 100:
		s.Signature = "bufferbloat"
		s.SignatureDetail = fmt.Sprintf("Queueing delay reaches %.0f ms before and during loss at about %.1f Mbit/s: a large FIFO buffer (≈%s) sits at the bottleneck.", s.MaxQueueMs, s.CapacityMbps, humanBytes(s.BufferBytes))
	default:
		s.Signature = "shaper"
		s.SignatureDetail = fmt.Sprintf("Delay rises to about %.0f ms and then loss begins at about %.1f Mbit/s: a shaper with a moderate buffer (≈%s).", s.MaxQueueMs, s.CapacityMbps, humanBytes(s.BufferBytes))
	}
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
}

// RunSizeSweep sends a fixed bit rate at each packet size to find
// packets-per-second limits (small-packet performance of middleboxes).
func RunSizeSweep(ctx context.Context, c *Client, dir results.Direction, rateBps float64, sizes []int, step time.Duration) (*results.SizeSweep, error) {
	if len(sizes) == 0 {
		sizes = []int{64, 128, 256, 512, 1024, 1200, 1400}
	}
	if step <= 0 {
		step = 2 * time.Second
	}
	res := &results.SizeSweep{Direction: dir, RateMbps: stats.Round(rateBps/1e6, 2)}
	for _, size := range sizes {
		raw, err := runUDPRaw(ctx, c, UDPParams{Direction: dir, RateBps: rateBps, Size: size, Duration: step, TOS: -1})
		if err != nil {
			return nil, err
		}
		u := buildUDP(UDPParams{Direction: dir, RateBps: rateBps, Size: size}, raw, false, math.NaN())
		res.Points = append(res.Points, results.SizePoint{Size: size, OfferedMbps: res.RateMbps, DeliveredMbps: u.Mbps, PPS: u.PPS, LossPct: stats.Round(u.Loss.LossPct, 3)})
		res.MaxPPS = math.Max(res.MaxPPS, u.PPS)
		sleepCtx(ctx, 500*time.Millisecond)
	}
	return res, nil
}
