package engine

import (
	"context"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// VoIPParams controls the simulated call.
type VoIPParams struct {
	Duration time.Duration
	// JitterBufferMs is the receive buffer; packets later than this count as lost.
	JitterBufferMs int
	// Load runs bulk TCP in both directions during the call.
	Load bool
}

// RunVoIP simulates a G.711 call (50 packets/s of 172 bytes each way, plus
// headers) and estimates MOS per direction with the E-model.
func RunVoIP(ctx context.Context, c *Client, vp VoIPParams) (*results.VoIP, error) {
	if vp.Duration <= 0 {
		vp.Duration = 15 * time.Second
	}
	if vp.JitterBufferMs <= 0 {
		vp.JitterBufferMs = 60
	}
	const pps, size = 50.0, 172 + 28 // RTP payload + RTP/UDP header bytes, as the call would send
	rate := pps * size * 8
	res := &results.VoIP{Codec: "G.711 (20 ms)", DurationS: vp.Duration.Seconds(), Loaded: vp.Load}

	var ready endpoint.UDPSinkReady
	if err := c.Call(ctx, "udp_sink", endpoint.UDPSinkReq{}, &ready); err != nil {
		return nil, err
	}
	downStream := c.LocalStream()
	downSink := flow.NewUDPSink(0)
	c.AddSink(downStream, downSink)
	defer c.AddSink(downStream, nil)
	c.punch()

	p := newProber(c, 0)
	defer p.close()
	callCtx, cancel := context.WithTimeout(ctx, vp.Duration)
	defer cancel()

	var wg sync.WaitGroup
	var upSent flow.UDPSendResult
	var downSent flow.UDPSendResult
	var errDown error
	wg.Add(3)
	go func() {
		defer wg.Done()
		upSent = flow.SendUDP(callCtx, c.Signer(), flow.UDPSendParams{Session: c.Session(), Stream: ready.Stream, RateBps: rate, Size: size, Duration: vp.Duration, TOS: 46 << 2}, c.SendUDP)
	}()
	go func() {
		defer wg.Done()
		if errDown = c.Call(ctx, "udp_send", endpoint.UDPSendReq{Stream: downStream, RateBps: rate, Size: size, DurationMs: int(vp.Duration / time.Millisecond), TOS: 46 << 2}, nil); errDown != nil {
			return
		}
		errDown = c.Call(ctx, "udp_send_result", endpoint.UDPSendResultReq{Stream: downStream}, &downSent)
	}()
	go func() {
		defer wg.Done()
		p.run(callCtx, 10)
	}()
	if vp.Load {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = RunTCP(ctx, c, TCPParams{Direction: results.Down, Streams: 4, Duration: vp.Duration})
		}()
		go func() {
			defer wg.Done()
			_, _ = RunTCP(ctx, c, TCPParams{Direction: results.Up, Streams: 4, Duration: vp.Duration})
		}()
	}
	wg.Wait()
	if errDown != nil {
		return nil, errDown
	}
	sleepCtx(ctx, 500*time.Millisecond)
	var upSink flow.SinkResult
	if err := c.Call(ctx, "udp_sink_result", endpoint.UDPSinkResultReq{Stream: ready.Stream, Sent: upSent.Sent, Close: true}, &upSink); err != nil {
		return nil, err
	}
	down := downSink.Result(downSent.Sent)

	p.close()
	p.mu.Lock()
	var rtts []float64
	for _, s := range p.order {
		if s.got {
			rtts = append(rtts, s.rttMs)
		}
	}
	p.mu.Unlock()
	rtt := stats.Summarize(rtts)
	res.OneWayMs = stats.Round(rtt.Median/2, 2)
	res.Up = voipDir(upSink, res.OneWayMs, vp.JitterBufferMs)
	res.Down = voipDir(down, res.OneWayMs, vp.JitterBufferMs)
	res.Verdict = worseVerdict(res.Up.Verdict, res.Down.Verdict)
	return res, nil
}

func voipDir(s flow.SinkResult, oneWayMs float64, jb int) results.VoIPDir {
	late := s.LatePct[jb]
	if late == 0 {
		// Use the nearest smaller computed threshold.
		for _, t := range []int{100, 60, 40, 20} {
			if t <= jb {
				late = s.LatePct[t]
				break
			}
		}
	}
	d := results.VoIPDir{Loss: s.Loss, JitterMs: stats.Round(s.JitterMs, 2), LateLossPct: stats.Round(late, 2)}
	// The jitter buffer adds delay; codec and packetisation add ~20 ms.
	mos, r := stats.MOS(oneWayMs+float64(jb)/2+20, s.JitterMs, s.Loss.LossPct+late)
	d.MOS, d.R = stats.Round(mos, 2), stats.Round(r, 1)
	d.Verdict = mosVerdict(d.MOS)
	return d
}

func mosVerdict(m float64) string {
	switch {
	case m >= 4.0:
		return "good"
	case m >= 3.6:
		return "acceptable"
	default:
		return "poor"
	}
}

func worseVerdict(a, b string) string {
	rank := map[string]int{"good": 0, "acceptable": 1, "poor": 2}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}
