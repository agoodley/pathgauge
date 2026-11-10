// Package server runs the PathGauge server: dashboard and REST API over
// HTTPS, agent control over WebSocket with mutual TLS, the built-in test
// endpoint, scheduling, monitoring storage, alerting and reports.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/guard"
	"github.com/agoodley/pathgauge/internal/pki"
	"github.com/agoodley/pathgauge/internal/store"
)

// Server is a running PathGauge server.
type Server struct {
	cfg    config.Server
	log    *slog.Logger
	store  *store.Store
	ca     *pki.CA
	cert   tls.Certificate
	ep     *endpoint.Server
	hub    *Hub
	sched  *Scheduler
	alerts *Alerts
	http   *http.Server

	keyMu    sync.Mutex
	keyCache map[string][]byte
	keyAt    time.Time

	loginMu    sync.Mutex
	loginFails map[string][]time.Time

	addr  net.Addr
	ready chan struct{}
}

// Ready is closed once the server is listening.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr returns the HTTPS listener address (after Ready).
func (s *Server) Addr() net.Addr { return s.addr }

// Open prepares a server from configuration (database, CA, certificates).
func Open(cfg config.Server, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	pkiDir := filepath.Join(cfg.DataDir, "pki")
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		return nil, err
	}
	ca, err := pki.LoadOrCreateCA(pkiDir)
	if err != nil {
		return nil, fmt.Errorf("certificate authority: %w", err)
	}
	cert, err := ca.ServerCert(pkiDir, certHosts(cfg.Names))
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	s := &Server{cfg: cfg, log: log, store: st, ca: ca, cert: cert, keyCache: map[string][]byte{}, loginFails: map[string][]time.Time{}, ready: make(chan struct{})}
	s.hub = newHub(s)
	s.sched = newScheduler(s)
	s.alerts = newAlerts(s)
	return s, nil
}

// Store exposes the database (for CLI administration).
func (s *Server) Store() *store.Store { return s.store }

// CA exposes the certificate authority (for CLI administration).
func (s *Server) CA() *pki.CA { return s.ca }

// Close releases resources.
func (s *Server) Close() error { return s.store.Close() }

// certHosts lists names for the server certificate: hostname, localhost,
// every local address, and configured names.
func certHosts(extra []string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil {
		hosts = append([]string{h}, hosts...)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	return append(hosts, extra...)
}

// Lookup implements endpoint.KeyStore: agents authenticate to the data
// plane with the key issued at enrollment.
func (s *Server) Lookup(keyID string) ([]byte, bool) {
	if keyID == "psk" {
		if s.cfg.DataPlane.PeerKey == "" {
			return nil, false
		}
		return []byte(s.cfg.DataPlane.PeerKey), true
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if k, ok := s.keyCache[keyID]; ok && time.Since(s.keyAt) < 30*time.Second {
		return k, true
	}
	a, err := s.store.Agent(context.Background(), keyID)
	if err != nil || a.Revoked {
		return nil, false
	}
	s.keyCache[keyID], s.keyAt = a.DataKey, time.Now()
	return a.DataKey, true
}

func (s *Server) forgetKey(id string) {
	s.keyMu.Lock()
	delete(s.keyCache, id)
	s.keyMu.Unlock()
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.store.FailStaleRuns(ctx)
	pol := config.DefaultPolicy()
	if s.cfg.PolicyFile != "" {
		p, err := config.LoadPolicy(s.cfg.PolicyFile)
		if err != nil {
			return err
		}
		pol = p
	}
	// The server's endpoint takes no shaping requests: every enrolled agent
	// can open a session to it, and none of them should be able to shape
	// the server's interfaces. The SQM tuner shapes the agent's own host.
	s.ep = endpoint.New(endpoint.Options{Listen: s.cfg.DataPlane.Listen, Keys: s, Policy: pol, Logger: s.log.With("component", "endpoint")})
	if err := s.ep.Listen(); err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- s.ep.Serve(ctx) }()

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    s.ca.Pool(),
		MinVersion:   tls.VersionTLS12,
	}
	s.http = &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.routes(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	s.addr = ln.Addr()
	close(s.ready)
	go func() { errc <- s.http.Serve(tls.NewListener(ln, tlsCfg)) }()
	// Background loops restart if a bug panics them, so one bad row or
	// message does not end the server.
	go guard.Loop(ctx, s.log, "scheduler", s.sched.Run)
	go guard.Loop(ctx, s.log, "alert evaluation", s.alerts.Run)
	go guard.Loop(ctx, s.log, "run watchdog", s.hub.watchdog)
	go guard.Loop(ctx, s.log, "housekeeping", s.housekeeping)
	s.log.Info("server ready", "dashboard", "https://"+displayAddr(s.cfg.Listen), "data_plane", s.ep.Addr().String(), "ca_fingerprint", s.ca.Fingerprint()[:16])
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(sctx)
	return nil
}

func displayAddr(listen string) string {
	host, port, _ := net.SplitHostPort(listen)
	if host == "" {
		host, _ = os.Hostname()
	}
	return net.JoinHostPort(host, port)
}

// DataPort returns the data-plane port agents should use.
func (s *Server) DataPort() int {
	if s.ep != nil {
		return s.ep.Addr().(*net.TCPAddr).Port
	}
	_, p, _ := net.SplitHostPort(s.cfg.DataPlane.Listen)
	n, _ := strconv.Atoi(p)
	return n
}

func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		days := s.cfg.RetentionDays
		if days <= 0 {
			days = 90
		}
		s.store.PruneSamples(ctx, time.Duration(days)*24*time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
