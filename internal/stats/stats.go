// Package stats holds the measurement maths shared by every test:
// summaries and percentiles, RFC 3550 jitter, sequence tracking (loss,
// reordering, duplicates, loss bursts) and voice-quality estimation.
package stats

import (
	"math"
	"slices"
)

// Summary describes a set of samples. All values use the samples' unit.
type Summary struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
	StdDev float64 `json:"stddev"`
}

// Summarize computes a Summary without modifying xs.
func Summarize(xs []float64) Summary {
	if len(xs) == 0 {
		return Summary{}
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	mean := sum / float64(len(s))
	var sq float64
	for _, x := range s {
		sq += (x - mean) * (x - mean)
	}
	return Summary{
		N:      len(s),
		Min:    s[0],
		Mean:   mean,
		Median: Percentile(s, 50),
		P90:    Percentile(s, 90),
		P95:    Percentile(s, 95),
		P99:    Percentile(s, 99),
		Max:    s[len(s)-1],
		StdDev: math.Sqrt(sq / float64(len(s))),
	}
}

// Percentile returns the p-th percentile (0-100) of sorted samples using
// linear interpolation between closest ranks.
func Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n == 1 || p <= 0:
		return sorted[0]
	case p >= 100:
		return sorted[n-1]
	}
	rank := p / 100 * float64(n-1)
	lo := int(math.Floor(rank))
	frac := rank - float64(lo)
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}

// Jitter is the RFC 3550 interarrival jitter estimator. Feed it the transit
// time (receive time minus send time, in any consistent unit) of packets in
// arrival order; clock offset between the ends cancels out.
type Jitter struct {
	prev float64
	have bool
	j    float64
}

// Add records one packet's transit time.
func (j *Jitter) Add(transit float64) {
	if j.have {
		d := math.Abs(transit - j.prev)
		j.j += (d - j.j) / 16
	}
	j.prev, j.have = transit, true
}

// Value returns the current jitter estimate.
func (j *Jitter) Value() float64 { return j.j }

// IPDV returns the mean absolute difference between consecutive delays
// (RFC 5481 inter-packet delay variation, averaged).
func IPDV(delays []float64) float64 {
	if len(delays) < 2 {
		return 0
	}
	var sum float64
	for i := 1; i < len(delays); i++ {
		sum += math.Abs(delays[i] - delays[i-1])
	}
	return sum / float64(len(delays)-1)
}

// SeqTracker counts loss, reordering and duplication from sequence numbers
// starting at 0, recorded in arrival order.
type SeqTracker struct {
	seen       []uint64 // bitset
	highest    uint64
	any        bool
	Received   uint64 // unique packets
	Duplicates uint64
	Reordered  uint64 // arrived after a higher sequence number
}

// Add records the arrival of sequence number seq.
func (t *SeqTracker) Add(seq uint64) {
	word, bit := seq/64, seq%64
	for uint64(len(t.seen)) <= word {
		t.seen = append(t.seen, 0)
	}
	if t.seen[word]&(1<<bit) != 0 {
		t.Duplicates++
		return
	}
	t.seen[word] |= 1 << bit
	t.Received++
	if t.any && seq < t.highest {
		t.Reordered++
	}
	if !t.any || seq > t.highest {
		t.highest = seq
	}
	t.any = true
}

// LossStats summarises missing sequence numbers.
type LossStats struct {
	Sent       uint64  `json:"sent"`
	Received   uint64  `json:"received"`
	Lost       uint64  `json:"lost"`
	LossPct    float64 `json:"loss_pct"`
	Duplicates uint64  `json:"duplicates"`
	Reordered  uint64  `json:"reordered"`
	ReorderPct float64 `json:"reorder_pct"`
	// Bursts is the number of runs of consecutive lost packets, and
	// MaxBurst the longest run: isolated losses suggest random loss,
	// long runs suggest congestion or outages.
	Bursts   uint64 `json:"bursts"`
	MaxBurst uint64 `json:"max_burst"`
}

// Loss computes loss statistics given how many packets were sent
// (sequence numbers 0..sent-1). When sent is 0 the highest sequence number
// seen is used instead.
func (t *SeqTracker) Loss(sent uint64) LossStats {
	if sent == 0 && t.any {
		sent = t.highest + 1
	}
	ls := LossStats{Sent: sent, Received: t.Received, Duplicates: t.Duplicates, Reordered: t.Reordered}
	var run uint64
	for seq := uint64(0); seq < sent; seq++ {
		word, bit := seq/64, seq%64
		got := word < uint64(len(t.seen)) && t.seen[word]&(1<<bit) != 0
		if got {
			if run > 0 {
				ls.Bursts++
				ls.MaxBurst = max(ls.MaxBurst, run)
				run = 0
			}
			continue
		}
		ls.Lost++
		run++
	}
	if run > 0 {
		ls.Bursts++
		ls.MaxBurst = max(ls.MaxBurst, run)
	}
	if sent > 0 {
		ls.LossPct = 100 * float64(ls.Lost) / float64(sent)
	}
	if t.Received > 0 {
		ls.ReorderPct = 100 * float64(t.Reordered) / float64(t.Received)
	}
	return ls
}

// MOS estimates voice quality (1-5) with the simplified ITU-T G.107
// E-model commonly used for network assessment: oneWayMs is mouth-to-ear
// network delay, jitterMs the jitter and lossPct the packet loss.
// It also returns the R-factor.
func MOS(oneWayMs, jitterMs, lossPct float64) (mos, r float64) {
	eff := oneWayMs + 2*jitterMs + 10
	if eff < 160 {
		r = 93.2 - eff/40
	} else {
		r = 93.2 - (eff-120)/10
	}
	r -= 2.5 * lossPct
	r = math.Max(0, math.Min(100, r))
	mos = 1 + 0.035*r + 0.000007*r*(r-60)*(100-r)
	return math.Max(1, math.Min(4.5, mos)), r
}

// Grade turns a latency increase under load (ms, usually p95 loaded minus
// idle median) into the bufferbloat grade used across the product.
// Thresholds follow the widely used A+..F scale and are documented in
// docs/PLAN.md §7.3.
func Grade(increaseMs float64) string {
	switch {
	case increaseMs < 5:
		return "A+"
	case increaseMs < 30:
		return "A"
	case increaseMs < 60:
		return "B"
	case increaseMs < 200:
		return "C"
	case increaseMs < 400:
		return "D"
	default:
		return "F"
	}
}

// GradeRank orders grades from best (0) to worst; unknown grades rank last.
func GradeRank(g string) int {
	if i := slices.Index([]string{"A+", "A", "B", "C", "D", "F"}, g); i >= 0 {
		return i
	}
	return 99
}

// WorstGrade returns the worse of two grades.
func WorstGrade(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || GradeRank(a) >= GradeRank(b) {
		return a
	}
	return b
}

// RPM converts a working latency (ms) to round-trips per minute, the
// responsiveness unit of the IETF IPPM responsiveness methodology.
func RPM(workingLatencyMs float64) float64 {
	if workingLatencyMs <= 0 {
		return 0
	}
	return 60000 / workingLatencyMs
}

// Mathis returns the loss-limited TCP throughput bound in bits/s
// (Mathis et al. 1997): MSS/RTT * 1.22/sqrt(p).
func Mathis(mssBytes, rttSec, lossFraction float64) float64 {
	if rttSec <= 0 || lossFraction <= 0 {
		return math.Inf(1)
	}
	return mssBytes * 8 / rttSec * 1.22 / math.Sqrt(lossFraction)
}

// Round rounds to n decimal places, for tidy JSON output.
func Round(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}
