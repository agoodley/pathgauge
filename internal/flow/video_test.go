package flow

import (
	"math"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/proto"
)

func TestVideoModelKeepsItsBitrate(t *testing.T) {
	for _, br := range []float64{0.5e6, 1.5e6, 3.8e6} {
		m := VideoModel{BitrateBps: br}.WithDefaults()
		frames := 4 * m.GOP // 8 s
		bytes := 0
		for i := 0; i < frames; i++ {
			sizes := m.FramePackets(i)
			sum := 0
			for _, s := range sizes {
				if s > m.PacketSize || s < proto.UDPHeaderLen {
					t.Fatalf("packet size %d out of range", s)
				}
				sum += s
			}
			if want := m.FrameBytes(i); sum < want || sum > want+proto.UDPHeaderLen {
				t.Fatalf("frame %d: packets carry %d bytes, frame is %d", i, sum, want)
			}
			bytes += sum
		}
		got := float64(bytes) * 8 / (float64(frames) / float64(m.FPS))
		if math.Abs(got-br)/br > 0.02 {
			t.Errorf("bitrate %.0f: model sends %.0f bit/s", br, got)
		}
		if k, d := m.FrameBytes(0), m.FrameBytes(1); math.Abs(float64(k)/float64(d)-m.KeyRatio) > 0.01 {
			t.Errorf("keyframe ratio %d/%d", k, d)
		}
	}
}

func TestFrameStartsAndSendTime(t *testing.T) {
	m := VideoModel{BitrateBps: 1.5e6}
	starts := m.FrameStarts(3)
	if starts[0] != 0 || starts[1] != uint64(len(m.FramePackets(0))) || starts[3] != starts[2]+uint64(len(m.FramePackets(2))) {
		t.Fatalf("frame starts %v", starts)
	}
	// A keyframe takes the pacer roughly 96 ms at any bitrate.
	if d := m.FrameSendTime(0); d < 80*time.Millisecond || d > 110*time.Millisecond {
		t.Errorf("keyframe send time %v", d)
	}
}

// synthLog builds the packet log a receiver would see for n frames sent
// exactly on the model's schedule, with per-frame extra delay and dropped
// sequence numbers.
func synthLog(m VideoModel, n int, extraMs func(frame int) float64, drop map[uint64]bool) (*PacketLog, uint64) {
	m = m.WithDefaults()
	log := &PacketLog{}
	var seq uint64
	const baseOWDus = 10_000 // 10 ms of propagation (and clock offset)
	for f := 0; f < n; f++ {
		frameAt := int64(f) * m.FrameInterval().Microseconds()
		var off time.Duration
		for _, size := range m.FramePackets(f) {
			t1 := frameAt + off.Microseconds()
			if !drop[seq] {
				log.Seq = append(log.Seq, seq)
				log.T1Us = append(log.T1Us, t1)
				log.RxUs = append(log.RxUs, t1+baseOWDus+int64(extraMs(f)*1000))
			}
			off += m.PacketGap(size)
			seq++
		}
	}
	return log, seq
}

func TestAnalyzeFramesOnTime(t *testing.T) {
	m := VideoModel{BitrateBps: 1.5e6}
	log, sent := synthLog(m, 90, func(int) float64 { return 0 }, nil)
	st := AnalyzeFrames(m, log, sent, 10, 20)
	// The buffer holds one round trip (20 ms) plus 10 ms for repairs.
	if st.Frames != 90 || st.OKPct != 100 || st.Freezes != 0 || st.DelayP95Ms > 0.5 || st.PlayoutMs > 31 {
		t.Fatalf("clean stream: %+v", st)
	}
}

func TestAnalyzeFramesRepairSpikesAndOutages(t *testing.T) {
	m := VideoModel{BitrateBps: 1.5e6}
	starts := m.FrameStarts(300)
	drop := map[uint64]bool{starts[5] + 1: true} // one packet of frame 5: repaired in time
	// A 333 ms outage (every packet of 10 frames), well after the delay
	// spike has left the buffer's 3 s memory.
	for f := 200; f < 210; f++ {
		for s := starts[f]; s < starts[f+1]; s++ {
			drop[s] = true
		}
	}
	spike := func(f int) float64 {
		if f >= 30 && f < 40 { // a sudden 300 ms jump in delay for 10 frames
			return 300
		}
		return 0
	}
	log, sent := synthLog(m, 300, spike, drop)
	st := AnalyzeFrames(m, log, sent, 10, 20)
	if st.Frames != 300 || st.LostPct != 0 {
		t.Fatalf("frames %d, lost %.1f%%", st.Frames, st.LostPct)
	}
	if got := math.Round(st.RepairedPct * 300 / 100); got != 11 {
		t.Errorf("want 11 frames repaired, got %.0f (%+v)", got, st)
	}
	// The delay jump and the outage each stall the picture for about
	// their length; the single repaired packet does not.
	if st.Freezes != 2 || st.FreezeS < 0.55 || st.FreezeS > 0.8 {
		t.Errorf("want two ~300 ms freezes, got %d / %.3f s (%+v)", st.Freezes, st.FreezeS, st)
	}
	if late := math.Round(st.LatePct * 300 / 100); late < 2 || late > 6 {
		t.Errorf("want a few late frames, got %.0f", late)
	}
	if st.DelayP95Ms < 100 {
		t.Errorf("the spike should show in the delay p95: %+v", st)
	}
}

