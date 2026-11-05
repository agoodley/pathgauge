package flow

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/stats"
)

// UDPSink accounts for one received UDP stream. It is safe for concurrent use.
type UDPSink struct {
	mu       sync.Mutex
	seq      stats.SeqTracker
	jitter   stats.Jitter
	owd      []float32 // raw one-way delay (rx - t1) in ms, includes clock offset
	bytes    uint64
	first    time.Time
	last     time.Time
	tos      map[int]uint64
	tosPairs map[[2]int]uint64 // (sent, received)
	ttl      map[int]uint64
	binDur   time.Duration
	bins     []sinkBin
	// Socket overflow counter at the first and latest packet.
	dropsFirst, dropsLast uint32
	haveDrops             bool
}

type sinkBin struct {
	count  uint64
	bytes  uint64
	owdMin float64
	owdSum float64
}

// maxOWDSamples bounds memory for very long or fast streams.
const maxOWDSamples = 2_000_000

// NewUDPSink creates a sink that also bins arrivals into binDur slots.
func NewUDPSink(binDur time.Duration) *UDPSink {
	if binDur <= 0 {
		binDur = 100 * time.Millisecond
	}
	return &UDPSink{tos: map[int]uint64{}, tosPairs: map[[2]int]uint64{}, ttl: map[int]uint64{}, binDur: binDur}
}

// Add records one verified packet.
func (s *UDPSink) Add(h proto.UDPHeader, size int, rx time.Time, ri netx.RecvInfo) {
	owd := float64(rx.UnixNano()-h.T1) / 1e6
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first.IsZero() {
		s.first = rx
	}
	// The kernel attaches the socket's cumulative overflow count only to
	// datagrams queued after a drop; no count means zero so far. (Our
	// sockets always enable SO_RXQ_OVFL.)
	if !s.haveDrops {
		s.dropsFirst, s.haveDrops = ri.Drops, true
	}
	s.dropsLast = max(s.dropsLast, ri.Drops)
	if rx.After(s.last) {
		s.last = rx
	}
	s.seq.Add(h.Seq)
	s.jitter.Add(owd)
	if len(s.owd) < maxOWDSamples {
		s.owd = append(s.owd, float32(owd))
	}
	s.bytes += uint64(size)
	if ri.TOS >= 0 {
		s.tos[ri.TOS]++
		s.tosPairs[[2]int{int(h.TOSSent), ri.TOS}]++
	}
	if ri.TTL >= 0 {
		s.ttl[ri.TTL]++
	}
	idx := int(rx.Sub(s.first) / s.binDur)
	for len(s.bins) <= idx {
		s.bins = append(s.bins, sinkBin{owdMin: math.Inf(1)})
	}
	b := &s.bins[idx]
	b.count++
	b.bytes += uint64(size)
	b.owdSum += owd
	b.owdMin = math.Min(b.owdMin, owd)
}

// SinkResult is the receiver-side view of a UDP stream.
type SinkResult struct {
	Loss        stats.LossStats `json:"loss"`
	Bytes       uint64          `json:"bytes"`
	Seconds     float64         `json:"seconds"`
	JitterMs    float64         `json:"jitter_ms"`
	OWDRaw      stats.Summary   `json:"owd_raw_ms"` // includes clock offset
	TOS         map[int]uint64  `json:"tos"`
	TOSPairs    [][3]int        `json:"tos_pairs"` // sent, received, count
	TTL         map[int]uint64  `json:"ttl"`
	Bins        []SinkBinResult `json:"bins"`
	FirstHalfB  uint64          `json:"first_half_bytes"`
	SecondHalfB uint64          `json:"second_half_bytes"`
	FirstUnixNs int64           `json:"first_unix_ns"`
	// HostDrops counts datagrams the receiving host itself dropped (socket
	// buffer full) during the stream: loss caused by the receiver, not
	// the network. -1 when the kernel does not report it.
	HostDrops int64 `json:"host_drops"`
	// LatePct is the share of received packets whose delay exceeds the
	// stream's minimum by more than N ms (jitter-buffer late loss), by N.
	LatePct map[int]float64 `json:"late_pct"`
}

// SinkBinResult is one bin of a SinkResult.
type SinkBinResult struct {
	Count     uint64  `json:"count"`
	Bytes     uint64  `json:"bytes"`
	OWDMeanMs float64 `json:"owd_mean_ms"`
	OWDMinMs  float64 `json:"owd_min_ms"`
}

