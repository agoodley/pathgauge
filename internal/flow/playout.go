package flow

import (
	"math"
	"time"
)

// PlayoutParams describes an adaptive jitter buffer. Real call software
// (WebRTC's NetEQ, phone firmware) sizes its buffer from recent delay
// variation, so a steady queue becomes extra delay rather than lost audio,
// while sudden delay spikes cause a few late packets until it adapts.
type PlayoutParams struct {
	Window time.Duration // how much history the buffer looks at
	Hold   time.Duration // how long a delay peak keeps the buffer large
	MinMs  float64       // smallest buffer
	MaxMs  float64       // largest buffer
	Margin float64       // added to the recent variation, ms
}

// VoicePlayout and VideoPlayout are the buffers used for simulated calls.
var (
	VoicePlayout = PlayoutParams{Window: 2 * time.Second, Hold: 500 * time.Millisecond, MinMs: 20, MaxMs: 300, Margin: 10}
	VideoPlayout = PlayoutParams{Window: 3 * time.Second, Hold: 500 * time.Millisecond, MinMs: 10, MaxMs: 500, Margin: 10}
)

// PlayoutResult is what the buffer did.
type PlayoutResult struct {
	Late       []bool    // per sample: arrived after its playout time
	Target     []float64 // per sample: its playout delay
	LatePct    float64   // share of samples that were late
	MeanTarget float64   // mean playout delay, in the samples' delay units
}

// Playout replays samples through the buffer. sendUs are send times in
// microseconds and delay the matching delays in ms (any constant offset,
// such as a clock offset, is fine), both in send order. The buffer plays
// each sample at the recent minimum delay plus the larger of the recent
// 95th-percentile variation and the peak variation within Hold, plus a
// margin (within MinMs..MaxMs). It re-plans every few samples and straight
// after a late one (a real buffer grows as soon as it runs dry); a sample
// arriving after its playout point is late.
func (pp PlayoutParams) Playout(sendUs []int64, delay []float64) PlayoutResult {
	n := len(delay)
	res := PlayoutResult{Late: make([]bool, n), Target: make([]float64, n)}
	if n == 0 {
		return res
	}
	win, hold := pp.Window.Microseconds(), pp.Hold.Microseconds()
	lo := 0
	target := delay[0] + pp.MinMs
	scratch := make([]float64, 0, 256)
	late := 0
	wasLate := false
	var sum float64
	for k := 0; k < n; k++ {
		for lo < k && sendUs[k]-sendUs[lo] > win {
			lo++
		}
		if lo == k && k > 0 {
			// Nothing was sent within the window before this sample (a
			// gap or outage longer than the buffer's memory), so there is
			// no history to plan from. Start again as at the beginning of
			// the stream, as a real buffer does after it has run dry.
			target = delay[k] + pp.MinMs
		} else if k > 0 && (k%4 == 0 || wasLate || k-lo < 8) {
			// Re-plan every few samples, after a late sample, and on every
			// sample while history is short.
			base := math.Inf(1)
			for _, d := range delay[lo:k] {
				base = math.Min(base, d)
			}
			scratch = scratch[:0]
			peak := 0.0
			for i := lo; i < k; i++ {
				v := delay[i] - base
				scratch = append(scratch, v)
				if sendUs[k]-sendUs[i] <= hold {
					peak = math.Max(peak, v)
				}
			}
			spread := math.Max(percentile(scratch, 95), peak)
			target = base + math.Max(pp.MinMs, math.Min(pp.MaxMs, spread+pp.Margin))
		}
		res.Target[k] = target
		wasLate = delay[k] > target
		if wasLate {
			res.Late[k] = true
			late++
		}
		sum += target
	}
	res.LatePct = 100 * float64(late) / float64(n)
	res.MeanTarget = sum / float64(n)
	return res
}

// PlayoutLog replays a stream's packet log (any order) through the buffer.
func (pp PlayoutParams) PlayoutLog(log *PacketLog) PlayoutResult {
	if log == nil || len(log.Seq) == 0 || len(log.T1Us) != len(log.Seq) || len(log.RxUs) != len(log.Seq) {
		return PlayoutResult{}
	}
	idx := make([]int, len(log.Seq))
	for i := range idx {
		idx[i] = i
	}
	// Send order is sequence order.
	sortIdx(idx, func(a, b int) bool { return log.Seq[a] < log.Seq[b] })
	send := make([]int64, len(idx))
	delay := make([]float64, len(idx))
	for i, k := range idx {
		send[i] = log.T1Us[k]
		delay[i] = float64(log.RxUs[k]-log.T1Us[k]) / 1000
	}
	return pp.Playout(send, delay)
}
