package endpoint

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/guard"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
)

// TCPPrepare asks the endpoint to take part in a TCP test.
type TCPPrepare struct {
	Role         string `json:"role"` // endpoint's role: send, recv, hold
	Streams      int    `json:"streams"`
	DurationMs   int    `json:"duration_ms"`
	IntervalMs   int    `json:"interval_ms"`
	Congestion   string `json:"congestion,omitempty"`
	PacingBps    uint64 `json:"pacing_bps,omitempty"` // per stream, bytes/s
	BufferBytes  int    `json:"buffer_bytes,omitempty"`
	Compressible bool   `json:"compressible,omitempty"`
	Live         bool   `json:"live,omitempty"`
	// Bytes, if > 0, makes a sending endpoint stop after this many bytes
	// per stream (short-flow tests).
	Bytes uint64 `json:"bytes,omitempty"`
	// CDP, if set, makes a sending endpoint replicate simulated changed
	// data (continuous data protection) across its streams instead of bulk
	// data; CDPLimitBps caps it like a replication throttle.
	CDP         *flow.CDPModel `json:"cdp,omitempty"`
	CDPLimitBps float64        `json:"cdp_limit_bps,omitempty"`
}

// TCPPrepared lists the stream IDs to connect.
type TCPPrepared struct {
	TestID  string   `json:"test_id"`
	Streams []uint32 `json:"streams"`
}

// TCPStreamResult is the endpoint's view of one stream.
type TCPStreamResult struct {
	Stream     uint32              `json:"stream"`
	Bytes      uint64              `json:"bytes"`
	Congestion string              `json:"congestion"`
	Final      netx.TCPInfoSummary `json:"final"`
}

// TCPResult is the endpoint's view of a TCP test.
type TCPResult struct {
	TestID     string            `json:"test_id"`
	Role       string            `json:"role"`
	IntervalMs int               `json:"interval_ms"`
	Streams    []TCPStreamResult `json:"streams"`
	Samples    []flow.Sample     `json:"samples"`
	Error      string            `json:"error,omitempty"`
	CDP        *flow.CDPResult   `json:"cdp,omitempty"` // CDP sends only
}

// TCPSampleEvent streams live samples to the client.
type TCPSampleEvent struct {
	TestID string      `json:"test_id"`
	Sample flow.Sample `json:"sample"`
}

type tcpTest struct {
	ss       *session
	id       string
	p        TCPPrepare
	ids      []uint32
	streams  []*flow.Stream
	mu       sync.Mutex
	attached int
	started  bool
	allIn    chan struct{}
	done     chan struct{}
	sampler  flow.Sampler
	err      error
	timer    *time.Timer
	cdp      *flow.CDPResult
}

func (ss *session) tcpPrepare(p TCPPrepare) (TCPPrepared, error) {
	if p.Streams < 1 || p.Streams > 128 {
		return TCPPrepared{}, errors.New("streams must be 1-128")
	}
	if p.Role != "send" && p.Role != "recv" && p.Role != "hold" {
		return TCPPrepared{}, errors.New("role must be send, recv or hold")
	}
	if p.CDP != nil {
		if p.CDP.RateBps <= 0 {
			return TCPPrepared{}, errors.New("CDP rate must be positive")
		}
		_, peak := p.CDP.Rates()
		if err := ss.checkRate(math.Max(peak*8, p.CDPLimitBps)); err != nil {
			return TCPPrepared{}, err
		}
	}
	if err := ss.checkDuration(time.Duration(p.DurationMs) * time.Millisecond); err != nil {
		return TCPPrepared{}, err
	}
	if p.PacingBps > 0 {
		if err := ss.checkRate(float64(p.PacingBps) * 8 * float64(p.Streams)); err != nil {
			return TCPPrepared{}, err
		}
	}
	idb := make([]byte, 8)
	copy(idb, proto.NewNonce())
	t := &tcpTest{
		ss: ss, id: hex.EncodeToString(idb), p: p,
		ids: ss.allocStreams(p.Streams), streams: make([]*flow.Stream, p.Streams),
		allIn: make(chan struct{}), done: make(chan struct{}),
	}
	t.sampler.Interval = time.Duration(max(p.IntervalMs, 20)) * time.Millisecond
	// Start with whatever connected if some streams never arrive. The timer
	// exists before the test is published, because start() stops it; it is
	// set under the lock start() reads it under.
	t.mu.Lock()
	t.timer = time.AfterFunc(attachTimeout, func() {
		defer func() { guard.Recovered(ss.srv.log, "TCP test start", recover()) }()
		t.start()
	})
	t.mu.Unlock()
	ss.mu.Lock()
	ss.tests[t.id] = t
	for _, id := range t.ids {
		ss.slots[id] = t
	}
	ss.mu.Unlock()
	return TCPPrepared{TestID: t.id, Streams: t.ids}, nil
}

// attachTimeout is how long a test waits for its data connections before
// starting with the ones that arrived.
var attachTimeout = 10 * time.Second

