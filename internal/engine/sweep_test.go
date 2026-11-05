package engine

import (
	"testing"

	"github.com/agoodley/pathgauge/internal/results"
)

func sweepOf(steps ...results.SweepStep) *results.Sweep {
	s := &results.Sweep{Direction: results.Down, Steps: steps}
	ClassifySweep(s)
	return s
}

func st(off, del, loss, q50, q95 float64) results.SweepStep {
	return results.SweepStep{OfferedMbps: off, SentMbps: off, DeliveredMbps: del, LossPct: loss, QueueP50ms: q50, QueueP95ms: q95, FirstHalfMbps: del, SecondHalfMbps: del}
}

func TestClassifySweep(t *testing.T) {
	cases := map[string]*results.Sweep{
		"policer":               sweepOf(st(10, 10, 0, 0.2, 0.5), st(20, 20, 0, 0.2, 0.6), st(30, 29.8, 0.5, 0.3, 1), st(40, 30, 25, 0.5, 2), st(50, 30, 40, 0.5, 2)),
		"shaper":                sweepOf(st(10, 10, 0, 0.2, 0.5), st(20, 20, 0, 0.3, 1), st(30, 29.5, 0.2, 20, 38), st(40, 30, 25, 38, 40), st(50, 30, 40, 39, 41)),
		"bufferbloat":           sweepOf(st(10, 10, 0, 0.2, 0.5), st(40, 40, 0, 1, 3), st(55, 50, 8, 250, 300), st(70, 50, 28, 290, 300)),
		"non-congestive-loss":   sweepOf(st(5, 4.95, 1, 0.1, 0.3), st(20, 19.8, 1, 0.1, 0.3), st(50, 49.5, 1, 0.2, 0.4), st(100, 99, 1, 0.2, 0.4)),
		"no-bottleneck-reached": sweepOf(st(10, 10, 0, 0.1, 0.2), st(20, 20, 0, 0.1, 0.2)),
	}
	for want, s := range cases {
		if s.Signature != want {
			t.Errorf("%s classified as %s (%s)", want, s.Signature, s.SignatureDetail)
		}
	}
	if s := cases["policer"]; s.CapacityMbps != 30 || s.KneeMbps != 29.8 {
		t.Errorf("policer capacity %v knee %v", s.CapacityMbps, s.KneeMbps)
	}
	burst := st(60, 40, 30, 5, 10)
	burst.FirstHalfMbps, burst.SecondHalfMbps = 60, 20
	if s := sweepOf(st(10, 10, 0, 1, 2), burst); s.Signature != "burst-allowance" {
		t.Errorf("burst classified as %s", s.Signature)
	}
	if s := sweepOf(); s.Signature != "unknown" {
		t.Error("empty sweep")
	}
}

func TestBisect(t *testing.T) {
	limit := 1400
	got := bisect(t.Context(), 576, 1500, func(n int) bool { return n <= limit })
	if got != limit {
		t.Errorf("bisect found %d, want %d", got, limit)
	}
	if bisect(t.Context(), 576, 1500, func(int) bool { return true }) != 1500 {
		t.Error("hi should win when it works")
	}
	if bisect(t.Context(), 576, 1500, func(int) bool { return false }) != 0 {
		t.Error("nothing works: 0")
	}
}

func TestHopsFromTTL(t *testing.T) {
	for ttl, want := range map[int]int{64: 0, 62: 2, 120: 8, 250: 5, 0: 0} {
		if got := hopsFromTTL(ttl); got != want {
			t.Errorf("hopsFromTTL(%d) = %d, want %d", ttl, got, want)
		}
	}
}

func TestClassifyTOS(t *testing.T) {
	pairs := [][3]int{{0, 0, 5}, {184, 0, 5}, {136, 104, 5}, {2, 0, 5}} // EF bleached, AF41 remarked to AF31, ECT(0) bleached
	res, ecn := classifyTOS(pairs)
	status := map[string]string{}
	for _, r := range res {
		status[r.Name] = r.Status
	}
	if status["EF (voice)"] != "bleached" || status["AF41"] != "remarked" || status["CS0 (best effort)"] != "preserved" || status["CS1"] != "lost" {
		t.Errorf("statuses %v", status)
	}
	if ecn != "bleached" {
		t.Errorf("ecn %s", ecn)
	}
}

func TestClassifySweepHostLimits(t *testing.T) {
	// Receiver dropped most of the "loss" itself; the sender fell short at
	// the top: the network never showed a bottleneck.
	hd := st(800, 760, 5, 1, 2)
	hd.HostDropPct, hd.NetLossPct = 4.8, 0.2
	hd.HostLimited = true
	short := st(1000, 870, 0, 1, 2)
	short.SentMbps, short.HostLimited = 870, true
	s := sweepOf(st(100, 100, 0, 0.3, 0.5), st(400, 400, 0, 0.4, 0.8), hd, short)
	if s.Signature != "host-limited" {
		t.Errorf("want host-limited, got %s (%s)", s.Signature, s.SignatureDetail)
	}
	if len(s.Steps) != 4 {
		t.Error("all steps must be kept in the result")
	}
	all := sweepOf(hd, short)
	if all.Signature != "host-limited" {
		t.Errorf("only host-limited steps: %s", all.Signature)
	}
	// One bursty step alone is noise, not a burst allowance.
	noisy := st(683, 635, 6.9, 1, 1.5)
	noisy.FirstHalfMbps, noisy.SecondHalfMbps = 725, 546
	if s := sweepOf(st(50, 50, 0, 0.3, 0.3), st(155, 155, 0, 0.5, 0.5), st(260, 260, 0, 0.7, 0.7), st(470, 470, 0, 1, 1.3), noisy, st(780, 780, 0.6, 1, 3)); s.Signature == "burst-allowance" {
		t.Error("a single noisy step must not be called a burst allowance")
	}
}
