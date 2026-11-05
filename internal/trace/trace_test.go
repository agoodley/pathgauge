package trace

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
)

func hop(ttl int, loss float64, dest bool) results.Hop {
	recv := int(100 - loss)
	return results.Hop{TTL: ttl, Sent: 100, Received: recv, LossPct: loss, Dest: dest}
}

func TestInterpretLoss(t *testing.T) {
	// Hop 2 rate-limits ICMP; the rest of the path is clean.
	from, rl := interpretLoss([]results.Hop{hop(1, 0, false), hop(2, 40, false), hop(3, 0, false), hop(4, 0, true)})
	if from != 0 || len(rl) != 1 || rl[0] != 2 {
		t.Errorf("rate limiting: from %d rl %v", from, rl)
	}
	// Real loss from hop 3 onwards.
	from, _ = interpretLoss([]results.Hop{hop(1, 0, false), hop(2, 0, false), hop(3, 10, false), hop(4, 11, false), hop(5, 10, true)})
	if from != 3 {
		t.Errorf("persistent loss should start at hop 3, got %d", from)
	}
	// The destination's own ICMP rate limiting is not evidence.
	from, _ = interpretLoss([]results.Hop{hop(1, 20, false), hop(2, 40, true)})
	if from != 0 {
		t.Errorf("destination-only evidence must not count, got %d", from)
	}
}

func TestTraceLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr := Run(ctx, netip.MustParseAddr("127.0.0.1"), Options{MaxHops: 3, Method: "udp", Timeout: time.Second})
	if !tr.Reached || len(tr.Hops) != 1 || tr.Hops[0].Addr != "127.0.0.1" || !tr.Hops[0].Dest {
		t.Errorf("loopback trace: %+v", tr)
	}
	if tr.Fingerprint == "" {
		t.Error("fingerprint missing")
	}
}
