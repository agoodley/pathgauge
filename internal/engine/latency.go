package engine

import (
	"context"
	"math"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// LatencyParams controls the idle latency test.
type LatencyParams struct {
	Duration   time.Duration
	RateHz     float64
	Size       int
	ICMP       bool
	TCPConnect bool
	// TrustOWD reports one-way delay (both clocks synchronised).
	TrustOWD bool
}

// probeSample is one reflected probe.
type probeSample struct {
	seq      uint64
	sent     time.Time
	rttMs    float64
	upMs     float64 // raw t2 - t1 (includes clock offset)
	downMs   float64 // raw t4 - t3 (includes clock offset)
	got      bool
	offsetMs float64
}

// prober sends probes on one stream and collects replies.
type prober struct {
	c      *Client
	stream uint32
	size   int
	mu     sync.Mutex
	sent   map[uint64]*probeSample
	order  []*probeSample
	sg     *proto.Signer
	buf    []byte
	seq    uint64
	onRTT  func(s *probeSample)
	tos    int // TOS byte for probes, -1 for unset
}

func newProber(c *Client, size int) *prober {
	if size < proto.UDPHeaderLen {
		size = proto.UDPHeaderLen
	}
	p := &prober{c: c, stream: c.LocalStream(), size: size, sent: map[uint64]*probeSample{}, sg: c.Signer(), buf: make([]byte, size), tos: -1}
	c.OnReply(p.stream, func(h proto.UDPHeader, rx time.Time, _ netx.RecvInfo) {
		p.mu.Lock()
		s := p.sent[h.Seq]
		if s == nil || s.got {
			p.mu.Unlock()
			return
		}
		t4 := rx.UnixNano()
		s.got = true
		s.rttMs = float64((t4-h.T1)-(h.T3-h.T2)) / 1e6
		s.upMs = float64(h.T2-h.T1) / 1e6
		s.downMs = float64(t4-h.T3) / 1e6
		s.offsetMs = (float64(h.T2-h.T1) + float64(h.T3-t4)) / 2 / 1e6
		fn := p.onRTT
		p.mu.Unlock()
		if fn != nil {
			fn(s)
		}
	})
	return p
}

func (p *prober) close() { p.c.OnReply(p.stream, nil) }

func (p *prober) send() *probeSample {
	p.mu.Lock()
	s := &probeSample{seq: p.seq, sent: time.Now()}
	p.sent[p.seq] = s
	p.order = append(p.order, s)
	h := proto.UDPHeader{Type: proto.UDPProbe, Session: p.c.Session(), Seq: p.seq, Stream: p.stream, T1: s.sent.UnixNano()}
	if p.tos >= 0 {
		h.TOSSent = byte(p.tos)
	}
	p.seq++
	p.sg.Encode(p.buf, &h)
	b, tos := p.buf, p.tos
	p.mu.Unlock()
	_ = p.c.SendUDP(b, tos)
	return s
}

// run sends probes at hz until ctx ends.
func (p *prober) run(ctx context.Context, hz float64) {
	if hz <= 0 {
		hz = 20
	}
	t := time.NewTicker(time.Duration(float64(time.Second) / hz))
	defer t.Stop()
	p.send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.send()
		}
	}
}

func (p *prober) samples() []*probeSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*probeSample(nil), p.order...)
}

