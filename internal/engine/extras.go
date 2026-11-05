package engine

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// LocalCongestionAlgorithms lists algorithms this host can use.
func LocalCongestionAlgorithms() []string {
	b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control")
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// RunCC compares congestion-control algorithms on single-stream uploads
// (the client chooses the algorithm for its own sends).
func RunCC(ctx context.Context, c *Client, algs []string, d time.Duration) (*results.CCCompare, error) {
	if len(algs) == 0 {
		algs = []string{"cubic", "bbr", "reno"}
	}
	res := &results.CCCompare{Direction: results.Up}
	best := 0.0
	for _, a := range algs {
		r, err := RunTCP(ctx, c, TCPParams{Direction: results.Up, Streams: 1, Duration: d, Omit: d / 5, Congestion: a})
		pt := results.CCPoint{Algorithm: a}
		if err != nil {
			pt.Error = err.Error()
		} else {
			pt.Mbps, pt.Retransmits, pt.RTTms = r.Mbps, r.Retransmits, r.RTT.Median
			if r.Mbps > best {
				best, res.Best = r.Mbps, a
			}
		}
		res.Points = append(res.Points, pt)
		sleepCtx(ctx, time.Second)
	}
	return res, nil
}

// RunShortFlows measures connection setup and completion times of small
// transfers, which is what interactive applications experience.
func RunShortFlows(ctx context.Context, c *Client, perSize int) (*results.ShortFlow, error) {
	if perSize <= 0 {
		perSize = 10
	}
	sizes := []struct {
		label string
		bytes uint64
	}{{"10KB", 10 << 10}, {"100KB", 100 << 10}, {"1MB", 1 << 20}}
	res := &results.ShortFlow{Completions: map[string]stats.Summary{}}
	var connects []float64
	for _, sz := range sizes {
		var times []float64
		for i := 0; i < perSize && ctx.Err() == nil; i++ {
			var prep endpoint.TCPPrepared
			if err := c.Call(ctx, "tcp_prepare", endpoint.TCPPrepare{Role: "send", Streams: 1, DurationMs: 10000, Bytes: sz.bytes}, &prep); err != nil {
				return nil, err
			}
			start := time.Now()
			conn, err := c.DialData(ctx, prep.Streams[0], DialOptions{})
			if err != nil {
				res.Errors++
				continue
			}
			connects = append(connects, float64(time.Since(start).Microseconds())/1000)
			n, _ := io.Copy(io.Discard, conn)
			el := time.Since(start)
			conn.Close()
			_ = c.Call(ctx, "tcp_result", map[string]any{"test_id": prep.TestID, "wait_ms": 5000}, nil)
			if uint64(n) < sz.bytes {
				res.Errors++
				continue
			}
			times = append(times, float64(el.Microseconds())/1000)
			res.Count++
		}
		res.Completions[sz.label] = roundSum(stats.Summarize(times))
	}
	res.Connect = roundSum(stats.Summarize(connects))
	return res, nil
}

// RunDiscrimination compares random and compressible TCP payloads (WAN
// optimisers) and UDP against TCP (UDP throttling) in one direction.
func RunDiscrimination(ctx context.Context, c *Client, dir results.Direction, d time.Duration) (*results.Discrimination, error) {
	res := &results.Discrimination{Direction: dir}
	random, err := RunTCP(ctx, c, TCPParams{Direction: dir, Streams: 4, Duration: d, Omit: d / 5})
	if err != nil {
		return nil, err
	}
	res.Points = append(res.Points, results.DiscriminationPoint{Label: "TCP, random payload", Mbps: random.Mbps})
	sleepCtx(ctx, time.Second)
	comp, err := RunTCP(ctx, c, TCPParams{Direction: dir, Streams: 4, Duration: d, Omit: d / 5, Compressible: true})
	pt := results.DiscriminationPoint{Label: "TCP, compressible payload"}
	if err != nil {
		pt.Error = err.Error()
	} else {
		pt.Mbps = comp.Mbps
	}
	res.Points = append(res.Points, pt)
	sleepCtx(ctx, time.Second)
	udpRate := math.Max(random.Mbps*0.9, 1) * 1e6
	u, err := RunUDP(ctx, c, UDPParams{Direction: dir, RateBps: udpRate, Size: 1200, Duration: d}, false)
	upt := results.DiscriminationPoint{Label: fmt.Sprintf("UDP at %.0f Mbit/s", udpRate/1e6)}
	if err != nil {
		upt.Error = err.Error()
	} else {
		upt.Mbps = u.Mbps
	}
	res.Points = append(res.Points, upt)
	var notes []string
	if pt.Mbps > 1.3*random.Mbps && random.Mbps > 0 {
		notes = append(notes, "compressible data moves much faster than random data: a WAN optimiser or compressor is in the path")
	}
	if upt.Error == "" && upt.Mbps < 0.7*udpRate/1e6 {
		notes = append(notes, fmt.Sprintf("UDP delivered only %.0f%% of a rate TCP sustains: UDP may be throttled or deprioritised", 100*upt.Mbps/(udpRate/1e6)))
	}
	if len(notes) == 0 {
		res.Verdict = "no discrimination detected between payload types or between TCP and UDP"
	} else {
		res.Verdict = strings.Join(notes, "; ")
	}
	return res, nil
}