// Result summarises the stream, given how many packets were sent (0 to infer).
func (s *UDPSink) Result(sent uint64) SinkResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := SinkResult{
		HostDrops: -1,
		Loss:      s.seq.Loss(sent),
		Bytes:     s.bytes,
		JitterMs:  s.jitter.Value(),
		TOS:       copyMap(s.tos),
		TTL:       copyMap(s.ttl),
	}
	if s.haveDrops {
		r.HostDrops = int64(s.dropsLast - s.dropsFirst)
	}
	if !s.first.IsZero() {
		r.Seconds = s.last.Sub(s.first).Seconds()
		r.FirstUnixNs = s.first.UnixNano()
	}
	owd := make([]float64, len(s.owd))
	for i, v := range s.owd {
		owd[i] = float64(v)
	}
	r.OWDRaw = stats.Summarize(owd)
	r.LatePct = map[int]float64{}
	if len(owd) > 0 {
		for _, thr := range []int{20, 40, 60, 100} {
			late := 0
			for _, v := range owd {
				if v-r.OWDRaw.Min > float64(thr) {
					late++
				}
			}
			r.LatePct[thr] = 100 * float64(late) / float64(len(owd))
		}
	}
	for k, n := range s.tosPairs {
		r.TOSPairs = append(r.TOSPairs, [3]int{k[0], k[1], int(n)})
	}
	sort.Slice(r.TOSPairs, func(i, j int) bool { return r.TOSPairs[i][0] < r.TOSPairs[j][0] })
	half := len(s.bins) / 2
	for i, b := range s.bins {
		br := SinkBinResult{Count: b.count, Bytes: b.bytes}
		if b.count > 0 {
			br.OWDMeanMs = b.owdSum / float64(b.count)
			br.OWDMinMs = b.owdMin
		}
		r.Bins = append(r.Bins, br)
		if i < half {
			r.FirstHalfB += b.bytes
		} else {
			r.SecondHalfB += b.bytes
		}
	}
	return r
}

func copyMap(m map[int]uint64) map[int]uint64 {
	out := make(map[int]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// UDPSendParams describes a paced UDP stream.
type UDPSendParams struct {
	Session  uint64
	Stream   uint32
	Type     byte
	RateBps  float64 // bits per second
	Size     int     // bytes per packet (UDP payload), >= proto.UDPHeaderLen
	Duration time.Duration
	TOS      int // -1: leave unset
	// Count, if > 0, sends exactly this many packets as fast as pacing allows.
	Count uint64
}

// UDPSendResult reports what was sent.
type UDPSendResult struct {
	Sent    uint64  `json:"sent"`
	Bytes   uint64  `json:"bytes"`
	Seconds float64 `json:"seconds"`
	Errors  uint64  `json:"errors"`
	LastErr string  `json:"last_error,omitempty"`
}

// SendUDP paces packets through send until Duration (or Count) is reached.
// send receives the fully signed packet and the TOS to apply (-1 for none).
func SendUDP(ctx context.Context, signer *proto.Signer, p UDPSendParams, send func(b []byte, tos int) error) UDPSendResult {
	if p.Size < proto.UDPHeaderLen {
		p.Size = proto.UDPHeaderLen
	}
	if p.Type == 0 {
		p.Type = proto.UDPData
	}
	pps := p.RateBps / 8 / float64(p.Size)
	if pps <= 0 {
		pps = 1
	}
	if p.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Duration)
		defer cancel()
	}
	buf := make([]byte, p.Size)
	src := Pool()
	copy(buf[proto.UDPHeaderLen:], src[:p.Size-proto.UDPHeaderLen])
	pacer := NewPacer(pps)
	start := time.Now()
	var res UDPSendResult
	tosByte := byte(0)
	if p.TOS >= 0 {
		tosByte = byte(p.TOS)
	}
	for p.Count == 0 || res.Sent < p.Count {
		n := pacer.Next(ctx, 64)
		if n == 0 {
			break
		}
		for i := 0; i < n && (p.Count == 0 || res.Sent < p.Count); i++ {
			h := proto.UDPHeader{Type: p.Type, Session: p.Session, Seq: res.Sent, Stream: p.Stream, T1: time.Now().UnixNano(), TOSSent: tosByte}
			signer.Encode(buf, &h)
			if err := send(buf, p.TOS); err != nil {
				res.Errors++
				res.LastErr = err.Error()
			} else {
				res.Bytes += uint64(p.Size)
			}
			res.Sent++
		}
	}
	res.Seconds = time.Since(start).Seconds()
	return res
}
