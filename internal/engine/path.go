package engine

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/trace"
)

// TraceParams controls the two-ended trace.
type TraceParams struct {
	Cycles   int // 1 = traceroute; >1 = MTR statistics
	MaxHops  int
	Resolve  bool
	Method   string
	Annotate func(string) (asn, org string)
}

// RunTrace traces the forward path (from this client) and the reverse
// path (from the endpoint back to this client's observed address) at the
// same time.
func RunTrace(ctx context.Context, c *Client, tp TraceParams) (fwd, rev *results.Trace, err error) {
	var wg sync.WaitGroup
	var revErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		t := trace.Run(ctx, c.EndpointIP, trace.Options{Cycles: tp.Cycles, MaxHops: tp.MaxHops, Resolve: tp.Resolve, Method: tp.Method})
		t.Direction, t.From = results.Up, c.Local
		fwd = &t
	}()
	go func() {
		defer wg.Done()
		var t results.Trace
		wait := time.Duration(max(tp.Cycles, 1))*3*time.Second + 30*time.Second
		cctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		revErr = c.Call(cctx, "trace", endpoint.TraceReq{Cycles: tp.Cycles, MaxHops: tp.MaxHops, Resolve: tp.Resolve, Method: tp.Method}, &t)
		if revErr == nil {
			t.From = c.Addr
			rev = &t
		}
	}()
	wg.Wait()
	if fwd != nil && tp.Annotate != nil {
		annotate(fwd, tp.Annotate)
	}
	if rev != nil && tp.Annotate != nil {
		annotate(rev, tp.Annotate)
	}
	return fwd, rev, revErr
}

func annotate(t *results.Trace, fn func(string) (string, string)) {
	for i := range t.Hops {
		if t.Hops[i].Addr != "" && t.Hops[i].ASN == "" {
			t.Hops[i].ASN, t.Hops[i].Org = fn(t.Hops[i].Addr)
		}
	}
}

// RunPMTU discovers the path MTU in both directions.
func RunPMTU(ctx context.Context, c *Client) (*results.PMTU, error) {
	up, err := pmtuUp(ctx, c)
	if err != nil {
		return nil, err
	}
	down := pmtuDown(ctx, c, up.LocalMTU)
	return &results.PMTU{Up: up, Down: down}, nil
}

func ipOverhead(c *Client) int {
	if c.EndpointIP.Is4() {
		return 28
	}
	return 48
}

// pmtuUp sends DF probes of decreasing/bisected size to the endpoint.
func pmtuUp(ctx context.Context, c *Client) (results.PMTUDir, error) {
	res := results.PMTUDir{Direction: results.Up}
	u, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: c.EndpointIP.AsSlice(), Port: c.Port})
	if err != nil {
		return res, err
	}
	defer u.Close()
	if err := netx.SetDFProbe(u); err != nil {
		return res, err
	}
	over := ipOverhead(c)
	// The interface MTU, not IP_MTU: the latter returns the kernel's cached
	// path MTU, which earlier probes may already have lowered.
	res.LocalMTU = netx.InterfaceMTU(u.LocalAddr().(*net.UDPAddr).IP)
	if res.LocalMTU <= 0 {
		res.LocalMTU = 1500
	}
	sg := c.Signer()
	try := func(size int) bool {
		payload := size - over
		b := make([]byte, payload)
		for attempt := 0; attempt < 3; attempt++ {
			sg.Encode(b, &proto.UDPHeader{Type: proto.UDPPMTU, Session: c.Session(), Seq: uint64(attempt), Stream: uint32(size), T1: time.Now().UnixNano()})
			if _, err := u.Write(b); err != nil {
				if errors.Is(err, syscall.EMSGSIZE) {
					return false
				}
			}
			deadline := time.Now().Add(400 * time.Millisecond)
			buf := make([]byte, 2048)
			for time.Now().Before(deadline) {
				_ = u.SetReadDeadline(deadline)
				n, err := u.Read(buf)
				if err != nil {
					// ICMP errors (e.g. fragmentation needed) surface here.
					for {
						se, qerr := netx.ReadErrQueue(u)
						if qerr != nil {
							break
						}
						if se.Errno == syscall.EMSGSIZE {
							res.PTBSeen = true
							if se.Info > 0 {
								res.PTBMTU = int(se.Info)
							}
						}
					}
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						break
					}
					continue
				}
				if !sg.Verify(buf[:n]) {
					continue
				}
				h, _ := proto.DecodeUDP(buf[:n])
				if h.Type == proto.UDPPMTUAck && int(h.Stream) == payload {
					return true
				}
			}
			if ctx.Err() != nil {
				return false
			}
		}
		return false
	}
	// Probe up to jumbo size at most: larger is never needed in practice.
	res.MTU = bisect(ctx, 576, min(max(res.LocalMTU, 576), 9000), func(size int) bool {
		ok := try(size)
		res.Tested = append(res.Tested, results.PMTUTry{Size: size, OK: ok})
		return ok
	})
	finishPMTU(&res)
	return res, nil
}

// pmtuDown asks the endpoint to send DF probes towards this client.
func pmtuDown(ctx context.Context, c *Client, localMTU int) results.PMTUDir {
	res := results.PMTUDir{Direction: results.Down, LocalMTU: localMTU}
	over := ipOverhead(c)
	c.punch()
	drain := func() {
		for {
			select {
			case <-c.pmtuRecv:
			default:
				return
			}
		}
	}
	try := func(size int) bool {
		drain()
		var resp endpoint.PMTUSendResp
		if err := c.Call(ctx, "pmtu_send", endpoint.PMTUSendReq{Size: size - over, Count: 3}, &resp); err != nil {
			res.Note = err.Error()
			return false
		}
		if resp.PTBSeen {
			res.PTBSeen = true
			res.PTBMTU = resp.PTBMTU
		}
		deadline := time.After(400 * time.Millisecond)
		for {
			select {
			case n := <-c.pmtuRecv:
				if n >= size-over {
					return true
				}
			case <-deadline:
				return false
			case <-ctx.Done():
				return false
			}
		}
	}
	hi := min(max(localMTU, 1500), 9000)
	res.MTU = bisect(ctx, 576, hi, func(size int) bool {
		ok := try(size)
		res.Tested = append(res.Tested, results.PMTUTry{Size: size, OK: ok})
		return ok
	})
	finishPMTU(&res)
	return res
}

// bisect finds the largest size in [lo, hi] for which ok is true,
// assuming lo succeeds; it checks hi first because it usually works.
func bisect(ctx context.Context, lo, hi int, ok func(int) bool) int {
	if ok(hi) {
		return hi
	}
	if !ok(lo) {
		return 0
	}
	for hi-lo > 1 && ctx.Err() == nil {
		mid := (lo + hi) / 2
		if ok(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

func finishPMTU(r *results.PMTUDir) {
	if r.MTU == 0 {
		r.Note = "no probe got through; UDP may be blocked"
		return
	}
	// Judge black holes against standard Ethernet: jumbo-capable hosts on
	// 1500-byte paths are normal, but a path that silently drops packets
	// below 1500 bytes breaks applications.
	if r.MTU < min(r.LocalMTU, 1500) && !r.PTBSeen {
		r.Blackhole = true
		r.Note = "large packets are dropped without an ICMP 'fragmentation needed' message (PMTU black hole)"
	}
}
