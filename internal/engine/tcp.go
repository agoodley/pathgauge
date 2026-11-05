package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// TCPParams controls a bulk TCP test.
type TCPParams struct {
	Direction    results.Direction
	Streams      int
	Duration     time.Duration
	Omit         time.Duration // ramp-up excluded from goodput
	Interval     time.Duration
	Congestion   string
	PacingBps    uint64 // per stream, bytes/s
	BufferBytes  int
	Compressible bool
	// OnInterval receives live throughput (Mbit/s) for progress displays.
	OnInterval func(dir results.Direction, t, mbps float64)
}

func (p *TCPParams) defaults() {
	if p.Streams <= 0 {
		p.Streams = 1
	}
	if p.Duration <= 0 {
		p.Duration = 10 * time.Second
	}
	if p.Omit < 0 || p.Omit >= p.Duration {
		p.Omit = 0
	}
	if p.Interval <= 0 {
		p.Interval = 100 * time.Millisecond
	}
}

// tcpRun is a TCP test in progress.
type tcpRun struct {
	c       *Client
	p       TCPParams
	prep    endpoint.TCPPrepared
	streams []*flow.Stream
	sampler flow.Sampler
	wg      sync.WaitGroup
	sampled chan struct{}
	started time.Time
}

// startTCP begins a TCP test and returns without waiting for it.
func startTCP(ctx context.Context, c *Client, p TCPParams) (*tcpRun, error) {
	p.defaults()
	role := "send"
	if p.Direction == results.Up {
		role = "recv"
	}
	r := &tcpRun{c: c, p: p, sampled: make(chan struct{})}
	err := c.Call(ctx, "tcp_prepare", endpoint.TCPPrepare{
		Role: role, Streams: p.Streams, DurationMs: int(p.Duration / time.Millisecond), IntervalMs: int(p.Interval / time.Millisecond),
		Congestion: congestionFor(p, results.Down), PacingBps: pacingFor(p, results.Down), BufferBytes: p.BufferBytes, Compressible: p.Compressible,
	}, &r.prep)
	if err != nil {
		return nil, err
	}
	opts := DialOptions{Congestion: congestionFor(p, results.Up), PacingBps: pacingFor(p, results.Up), BufferBytes: p.BufferBytes}
	r.streams = make([]*flow.Stream, len(r.prep.Streams))
	var dialErr error
	var dmu sync.Mutex
	var dwg sync.WaitGroup
	for i, id := range r.prep.Streams {
		dwg.Add(1)
		go func(i int, id uint32) {
			defer dwg.Done()
			conn, err := c.DialData(ctx, id, opts)
			dmu.Lock()
			defer dmu.Unlock()
			if err != nil {
				dialErr = err
				return
			}
			r.streams[i] = &flow.Stream{ID: id, Conn: conn}
		}(i, id)
	}
	dwg.Wait()
	if dialErr != nil {
		for _, s := range r.streams {
			if s != nil {
				s.Conn.Close()
			}
		}
		return nil, fmt.Errorf("open data connection: %w", dialErr)
	}
	r.started = time.Now()
	var runCtx context.Context
	var cancel context.CancelFunc
	if p.Direction == results.Up {
		runCtx, cancel = context.WithTimeout(ctx, p.Duration)
	} else {
		runCtx, cancel = context.WithTimeout(ctx, p.Duration+15*time.Second)
	}
	r.sampler.Interval = p.Interval
	var lastAcked []uint64
	onSample := func(s flow.Sample) {
		if p.OnInterval == nil {
			return
		}
		var bytes uint64
		if p.Direction == results.Up {
			// Live upload rate from acknowledged bytes, not bytes written.
			if lastAcked == nil {
				lastAcked = make([]uint64, len(s.Info))
			}
			for i, inf := range s.Info {
				bytes += inf.BytesAcked - lastAcked[i]
				lastAcked[i] = inf.BytesAcked
			}
		} else {
			for _, b := range s.Bytes {
				bytes += b
			}
		}
		p.OnInterval(p.Direction, s.T, float64(bytes)*8/p.Interval.Seconds()/1e6)
	}
	go func() {
		r.sampler.Run(runCtx, r.streams, onSample)
		close(r.sampled)
	}()
	for _, s := range r.streams {
		r.wg.Add(1)
		go func(s *flow.Stream) {
			defer r.wg.Done()
			if p.Direction == results.Up {
				_ = s.Send(runCtx, 128<<10, p.Compressible, 0)
				_ = s.Conn.CloseWrite()
			} else {
				_ = s.Receive(runCtx)
			}
		}(s)
	}
	go func() {
		r.wg.Wait()
		cancel()
	}()
	return r, nil
}

