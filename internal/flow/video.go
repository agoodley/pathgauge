package flow

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/agoodley/pathgauge/internal/proto"
)

// VideoModel describes a simulated real-time video stream, as a video call
// sends it: frames at a fixed rate, a larger keyframe at the start of every
// group of pictures, and each frame split into packets that leave through a
// pacer running faster than the average bitrate (WebRTC paces at 2.5x).
//
// The model is deterministic, so the receiver can rebuild which packets
// belong to which frame from sequence numbers alone. The bitrate is fixed:
// real applications lower it under congestion, so results show the worst
// case for the chosen quality.
type VideoModel struct {
	BitrateBps float64 `json:"bitrate_bps"`
	FPS        int     `json:"fps,omitempty"`
	GOP        int     `json:"gop,omitempty"`         // frames from one keyframe to the next
	KeyRatio   float64 `json:"key_ratio,omitempty"`   // keyframe size / delta-frame size
	PacketSize int     `json:"packet_size,omitempty"` // largest UDP payload, bytes
	Pacing     float64 `json:"pacing,omitempty"`      // pacer rate as a multiple of the bitrate
}

// WithDefaults fills unset fields: 30 frames/s, a keyframe every 2 s that
// is 8x a delta frame, 1200-byte packets and a 2.5x pacer.
func (m VideoModel) WithDefaults() VideoModel {
	if m.FPS <= 0 {
		m.FPS = 30
	}
	if m.GOP <= 0 {
		m.GOP = 2 * m.FPS
	}
	if m.KeyRatio < 1 {
		m.KeyRatio = 8
	}
	if m.PacketSize <= 0 {
		m.PacketSize = 1200
	}
	if m.Pacing < 1 {
		m.Pacing = 2.5
	}
	return m
}

// Validate rejects models an endpoint should not run.
func (m VideoModel) Validate() error {
	switch {
	case m.BitrateBps <= 0 || m.BitrateBps > 1e9:
		return errors.New("video bitrate must be between 0 and 1 Gbit/s")
	case m.FPS > 240, m.GOP > 10000, m.KeyRatio > 100, m.Pacing > 20:
		return errors.New("video model out of range")
	case m.PacketSize != 0 && (m.PacketSize < proto.UDPHeaderLen || m.PacketSize > 9000):
		return errors.New("video packet size out of range")
	}
	return nil
}

// FrameInterval is the time between frames.
func (m VideoModel) FrameInterval() time.Duration {
	m = m.WithDefaults()
	return time.Second / time.Duration(m.FPS)
}

// FrameBytes returns the size of frame i (0-based).
func (m VideoModel) FrameBytes(i int) int {
	m = m.WithDefaults()
	gopBytes := m.BitrateBps / 8 * float64(m.GOP) / float64(m.FPS)
	delta := gopBytes / (float64(m.GOP-1) + m.KeyRatio)
	b := delta
	if i%m.GOP == 0 {
		b *= m.KeyRatio
	}
	return max(int(math.Round(b)), proto.UDPHeaderLen)
}

// FramePackets returns the packet sizes of frame i: full packets and a
// smaller last one, each at least a header long.
func (m VideoModel) FramePackets(i int) []int {
	m = m.WithDefaults()
	left := m.FrameBytes(i)
	var sizes []int
	for left > 0 {
		n := min(left, m.PacketSize)
		left -= n
		sizes = append(sizes, max(n, proto.UDPHeaderLen))
	}
	return sizes
}

// PacketGap is how long the pacer spaces a packet of size bytes.
func (m VideoModel) PacketGap(size int) time.Duration {
	m = m.WithDefaults()
	return time.Duration(float64(size*8) / (m.BitrateBps * m.Pacing) * float64(time.Second))
}

// FrameSendTime is how long the pacer takes to release all of frame i
// (from its first packet to its last).
func (m VideoModel) FrameSendTime(i int) time.Duration {
	sizes := m.FramePackets(i)
	var d time.Duration
	for _, s := range sizes[:len(sizes)-1] {
		d += m.PacketGap(s)
	}
	return d
}

// FrameStarts returns the first sequence number of frames 0..n-1, plus the
// sequence number after the last frame (len n+1).
func (m VideoModel) FrameStarts(n int) []uint64 {
	out := make([]uint64, n+1)
	for i := 0; i < n; i++ {
		out[i+1] = out[i] + uint64(len(m.FramePackets(i)))
	}
	return out
}

