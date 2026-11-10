package endpoint

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/guard"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
)

type sinkEntry struct {
	sink *flow.UDPSink
}

type sendEntry struct {
	done   chan struct{}
	result flow.UDPSendResult
	cancel context.CancelFunc
	peer   atomic.Pointer[net.UDPAddr] // per-stream return address, if punched
}

// maxStreamPeers bounds per-stream return addresses kept for one session.
const maxStreamPeers = 4096

// setStreamPeer records the return address for one stream.
func (ss *session) setStreamPeer(stream uint32, from *net.UDPAddr) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if _, ok := ss.peers[stream]; !ok && len(ss.peers) >= maxStreamPeers {
		return
	}
	a := *from
	ss.peers[stream] = &a
	if e := ss.sends[stream]; e != nil {
		e.peer.Store(&a)
	}
}

// udpLoop receives every UDP packet: reflects probes, feeds sinks,
// records NAT bindings and acknowledges PMTU probes.
func (s *Server) udpLoop(ctx context.Context) {
	buf := make([]byte, 65536)
	oob := make([]byte, netx.OOBSize)
	signers := map[uint64]*proto.Signer{}
	lastGC := time.Now()
	for {
		n, oobn, _, from, err := s.udp.ReadMsgUDP(buf, oob)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		rx := time.Now()
		ri := netx.ParseRecvInfo(oob[:oobn])
		if !ri.Time.IsZero() {
			rx = ri.Time
		}
		pkt := buf[:n]
		short, err := proto.PeekSession(pkt)
		if err != nil {
			continue
		}
		s.mu.Lock()
		sess := s.sessions[short]
		s.mu.Unlock()
		if sess == nil {
			continue
		}
		sg := signers[short]
		if sg == nil {
			sg = proto.NewSigner(sess.key)
			signers[short] = sg
		}
		if !sg.Verify(pkt) {
			continue
		}
		h, _ := proto.DecodeUDP(pkt)
		switch h.Type {
		case proto.UDPProbe:
			sess.udpPeer.Store(from)
			sess.mu.Lock()
			if ri.TTL >= 0 {
				sess.probeTTL[ri.TTL]++
			}
			if ri.TOS >= 0 {
				sess.probeTOS[ri.TOS]++
			}
			sess.mu.Unlock()
			h.Type, h.Flags, h.T2 = proto.UDPReply, proto.FlagReflected, rx.UnixNano()
			h.T3 = time.Now().UnixNano()
			sg.Encode(pkt, &h) // same length as received: never amplifies
			if h.TOSSent != 0 {
				// Reflect with the sender's marking (as TWAMP reflectors
				// do), so QoS treatment applies in both directions.
				_, _, _ = s.udp.WriteMsgUDP(pkt, netx.TOSControl(int(h.TOSSent), netx.IsV4(from)), from)
			} else {
				_, _ = s.udp.WriteToUDP(pkt, from)
			}
		case proto.UDPData:
			sess.mu.Lock()
			e := sess.sinks[h.Stream]
			sess.mu.Unlock()
			if e != nil {
				e.sink.Add(h, n, rx, ri)
			}
		case proto.UDPPunch, proto.UDPKeepalive:
			if h.Stream != 0 {
				sess.setStreamPeer(h.Stream, from)
			} else {
				sess.udpPeer.Store(from)
			}
		case proto.UDPPMTU:
			sess.mu.Lock()
			sess.pmtuSeen = max(sess.pmtuSeen, n)
			sess.mu.Unlock()
			ack := make([]byte, proto.UDPHeaderLen)
			sg.Encode(ack, &proto.UDPHeader{Type: proto.UDPPMTUAck, Flags: proto.FlagReflected, Session: short, Seq: h.Seq, Stream: uint32(n), T1: h.T1, T2: rx.UnixNano()})
			_, _ = s.udp.WriteToUDP(ack, from)
		}
		if time.Since(lastGC) > 10*time.Second {
			lastGC = time.Now()
			s.mu.Lock()
			for k := range signers {
				if s.sessions[k] == nil {
					delete(signers, k)
				}
			}
			s.mu.Unlock()
		}
	}
}

// UDPSinkReq registers a receiving stream.
type UDPSinkReq struct {
	BinMs int `json:"bin_ms"`
	// Log keeps a per-packet log and returns it with the result.
	Log bool `json:"log,omitempty"`
}

// UDPSinkReady returns the stream ID to send on.
type UDPSinkReady struct {
	Stream uint32 `json:"stream"`
}

// UDPSinkResultReq asks for a sink's result.
type UDPSinkResultReq struct {
	Stream uint32 `json:"stream"`
	Sent   uint64 `json:"sent"`
	Close  bool   `json:"close"`
}