func congestionFor(p TCPParams, sender results.Direction) string {
	if p.Direction == sender {
		return p.Congestion
	}
	return ""
}

func pacingFor(p TCPParams, sender results.Direction) uint64 {
	if p.Direction == sender {
		return p.PacingBps
	}
	return 0
}

// wait finishes the test and builds the result.
func (r *tcpRun) wait(ctx context.Context) (*results.TCP, error) {
	r.wg.Wait()
	<-r.sampled
	var finals []netx.TCPInfoSummary
	congestion := ""
	for _, s := range r.streams {
		if ti, err := netx.TCPInfo(s.Conn); err == nil {
			finals = append(finals, netx.Summarize(ti))
		}
		if congestion == "" {
			congestion = netx.Congestion(s.Conn)
		}
	}
	var er endpoint.TCPResult
	err := r.c.Call(ctx, "tcp_result", map[string]any{"test_id": r.prep.TestID, "wait_ms": int((r.p.Duration + 30*time.Second) / time.Millisecond)}, &er)
	for _, s := range r.streams {
		s.Conn.Close()
	}
	if err != nil {
		return nil, err
	}
	if er.Error != "" {
		return nil, errors.New(er.Error)
	}
	local := r.sampler.Samples()
	var recvSamples, sendSamples []flow.Sample
	var senderFinals []netx.TCPInfoSummary
	if r.p.Direction == results.Up {
		recvSamples, sendSamples, senderFinals = er.Samples, local, finals
	} else {
		recvSamples, sendSamples = local, er.Samples
		for _, s := range er.Streams {
			senderFinals = append(senderFinals, s.Final)
			if s.Congestion != "" {
				congestion = s.Congestion
			}
		}
	}
	return buildTCP(r.p, recvSamples, sendSamples, senderFinals, congestion), nil
}

// RunTCP runs one bulk TCP test.
func RunTCP(ctx context.Context, c *Client, p TCPParams) (*results.TCP, error) {
	r, err := startTCP(ctx, c, p)
	if err != nil {
		return nil, err
	}
	return r.wait(ctx)
}