// SendVideo sends frames following m until the duration in p elapses or
// ctx ends. Only p.Session, p.Stream, p.Type, p.Duration and p.TOS are
// used; the model sets sizes and timing.
func SendVideo(ctx context.Context, signer *proto.Signer, p UDPSendParams, m VideoModel, send func(b []byte, tos int) error) UDPSendResult {
	m = m.WithDefaults()
	if p.Type == 0 {
		p.Type = proto.UDPData
	}
	if p.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Duration)
		defer cancel()
	}
	buf := make([]byte, m.PacketSize)
	copy(buf[proto.UDPHeaderLen:], Pool()[:m.PacketSize-proto.UDPHeaderLen])
	tosByte := byte(0)
	if p.TOS >= 0 {
		tosByte = byte(p.TOS)
	}
	var res UDPSendResult
	start := time.Now()
	next := start
	interval := m.FrameInterval()
	for frame := 0; ctx.Err() == nil; frame++ {
		frameAt := start.Add(time.Duration(frame) * interval)
		if p.Duration > 0 && frameAt.Sub(start) >= p.Duration {
			break
		}
		if next.Before(frameAt) {
			next = frameAt
		}
		for _, size := range m.FramePackets(frame) {
			if !sleepUntil(ctx, next) {
				break
			}
			h := proto.UDPHeader{Type: p.Type, Session: p.Session, Seq: res.Sent, Stream: p.Stream, T1: time.Now().UnixNano(), TOSSent: tosByte}
			b := buf[:size]
			signer.Encode(b, &h)
			if err := send(b, p.TOS); err != nil {
				res.Errors++
				res.LastErr = err.Error()
			} else {
				res.Bytes += uint64(size)
			}
			res.Sent++
			next = next.Add(m.PacketGap(size))
		}
	}
	res.Seconds = time.Since(start).Seconds()
	return res
}

// sleepUntil waits until t; it returns false if ctx ended first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}

// FrameStats summarises how frames arrived.
type FrameStats struct {
	Frames  int     `json:"frames"`
	OKPct   float64 `json:"ok_pct"`   // in time for playout (after any repair)
	LatePct float64 `json:"late_pct"` // after their playout time
	// RepairedPct is the share of frames that lost packets and needed a
	// retransmission; LostPct those that could not be repaired at all.
	RepairedPct float64 `json:"repaired_pct"`
	LostPct     float64 `json:"lost_pct"`
	Freezes     int     `json:"freezes"` // stalls of at least FreezeMs
	FreezeS     float64 `json:"freeze_s"`
	DelayP50Ms  float64 `json:"delay_p50_ms"` // network delay added per frame, vs the idle baseline
	DelayP95Ms  float64 `json:"delay_p95_ms"`
	// PlayoutMs is the mean delay the receiver's jitter buffer added on top
	// of the idle path (queueing plus buffering): the extra latency a
	// viewer would notice.
	PlayoutMs float64 `json:"playout_ms"`
}

// FreezeMs is the shortest stall counted as a visible freeze.
const FreezeMs = 200

// maxRepairLoss is the packet loss above which retransmission is not
// modelled (the repairs would be lost too, and real senders fall back to
// keyframes).
const maxRepairLoss = 0.10