func (ss *session) attachStream(id uint32, c *net.TCPConn) {
	ss.mu.Lock()
	t := ss.slots[id]
	delete(ss.slots, id)
	ss.mu.Unlock()
	if t == nil {
		c.Close()
		return
	}
	idx := -1
	for i, sid := range t.ids {
		if sid == id {
			idx = i
		}
	}
	_ = netx.SetCongestion(c, t.p.Congestion)
	_ = netx.SetPacingRate(c, t.p.PacingBps)
	netx.SetBuffers(c, t.p.BufferBytes)
	t.mu.Lock()
	// A test that has started (all streams in, or the wait for them ran
	// out) has a fixed set of streams: start() rebuilt t.streams without
	// the missing ones, so a late connection has no place in it.
	if t.started || idx < 0 || idx >= len(t.streams) || t.streams[idx] != nil {
		t.mu.Unlock()
		c.Close()
		return
	}
	t.streams[idx] = &flow.Stream{ID: id, Conn: c}
	t.attached++
	all := t.attached == len(t.ids)
	t.mu.Unlock()
	if all {
		t.start()
	}
}

func (t *tcpTest) start() {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.timer.Stop()
	var streams []*flow.Stream
	for _, s := range t.streams {
		if s != nil {
			streams = append(streams, s)
		}
	}
	t.streams = streams
	t.mu.Unlock()
	// Streams that never arrived can no longer join.
	t.ss.mu.Lock()
	for _, id := range t.ids {
		if t.ss.slots[id] == t {
			delete(t.ss.slots, id)
		}
	}
	t.ss.mu.Unlock()
	close(t.allIn)
	if len(streams) == 0 {
		t.err = errors.New("no data connections arrived")
		close(t.done)
		return
	}
	go t.run(streams)
}

func (t *tcpTest) run(streams []*flow.Stream) {
	defer close(t.done)
	log := t.ss.srv.log
	defer func() {
		if guard.Recovered(log, "TCP test", recover()) {
			t.err = errors.New("internal error in the endpoint (see its log)")
		}
	}()
	if t.p.Role == "hold" {
		<-t.ss.ctx.Done()
		return
	}
	d := time.Duration(t.p.DurationMs) * time.Millisecond
	var ctx context.Context
	var cancel context.CancelFunc
	if t.p.Role == "send" {
		ctx, cancel = context.WithTimeout(t.ss.ctx, d)
	} else {
		// The client closes its streams; allow grace for late data.
		ctx, cancel = context.WithTimeout(t.ss.ctx, d+10*time.Second)
	}
	defer cancel()
	var onSample func(flow.Sample)
	if t.p.Live {
		onSample = func(s flow.Sample) { t.ss.event("tcp_sample", TCPSampleEvent{TestID: t.id, Sample: s}) }
	}
	sctx, scancel := context.WithCancel(ctx)
	sampDone := make(chan struct{})
	go func() {
		defer close(sampDone)
		defer func() { guard.Recovered(log, "TCP test sampler", recover()) }()
		t.sampler.Run(sctx, streams, onSample)
	}()
	if t.p.Role == "send" && t.p.CDP != nil {
		res := flow.SendCDP(ctx, streams, *t.p.CDP, t.p.CDPLimitBps, d)
		t.cdp = &res
		for _, s := range streams {
			_ = s.Conn.CloseWrite()
		}
		scancel()
		<-sampDone
		return
	}
	var wg sync.WaitGroup
	for _, s := range streams {
		wg.Add(1)
		go func(s *flow.Stream) {
			defer wg.Done()
			defer func() { guard.Recovered(log, "TCP test stream", recover()) }()
			if t.p.Role == "send" {
				_ = s.Send(ctx, 128<<10, t.p.Compressible, t.p.Bytes)
				_ = s.Conn.CloseWrite()
			} else {
				_ = s.Receive(ctx)
			}
		}(s)
	}
	wg.Wait()
	scancel()
	<-sampDone
}

func (t *tcpTest) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.streams {
		if s != nil && s.Conn != nil {
			s.Conn.Close()
		}
	}
}

func (ss *session) tcpResult(id string, wait time.Duration) (TCPResult, error) {
	ss.mu.Lock()
	t := ss.tests[id]
	ss.mu.Unlock()
	if t == nil {
		return TCPResult{}, errors.New("unknown test")
	}
	if t.p.Role == "hold" {
		return ss.connInfo(t), nil
	}
	select {
	case <-t.done:
	case <-time.After(wait):
		return TCPResult{}, errors.New("timed out waiting for the test to finish")
	}
	r := TCPResult{TestID: id, Role: t.p.Role, IntervalMs: int(t.sampler.Interval / time.Millisecond), Samples: t.sampler.Samples()}
	if t.err != nil {
		r.Error = t.err.Error()
	}
	r.CDP = t.cdp
	for _, s := range t.streams {
		sr := TCPStreamResult{Stream: s.ID, Bytes: s.Bytes(), Congestion: netx.Congestion(s.Conn)}
		if ti, err := netx.TCPInfo(s.Conn); err == nil {
			sr.Final = netx.Summarize(ti)
		}
		r.Streams = append(r.Streams, sr)
		s.Conn.Close()
	}
	ss.mu.Lock()
	delete(ss.tests, id)
	ss.mu.Unlock()
	return r, nil
}

// connInfo reports TCP_INFO for held connections (middlebox checks).
func (ss *session) connInfo(t *tcpTest) TCPResult {
	select {
	case <-t.allIn:
	case <-time.After(5 * time.Second):
	}
	r := TCPResult{TestID: t.id, Role: "hold"}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.streams {
		if s == nil {
			continue
		}
		sr := TCPStreamResult{Stream: s.ID, Congestion: netx.Congestion(s.Conn)}
		if ti, err := netx.TCPInfo(s.Conn); err == nil {
			sr.Final = netx.Summarize(ti)
		}
		r.Streams = append(r.Streams, sr)
	}
	return r
}
