package trace

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"

	"github.com/agoodley/pathgauge/internal/netx"
)

// ---- unprivileged: one socket per probe, ICMP via the error queue ----

type simple struct {
	target netip.Addr
	o      Options
}

func newSimple(target netip.Addr, o Options) *simple { return &simple{target: target, o: o} }

func (s *simple) name() string { return "udp" }
func (s *simple) close()       {}

func (s *simple) cycle(ctx context.Context, maxTTL int) []reply {
	var mu sync.Mutex
	var out []reply
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for ttl := 1; ttl <= maxTTL; ttl++ {
		for p := 0; p < s.o.Probes; p++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(ttl, p int) {
				defer wg.Done()
				defer func() { <-sem }()
				r := s.probe(ctx, ttl, s.o.Port+ttl*s.o.Probes+p)
				mu.Lock()
				out = append(out, r)
				mu.Unlock()
			}(ttl, p)
		}
	}
	wg.Wait()
	return out
}

func (s *simple) probe(ctx context.Context, ttl, port int) reply {
	r := reply{ttl: ttl}
	network := "udp4"
	if s.target.Is6() {
		network = "udp6"
	}
	c, err := net.DialUDP(network, nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(s.target, uint16(port))))
	if err != nil {
		r.errMsg = err.Error()
		return r
	}
	defer c.Close()
	_ = netx.SetTTL(c, ttl)
	_ = netx.EnableRecvErr(c)
	sent := time.Now()
	if _, err := c.Write([]byte("PATHGAUGE-TRACE")); err != nil {
		r.errMsg = err.Error()
		return r
	}
	deadline := sent.Add(s.o.Timeout)
	_ = c.SetReadDeadline(deadline)
	rc, err := c.SyscallConn()
	if err != nil {
		r.errMsg = err.Error()
		return r
	}
	for time.Now().Before(deadline) && ctx.Err() == nil {
		var se netx.SockErr
		var rerr error
		werr := rc.Read(func(fd uintptr) bool {
			se, rerr = netx.ReadErrQueue(c)
			return !errors.Is(rerr, netx.ErrNoError)
		})
		if werr != nil { // deadline
			return r
		}
		if rerr != nil {
			continue
		}
		if !se.Offender.IsValid() {
			continue
		}
		r.from = se.Offender.Unmap()
		r.rtt = se.Time.Sub(sent)
		if r.rtt <= 0 {
			r.rtt = time.Since(sent)
		}
		r.ok = true
		r.dest = se.IsPortUnreachable() || r.from == s.target
		r.blocked = se.IsUnreachable() && !r.dest
		return r
	}
	return r
}

// ---- privileged Paris-style UDP with a raw ICMP listener (IPv4) ----

type paris struct {
	target netip.Addr
	o      Options
	icmp   *icmp.PacketConn
	udp    *net.UDPConn
	lport  int
	dst    netip.AddrPort

	mu      sync.Mutex
	pending map[int]*pendingProbe // by UDP length
}

type pendingProbe struct {
	ttl     int
	sent    time.Time
	done    chan reply
	sendErr string // why the probe could not be sent, if it could not
}

func newParis(target netip.Addr, o Options) (*paris, error) {
	if !target.Is4() {
		return nil, errors.New("udp-paris supports IPv4 only")
	}
	ic, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNeedRaw, err)
	}
	// The socket is not connected on purpose. On a connected UDP socket the
	// kernel turns the destination's port-unreachable into a pending socket
	// error, and the next probe's send fails with ECONNREFUSED instead of
	// going out: a probe never sent, counted as lost. An unconnected socket
	// keeps one source port (so one flow) without that; the raw socket sees
	// the ICMP replies.
	u, err := net.ListenUDP("udp4", nil)
	if err != nil {
		_ = ic.Close()
		return nil, err
	}
	p := &paris{
		target: target, o: o, icmp: ic, udp: u, lport: u.LocalAddr().(*net.UDPAddr).Port,
		dst:     netip.AddrPortFrom(target, uint16(o.Port)),
		pending: map[int]*pendingProbe{},
	}
	go p.readICMP()
	return p, nil
}

func (p *paris) name() string { return "udp-paris" }

func (p *paris) close() {
	_ = p.icmp.Close()
	_ = p.udp.Close()
}