// AnalyzeFrames rebuilds the frames of a video stream from its packet log
// and plays them through an adaptive video jitter buffer.
//
// Like real video calls (WebRTC NACK), a receiver that notices a missing
// packet (when the next one arrives) asks for it again, and the repair
// arrives one round trip (rttMs, plus 10 ms) later. A frame that misses
// its playout time stalls the picture for as long as it is late.
//
// baseOWDms is the raw one-way delay (receive time minus send time,
// including any clock offset) of the idle path in this direction; if NaN,
// the smallest frame delay is used.
func AnalyzeFrames(m VideoModel, log *PacketLog, sent uint64, baseOWDms, rttMs float64) FrameStats {
	m = m.WithDefaults()
	var st FrameStats
	if log == nil || sent == 0 {
		return st
	}
	// Count whole frames that were sent.
	nFrames := 0
	for total := uint64(0); total < sent; nFrames++ {
		total += uint64(len(m.FramePackets(nFrames)))
		if total > sent {
			break // the last frame was cut off by the end of the test
		}
	}
	if nFrames == 0 {
		return st
	}
	starts := m.FrameStarts(nFrames)
	total := starts[nFrames]
	rx := make([]int64, total)
	t1 := make([]int64, total)
	for i := range rx {
		rx[i] = -1
	}
	received := 0
	for k, seq := range log.Seq {
		if seq >= total {
			continue
		}
		if rx[seq] < 0 {
			received++
			rx[seq], t1[seq] = log.RxUs[k], log.T1Us[k]
		} else if log.RxUs[k] < rx[seq] {
			rx[seq], t1[seq] = log.RxUs[k], log.T1Us[k]
		}
	}
	if received == 0 {
		st.Frames, st.LostPct = nFrames, 100
		return st
	}
	// next[s] is the arrival of the first packet received at or after s:
	// when the receiver learns that an earlier packet is missing.
	next := make([]int64, total+1)
	next[total] = -1
	for s := int(total) - 1; s >= 0; s-- {
		if rx[s] >= 0 {
			next[s] = rx[s]
		} else {
			next[s] = next[s+1]
		}
	}
	interval := m.FrameInterval().Microseconds()
	// Capture time of each frame: from its received packets' send times
	// (minus the pacer's offset), or extrapolated from the others.
	capture := make([]int64, nFrames)
	have := make([]bool, nFrames)
	var offs []float64
	for i := 0; i < nFrames; i++ {
		var off time.Duration
		for j, size := range m.FramePackets(i) {
			s := starts[i] + uint64(j)
			if rx[s] >= 0 {
				if c := t1[s] - off.Microseconds(); !have[i] || c < capture[i] {
					capture[i], have[i] = c, true
				}
			}
			off += m.PacketGap(size)
		}
		if have[i] {
			offs = append(offs, float64(capture[i]-int64(i)*interval))
		}
	}
	t0 := int64(percentile(offs, 50))
	for i := range capture {
		if !have[i] {
			capture[i] = t0 + int64(i)*interval
		}
	}
	repair := !math.IsNaN(rttMs) && rttMs >= 0 && 1-float64(received)/float64(total) < maxRepairLoss
	repairUs := int64((rttMs + 10) * 1000)

	// When each frame became complete, if it ever did.
	arrival := make([]int64, nFrames)
	complete := make([]bool, nFrames)
	needRepair := make([]bool, nFrames)
	for i := 0; i < nFrames; i++ {
		last, repairAt := int64(-1), int64(-1)
		unrecoverable := false
		for s := starts[i]; s < starts[i+1]; s++ {
			if rx[s] >= 0 {
				last = max(last, rx[s])
				continue
			}
			needRepair[i] = true
			if d := next[s+1]; d >= 0 {
				repairAt = max(repairAt, d+repairUs)
			} else {
				unrecoverable = true // nothing arrived after it
			}
		}
		switch {
		case !needRepair[i]:
			complete[i], arrival[i] = true, last
		case repair && !unrecoverable:
			complete[i], arrival[i] = true, max(last, repairAt)
		}
	}
	var sendUs []int64
	var delays []float64
	for i := range complete {
		if complete[i] {
			sendUs = append(sendUs, capture[i])
			// Network delay, minus the pacer's own spreading of the frame.
			delays = append(delays, float64(arrival[i]-capture[i])/1000-float64(m.FrameSendTime(i).Microseconds())/1000)
		}
	}
	base := baseOWDms
	if math.IsNaN(base) {
		base = math.Inf(1)
		for _, d := range delays {
			base = math.Min(base, d)
		}
	}
	// Receivers that ask for retransmissions keep about a round trip of
	// buffer for them (WebRTC adds the RTT to its target delay); on long
	// paths they lean on error correction instead, so the RTT counted here
	// is capped.
	pp := VideoPlayout
	if repair {
		pp.MinMs = math.Max(pp.MinMs, math.Min(rttMs, 100)+10)
	}
	po := pp.Playout(sendUs, delays)
	frameMs := float64(interval) / 1000
	bad := make([]bool, nFrames)
	lateness := make([]float64, nFrames)
	ok, late, lost, repaired := 0, 0, 0, 0
	added := make([]float64, 0, len(delays))
	ci := 0
	for i := range complete {
		if needRepair[i] {
			repaired++
		}
		if !complete[i] {
			lost++
			bad[i], lateness[i] = true, frameMs
			continue
		}
		added = append(added, math.Max(0, delays[ci]-base))
		if po.Late[ci] {
			late++
			bad[i], lateness[i] = true, delays[ci]-po.Target[ci]
		} else {
			ok++
		}
		ci++
	}
	st.Frames = nFrames
	st.OKPct = 100 * float64(ok) / float64(nFrames)
	st.LatePct = 100 * float64(late) / float64(nFrames)
	st.LostPct = 100 * float64(lost) / float64(nFrames)
	st.RepairedPct = 100 * float64(repaired) / float64(nFrames)
	// A run of bad frames stalls the picture for the longer of the run and
	// its worst lateness (a frame 300 ms late is a 300 ms freeze).
	run, worst := 0, 0.0
	flush := func() {
		if run > 0 {
			if stall := math.Max(float64(run)*frameMs, worst); stall >= FreezeMs {
				st.Freezes++
				st.FreezeS += stall / 1000
			}
		}
		run, worst = 0, 0
	}
	for i, b := range bad {
		if b {
			run++
			worst = math.Max(worst, lateness[i])
		} else {
			flush()
		}
	}
	flush()
	if len(added) > 0 {
		st.DelayP50Ms = percentile(added, 50)
		st.DelayP95Ms = percentile(added, 95)
		st.PlayoutMs = math.Max(0, po.MeanTarget-base)
	}
	return st
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	r := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(r))
	hi := int(math.Ceil(r))
	return s[lo] + (s[hi]-s[lo])*(r-float64(lo))
}

func sortIdx(idx []int, less func(a, b int) bool) {
	sort.Slice(idx, func(i, j int) bool { return less(idx[i], idx[j]) })
}
