package endpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"

	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/trace"
	"github.com/agoodley/pathgauge/internal/version"
)

// InfoResp describes the endpoint.
type InfoResp struct {
	Software string           `json:"software"`
	Host     *hostinfo.Report `json:"host"`
	Clock    results.Clock    `json:"clock"`
	Policy   results.Policy   `json:"policy"`
	Observed string           `json:"observed"`
}

// TraceReq asks the endpoint to trace back towards the client.
type TraceReq struct {
	Target  string `json:"target,omitempty"` // default: the client's observed address
	Cycles  int    `json:"cycles"`
	MaxHops int    `json:"max_hops"`
	Method  string `json:"method,omitempty"`
	Resolve bool   `json:"resolve"`
}

// PMTUSendReq asks for DF probes of one size towards the client.
type PMTUSendReq struct {
	Size  int `json:"size"`
	Count int `json:"count"`
}

// PMTUSendResp reports local send errors.
type PMTUSendResp struct {
	Sent     int    `json:"sent"`
	Errors   int    `json:"errors"`
	LastErr  string `json:"last_error,omitempty"`
	LocalMTU int    `json:"local_mtu"`
	PTBSeen  bool   `json:"ptb_seen"`
	PTBMTU   int    `json:"ptb_mtu,omitempty"`
}

// ProbeStats reports what the endpoint saw on the client's probes.
type ProbeStats struct {
	TTL      map[int]uint64 `json:"ttl"`
	TOS      map[int]uint64 `json:"tos"`
	PMTUSeen int            `json:"pmtu_seen"`
}

// CurrentClock reads this host's clock state.
func CurrentClock() results.Clock {
	c := results.Clock{UnixNanoAtMsg: time.Now().UnixNano()}
	if st, err := hostinfo.SystemSources().Clock(); err == nil {
		c.Synced = st.Synced
		c.EstErrorMs = float64(st.EstErrorMicros) / 1000
		c.MaxErrorMs = float64(st.MaxErrorMicros) / 1000
	}
	return c
}

func decode[T any](data json.RawMessage) (T, error) {
	var v T
	if len(data) == 0 {
		return v, nil
	}
	err := json.Unmarshal(data, &v)
	return v, err
}

func (ss *session) handle(op string, data json.RawMessage) (any, error) {
	switch op {
	case "ping":
		return map[string]int64{"unix_nano": time.Now().UnixNano()}, nil
	case "info":
		rep := hostinfo.Collect(hostinfo.SystemSources())
		p := ss.srv.opts.Policy
		return InfoResp{
			Software: version.Get().Version,
			Host:     &rep,
			Clock:    CurrentClock(),
			Policy:   results.Policy{MaxRateMbps: p.MaxRateMbps, MaxTestDurationSec: int(p.MaxTestDuration / time.Second), Shaping: string(p.Shaping)},
			Observed: ss.observed.String(),
		}, nil
	case "tcp_prepare":
		p, err := decode[TCPPrepare](data)
		if err != nil {
			return nil, err
		}
		return ss.tcpPrepare(p)
	case "tcp_result":
		r, err := decode[struct {
			TestID string `json:"test_id"`
			WaitMs int    `json:"wait_ms"`
		}](data)
		if err != nil {
			return nil, err
		}
		return ss.tcpResult(r.TestID, time.Duration(max(r.WaitMs, 1000))*time.Millisecond)
	case "udp_sink":
		r, err := decode[UDPSinkReq](data)
		if err != nil {
			return nil, err
		}
		return ss.udpSink(r), nil
	case "udp_sink_result":
		r, err := decode[UDPSinkResultReq](data)
		if err != nil {
			return nil, err
		}
		return ss.udpSinkResult(r)
	case "udp_send":
		r, err := decode[UDPSendReq](data)
		if err != nil {
			return nil, err
		}
		return ss.udpSend(r)
	case "udp_send_result":
		r, err := decode[UDPSendResultReq](data)
		if err != nil {
			return nil, err
		}
		return ss.udpSendResult(r)
	case "probe_stats":
		ss.mu.Lock()
		defer ss.mu.Unlock()
		out := ProbeStats{TTL: map[int]uint64{}, TOS: map[int]uint64{}, PMTUSeen: ss.pmtuSeen}
		for k, v := range ss.probeTTL {
			out.TTL[k] = v
		}
		for k, v := range ss.probeTOS {
			out.TOS[k] = v
		}
		ss.pmtuSeen = 0
		return out, nil
	case "trace":
		r, err := decode[TraceReq](data)
		if err != nil {
			return nil, err
		}
		return ss.trace(r)
	case "pmtu_send":
		r, err := decode[PMTUSendReq](data)
		if err != nil {
			return nil, err
		}
		return ss.pmtuSend(r)
	case "shape_apply":
		if ss.srv.opts.Shaper == nil || ss.srv.opts.Policy.Shaping == "none" {
			return nil, errors.New("shaping is not permitted on this endpoint")
		}
		ss.usedShaping.Store(true)
		return ss.srv.opts.Shaper.Apply(ss.ctx, data, ss.observed.Addr())
	case "shape_clear":
		if ss.srv.opts.Shaper == nil {
			return nil, errors.New("shaping is not available on this endpoint")
		}
		return map[string]bool{"cleared": true}, ss.srv.opts.Shaper.Clear(ss.ctx)
	}
	return nil, fmt.Errorf("unknown operation %q", op)
}

func (ss *session) trace(r TraceReq) (results.Trace, error) {
	target := ss.observed.Addr()
	if r.Target != "" {
		a, err := netip.ParseAddr(r.Target)
		if err != nil {
			return results.Trace{}, err
		}
		// Only allow tracing back to the client, never arbitrary hosts.
		if a != target {
			return results.Trace{}, errors.New("an endpoint only traces back to its client")
		}
		target = a
	}
	cycles := min(max(r.Cycles, 1), 100)
	tr := trace.Run(ss.ctx, target.Unmap(), trace.Options{Cycles: cycles, MaxHops: r.MaxHops, Method: r.Method, Resolve: r.Resolve})
	tr.Direction = results.Down
	return tr, nil
}

func (ss *session) pmtuSend(r PMTUSendReq) (PMTUSendResp, error) {
	peer := ss.peerAddr()
	if peer == nil {
		return PMTUSendResp{}, errors.New("no UDP path from the client yet")
	}
	if r.Size < proto.UDPHeaderLen || r.Size > 9000 {
		return PMTUSendResp{}, errors.New("size out of range")
	}
	count := min(max(r.Count, 1), 5)
	sg := proto.NewSigner(ss.key)
	b := make([]byte, r.Size)
	var resp PMTUSendResp
	for i := 0; i < count; i++ {
		sg.Encode(b, &proto.UDPHeader{Type: proto.UDPPMTU, Session: ss.id.Short(), Seq: uint64(i), Stream: uint32(r.Size), T1: time.Now().UnixNano()})
		if _, err := ss.srv.udp.WriteToUDP(b, peer); err != nil {
			resp.Errors++
			resp.LastErr = err.Error()
		} else {
			resp.Sent++
		}
		time.Sleep(5 * time.Millisecond)
	}
	// ICMP "fragmentation needed" replies to these probes land in the
	// listener's error queue.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 64; i++ {
		se, err := netx.ReadErrQueue(ss.srv.udp)
		if err != nil {
			break
		}
		if se.Errno == syscall.EMSGSIZE {
			resp.PTBSeen = true
			if se.Info > 0 {
				resp.PTBMTU = int(se.Info)
			}
		}
	}
	return resp, nil
}