func (ss *session) udpSink(r UDPSinkReq) UDPSinkReady {
	id := ss.allocStreams(1)[0]
	ss.mu.Lock()
	sink := flow.NewUDPSink(time.Duration(max(r.BinMs, 10)) * time.Millisecond)
	if r.Log {
		sink.EnableLog()
	}
	ss.sinks[id] = &sinkEntry{sink: sink}
	ss.mu.Unlock()
	return UDPSinkReady{Stream: id}
}

func (ss *session) udpSinkResult(r UDPSinkResultReq) (flow.SinkResult, error) {
	ss.mu.Lock()
	e := ss.sinks[r.Stream]
	if r.Close {
		delete(ss.sinks, r.Stream)
	}
	ss.mu.Unlock()
	if e == nil {
		return flow.SinkResult{}, errors.New("unknown stream")
	}
	return e.sink.Result(r.Sent), nil
}

// UDPSendReq asks the endpoint to send a paced stream to the client.
type UDPSendReq struct {
	Stream     uint32  `json:"stream"`
	RateBps    float64 `json:"rate_bps"`
	Size       int     `json:"size"`
	DurationMs int     `json:"duration_ms"`
	TOS        int     `json:"tos"`
	Count      uint64  `json:"count,omitempty"`
	Type       byte    `json:"type,omitempty"`
	Wait       bool    `json:"wait,omitempty"`
	// Video, if set, sends frames following this model instead of a
	// constant-rate stream (RateBps and Size are ignored).
	Video *flow.VideoModel `json:"video,omitempty"`
}

func (ss *session) udpSend(r UDPSendReq) (any, error) {
	peer := ss.udpPeer.Load()
	if peer == nil {
		return nil, errors.New("no UDP path from the client yet (send a punch first)")
	}
	rate := r.RateBps
	if r.Video != nil {
		if err := r.Video.Validate(); err != nil {
			return nil, err
		}
		rate = r.Video.BitrateBps
	}
	if err := ss.checkRate(rate); err != nil {
		return nil, err
	}
	if err := ss.checkDuration(time.Duration(r.DurationMs) * time.Millisecond); err != nil {
		return nil, err
	}
	if r.Size > 9000 {
		return nil, errors.New("size too large")
	}
	ctx, cancel := context.WithCancel(ss.ctx)
	e := &sendEntry{done: make(chan struct{}), cancel: cancel}
	ss.mu.Lock()
	ss.sends[r.Stream] = e
	if p := ss.peers[r.Stream]; p != nil {
		e.peer.Store(p)
	}
	ss.mu.Unlock()
	signer := proto.NewSigner(ss.key)
	v4 := netx.IsV4(peer)
	udp := ss.srv.udp
	run := func() {
		defer close(e.done)
		defer cancel()
		params := flow.UDPSendParams{
			Session: ss.id.Short(), Stream: r.Stream, Type: r.Type, RateBps: r.RateBps, Size: r.Size,
			Duration: time.Duration(r.DurationMs) * time.Millisecond, TOS: r.TOS, Count: r.Count,
		}
		send := func(b []byte, tos int) error {
			p := e.peer.Load()
			if p == nil {
				p = ss.udpPeer.Load()
			}
			if p == nil {
				p = peer
			}
			var err error
			if tos >= 0 {
				_, _, err = udp.WriteMsgUDP(b, netx.TOSControl(tos, v4), p)
			} else {
				_, err = udp.WriteToUDP(b, p)
			}
			return err
		}
		if r.Video != nil {
			e.result = flow.SendVideo(ctx, signer, params, *r.Video, send)
		} else {
			e.result = flow.SendUDP(ctx, signer, params, send)
		}
	}
	if r.Wait {
		run()
		return e.result, nil
	}
	guard.Go(ss.srv.log, "UDP send", run)
	// "pattern" tells clients this endpoint understood the video model
	// (older endpoints ignore it and send a constant-rate stream).
	if r.Video != nil {
		return map[string]any{"started": true, "pattern": "video"}, nil
	}
	return map[string]any{"started": true}, nil
}

// UDPSendResultReq waits for a send to finish.
type UDPSendResultReq struct {
	Stream uint32 `json:"stream"`
}

func (ss *session) udpSendResult(r UDPSendResultReq) (flow.UDPSendResult, error) {
	ss.mu.Lock()
	e := ss.sends[r.Stream]
	ss.mu.Unlock()
	if e == nil {
		return flow.UDPSendResult{}, errors.New("unknown stream")
	}
	select {
	case <-e.done:
	case <-time.After(5 * time.Minute):
		e.cancel()
		<-e.done
	}
	ss.mu.Lock()
	delete(ss.sends, r.Stream)
	delete(ss.peers, r.Stream)
	ss.mu.Unlock()
	return e.result, nil
}

// peerAddr returns the client's UDP address for reverse tests.
func (ss *session) peerAddr() *net.UDPAddr { return ss.udpPeer.Load() }
