// Package trace runs traceroute and MTR-style path measurements.
//
// Two methods are available:
//
//   - "udp-paris" (needs CAP_NET_RAW, IPv4): every probe uses the same UDP
//     5-tuple so per-flow load balancers keep one path, and probes are told
//     apart by their UDP length. A raw ICMP socket collects the replies,
//     including MPLS label stacks from ICMP extensions.
//   - "udp" (unprivileged): one socket per probe with IP_RECVERR, reading
//     ICMP errors from the socket error queue. Source ports differ per
//     probe, so ECMP paths may be mixed.
//
// Repeating probes over several cycles gives MTR statistics.
package trace

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// Options controls a trace.
type Options struct {
	MaxHops  int           // default 30
	Probes   int           // per hop per cycle, default 1
	Cycles   int           // default 1; >1 gives MTR statistics
	Timeout  time.Duration // per probe, default 1s
	Interval time.Duration // between cycles, default 1s
	Port     int           // destination UDP port, default 33434
	Method   string        // auto, udp-paris, udp
	Resolve  bool          // reverse DNS for hop addresses
	// Annotate, if set, may fill ASN/Org for each hop address.
	Annotate func(addr netip.Addr) (asn, org string)
}

func (o *Options) defaults() {
	if o.MaxHops <= 0 {
		o.MaxHops = 30
	}
	// One probe per hop per cycle, cycles a second apart (like mtr):
	// routers rate-limit ICMP (Linux: a burst of ~6, then 1/s per
	// destination), so faster probing shows loss that is not there.
	if o.Probes <= 0 {
		o.Probes = 1
	}
	if o.Cycles <= 0 {
		o.Cycles = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = time.Second
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.Port <= 0 {
		o.Port = 33434
	}
	if o.Method == "" {
		o.Method = "auto"
	}
}

// reply is one probe outcome.
type reply struct {
	ttl    int
	from   netip.Addr
	rtt    time.Duration
	ok     bool
	dest   bool
	mpls   []string
	errMsg string
}

// prober sends one cycle of probes and returns the replies.
type prober interface {
	cycle(ctx context.Context, maxTTL int) []reply
	close()
	name() string
}

// Run traces the path to target.
func Run(ctx context.Context, target netip.Addr, o Options) results.Trace {
	o.defaults()
	tr := results.Trace{Target: target.String(), Cycles: o.Cycles}
	p, err := newProber(target, o)
	if err != nil {
		tr.Note = err.Error()
		return tr
	}
	defer p.close()
	tr.Method = p.name()

	type agg struct {
		sent, recv int
		rtts       []float64
		addrs      map[string]int
		mpls       map[string]bool
		dest       bool
	}
	hops := make([]agg, o.MaxHops+1)
	for i := range hops {
		hops[i].addrs = map[string]int{}
		hops[i].mpls = map[string]bool{}
	}
	destTTL := o.MaxHops + 1
	for c := 0; c < o.Cycles && ctx.Err() == nil; c++ {
		if c > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(o.Interval):
			}
		}
		limit := min(o.MaxHops, destTTL)
		for _, r := range p.cycle(ctx, limit) {
			if r.ttl < 1 || r.ttl > o.MaxHops {
				continue
			}
			h := &hops[r.ttl]
			h.sent++
			if !r.ok {
				continue
			}
			h.recv++
			h.rtts = append(h.rtts, float64(r.rtt.Microseconds())/1000)
			h.addrs[r.from.String()]++
			for _, l := range r.mpls {
				h.mpls[l] = true
			}
			if r.dest || r.from == target {
				h.dest = true
				destTTL = min(destTTL, r.ttl)
			}
		}
	}
	last := min(o.MaxHops, destTTL)
	// Trim trailing silent hops after the last responder when the
	// destination never answered.
	if destTTL > o.MaxHops {
		for last > 1 && hops[last].recv == 0 {
			last--
		}
		last = min(o.MaxHops, last+1) // keep one silent hop to show where it stops
	}
	var ids []string
	for ttl := 1; ttl <= last; ttl++ {
		h := hops[ttl]
		hop := results.Hop{TTL: ttl, Sent: h.sent, Received: h.recv, Dest: h.dest}
		if h.sent > 0 {
			hop.LossPct = stats.Round(100*float64(h.sent-h.recv)/float64(h.sent), 1)
		}
		hop.RTT = roundSummary(stats.Summarize(h.rtts))
		best, bestN := "", 0
		for a, n := range h.addrs {
			hop.Addrs = append(hop.Addrs, a)
			if n > bestN || (n == bestN && a < best) {
				best, bestN = a, n
			}
		}
		sort.Strings(hop.Addrs)
		if len(hop.Addrs) <= 1 {
			hop.Addrs = nil
		}
		for l := range h.mpls {
			hop.MPLS = append(hop.MPLS, l)
		}
		sort.Strings(hop.MPLS)
		hop.Addr = best
		if a, err := netip.ParseAddr(best); err == nil {
			hop.Private = a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()
			hop.CGNAT = cgnat.Contains(a)
			if o.Annotate != nil {
				hop.ASN, hop.Org = o.Annotate(a)
			}
		}
		ids = append(ids, best)
		tr.Hops = append(tr.Hops, hop)
		if h.dest {
			tr.Reached = true
		}
	}
	if o.Resolve {
		resolveNames(ctx, tr.Hops)
	}
	tr.LossFromHop, tr.RateLimitedHops = interpretLoss(tr.Hops)
	sum := sha1.Sum([]byte(strings.Join(ids, ">")))
	tr.Fingerprint = hex.EncodeToString(sum[:6])
	if !tr.Reached && tr.Note == "" {
		tr.Note = "the destination did not answer trace probes (commonly filtered); hops up to the last responder are shown"
	}
	return tr
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func roundSummary(s stats.Summary) stats.Summary {
	r := func(x float64) float64 { return stats.Round(x, 2) }
	return stats.Summary{N: s.N, Min: r(s.Min), Mean: r(s.Mean), Median: r(s.Median), P90: r(s.P90), P95: r(s.P95), P99: r(s.P99), Max: r(s.Max), StdDev: r(s.StdDev)}
}

