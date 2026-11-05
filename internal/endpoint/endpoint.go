// Package endpoint implements the responder side of PathGauge's data
// plane: one port for authenticated TCP control sessions, TCP data streams
// and UDP probes/streams. Clients always open connections, so an endpoint
// works with clients behind NAT.
package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/netx"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/version"
)

// KeyStore resolves a client's key ID to its shared secret.
type KeyStore interface {
	Lookup(keyID string) ([]byte, bool)
}

// StaticKeys is a fixed KeyStore.
type StaticKeys map[string][]byte

// Lookup implements KeyStore.
func (s StaticKeys) Lookup(id string) ([]byte, bool) {
	k, ok := s[id]
	return k, ok
}

// Shaper applies time-limited traffic shaping on request (optional).
type Shaper interface {
	Apply(ctx context.Context, req json.RawMessage) (any, error)
	Clear(ctx context.Context) error
}

// Options configures an endpoint.
type Options struct {
	Listen string
	Keys   KeyStore
	Policy config.Policy
	Logger *slog.Logger
	Shaper Shaper
	// OnSession is called when a session authenticates (for the server's
	// bookkeeping); keyID identifies the client.
	OnSession func(keyID, observed string)
}

// Server is a running endpoint.
type Server struct {
	opts Options
	log  *slog.Logger
	tcp  net.Listener
	udp  *net.UDPConn

	mu       sync.Mutex
	sessions map[uint64]*session
	active   atomic.Int64
}

// New creates an endpoint; call Listen then Serve.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Policy.MaxRateMbps == 0 {
		opts.Policy = config.DefaultPolicy()
	}
	return &Server{opts: opts, log: opts.Logger, sessions: map[uint64]*session{}}
}

// Listen binds the TCP and UDP sockets.
func (s *Server) Listen() error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", s.opts.Listen)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", s.opts.Listen, err)
	}
	// UDP uses the same port as TCP (useful when Listen has port 0).
	port := ln.Addr().(*net.TCPAddr).Port
	host, _, _ := net.SplitHostPort(s.opts.Listen)
	u, err := net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("listen udp: %w", err)
	}
	uc := u.(*net.UDPConn)
	_ = netx.EnableRecvInfo(uc)
	// Every datagram the endpoint sends has DF set and ignores the cached
	// path MTU, so reverse PMTU probes are honest.
	_ = netx.SetDFProbe(uc)
	netx.SetBuffers(uc, 8<<20)
	s.tcp, s.udp = ln, uc
	return nil
}

// Addr returns the bound TCP address.
func (s *Server) Addr() net.Addr { return s.tcp.Addr() }

// Sessions returns the number of active sessions.
func (s *Server) Sessions() int { return int(s.active.Load()) }

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.tcp.Close()
		_ = s.udp.Close()
	}()
	go s.udpLoop(ctx)
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.handleConn(ctx, c.(*net.TCPConn))
	}
}

func (s *Server) handleConn(ctx context.Context, c *net.TCPConn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	pre := make([]byte, 5)
	if _, err := io.ReadFull(c, pre); err != nil || string(pre[:4]) != proto.Magic {
		c.Close() // includes plain TCP-connect latency probes
		return
	}
	switch pre[4] {
	case proto.ConnControl:
		s.handleControl(ctx, c)
	case proto.ConnData:
		s.handleData(c)
	default:
		c.Close()
	}
}

