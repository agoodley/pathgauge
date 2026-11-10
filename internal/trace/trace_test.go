package trace

import (
	"context"
	"net/netip"
	"strings"
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

// A probe that never answers must not cost the probes behind it their
// answers: once the timeout has passed, answers that already arrived still
// count. (The old code chose at random between "answered" and "timed out"
// for every probe after the first silent one.)
func TestCollectKeepsAnswersBehindASilentHop(t *testing.T) {
	for round := 0; round < 200; round++ {
		probes := make([]*pendingProbe, 8)
		for i := range probes {
			probes[i] = &pendingProbe{ttl: i + 1, done: make(chan reply, 1)}
			if i != 2 { // hop 3 is silent
				probes[i].done <- reply{ttl: i + 1, ok: true}
			}
		}
		out := collect(context.Background(), probes, time.Millisecond)
		if len(out) != len(probes) {
			t.Fatalf("got %d replies for %d probes", len(out), len(probes))
		}
		for i, r := range out {
			if want := i != 2; r.ok != want || r.ttl != i+1 {
				t.Fatalf("round %d: hop %d answered=%v, want %v", round, i+1, r.ok, want)
			}
		}
	}
}

func TestCollectStopsWaitingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probes := []*pendingProbe{
		{ttl: 1, done: make(chan reply, 1)},
		{ttl: 2, done: make(chan reply, 1)},
	}
	probes[1].done <- reply{ttl: 2, ok: true}
	start := time.Now()
	out := collect(ctx, probes, time.Minute)
	if time.Since(start) > 5*time.Second {
		t.Fatal("collect kept waiting after the context ended")
	}
	if out[0].ok || !out[1].ok {
		t.Errorf("want hop 1 unanswered and hop 2 answered, got %+v", out)
	}
}

// fakeProber answers from a table: hop -> reply (a missing hop is silent).
type fakeProber struct {
	hops   map[int]reply
	cycles int
	limits []int
}

func (f *fakeProber) name() string { return "fake" }
func (f *fakeProber) close()       {}
func (f *fakeProber) cycle(_ context.Context, maxTTL int) []reply {
	f.cycles++
	f.limits = append(f.limits, maxTTL)
	var out []reply
	for ttl := 1; ttl <= maxTTL; ttl++ {
		r, ok := f.hops[ttl]
		if !ok {
			// Beyond the last scripted hop, the last one keeps answering
			// (as a destination or a refusing firewall does).
			last := 0
			for k := range f.hops {
				last = max(last, k)
			}
			if ttl > last && (f.hops[last].dest || f.hops[last].blocked) {
				r = f.hops[last]
			}
		}
		r.ttl = ttl
		out = append(out, r)
	}
	return out
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestRunWithSilentHopAndDestination(t *testing.T) {
	target := addr("203.0.113.9")
	p := &fakeProber{hops: map[int]reply{
		1: {ok: true, from: addr("192.168.1.1"), rtt: time.Millisecond},
		// hop 2 silent
		3: {ok: true, from: addr("198.51.100.1"), rtt: 5 * time.Millisecond},
		4: {ok: true, from: target, rtt: 9 * time.Millisecond, dest: true},
	}}
	o := Options{MaxHops: 30, Cycles: 5, Interval: time.Millisecond}
	o.defaults()
	tr := run(context.Background(), target, o, p)
	if !tr.Reached || len(tr.Hops) != 4 {
		t.Fatalf("want 4 hops and reached, got %d hops, reached=%v", len(tr.Hops), tr.Reached)
	}
	for _, h := range tr.Hops {
		wantLoss := 0.0
		if h.TTL == 2 {
			wantLoss = 100
		}
		if h.LossPct != wantLoss || h.Sent != 5 {
			t.Errorf("hop %d: sent %d loss %.0f%%, want 5 and %.0f%%", h.TTL, h.Sent, h.LossPct, wantLoss)
		}
	}
	if tr.LossFromHop != 0 {
		t.Errorf("a silent hop is not loss on the path, got loss from hop %d", tr.LossFromHop)
	}
	if got := p.limits[len(p.limits)-1]; got != 4 {
		t.Errorf("later cycles should stop at the destination's hop, probed to %d", got)
	}
	if tr.Note != "" {
		t.Errorf("unexpected note %q", tr.Note)
	}
}

// A firewall on the way that answers "administratively prohibited" ends
// the trace, but it is not the destination.
func TestRunStopsAtARefusingHopWithoutCallingItTheDestination(t *testing.T) {
	target := addr("203.0.113.9")
	fw := addr("198.51.100.7")
	p := &fakeProber{hops: map[int]reply{
		1: {ok: true, from: addr("192.168.1.1"), rtt: time.Millisecond},
		2: {ok: true, from: fw, rtt: 4 * time.Millisecond},
		3: {ok: true, from: fw, rtt: 4 * time.Millisecond, blocked: true},
	}}
	o := Options{MaxHops: 30, Cycles: 3, Interval: time.Millisecond}
	o.defaults()
	tr := run(context.Background(), target, o, p)
	if tr.Reached {
		t.Error("a refusing firewall must not count as reaching the destination")
	}
	if len(tr.Hops) != 3 {
		t.Fatalf("want the trace to stop at hop 3, got %d hops", len(tr.Hops))
	}
	for _, h := range tr.Hops {
		if h.Dest {
			t.Errorf("hop %d marked as the destination", h.TTL)
		}
	}
	if !strings.Contains(tr.Note, fw.String()) || !strings.Contains(tr.Note, "hop 3") {
		t.Errorf("the note should name the refusing hop, got %q", tr.Note)
	}
}

// A probe that could not be sent is not a lost packet.
func TestRunDoesNotCountUnsentProbesAsLoss(t *testing.T) {
	target := addr("203.0.113.9")
	p := &fakeProber{hops: map[int]reply{
		1: {errMsg: "sendto: operation not permitted"},
		2: {errMsg: "sendto: operation not permitted"},
	}}
	o := Options{MaxHops: 2, Cycles: 2, Interval: time.Millisecond}
	o.defaults()
	tr := run(context.Background(), target, o, p)
	for _, h := range tr.Hops {
		if h.Sent != 0 || h.LossPct != 0 {
			t.Errorf("hop %d: sent %d loss %.0f%%, want nothing sent", h.TTL, h.Sent, h.LossPct)
		}
	}
	if !strings.Contains(tr.Note, "could not be sent") {
		t.Errorf("the note should say probes could not be sent, got %q", tr.Note)
	}
}
