package flow

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/netx"
)

// CDPModel describes simulated continuous data protection (CDP)
// replication, such as Veeam CDP: changed data is produced continuously as
// virtual machines write, with periodic bursts, and must reach the target
// site within the RPO. The rate is the data on the wire, so it should be
// the change rate after the replication software's compression.
type CDPModel struct {
	RateBps     float64 `json:"rate_bps"`     // average rate of changed data, bits/s
	BurstFactor float64 `json:"burst_factor"` // write-burst rate as a multiple of the average (default 3)
	BurstS      float64 `json:"burst_s"`      // length of each burst, s (default 10)
	PeriodS     float64 `json:"period_s"`     // time from one burst to the next, s (default 60)
}

// WithDefaults fills unset fields and keeps the burst feasible (the rate
// outside bursts can't go below zero).
func (m CDPModel) WithDefaults() CDPModel {
	if m.BurstFactor < 1 {
		m.BurstFactor = 3
	}
	if m.BurstS <= 0 {
		m.BurstS = 10
	}
	if m.PeriodS <= 0 {
		m.PeriodS = 60
	}
	if m.BurstS > m.PeriodS {
		m.BurstS = m.PeriodS
	}
	m.BurstFactor = math.Min(m.BurstFactor, m.PeriodS/m.BurstS)
	return m
}

// Rates returns the rate outside and during bursts, in bytes/s.
func (m CDPModel) Rates() (base, peak float64) {
	m = m.WithDefaults()
	avg := m.RateBps / 8
	peak = avg * m.BurstFactor
	if m.PeriodS == m.BurstS {
		return peak, peak
	}
	base = (avg*m.PeriodS - peak*m.BurstS) / (m.PeriodS - m.BurstS)
	return math.Max(0, base), peak
}

// burstStart is where the burst sits within each period (in the middle, so
// a test of one period sees a whole burst).
func (m CDPModel) burstStart() float64 { return (m.PeriodS - m.BurstS) / 2 }

// Generated returns the bytes of changed data produced by time t (seconds).
func (m CDPModel) Generated(t float64) float64 {
	m = m.WithDefaults()
	if t <= 0 {
		return 0
	}
	base, peak := m.Rates()
	perPeriod := base*(m.PeriodS-m.BurstS) + peak*m.BurstS
	k := math.Floor(t / m.PeriodS)
	x := t - k*m.PeriodS
	b0 := m.burstStart()
	b1 := b0 + m.BurstS
	var in float64
	switch {
	case x <= b0:
		in = base * x
	case x <= b1:
		in = base*b0 + peak*(x-b0)
	default:
		in = base*b0 + peak*m.BurstS + base*(x-b1)
	}
	return k*perPeriod + in
}

// GenTime returns when byte b of the changed data was produced.
func (m CDPModel) GenTime(b float64) float64 {
	m = m.WithDefaults()
	if b < 0 {
		return 0
	}
	// b == 0 is not a shortcut: when nothing is produced between bursts the
	// first byte appears when the first burst starts, not at time 0.
	base, peak := m.Rates()
	perPeriod := base*(m.PeriodS-m.BurstS) + peak*m.BurstS
	if perPeriod <= 0 {
		return math.Inf(1)
	}
	k := math.Floor(b / perPeriod)
	r := b - k*perPeriod
	b0 := m.burstStart()
	pre := base * b0
	burst := peak * m.BurstS
	var x float64
	switch {
	case r <= pre && base > 0:
		x = r / base
	case r <= pre+burst:
		x = b0 + (r-pre)/peak
	case base > 0:
		x = b0 + m.BurstS + (r-pre-burst)/base
	default:
		x = b0 + m.BurstS
	}
	return k*m.PeriodS + x
}

// CDPPoint is one second of a CDP run.
type CDPPoint struct {
	T        float64 `json:"t"`
	LagS     float64 `json:"lag_s"`     // worst replication lag during the second
	GenMbps  float64 `json:"gen_mbps"`  // changed data produced
	SentMbps float64 `json:"sent_mbps"` // changed data handed to TCP
}

// CDPResult is what a CDP sender measured.
type CDPResult struct {
	Seconds   float64    `json:"seconds"`
	Generated uint64     `json:"generated"` // bytes of changed data produced
	Sent      uint64     `json:"sent"`      // bytes sent
	Points    []CDPPoint `json:"points"`
	// Lag is the replication lag every 100 ms, in seconds: the age of the
	// oldest change not yet sent. It is the RPO the replica would have had.
	Lag []float64 `json:"lag"`
}

