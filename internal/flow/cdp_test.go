package flow

import (
	"context"
	"io"
	"math"
	"net"
	"testing"
	"time"
)

func TestCDPModelArithmetic(t *testing.T) {
	m := CDPModel{RateBps: 80e6}.WithDefaults() // 10 MB/s average
	base, peak := m.Rates()
	if math.Abs(peak-30e6) > 1 || math.Abs(base-6e6) > 1 {
		t.Fatalf("rates: base %.0f peak %.0f", base, peak)
	}
	// The average over whole periods is the configured rate.
	if got := m.Generated(3 * m.PeriodS); math.Abs(got-3*m.PeriodS*10e6) > 1 {
		t.Errorf("generated over 3 periods: %.0f", got)
	}
	// GenTime inverts Generated.
	for _, x := range []float64{0.5, 10, 24.9, 25, 30, 35.2, 59.9, 61, 150} {
		if back := m.GenTime(m.Generated(x)); math.Abs(back-x) > 1e-6 {
			t.Errorf("GenTime(Generated(%v)) = %v", x, back)
		}
	}
	// With nothing produced between bursts, the first byte appears when the
	// first burst starts (regression: GenTime(0) returned 0, a false lag).
	z := CDPModel{RateBps: 8e6, BurstFactor: 3, BurstS: 2, PeriodS: 6}.WithDefaults()
	if b, _ := z.Rates(); b != 0 || z.GenTime(0) != z.burstStart() || z.GenTime(z.Generated(6)) != 6+z.burstStart() {
		t.Errorf("zero-base model: GenTime(0)=%v, want %v; next period %v", z.GenTime(0), z.burstStart(), z.GenTime(z.Generated(6)))
	}
	// An impossible burst is clamped so the rate outside bursts isn't negative.
	c := CDPModel{RateBps: 8e6, BurstFactor: 10, BurstS: 10, PeriodS: 20}.WithDefaults()
	if b, _ := c.Rates(); c.BurstFactor != 2 || b != 0 {
		t.Errorf("clamp: factor %v base %v", c.BurstFactor, b)
	}
}

// tcpPair returns n connected TCP streams whose far ends are drained.
func tcpPair(t *testing.T, n int) []*Stream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	var s []*Stream
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		s = append(s, &Stream{ID: uint32(i), Conn: c.(*net.TCPConn)})
	}
	return s
}

func TestSendCDPKeepsUpWhenThePathIsFast(t *testing.T) {
	m := CDPModel{RateBps: 100e6, BurstS: 0.5, PeriodS: 1.5}
	r := SendCDP(context.Background(), tcpPair(t, 4), m, 0, 3*time.Second)
	if r.Sent == 0 || float64(r.Sent) < float64(r.Generated)*0.97 {
		t.Fatalf("sent %d of %d", r.Sent, r.Generated)
	}
	for _, l := range r.Lag {
		if l > 0.3 {
			t.Fatalf("lag %.2f s on loopback", l)
		}
	}
}

func TestSendCDPThrottleCapsTheRateAndLagGrows(t *testing.T) {
	// 200 Mbit/s of changes through a 50 Mbit/s throttle.
	m := CDPModel{RateBps: 200e6, BurstFactor: 1, BurstS: 1, PeriodS: 1}
	r := SendCDP(context.Background(), tcpPair(t, 2), m, 50e6, 3*time.Second)
	for _, p := range r.Points {
		if p.SentMbps > 50*1.15 {
			t.Errorf("second %v sent %.1f Mbit/s through a 50 Mbit/s throttle", p.T, p.SentMbps)
		}
	}
	// After 3 s the replica is far behind: 3 s of changes at 4x the
	// throttle leaves the oldest unsent change about 2.2 s old.
	if end := r.Lag[len(r.Lag)-1]; end < 1.5 {
		t.Errorf("end lag %.2f s, want it growing past 1.5 s", end)
	}
}

func TestSendCDPNoFalseLagWhenQuietBetweenBursts(t *testing.T) {
	// All the data comes in bursts (factor = period / burst): the lag must
	// stay near zero on a fast path, including just after each burst starts.
	m := CDPModel{RateBps: 50e6, BurstFactor: 3, BurstS: 0.5, PeriodS: 1.5}
	r := SendCDP(context.Background(), tcpPair(t, 2), m, 0, 4*time.Second)
	for i, l := range r.Lag {
		if l > 0.3 {
			t.Fatalf("lag %.2f s at sample %d on loopback", l, i)
		}
	}
}