func (s *Server) handleData(c *net.TCPConn) {
	b := make([]byte, proto.DataHeaderLen)
	if _, err := io.ReadFull(c, b); err != nil {
		c.Close()
		return
	}
	h, err := proto.UnmarshalDataHeader(b)
	if err != nil {
		c.Close()
		return
	}
	s.mu.Lock()
	sess := s.sessions[h.Session.Short()]
	s.mu.Unlock()
	if sess == nil || sess.id != h.Session || !h.Valid(sess.key) {
		s.log.Debug("rejected data connection", "remote", c.RemoteAddr())
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	sess.attachStream(h.Stream, c)
}

// session is one authenticated client.
type session struct {
	srv      *Server
	id       proto.SessionID
	keyID    string
	key      []byte
	ctrl     net.Conn
	wmu      sync.Mutex
	observed netip.AddrPort
	ctx      context.Context
	cancel   context.CancelFunc

	udpPeer atomic.Pointer[net.UDPAddr]

	mu         sync.Mutex
	nextStream uint32
	slots      map[uint32]*tcpTest
	tests      map[string]*tcpTest
	sinks      map[uint32]*sinkEntry
	sends      map[uint32]*sendEntry
	probeTTL   map[int]uint64
	probeTOS   map[int]uint64
	pmtuSeen   int

	usedShaping bool
}

func (s *Server) handleControl(ctx context.Context, c *net.TCPConn) {
	defer c.Close()
	sess, err := s.handshake(c)
	if err != nil {
		s.log.Debug("handshake failed", "remote", c.RemoteAddr(), "err", err)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	sess.ctx, sess.cancel = context.WithCancel(ctx)
	s.mu.Lock()
	s.sessions[sess.id.Short()] = sess
	s.mu.Unlock()
	s.active.Add(1)
	s.log.Info("session started", "client", sess.keyID, "observed", sess.observed)
	if s.opts.OnSession != nil {
		s.opts.OnSession(sess.keyID, sess.observed.String())
	}
	defer func() {
		sess.cancel()
		sess.closeStreams()
		s.mu.Lock()
		delete(s.sessions, sess.id.Short())
		s.mu.Unlock()
		s.active.Add(-1)
		if sess.usedShaping && s.opts.Shaper != nil {
			_ = s.opts.Shaper.Clear(context.Background())
		}
		s.log.Info("session ended", "client", sess.keyID)
	}()
	for {
		var m proto.Msg
		if err := proto.ReadMsg(c, &m); err != nil {
			return
		}
		if m.Type != "req" {
			continue
		}
		go sess.dispatch(&m)
	}
}

func (s *Server) handshake(c *net.TCPConn) (*session, error) {
	var m proto.Msg
	if err := proto.ReadMsg(c, &m); err != nil || m.Type != "hello" {
		return nil, errors.New("expected hello")
	}
	var hello proto.Hello
	if err := json.Unmarshal(m.Data, &hello); err != nil {
		return nil, err
	}
	key, ok := s.opts.Keys.Lookup(hello.KeyID)
	if !ok || len(hello.ClientNonce) != 16 {
		// Same response shape as a bad MAC: don't reveal which keys exist.
		_ = proto.WriteMsg(c, &proto.Msg{Type: "error", Error: "authentication failed"})
		return nil, errors.New("unknown key id")
	}
	observed, _ := netip.ParseAddrPort(c.RemoteAddr().String())
	observed = netip.AddrPortFrom(observed.Addr().Unmap(), observed.Port())
	sn := proto.NewNonce()
	if err := proto.WriteMsg(c, &proto.Msg{Type: "challenge", Data: proto.Encode(proto.Challenge{
		Version: proto.Version, ServerNonce: sn, ObservedAddr: observed.String(), Software: version.Get().Version,
	})}); err != nil {
		return nil, err
	}
	if err := proto.ReadMsg(c, &m); err != nil || m.Type != "auth" {
		return nil, errors.New("expected auth")
	}
	var auth proto.Auth
	if err := json.Unmarshal(m.Data, &auth); err != nil {
		return nil, err
	}
	if !proto.Equal(auth.MAC, proto.ClientMAC(key, hello.ClientNonce, sn)) {
		_ = proto.WriteMsg(c, &proto.Msg{Type: "error", Error: "authentication failed"})
		return nil, errors.New("bad mac")
	}
	sess := &session{
		srv:      s,
		id:       proto.NewSessionID(),
		keyID:    hello.KeyID,
		key:      proto.SessionKey(key, hello.ClientNonce, sn),
		ctrl:     c,
		observed: observed,
		slots:    map[uint32]*tcpTest{},
		tests:    map[string]*tcpTest{},
		sinks:    map[uint32]*sinkEntry{},
		sends:    map[uint32]*sendEntry{},
		probeTTL: map[int]uint64{},
		probeTOS: map[int]uint64{},
	}
	err := proto.WriteMsg(c, &proto.Msg{Type: "welcome", Data: proto.Encode(proto.Welcome{
		SessionID: sess.id.String(), MAC: proto.ServerMAC(key, hello.ClientNonce, sn),
	})})
	return sess, err
}

func (ss *session) send(m *proto.Msg) error {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	_ = ss.ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return proto.WriteMsg(ss.ctrl, m)
}

func (ss *session) event(op string, data any) {
	_ = ss.send(&proto.Msg{Type: "event", Op: op, Data: proto.Encode(data)})
}

func (ss *session) dispatch(m *proto.Msg) {
	res, err := ss.handle(m.Op, m.Data)
	resp := &proto.Msg{Type: "resp", ID: m.ID, Op: m.Op, OK: err == nil}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Data = proto.Encode(res)
	}
	if err := ss.send(resp); err != nil {
		ss.srv.log.Debug("send response", "err", err)
	}
}

func (ss *session) allocStreams(n int) []uint32 {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ids := make([]uint32, n)
	for i := range ids {
		ss.nextStream++
		ids[i] = ss.nextStream
	}
	return ids
}

func (ss *session) closeStreams() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, t := range ss.tests {
		t.closeAll()
	}
}

// checkRate enforces the endpoint's local policy.
func (ss *session) checkRate(bps float64) error {
	limit := float64(ss.srv.opts.Policy.MaxRateMbps) * 1e6
	if bps > limit {
		return fmt.Errorf("requested rate %.0f Mbit/s exceeds this endpoint's policy limit of %d Mbit/s", bps/1e6, ss.srv.opts.Policy.MaxRateMbps)
	}
	return nil
}

func (ss *session) checkDuration(d time.Duration) error {
	if d > ss.srv.opts.Policy.MaxTestDuration {
		return fmt.Errorf("requested duration %s exceeds this endpoint's policy limit of %s", d, ss.srv.opts.Policy.MaxTestDuration)
	}
	return nil
}