// RunLatency measures idle round-trip time, jitter and loss.
func RunLatency(ctx context.Context, c *Client, lp LatencyParams) (*results.Latency, error) {
	if lp.Duration <= 0 {
		lp.Duration = 10 * time.Second
	}
	if lp.RateHz <= 0 {
		lp.RateHz = 20
	}
	res := &results.Latency{DurationS: lp.Duration.Seconds(), RateHz: lp.RateHz}
	p := newProber(c, lp.Size)
	defer p.close()

	var wg sync.WaitGroup
	var icmpRTT []float64
	var icmpSent, icmpRecv int
	var icmpNote string
	var connect []float64
	runCtx, cancel := context.WithTimeout(ctx, lp.Duration)
	defer cancel()
	if lp.ICMP {
		wg.Add(1)
		go func() {
			defer wg.Done()
			icmpRTT, icmpSent, icmpRecv, icmpNote = ping(runCtx, c.EndpointIP, 200*time.Millisecond)
		}()
	}
	if lp.TCPConnect {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					if d, err := c.ConnectTime(runCtx); err == nil {
						connect = append(connect, float64(d.Microseconds())/1000)
					}
				}
			}
		}()
	}
	p.run(runCtx, lp.RateHz)
	wg.Wait()
	// Allow late replies.
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
	}

	p.close()
	all := p.samples()
	p.mu.Lock()
	defer p.mu.Unlock()
	var rtts, ups, downs, series []float64
	var seq stats.SeqTracker
	var jUp, jDown stats.Jitter
	best := math.Inf(1)
	for _, s := range all {
		if !s.got {
			series = append(series, -1)
			continue
		}
		seq.Add(s.seq)
		rtts = append(rtts, s.rttMs)
		ups = append(ups, s.upMs)
		downs = append(downs, s.downMs)
		series = append(series, stats.Round(s.rttMs, 3))
		jUp.Add(s.upMs)
		jDown.Add(s.downMs)
		if s.rttMs < best {
			best = s.rttMs
			res.ClockOffsetMs = s.offsetMs
		}
	}
	res.UDP = roundSum(stats.Summarize(rtts))
	res.Loss = seq.Loss(uint64(len(all)))
	res.IPDVms = stats.Round(stats.IPDV(rtts), 3)
	var jr stats.Jitter
	for _, r := range rtts {
		jr.Add(r)
	}
	res.UDPJitterMs = stats.Round(jr.Value(), 3)
	res.UpJitterMs = stats.Round(jUp.Value(), 3)
	res.DownJitterMs = stats.Round(jDown.Value(), 3)
	res.ClockOffsetMs = stats.Round(res.ClockOffsetMs, 3)
	if lp.TrustOWD && len(ups) > 0 {
		u, d := roundSum(stats.Summarize(ups)), roundSum(stats.Summarize(downs))
		res.OWDUp, res.OWDDown = &u, &d
	}
	res.Series = series
	if lp.ICMP {
		if len(icmpRTT) > 0 {
			s := roundSum(stats.Summarize(icmpRTT))
			res.ICMP = &s
		}
		if icmpSent > 0 {
			res.ICMPLossPct = stats.Round(100*float64(icmpSent-icmpRecv)/float64(icmpSent), 2)
		}
		res.ICMPNote = icmpNote
	}
	if len(connect) > 0 {
		s := roundSum(stats.Summarize(connect))
		res.TCPConnect = &s
	}
	return res, nil
}

func roundSum(s stats.Summary) stats.Summary {
	r := func(x float64) float64 { return stats.Round(x, 3) }
	return stats.Summary{N: s.N, Min: r(s.Min), Mean: r(s.Mean), Median: r(s.Median), P90: r(s.P90), P95: r(s.P95), P99: r(s.P99), Max: r(s.Max), StdDev: r(s.StdDev)}
}

// ping sends ICMP echo requests using an unprivileged ping socket where the
// kernel allows it, or a raw socket with CAP_NET_RAW.
func ping(ctx context.Context, target netip.Addr, interval time.Duration) (rtts []float64, sent, recv int, note string) {
	var c *icmp.PacketConn
	var err error
	v6 := target.Is6()
	privileged := false
	if v6 {
		c, err = icmp.ListenPacket("udp6", "::")
		if err != nil {
			c, err = icmp.ListenPacket("ip6:ipv6-icmp", "::")
			privileged = true
		}
	} else {
		c, err = icmp.ListenPacket("udp4", "0.0.0.0")
		if err != nil {
			c, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0")
			privileged = true
		}
	}
	if err != nil {
		return nil, 0, 0, "ICMP unavailable to this user (needs ping_group_range or CAP_NET_RAW)"
	}
	defer c.Close()
	id := os.Getpid() & 0xffff
	var mu sync.Mutex
	sentAt := map[int]time.Time{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, _, err := c.ReadFrom(buf)
			now := time.Now()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			proto := 1
			if v6 {
				proto = 58
			}
			m, err := icmp.ParseMessage(proto, buf[:n])
			if err != nil {
				continue
			}
			echo, ok := m.Body.(*icmp.Echo)
			if !ok || (m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply) {
				continue
			}
			if privileged && echo.ID != id {
				continue
			}
			mu.Lock()
			if t, ok := sentAt[echo.Seq]; ok {
				rtts = append(rtts, float64(now.Sub(t).Microseconds())/1000)
				recv++
				delete(sentAt, echo.Seq)
			}
			mu.Unlock()
		}
	}()
	var dst net.Addr = &net.UDPAddr{IP: target.AsSlice()}
	if privileged {
		dst = &net.IPAddr{IP: target.AsSlice()}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for seq := 1; ctx.Err() == nil; seq++ {
		var typ icmp.Type = ipv4.ICMPTypeEcho
		if v6 {
			typ = ipv6.ICMPTypeEchoRequest
		}
		msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: id, Seq: seq & 0xffff, Data: []byte("PATHGAUGE")}}
		b, _ := msg.Marshal(nil)
		mu.Lock()
		sentAt[seq&0xffff] = time.Now()
		mu.Unlock()
		if _, err := c.WriteTo(b, dst); err == nil {
			sent++
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	time.Sleep(500 * time.Millisecond)
	_ = c.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if sent > 0 && recv == 0 {
		note = "no ICMP echo replies (ICMP filtered on the path or at the endpoint)"
	}
	return rtts, sent, recv, note
}