// interpretLoss separates real loss (persisting to the end of the path)
// from ICMP rate limiting at individual routers (loss that does not carry
// on downstream). It returns the first hop with persistent loss (0 if
// none) and the hops whose loss is only rate limiting. The destination's
// own ICMP replies are rate limited too, so it is not used as evidence:
// callers confirm real loss with an end-to-end measurement.
func interpretLoss(hops []results.Hop) (lossFrom int, rateLimited []int) {
	const minLoss = 2.0 // percent
	for i, h := range hops {
		if h.Dest || h.Received == 0 || h.LossPct < minLoss {
			continue
		}
		persists := true
		laterResponders := 0
		for _, later := range hops[i+1:] {
			if later.Received == 0 || later.Dest {
				continue
			}
			laterResponders++
			if later.LossPct < h.LossPct*0.5 {
				persists = false
				break
			}
		}
		if persists && laterResponders > 0 {
			if lossFrom == 0 {
				lossFrom = h.TTL
			}
		} else {
			rateLimited = append(rateLimited, h.TTL)
		}
	}
	return lossFrom, rateLimited
}

func resolveNames(ctx context.Context, hops []results.Hop) {
	var wg sync.WaitGroup
	for i := range hops {
		if hops[i].Addr == "" {
			continue
		}
		wg.Add(1)
		go func(h *results.Hop) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer cancel()
			if names, err := net.DefaultResolver.LookupAddr(rctx, h.Addr); err == nil && len(names) > 0 {
				h.Name = strings.TrimSuffix(names[0], ".")
			}
		}(&hops[i])
	}
	wg.Wait()
}

func newProber(target netip.Addr, o Options) (prober, error) {
	switch o.Method {
	case "udp-paris":
		return newParis(target, o)
	case "udp":
		return newSimple(target, o), nil
	case "auto":
		if target.Is4() {
			if p, err := newParis(target, o); err == nil {
				return p, nil
			}
		}
		return newSimple(target, o), nil
	}
	return nil, fmt.Errorf("unknown trace method %q", o.Method)
}

// ErrNeedRaw is returned when the Paris method lacks CAP_NET_RAW.
var ErrNeedRaw = errors.New("udp-paris traceroute needs CAP_NET_RAW (run via the service or as root)")
