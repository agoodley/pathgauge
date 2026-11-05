package tlsprofile

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/pki"
)

// helloServer is a TLS server that records each ClientHello it sees.
func helloServer(t *testing.T) (string, func() []*tls.ClientHelloInfo) {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServerCert(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var hellos []*tls.ClientHelloInfo
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		hellos = append(hellos, h)
		mu.Unlock()
		return nil, nil
	}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return ln.Addr().String(), func() []*tls.ClientHelloInfo {
		mu.Lock()
		defer mu.Unlock()
		return append([]*tls.ClientHelloInfo(nil), hellos...)
	}
}

func TestProfilesShapeTheHello(t *testing.T) {
	addr, hellos := helloServer(t)
	base := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server
	for _, p := range All {
		r, conn := Probe(context.Background(), addr, p.Name, base, 3*time.Second)
		if !r.OK() || conn == nil {
			t.Fatalf("%s: %s", p.Name, r.Summary())
		}
		conn.Close()
	}
	h := hellos()
	if len(h) != len(All) {
		t.Fatalf("saw %d hellos", len(h))
	}
	byName := map[string]*tls.ClientHelloInfo{}
	for i, p := range All {
		byName[p.Name] = h[i]
	}
	if !slices.Contains(byName["default"].SupportedCurves, tls.X25519MLKEM768) {
		t.Error("default should offer post-quantum key exchange")
	}
	for _, n := range []string{"classic", "browser", "tls12"} {
		if slices.Contains(byName[n].SupportedCurves, tls.X25519MLKEM768) {
			t.Errorf("%s should not offer post-quantum key exchange", n)
		}
	}
	if len(byName["default"].SupportedProtos) != 0 || !slices.Contains(byName["browser"].SupportedProtos, "http/1.1") {
		t.Errorf("ALPN: default %v, browser %v", byName["default"].SupportedProtos, byName["browser"].SupportedProtos)
	}
	if slices.Contains(byName["tls12"].SupportedVersions, tls.VersionTLS13) {
		t.Error("tls12 must not offer TLS 1.3")
	}
	if err := Apply("nonsense", &tls.Config{}); err == nil {
		t.Error("unknown profile accepted")
	}
}

func TestProbeTellsStallsFromRefusals(t *testing.T) {
	// A listener that accepts and never answers: a dropped hello.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	r, conn := Probe(context.Background(), ln.Addr().String(), "default", &tls.Config{InsecureSkipVerify: true}, 300*time.Millisecond) //nolint:gosec // test
	if conn != nil || r.Stage != StageTLS || !r.Stalled {
		t.Errorf("silent server: %+v", r)
	}
	// A closed port: the connection is refused.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	dead.Close()
	r, _ = Probe(context.Background(), addr, "default", &tls.Config{InsecureSkipVerify: true}, time.Second) //nolint:gosec // test
	if r.Stage != StageConnect || r.Err == nil {
		t.Errorf("closed port: %+v", r)
	}
}