func TestAnalyzeFramesHeavyLossIsNotRepaired(t *testing.T) {
	m := VideoModel{BitrateBps: 1.5e6}
	drop := map[uint64]bool{}
	for s := uint64(0); s < 5000; s += 5 { // 20% loss
		drop[s] = true
	}
	log, sent := synthLog(m, 90, func(int) float64 { return 0 }, drop)
	if st := AnalyzeFrames(m, log, sent, 10, 20); st.LostPct < 50 || st.OKPct > 50 {
		t.Errorf("20%% loss is beyond repair: %+v", st)
	}
}

func TestAnalyzeFramesStandingQueueIsDelayNotLoss(t *testing.T) {
	m := VideoModel{BitrateBps: 1.5e6}
	// A queue that builds to 200 ms over ~3 s and stays: the buffer follows it.
	gentle := func(f int) float64 { return math.Min(200, float64(f)*2) }
	log, sent := synthLog(m, 300, gentle, nil)
	st := AnalyzeFrames(m, log, sent, 10, 20)
	if st.OKPct < 99 || st.Freezes != 0 {
		t.Errorf("a slowly building queue should only add delay: %+v", st)
	}
	if st.PlayoutMs < 150 {
		t.Errorf("the added latency must be reported: %+v", st)
	}
	// A queue that fills in under a second (a big upload starting on a
	// bloated link) stalls the picture briefly, then playback recovers.
	fast := func(f int) float64 { return math.Min(200, float64(f)*10) }
	log, sent = synthLog(m, 300, fast, nil)
	st = AnalyzeFrames(m, log, sent, 10, 20)
	if st.Freezes > 1 || st.FreezeS > 1 || st.OKPct < 90 {
		t.Errorf("fast onset: one short stall at most: %+v", st)
	}
}

func TestAnalyzeFramesIgnoresCutOffLastFrame(t *testing.T) {
	m := VideoModel{BitrateBps: 1.5e6}
	log, sent := synthLog(m, 10, func(int) float64 { return 0 }, nil)
	// The sender stopped part-way through frame 10.
	st := AnalyzeFrames(m, log, sent+2, 10, 20)
	if st.Frames != 10 || st.OKPct != 100 {
		t.Errorf("a cut-off final frame must not count: %+v", st)
	}
	if st2 := AnalyzeFrames(m, log, sent, math.NaN(), 20); st2.OKPct != 100 {
		t.Errorf("without a baseline: %+v", st2)
	}
}

func TestVoicePlayout(t *testing.T) {
	n := 1000 // 20 s of 20 ms packets
	send := make([]int64, n)
	steady, spiky, queue := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range send {
		send[i] = int64(i) * 20_000
		steady[i] = 10
		spiky[i] = 10
		if i%250 >= 200 && i%250 < 230 { // 600 ms of +120 ms every 5 s (page loads)
			spiky[i] = 130
		}
		queue[i] = 10 + 150 + 5*math.Sin(float64(i)/7) // standing queue with a little wobble
	}
	if r := VoicePlayout.Playout(send, steady); r.LatePct != 0 || r.MeanTarget-10 > VoicePlayout.MinMs+0.01 {
		t.Errorf("steady: %+v", r.MeanTarget)
	}
	r := VoicePlayout.Playout(send, spiky)
	if r.LatePct <= 0 || r.LatePct > 3 {
		t.Errorf("spikes should cost a few late packets each, got %.1f%%", r.LatePct)
	}
	q := VoicePlayout.Playout(send, queue)
	if q.LatePct > 1 || q.MeanTarget < 160 {
		t.Errorf("a standing queue is delay, not loss: late %.1f%%, target %.0f", q.LatePct, q.MeanTarget)
	}
	log := &PacketLog{Seq: []uint64{2, 0, 1}, T1Us: []int64{40_000, 0, 20_000}, RxUs: []int64{50_000, 10_000, 30_000}}
	if r := VoicePlayout.PlayoutLog(log); r.LatePct != 0 || len(r.Late) != 3 {
		t.Errorf("log replay: %+v", r)
	}
}