// cdpChunk is how much changed data one write carries.
const cdpChunk = 64 << 10

// SendCDP replicates a CDPModel's changed data over streams for d. Each
// stream sends the oldest unsent changes as fast as TCP takes them;
// limitBps (if > 0) caps the total rate like a replication throttle (a
// token bucket holding at most 100 ms of allowance, so quiet periods don't
// save up a burst). The kernel
// holds at most a little unsent data per socket (TCP_NOTSENT_LOWAT), so the
// backlog, and the lag, is measured here.
func SendCDP(ctx context.Context, streams []*Stream, m CDPModel, limitBps float64, d time.Duration) CDPResult {
	m = m.WithDefaults()
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for _, s := range streams {
		_ = netx.SetNotSentLowat(s.Conn, 128<<10)
		stop := context.AfterFunc(ctx, func() { _ = s.Conn.SetWriteDeadline(time.Unix(1, 0)) })
		defer stop()
	}
	start := time.Now()
	var mu sync.Mutex
	var claimed, sent uint64
	inflight := map[int]uint64{} // stream index -> start of the chunk being written
	// Throttle: a token bucket in bytes.
	bucketMax := math.Max(limitBps/8*0.1, 16<<10)
	tokens, lastRefill := 0.0, 0.0
	refill := func(t float64) {
		tokens = math.Min(bucketMax, tokens+(t-lastRefill)*limitBps/8)
		lastRefill = t
	}
	pool := Pool()
	var wg sync.WaitGroup
	for i, s := range streams {
		wg.Add(1)
		go func(i int, s *Stream) {
			defer wg.Done()
			off := (i * 7919 * 4099) % (len(pool) - cdpChunk)
			for ctx.Err() == nil {
				mu.Lock()
				t := time.Since(start).Seconds()
				a := uint64(m.Generated(t))
				if claimed >= a {
					mu.Unlock()
					sleepUntil(ctx, time.Now().Add(time.Millisecond))
					continue
				}
				n := min(uint64(cdpChunk), a-claimed)
				if limitBps > 0 {
					refill(t)
					if tokens < 1024 {
						mu.Unlock()
						sleepUntil(ctx, time.Now().Add(time.Millisecond))
						continue
					}
					n = min(n, uint64(tokens))
					tokens -= float64(n)
				}
				inflight[i] = claimed
				claimed += n
				mu.Unlock()
				w, err := s.Conn.Write(pool[off : off+int(n)])
				s.bytes.Add(uint64(w))
				mu.Lock()
				delete(inflight, i)
				sent += uint64(w)
				if uint64(w) < n {
					// Hand the unsent part back (the test is ending or the
					// connection failed).
					claimed -= n - uint64(w)
				}
				mu.Unlock()
				if err != nil {
					return
				}
				off += 4099
				if off >= len(pool)-cdpChunk {
					off %= 65536
				}
			}
		}(i, s)
	}

	var res CDPResult
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var secLag float64
	var lastSent uint64
	lastGen := 0.0
	sec := 1
	sample := func() {
		t := time.Since(start).Seconds()
		mu.Lock()
		oldest := claimed
		for _, o := range inflight {
			oldest = min(oldest, o)
		}
		s := sent
		mu.Unlock()
		lag := 0.0
		if gen := m.Generated(t); float64(oldest) < gen-1 {
			lag = math.Max(0, t-m.GenTime(float64(oldest)))
		}
		res.Lag = append(res.Lag, math.Round(lag*1000)/1000)
		secLag = math.Max(secLag, lag)
		if t >= float64(sec) {
			gen := m.Generated(float64(sec))
			res.Points = append(res.Points, CDPPoint{T: float64(sec), LagS: math.Round(secLag*100) / 100,
				GenMbps: math.Round((gen-lastGen)*8/1e4) / 100, SentMbps: math.Round(float64(s-lastSent)*8/1e4) / 100})
			lastGen, lastSent, secLag = gen, s, 0
			sec++
		}
	}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			sample()
		}
	}
	wg.Wait()
	res.Seconds = time.Since(start).Seconds()
	res.Generated = uint64(m.Generated(math.Min(res.Seconds, d.Seconds())))
	mu.Lock()
	res.Sent = sent
	mu.Unlock()
	return res
}
