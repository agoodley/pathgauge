package flow

import (
	"math"
	"testing"
)

// One outage longer than the buffer's window used to leave it with no
// history: the target became +Inf, and so did the mean, which turned a
// perfect call into MOS 1.0.
func TestPlayoutAfterAGapLongerThanTheWindow(t *testing.T) {
	var send []int64
	var delay []float64
	for i := 0; i < 500; i++ { // 10 s of 20 ms packets
		send = append(send, int64(i)*20_000)
		delay = append(delay, 10)
	}
	gap := int64(5_000_000) // 5 s with nothing received; the window is 2 s
	for i := 0; i < 500; i++ {
		send = append(send, send[499]+gap+int64(i)*20_000)
		delay = append(delay, 10)
	}
	r := VoicePlayout.Playout(send, delay)
	for k, v := range r.Target {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			t.Fatalf("sample %d: playout target is %v", k, v)
		}
	}
	if r.LatePct != 0 {
		t.Errorf("a steady stream either side of an outage has no late packets, got %.1f%%", r.LatePct)
	}
	if got, want := r.MeanTarget, 10+VoicePlayout.MinMs; math.Abs(got-want) > 0.01 {
		t.Errorf("mean playout delay %.2f ms, want %.2f", got, want)
	}
}

func TestPlayoutLogRejectsMismatchedLengths(t *testing.T) {
	log := &PacketLog{Seq: []uint64{0, 1, 2}, T1Us: []int64{0}, RxUs: []int64{10_000, 30_000, 50_000}}
	if r := VoicePlayout.PlayoutLog(log); len(r.Late) != 0 {
		t.Errorf("a log whose columns differ in length must be ignored, got %+v", r)
	}
}
