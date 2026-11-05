// Package engine runs PathGauge tests from the client side. The client
// opens every TCP connection and UDP flow, so it works behind NAT; the
// endpoint only ever replies or sends to the address it observed.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/version"
)

// Client is an authenticated session with an endpoint.
type Client struct {
	Addr             string // host:port as dialled
	EndpointIP       netip.Addr
	Port             int
	Observed         string // our address as the endpoint sees it
	Local            string // our local address
	EndpointSoftware string

	ctrl *net.TCPConn
	sid  proto.SessionID
	skey []byte
	wmu  sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan *proto.Msg
	events  map[string]func(json.RawMessage)

	udp       *net.UDPConn
	udpMu     sync.Mutex
	replyFn   map[uint32]func(h proto.UDPHeader, rx time.Time, ri netx.RecvInfo)
	sinks     map[uint32]*flow.UDPSink
	pmtuRecv  chan int
	replyTTL  map[int]uint64
	nextLocal atomic.Uint32

	closed chan struct{}
	once   sync.Once
	err    atomic.Value
}

// Dial connects and authenticates. keyID is "psk" in peer mode.
func Dial(ctx context.Context, addr, keyID string, key []byte) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "7400")
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to endpoint %s: %w", addr, err)
	}
	c := &Client{
		Addr: addr, ctrl: conn.(*net.TCPConn),
		pending: map[uint64]chan *proto.Msg{}, events: map[string]func(json.RawMessage){},
		replyFn: map[uint32]func(proto.UDPHeader, time.Time, netx.RecvInfo){}, sinks: map[uint32]*flow.UDPSink{},
		pmtuRecv: make(chan int, 64), replyTTL: map[int]uint64{}, closed: make(chan struct{}),
	}
	c.nextLocal.Store(0x40000000)
	ra := conn.RemoteAddr().(*net.TCPAddr)
	ip, _ := netip.AddrFromSlice(ra.IP)
	c.EndpointIP, c.Port = ip.Unmap(), ra.Port
	c.Local = conn.LocalAddr().String()
	if err := c.handshake(keyID, key); err != nil {
		conn.Close()
		return nil, err
	}
	u, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ra.IP, Port: ra.Port})
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = netx.EnableRecvInfo(u)
	netx.SetBuffers(u, 8<<20)
	c.udp = u
	go c.readCtrl()
	go c.readUDP()
	go c.keepalive()
	c.punch()
	return c, nil
}

func (c *Client) handshake(keyID string, key []byte) error {
	_ = c.ctrl.SetDeadline(time.Now().Add(15 * time.Second))
	defer func() { _ = c.ctrl.SetDeadline(time.Time{}) }()
	if _, err := c.ctrl.Write([]byte{proto.Magic[0], proto.Magic[1], proto.Magic[2], proto.Magic[3], proto.ConnControl}); err != nil {
		return err
	}
	cn := proto.NewNonce()
	if err := proto.WriteMsg(c.ctrl, &proto.Msg{Type: "hello", Data: proto.Encode(proto.Hello{
		Version: proto.Version, KeyID: keyID, ClientNonce: cn, LocalAddr: c.Local, Software: version.Get().Version,
	})}); err != nil {
		return err
	}
	var m proto.Msg
	if err := proto.ReadMsg(c.ctrl, &m); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if m.Type == "error" {
		return fmt.Errorf("endpoint refused: %s (check the key)", m.Error)
	}
	var ch proto.Challenge
	if err := json.Unmarshal(m.Data, &ch); err != nil {
		return err
	}
	c.Observed, c.EndpointSoftware = ch.ObservedAddr, ch.Software
	if err := proto.WriteMsg(c.ctrl, &proto.Msg{Type: "auth", Data: proto.Encode(proto.Auth{MAC: proto.ClientMAC(key, cn, ch.ServerNonce)})}); err != nil {
		return err
	}
	if err := proto.ReadMsg(c.ctrl, &m); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if m.Type != "welcome" {
		return fmt.Errorf("endpoint refused: %s (check the key)", m.Error)
	}
	var w proto.Welcome
	if err := json.Unmarshal(m.Data, &w); err != nil {
		return err
	}
	if !proto.Equal(w.MAC, proto.ServerMAC(key, cn, ch.ServerNonce)) {
		return errors.New("endpoint failed to prove it knows the key")
	}
	sid, err := proto.ParseSessionID(w.SessionID)
	if err != nil {
		return err
	}
	c.sid, c.skey = sid, proto.SessionKey(key, cn, ch.ServerNonce)
	return nil
}

// Close ends the session.
func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.ctrl.Close()
		c.udp.Close()
	})
	return nil
}

func (c *Client) readCtrl() {
	defer c.Close()
	for {
		var m proto.Msg
		if err := proto.ReadMsg(c.ctrl, &m); err != nil {
			c.err.Store(err)
			c.mu.Lock()
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		switch m.Type {
		case "resp":
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		case "event":
			c.mu.Lock()
			fn := c.events[m.Op]
			c.mu.Unlock()
			if fn != nil {
				fn(m.Data)
			}
		}
	}
}

// OnEvent registers a handler for endpoint events of one kind.
func (c *Client) OnEvent(op string, fn func(json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		delete(c.events, op)
		return
	}
	c.events[op] = fn
}

// callTimeout bounds any single operation when the caller set no deadline,
// so a lost response can never hang a run forever.
const callTimeout = 5 * time.Minute

// Call performs one request/response operation.
func (c *Client) Call(ctx context.Context, op string, req, resp any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	ch := make(chan *proto.Msg, 1)
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	c.wmu.Lock()
	err := proto.WriteMsg(c.ctrl, &proto.Msg{Type: "req", ID: id, Op: op, Data: proto.Encode(req)})
	c.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: connection to endpoint lost", op)
		}
		if !m.OK {
			return fmt.Errorf("%s: %s", op, m.Error)
		}
		if resp != nil && len(m.Data) > 0 {
			return json.Unmarshal(m.Data, resp)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("%s: session closed", op)
	}
}

