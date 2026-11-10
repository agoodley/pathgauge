package flow

import (
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
)

// Arrival times come from the wall clock, which can be stepped (NTP, a VM
// resume) while a stream runs. A step backwards used to index bin -N and
// end the process; a step forwards allocated bins for the whole gap.
func TestSinkSurvivesClockSteps(t *testing.T) {
	s := NewUDPSink(10 * time.Millisecond)
	ri := netx.RecvInfo{TOS: -1, TTL: -1}
	t0 := time.Unix(1_700_000_000, 0)
	add := func(seq uint64, rx time.Time) {
		s.Add(proto.UDPHeader{Seq: seq, T1: rx.Add(-5 * time.Millisecond).UnixNano()}, 1200, rx, ri)
	}
	add(0, t0)
	add(1, t0.Add(20*time.Millisecond))
	add(2, t0.Add(-3*time.Second))   // the clock stepped back
	add(3, t0.Add(400*24*time.Hour)) // and far forward
	add(4, t0.Add(30*time.Millisecond))

	if len(s.bins) > maxSinkBins {
		t.Errorf("bins grew to %d, past the limit of %d", len(s.bins), maxSinkBins)
	}
	if len(s.bins) > 1000 {
		t.Errorf("a packet stamped far in the future allocated %d bins", len(s.bins))
	}
	res := s.Result(5)
	if res.Loss.Received != 5 || res.Bytes != 5*1200 {
		t.Errorf("every packet still counts in the totals: received %d, bytes %d", res.Loss.Received, res.Bytes)
	}
}
