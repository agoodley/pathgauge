// Package flow holds traffic helpers shared by the client engine and the
// endpoint: an incompressible payload pool, TCP stream sending/receiving
// with per-interval sampling, and paced UDP sending.
package flow

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agoodley/pathgauge/internal/netx"
)

// poolSize is the size of the random payload pool. Writes start at
// rotating offsets so consecutive writes are not byte-identical; a 16 MiB
// pool defeats the compression and deduplication of typical WAN optimisers.
const poolSize = 16 << 20

var (
	poolOnce sync.Once
	pool     []byte
)

// Pool returns the shared random payload pool.
func Pool() []byte {
	poolOnce.Do(func() {
		var seed [32]byte
		for i := range seed {
			seed[i] = byte(rand.Uint32())
		}
		r := rand.NewChaCha8(seed)
		pool = make([]byte, poolSize+1<<20)
		_, _ = r.Read(pool)
	})
	return pool
}

// CompressiblePool returns a highly compressible payload (zeros), used
// only by the compressibility test that detects WAN optimisers.
func CompressiblePool() []byte { return make([]byte, 1<<20+128<<10) }

// Stream is one TCP data stream's live counters.
type Stream struct {
	ID    uint32
	Conn  *net.TCPConn
	bytes atomic.Uint64
	done  atomic.Bool
}

// Bytes returns bytes moved so far.
func (s *Stream) Bytes() uint64 { return s.bytes.Load() }

// Done reports whether the stream finished.
func (s *Stream) Done() bool { return s.done.Load() }

// Send writes payload until ctx ends, the peer closes, or limit bytes
// (if > 0) have been written. chunk is the write size. compressible
// selects the zero payload.
func (s *Stream) Send(ctx context.Context, chunk int, compressible bool, limit uint64) error {
	defer s.done.Store(true)
	src := Pool()
	if compressible {
		src = CompressiblePool()
	}
	if chunk <= 0 {
		chunk = 128 << 10
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Conn.SetWriteDeadline(time.Unix(1, 0)) })
	defer stop()
	off := 0
	end := len(src) - chunk
	for ctx.Err() == nil {
		want := chunk
		if limit > 0 {
			left := limit - s.bytes.Load()
			if left == 0 {
				return nil
			}
			want = int(min(uint64(chunk), left))
		}
		n, err := s.Conn.Write(src[off : off+want])
		s.bytes.Add(uint64(n))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		off += 4099 // odd stride: successive writes never align
		if off >= end {
			off %= 65536
		}
	}
	return nil
}

// Receive reads until EOF or ctx ends.
func (s *Stream) Receive(ctx context.Context) error {
	defer s.done.Store(true)
	stop := context.AfterFunc(ctx, func() { _ = s.Conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	buf := make([]byte, 256<<10)
	for {
		n, err := s.Conn.Read(buf)
		s.bytes.Add(uint64(n))
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// Sample is one interval's snapshot of a set of streams.
type Sample struct {
	T     float64               // seconds since sampling began
	Bytes []uint64              // per stream, bytes in this interval
	Info  []netx.TCPInfoSummary // per stream, TCP_INFO at the end of the interval
}

// Sampler records per-interval byte counts and TCP_INFO for streams.
type Sampler struct {
	Interval time.Duration
	mu       sync.Mutex
	samples  []Sample
	start    time.Time
}

// Run samples until ctx ends or every stream is done. onSample, if set,
// is called after each interval (for live progress).
func (sm *Sampler) Run(ctx context.Context, streams []*Stream, onSample func(Sample)) {
	if sm.Interval <= 0 {
		sm.Interval = 100 * time.Millisecond
	}
	sm.start = time.Now()
	last := make([]uint64, len(streams))
	tick := time.NewTicker(sm.Interval)
	defer tick.Stop()
	snap := func() bool {
		s := Sample{T: time.Since(sm.start).Seconds(), Bytes: make([]uint64, len(streams)), Info: make([]netx.TCPInfoSummary, len(streams))}
		allDone := true
		for i, st := range streams {
			b := st.Bytes()
			s.Bytes[i] = b - last[i]
			last[i] = b
			if st.Conn != nil {
				if ti, err := netx.TCPInfo(st.Conn); err == nil {
					s.Info[i] = netx.Summarize(ti)
				}
			}
			if !st.Done() {
				allDone = false
			}
		}
		sm.mu.Lock()
		sm.samples = append(sm.samples, s)
		sm.mu.Unlock()
		if onSample != nil {
			onSample(s)
		}
		return allDone
	}
	for {
		select {
		case <-ctx.Done():
			snap()
			return
		case <-tick.C:
			if snap() {
				return
			}
		}
	}
}

// Samples returns the recorded samples.
func (sm *Sampler) Samples() []Sample {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return append([]Sample(nil), sm.samples...)
}

// Pacer releases packets at a target rate in small batches, which keeps
// the timer overhead low at high packet rates while staying smooth.
type Pacer struct {
	start    time.Time
	pps      float64
	released float64
}

// NewPacer starts pacing at pps packets per second.
func NewPacer(pps float64) *Pacer { return &Pacer{start: time.Now(), pps: pps} }

// Next blocks until at least one more packet is due and returns how many
// may be sent now (capped at maxBatch). It returns 0 when ctx ends. The
// context is checked on every call: a sender that cannot keep up is always
// behind schedule and would otherwise never wait, so never see the
// deadline (found on a 1-vCPU VPS sending at ~100k packets/s).
func (p *Pacer) Next(ctx context.Context, maxBatch int) int {
	for {
		if ctx.Err() != nil {
			return 0
		}
		due := time.Since(p.start).Seconds()*p.pps - p.released
		if due >= 1 {
			n := min(int(due), maxBatch)
			p.released += float64(n)
			return n
		}
		wait := time.Duration((1 - due) / p.pps * float64(time.Second))
		if wait < 50*time.Microsecond {
			wait = 50 * time.Microsecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return 0
		case <-t.C:
		}
	}
}
