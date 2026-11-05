package engine

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// UDPParams controls a paced UDP stream.
type UDPParams struct {
	Direction results.Direction
	RateBps   float64
	Size      int
	Duration  time.Duration
	TOS       int // -1 for unset
	Count     uint64
	BinMs     int
}

// udpRaw is the raw outcome of one UDP stream.
type udpRaw struct {
	sent flow.UDPSendResult
	sink flow.SinkResult
}

// runUDPRaw sends one stream and returns sender and receiver views.
func runUDPRaw(ctx context.Context, c *Client, p UDPParams) (udpRaw, error) {
	if p.Size <= 0 {
		p.Size = 1200
	}
	if p.BinMs <= 0 {
		p.BinMs = 100
	}
	var raw udpRaw
	if p.Direction == results.Up {
		var ready endpoint.UDPSinkReady
		if err := c.Call(ctx, "udp_sink", endpoint.UDPSinkReq{BinMs: p.BinMs}, &ready); err != nil {
			return raw, err
		}
		raw.sent = flow.SendUDP(ctx, c.Signer(), flow.UDPSendParams{
			Session: c.Session(), Stream: ready.Stream, RateBps: p.RateBps, Size: p.Size, Duration: p.Duration, TOS: p.TOS, Count: p.Count,
		}, c.SendUDP)
		sleepCtx(ctx, 400*time.Millisecond)
		err := c.Call(ctx, "udp_sink_result", endpoint.UDPSinkResultReq{Stream: ready.Stream, Sent: raw.sent.Sent, Close: true}, &raw.sink)
		return raw, err
	}
	stream := c.LocalStream()
	sink := flow.NewUDPSink(time.Duration(p.BinMs) * time.Millisecond)
	c.AddSink(stream, sink)
	defer c.AddSink(stream, nil)
	c.punch()
	if err := c.Call(ctx, "udp_send", endpoint.UDPSendReq{
		Stream: stream, RateBps: p.RateBps, Size: p.Size, DurationMs: int(p.Duration / time.Millisecond), TOS: p.TOS, Count: p.Count,
	}, nil); err != nil {
		return raw, err
	}
	if err := c.Call(ctx, "udp_send_result", endpoint.UDPSendResultReq{Stream: stream}, &raw.sent); err != nil {
		return raw, err
	}
	sleepCtx(ctx, 400*time.Millisecond)
	raw.sink = sink.Result(raw.sent.Sent)
	return raw, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// RunUDP runs a paced UDP stream and reports delivery, loss and jitter.
func RunUDP(ctx context.Context, c *Client, p UDPParams, trustOWD bool) (*results.UDP, error) {
	if p.TOS == 0 {
		p.TOS = -1
	}
	raw, err := runUDPRaw(ctx, c, p)
	if err != nil {
		return nil, err
	}
	return buildUDP(p, raw, trustOWD, math.NaN()), nil
}

// buildUDP converts raw stream data. minOWD, if not NaN, is the baseline
// one-way delay used for queueing delay (for sweeps); otherwise the
// stream's own minimum is used.
func buildUDP(p UDPParams, raw udpRaw, trustOWD bool, minOWD float64) *results.UDP {
	if p.Size <= 0 {
		p.Size = 1200
	}
	s := raw.sink
	res := &results.UDP{
		Direction: p.Direction, TargetMbps: stats.Round(p.RateBps/1e6, 3), SizeBytes: p.Size,
		DurationS: stats.Round(raw.sent.Seconds, 3), Loss: s.Loss, JitterMs: stats.Round(s.JitterMs, 3),
		TOSSent: p.TOS, MinOWDRawMs: s.OWDRaw.Min,
	}
	if raw.sent.Seconds > 0 {
		res.SentMbps = stats.Round(float64(raw.sent.Bytes)*8/raw.sent.Seconds/1e6, 3)
	}
	res.HostDrops = s.HostDrops
	res.NetLossPct = stats.Round(s.Loss.LossPct, 3)
	if s.HostDrops > 0 && s.Loss.Sent > 0 {
		net := math.Max(0, float64(s.Loss.Lost)-float64(s.HostDrops))
		res.NetLossPct = stats.Round(100*net/float64(s.Loss.Sent), 3)
	}
	// Delivered rate over the sender's duration (the receiver span can be
	// shorter than the send time when the tail is lost).
	span := math.Max(raw.sent.Seconds, s.Seconds)
	if span > 0 {
		res.Mbps = stats.Round(float64(s.Bytes)*8/span/1e6, 3)
		res.PPS = stats.Round(float64(s.Loss.Received)/span, 1)
	}
	base := s.OWDRaw.Min
	if !math.IsNaN(minOWD) {
		base = minOWD
	}
	if s.OWDRaw.N > 0 {
		q := s.OWDRaw
		r := func(x float64) float64 { return stats.Round(math.Max(0, x-base), 3) }
		res.QueueDelay = stats.Summary{N: q.N, Min: r(q.Min), Mean: r(q.Mean), Median: r(q.Median), P90: r(q.P90), P95: r(q.P95), P99: r(q.P99), Max: r(q.Max), StdDev: stats.Round(q.StdDev, 3)}
		if trustOWD {
			o := roundSum(q)
			res.OWD = &o
		}
	}
	if len(s.TOS) > 0 {
		res.TOSReceived = map[int]uint64{}
		for k, v := range s.TOS {
			res.TOSReceived[k] = v
		}
	}
	if len(s.TTL) > 0 {
		res.TTLReceived = map[int]uint64{}
		for k, v := range s.TTL {
			res.TTLReceived[k] = v
		}
	}
	bin := float64(max(p.BinMs, 100)) / 1000
	for i, b := range s.Bins {
		ub := results.UDPBin{T: stats.Round(float64(i+1)*bin, 2), Mbps: stats.Round(float64(b.Bytes)*8/bin/1e6, 3), Received: b.Count}
		if b.Count > 0 {
			ub.QueueDelayMs = stats.Round(math.Max(0, b.OWDMeanMs-base), 3)
		}
		res.Bins = append(res.Bins, ub)
	}
	return res
}

// commonValue returns the most frequent key of a histogram (-1 if empty).
func commonValue(h map[int]uint64) int {
	best, bestN := -1, uint64(0)
	keys := make([]int, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		if h[k] > bestN {
			best, bestN = k, h[k]
		}
	}
	return best
}