// buildTCP turns raw samples into a result.
func buildTCP(p TCPParams, recv, send []flow.Sample, senderFinals []netx.TCPInfoSummary, congestion string) *results.TCP {
	p.defaults()
	res := &results.TCP{
		Direction: p.Direction, Streams: p.Streams, DurationS: p.Duration.Seconds(), OmitS: p.Omit.Seconds(),
		Congestion: congestion, Compressible: p.Compressible,
	}
	if p.PacingBps > 0 {
		res.PacingMbps = stats.Round(float64(p.PacingBps)*8/1e6, 2)
	}
	nStreams := 0
	for _, s := range recv {
		nStreams = max(nStreams, len(s.Bytes))
	}
	perStream := make([]uint64, nStreams)
	var goodBytes uint64
	var goodStart, goodEnd float64 = -1, 0
	prevT := 0.0
	for _, s := range recv {
		dt := s.T - prevT
		var sum uint64
		for i, b := range s.Bytes {
			sum += b
			res.Bytes += b
			if s.T > p.Omit.Seconds() {
				perStream[i] += b
			}
		}
		if s.T > p.Omit.Seconds() && sum > 0 {
			if goodStart < 0 {
				goodStart = prevT
			}
			goodEnd = s.T
			goodBytes += sum
		}
		iv := results.TCPInterval{T: stats.Round(s.T, 3)}
		if dt > 0 {
			iv.Mbps = stats.Round(float64(sum)*8/dt/1e6, 2)
			if len(s.Bytes) > 1 {
				for _, b := range s.Bytes {
					iv.PerStream = append(iv.PerStream, stats.Round(float64(b)*8/dt/1e6, 2))
				}
			}
		}
		res.Intervals = append(res.Intervals, iv)
		res.PeakMbps = math.Max(res.PeakMbps, iv.Mbps)
		prevT = s.T
	}
	if goodStart >= 0 && goodEnd > goodStart {
		span := goodEnd - goodStart
		res.Mbps = stats.Round(float64(goodBytes)*8/span/1e6, 2)
		for _, b := range perStream {
			res.PerStreamMbps = append(res.PerStreamMbps, stats.Round(float64(b)*8/span/1e6, 2))
		}
	}
	res.Fairness = stats.Round(jain(res.PerStreamMbps), 3)

	// Sender-side TCP_INFO: RTT and cwnd per interval, retransmissions.
	var rtts []float64
	var lastRetrans uint32
	for i, s := range send {
		var rttSum, cwndSum float64
		var n int
		var retrans uint32
		for _, inf := range s.Info {
			if inf.RTTus > 0 {
				rttSum += float64(inf.RTTus) / 1000
				cwndSum += float64(inf.Cwnd)
				n++
			}
			retrans += inf.TotalRetrans
		}
		if n > 0 {
			rtts = append(rtts, rttSum/float64(n))
			if i < len(res.Intervals) {
				res.Intervals[i].RTTms = stats.Round(rttSum/float64(n), 2)
				res.Intervals[i].Cwnd = stats.Round(cwndSum/float64(n), 1)
				if retrans >= lastRetrans {
					res.Intervals[i].Retrans = retrans - lastRetrans
				}
			}
		}
		lastRetrans = retrans
	}
	res.RTT = roundSum(stats.Summarize(rtts))
	var busy, rwnd, sndbuf uint64
	var segsBytes uint64
	minRTT := math.Inf(1)
	for _, f := range senderFinals {
		res.Retransmits += uint64(f.TotalRetrans)
		busy += f.BusyUs
		rwnd += f.RwndLimitedUs
		sndbuf += f.SndbufLimUs
		segsBytes += f.BytesAcked
		if f.SndMSS > 0 {
			res.MSS = f.SndMSS
		}
		if f.MinRTTus > 0 {
			minRTT = math.Min(minRTT, float64(f.MinRTTus)/1000)
		}
	}
	if !math.IsInf(minRTT, 1) {
		res.MinRTTms = stats.Round(minRTT, 3)
	}
	if busy > 0 {
		res.RwndLimitedPct = stats.Round(100*float64(rwnd)/float64(busy), 1)
		res.SndbufLimitedPct = stats.Round(100*float64(sndbuf)/float64(busy), 1)
	}
	if res.MSS > 0 && res.Bytes > 0 {
		segments := float64(res.Bytes) / float64(res.MSS)
		res.RetransPct = stats.Round(100*float64(res.Retransmits)/segments, 3)
	}
	// Ramp: first interval reaching 90% of steady goodput.
	if res.Mbps > 0 {
		for _, iv := range res.Intervals {
			if iv.Mbps >= 0.9*res.Mbps {
				res.RampS = iv.T
				break
			}
		}
	}
	return res
}

// jain is Jain's fairness index (1 = perfectly fair).
func jain(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum, sq float64
	for _, x := range xs {
		sum += x
		sq += x * x
	}
	if sq == 0 {
		return 0
	}
	return sum * sum / (float64(len(xs)) * sq)
}

// RunStreams compares stream counts in one direction.
func RunStreams(ctx context.Context, c *Client, dir results.Direction, counts []int, d time.Duration, cc string, onInterval func(results.Direction, float64, float64)) (*results.StreamScaling, error) {
	res := &results.StreamScaling{Direction: dir}
	for _, n := range counts {
		r, err := RunTCP(ctx, c, TCPParams{Direction: dir, Streams: n, Duration: d, Omit: d / 5, Congestion: cc, OnInterval: onInterval})
		if err != nil {
			return nil, err
		}
		res.Points = append(res.Points, results.StreamPoint{Streams: n, Mbps: r.Mbps, PerStreamMbps: r.PerStreamMbps, Retransmits: r.Retransmits, RTTms: r.RTT.Median})
		res.BestMbps = math.Max(res.BestMbps, r.Mbps)
		time.Sleep(time.Second) // let queues drain between points
	}
	if res.BestMbps > 0 && len(res.Points) > 0 && res.Points[0].Streams == 1 {
		res.SingleStreamEfficiency = stats.Round(res.Points[0].Mbps/res.BestMbps, 3)
	}
	return res, nil
}