// DialOptions tunes a data connection.
type DialOptions struct {
	Congestion  string
	PacingBps   uint64 // bytes/s
	BufferBytes int
}

// DialData opens an authenticated TCP data connection for a stream.
func (c *Client) DialData(ctx context.Context, stream uint32, o DialOptions) (*net.TCPConn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	if o.BufferBytes > 0 {
		// Buffer sizes must be set before connect to affect window scaling.
		d.Control = func(_, _ string, rc syscall.RawConn) error {
			return rc.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, o.BufferBytes)
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, o.BufferBytes)
			})
		}
	}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, err
	}
	tc := conn.(*net.TCPConn)
	if err := netx.SetCongestion(tc, o.Congestion); err != nil {
		tc.Close()
		return nil, err
	}
	_ = netx.SetPacingRate(tc, o.PacingBps)
	h := proto.NewDataHeader(c.sid, stream, c.skey)
	if _, err := tc.Write(h.Marshal()); err != nil {
		tc.Close()
		return nil, err
	}
	return tc, nil
}

// ConnectTime measures a plain TCP handshake to the endpoint.
func (c *Client) ConnectTime(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return 0, err
	}
	el := time.Since(start)
	conn.Close()
	return el, nil
}

// LocalStream allocates a client-side stream ID (for reverse UDP flows).
func (c *Client) LocalStream() uint32 { return c.nextLocal.Add(1) }

// Session returns the short session ID used in UDP packets.
func (c *Client) Session() uint64 { return c.sid.Short() }

// Signer returns a new UDP signer for this session.
func (c *Client) Signer() *proto.Signer { return proto.NewSigner(c.skey) }

// SendUDP sends one signed packet with an optional TOS.
func (c *Client) SendUDP(b []byte, tos int) error {
	if tos >= 0 {
		_, _, err := c.udp.WriteMsgUDP(b, netx.TOSControl(tos, c.EndpointIP.Is4()), nil)
		return err
	}
	_, err := c.udp.Write(b)
	return err
}

// OnReply registers a handler for reflected probes on a stream.
func (c *Client) OnReply(stream uint32, fn func(h proto.UDPHeader, rx time.Time, ri netx.RecvInfo)) {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if fn == nil {
		delete(c.replyFn, stream)
		return
	}
	c.replyFn[stream] = fn
}

// AddSink registers a local sink for a reverse stream.
func (c *Client) AddSink(stream uint32, s *flow.UDPSink) {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if s == nil {
		delete(c.sinks, stream)
		return
	}
	c.sinks[stream] = s
}

// ReplyTTLs returns TTLs seen on reflected probes (for hop counting).
func (c *Client) ReplyTTLs() map[int]uint64 {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	out := map[int]uint64{}
	for k, v := range c.replyTTL {
		out[k] = v
	}
	return out
}

func (c *Client) readUDP() {
	buf := make([]byte, 65536)
	oob := make([]byte, netx.OOBSize)
	sg := c.Signer()
	for {
		n, oobn, _, _, err := c.udp.ReadMsgUDP(buf, oob)
		if err != nil {
			select {
			case <-c.closed:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// ICMP errors on a connected socket surface as read errors; keep going.
			time.Sleep(time.Millisecond)
			continue
		}
		rx := time.Now()
		ri := netx.ParseRecvInfo(oob[:oobn])
		if !ri.Time.IsZero() {
			rx = ri.Time
		}
		pkt := buf[:n]
		if !sg.Verify(pkt) {
			continue
		}
		h, err := proto.DecodeUDP(pkt)
		if err != nil {
			continue
		}
		switch h.Type {
		case proto.UDPReply:
			c.udpMu.Lock()
			fn := c.replyFn[h.Stream]
			if ri.TTL >= 0 {
				c.replyTTL[ri.TTL]++
			}
			c.udpMu.Unlock()
			if fn != nil {
				fn(h, rx, ri)
			}
		case proto.UDPData:
			c.udpMu.Lock()
			s := c.sinks[h.Stream]
			c.udpMu.Unlock()
			if s != nil {
				s.Add(h, n, rx, ri)
			}
		case proto.UDPPMTU:
			select {
			case c.pmtuRecv <- n:
			default:
			}
		}
	}
}

// punch opens or refreshes the NAT binding the endpoint sends to.
func (c *Client) punch() {
	b := make([]byte, proto.UDPHeaderLen)
	sg := c.Signer()
	sg.Encode(b, &proto.UDPHeader{Type: proto.UDPPunch, Session: c.sid.Short(), T1: time.Now().UnixNano()})
	_, _ = c.udp.Write(b)
}

// keepalive refreshes the NAT binding every 5 s, well inside the 30 s UDP
// timeouts some NATs use (docs/PLAN.md Phase 1, client behind NAT).
func (c *Client) keepalive() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			c.punch()
		}
	}
}

// Ensure the session is still alive.
func (c *Client) Alive() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}