func (p *paris) readICMP() {
	buf := make([]byte, 1500)
	for {
		n, peer, err := p.icmp.ReadFrom(buf)
		if err != nil {
			return
		}
		now := time.Now()
		msg, err := icmp.ParseMessage(1, buf[:n])
		if err != nil {
			continue
		}
		var data []byte
		var exts []icmp.Extension
		unreach := false
		switch b := msg.Body.(type) {
		case *icmp.TimeExceeded:
			data, exts = b.Data, b.Extensions
		case *icmp.DstUnreach:
			data, exts = b.Data, b.Extensions
			unreach = true
		default:
			continue
		}
		length, ok := p.match(data)
		if !ok {
			continue
		}
		p.mu.Lock()
		pp := p.pending[length]
		if pp != nil {
			delete(p.pending, length)
		}
		p.mu.Unlock()
		if pp == nil {
			continue
		}
		from, _ := netip.ParseAddr(peer.(*net.IPAddr).IP.String())
		r := reply{ttl: pp.ttl, from: from.Unmap(), rtt: now.Sub(pp.sent), ok: true}
		// Only the target, or a port-unreachable (the probe reached a host
		// with nothing listening), is the end of the path. Any other
		// unreachable is a device on the way refusing the probe.
		r.dest = r.from == p.target || (unreach && msg.Code == icmpPortUnreachable)
		r.blocked = unreach && !r.dest
		for _, e := range exts {
			if m, ok := e.(*icmp.MPLSLabelStack); ok {
				for _, l := range m.Labels {
					r.mpls = append(r.mpls, fmt.Sprintf("%d", l.Label))
				}
			}
		}
		pp.done <- r
	}
}

// match checks a quoted IPv4+UDP header belongs to us and returns the UDP length.
func (p *paris) match(data []byte) (int, bool) {
	if len(data) < 20 {
		return 0, false
	}
	ihl := int(data[0]&0x0f) * 4
	if len(data) < ihl+8 || data[9] != unix.IPPROTO_UDP {
		return 0, false
	}
	dst := netip.AddrFrom4([4]byte(data[16:20]))
	udp := data[ihl:]
	if dst != p.target || int(binary.BigEndian.Uint16(udp[0:2])) != p.lport || int(binary.BigEndian.Uint16(udp[2:4])) != p.o.Port {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(udp[4:6])), true
}

func (p *paris) cycle(ctx context.Context, maxTTL int) []reply {
	pc := ipv4.NewConn(p.udp)
	var probes []*pendingProbe
	payload := make([]byte, 2+maxTTL*p.o.Probes+8)
	copy(payload, "PG")
	idx := 0
	for ttl := 1; ttl <= maxTTL && ctx.Err() == nil; ttl++ {
		for k := 0; k < p.o.Probes; k++ {
			idx++
			plen := 2 + idx // UDP length = 8 + plen identifies the probe
			pp := &pendingProbe{ttl: ttl, done: make(chan reply, 1)}
			p.mu.Lock()
			p.pending[8+plen] = pp
			p.mu.Unlock()
			_ = pc.SetTTL(ttl)
			pp.sent = time.Now()
			if _, err := p.udp.WriteToUDPAddrPort(payload[:plen], p.dst); err != nil {
				pp.sendErr = err.Error()
			}
			probes = append(probes, pp)
			time.Sleep(2 * time.Millisecond) // gentle on ICMP rate limiters
		}
	}
	out := collect(ctx, probes, p.o.Timeout)
	p.mu.Lock()
	clear(p.pending)
	p.mu.Unlock()
	return out
}

// collect waits up to timeout for the probes' answers. Once the time is up
// (or ctx ends) it still takes every answer that has already arrived: only
// a probe with no answer by then is lost.
func collect(ctx context.Context, probes []*pendingProbe, timeout time.Duration) []reply {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	waiting := true
	out := make([]reply, 0, len(probes))
	for _, pp := range probes {
		if waiting {
			select {
			case r := <-pp.done:
				out = append(out, r)
				continue
			case <-deadline.C:
			case <-ctx.Done():
			}
			waiting = false
		}
		select {
		case r := <-pp.done:
			out = append(out, r)
		default:
			out = append(out, reply{ttl: pp.ttl, errMsg: pp.sendErr})
		}
	}
	return out
}

// icmpPortUnreachable is the ICMPv4 destination-unreachable code a host
// sends when nothing listens on the probed port.
const icmpPortUnreachable = 3
