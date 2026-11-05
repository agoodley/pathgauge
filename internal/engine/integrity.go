package engine

import (
	"context"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/results"
)

// DSCP code points tested, by name.
var dscpValues = []struct {
	name string
	dscp int
}{
	{"CS0 (best effort)", 0}, {"CS1", 8}, {"AF11", 10}, {"AF21", 18}, {"AF31", 26},
	{"AF41", 34}, {"CS5", 40}, {"EF (voice)", 46}, {"CS6", 48}, {"CS7", 56},
}

// RunIntegrity checks what middleboxes change: DSCP and ECN marks, TCP MSS
// and options, NAT, and hop counts from TTLs.
func RunIntegrity(ctx context.Context, c *Client) (*results.Integrity, error) {
	res := &results.Integrity{}

	// DSCP and ECN, client → endpoint.
	var ready endpoint.UDPSinkReady
	if err := c.Call(ctx, "udp_sink", endpoint.UDPSinkReq{}, &ready); err != nil {
		return nil, err
	}
	sg := c.Signer()
	var sent uint64
	tosList := tosProbes()
	for _, tos := range tosList {
		r := flow.SendUDP(ctx, sg, flow.UDPSendParams{Session: c.Session(), Stream: ready.Stream, RateBps: 1e6, Size: 200, TOS: tos, Count: 5}, c.SendUDP)
		sent += r.Sent
	}
	sleepCtx(ctx, 300*time.Millisecond)
	var upSink flow.SinkResult
	if err := c.Call(ctx, "udp_sink_result", endpoint.UDPSinkResultReq{Stream: ready.Stream, Sent: sent, Close: true}, &upSink); err != nil {
		return nil, err
	}
	res.DSCPUp, res.ECNUp = classifyTOS(upSink.TOSPairs)

	// DSCP and ECN, endpoint → client.
	stream := c.LocalStream()
	sink := flow.NewUDPSink(0)
	c.AddSink(stream, sink)
	c.punch()
	for _, tos := range tosList {
		if err := c.Call(ctx, "udp_send", endpoint.UDPSendReq{Stream: stream, RateBps: 1e6, Size: 200, TOS: tos, Count: 5, Wait: true}, nil); err != nil {
			c.AddSink(stream, nil)
			return nil, err
		}
	}
	sleepCtx(ctx, 300*time.Millisecond)
	c.AddSink(stream, nil)
	down := sink.Result(0)
	res.DSCPDown, res.ECNDown = classifyTOS(down.TOSPairs)

	// TCP MSS and options: hold one connection and compare both views.
	var prep endpoint.TCPPrepared
	if err := c.Call(ctx, "tcp_prepare", endpoint.TCPPrepare{Role: "hold", Streams: 1, DurationMs: 1000}, &prep); err != nil {
		return nil, err
	}
	conn, err := c.DialData(ctx, prep.Streams[0], DialOptions{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	sleepCtx(ctx, 200*time.Millisecond)
	local, _ := netx.TCPInfo(conn)
	var held endpoint.TCPResult
	if err := c.Call(ctx, "tcp_result", map[string]any{"test_id": prep.TestID}, &held); err != nil {
		return nil, err
	}
	if local != nil && len(held.Streams) > 0 {
		li, ri := netx.Summarize(local), held.Streams[0].Final
		res.ClientAdvMSS, res.ClientSeesMSS = li.AdvMSS, li.SndMSS
		res.EndpointAdvMSS, res.EndpointSeesMSS = ri.AdvMSS, ri.SndMSS
		ts := uint32(0)
		if li.Options&netx.OptTimestamps != 0 {
			ts = 12
		}
		// The endpoint's send MSS derives from the MSS option in our SYN;
		// if it is well below what we advertised (and what the endpoint
		// could use), a middlebox rewrote it on the way up.
		// Each side's send MSS is bounded by the MSS option it received and
		// by its own cached path MTU (which a reduced-MTU path, or our own
		// PMTU probes, may have lowered). Only a send MSS below both points
		// to a middlebox rewriting the MSS option. Jumbo/loopback MTUs are
		// ignored: there the kernel bounds the MSS by half the window.
		hdr := uint32(40)
		if !c.EndpointIP.Is4() {
			hdr = 60
		}
		bound := func(peerAdv, ownAdv, pmtu uint32) uint32 {
			b := min(peerAdv, ownAdv)
			if pmtu > hdr && pmtu-hdr < b {
				b = pmtu - hdr
			}
			return b
		}
		if min(li.AdvMSS, ri.AdvMSS) <= 8960 {
			upBound := bound(li.AdvMSS, ri.AdvMSS, ri.PMTU)
			downBound := bound(ri.AdvMSS, li.AdvMSS, li.PMTU)
			res.MSSClampedUp = ri.SndMSS > 0 && ri.SndMSS+ts+40 < upBound
			res.MSSClampedDown = li.SndMSS > 0 && li.SndMSS+ts+40 < downBound
		}
		res.ClientOptions = netx.OptionNames(li.Options)
		res.EndpointOptions = netx.OptionNames(ri.Options)
		for opt, sysctl := range map[string]string{"timestamps": "tcp_timestamps", "sack": "tcp_sack", "window_scaling": "tcp_window_scaling"} {
			if sysctlOn(sysctl) && !slices.Contains(res.ClientOptions, opt) {
				res.StrippedOptions = append(res.StrippedOptions, opt)
			}
		}
		slices.Sort(res.StrippedOptions)
	}

	// NAT: compare our local address with what the endpoint observed.
	res.ClientLocal, res.ClientObserved = c.Local, c.Observed
	lap, err1 := netip.ParseAddrPort(c.Local)
	oap, err2 := netip.ParseAddrPort(c.Observed)
	if err1 == nil && err2 == nil {
		res.NAT = lap.Addr().Unmap() != oap.Addr().Unmap()
		res.PortPreserved = lap.Port() == oap.Port()
		res.CGNAT = netip.MustParsePrefix("100.64.0.0/10").Contains(oap.Addr().Unmap())
	}

	// TTLs: endpoint's view of our probes, our view of its replies.
	p := newProber(c, 0)
	for i := 0; i < 5; i++ {
		p.send()
		sleepCtx(ctx, 20*time.Millisecond)
	}
	sleepCtx(ctx, 300*time.Millisecond)
	p.close()
	var ps endpoint.ProbeStats
	if err := c.Call(ctx, "probe_stats", nil, &ps); err == nil {
		res.TTLUp = commonValue(ps.TTL)
		res.HopsUp = hopsFromTTL(res.TTLUp)
	}
	res.TTLDown = commonValue(c.ReplyTTLs())
	res.HopsDown = hopsFromTTL(res.TTLDown)
	return res, nil
}

// tosProbes lists TOS bytes to send: each DSCP with Not-ECT, plus ECT(0),
// ECT(1) and CE on best effort.
func tosProbes() []int {
	var out []int
	for _, d := range dscpValues {
		out = append(out, d.dscp<<2)
	}
	return append(out, 0x02, 0x01, 0x03)
}

func classifyTOS(pairs [][3]int) ([]results.DSCPResult, string) {
	recv := map[int]map[int]int{}
	for _, p := range pairs {
		if recv[p[0]] == nil {
			recv[p[0]] = map[int]int{}
		}
		recv[p[0]][p[1]] += p[2]
	}
	most := func(sent int) int {
		best, bestN := -1, 0
		for r, n := range recv[sent] {
			if n > bestN || (n == bestN && r < best) {
				best, bestN = r, n
			}
		}
		return best
	}
	var out []results.DSCPResult
	for _, d := range dscpValues {
		tos := d.dscp << 2
		r := most(tos)
		dr := results.DSCPResult{Name: d.name, Sent: d.dscp, Received: -1, Status: "lost"}
		if r >= 0 {
			dr.Received = r >> 2
			switch dr.Received {
			case d.dscp:
				dr.Status = "preserved"
			case 0:
				dr.Status = "bleached"
			default:
				dr.Status = "remarked"
			}
		}
		out = append(out, dr)
	}
	ecn := "untested"
	if r := most(0x02); r >= 0 {
		switch r & 3 {
		case 2:
			ecn = "preserved"
		case 0:
			ecn = "bleached"
		case 3:
			ecn = "CE-marked"
		default:
			ecn = "altered"
		}
	} else if len(recv) > 0 {
		ecn = "ECT packets lost"
	}
	return out, ecn
}

// hopsFromTTL guesses hop count from a received TTL, assuming the usual
// initial TTLs of 64, 128 or 255.
func hopsFromTTL(ttl int) int {
	if ttl <= 0 {
		return 0
	}
	for _, init := range []int{64, 128, 255} {
		if ttl <= init {
			return init - ttl
		}
	}
	return 0
}

func sysctlOn(name string) bool {
	b, err := os.ReadFile("/proc/sys/net/ipv4/" + name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) != "0"
}
