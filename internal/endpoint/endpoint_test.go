package endpoint_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/engine"
)

// syncBuffer is a log sink that is safe to read while the endpoint writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeShaper records what the endpoint passes on, and can panic on demand.
type fakeShaper struct {
	mu      sync.Mutex
	clients []netip.Addr
	cleared int
	panic   bool
}

func (f *fakeShaper) Apply(_ context.Context, _ json.RawMessage, client netip.Addr) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panic {
		panic("shaper bug")
	}
	f.clients = append(f.clients, client)
	return map[string]bool{"applied": true}, nil
}

func (f *fakeShaper) Clear(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared++
	return nil
}

func start(t *testing.T, opts endpoint.Options) (*engine.Client, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	key := []byte("test-key")
	logs := &syncBuffer{}
	opts.Listen, opts.Keys = "127.0.0.1:0", endpoint.StaticKeys{"psk": key}
	opts.Logger = slog.New(slog.NewTextHandler(logs, nil))
	srv := endpoint.New(opts)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ctx) }()
	c, err := engine.Dial(ctx, srv.Addr().String(), "psk", key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, logs
}

// A data connection that arrives after the test started without it used to
// index past the rebuilt stream list and take the whole process down.
func TestLateDataConnectionIsRefused(t *testing.T) {
	defer endpoint.SetAttachTimeout(100 * time.Millisecond)()
	c, logs := start(t, endpoint.Options{})
	ctx := context.Background()

	var prep endpoint.TCPPrepared
	if err := c.Call(ctx, "tcp_prepare", endpoint.TCPPrepare{Role: "recv", Streams: 2, DurationMs: 1000}, &prep); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // the wait for data connections has run out
	for _, id := range prep.Streams {
		conn, err := c.DialData(ctx, id, engine.DialOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// The endpoint closes a connection it has no place for.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil {
			t.Errorf("late data connection was kept open (read %d bytes)", n)
		}
		conn.Close()
	}
	if err := c.Call(ctx, "ping", nil, nil); err != nil {
		t.Fatalf("endpoint no longer answers after a late data connection: %v", err)
	}
	var res endpoint.TCPResult
	if err := c.Call(ctx, "tcp_result", map[string]any{"test_id": prep.TestID, "wait_ms": 2000}, &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Error, "no data connections") || len(res.Streams) != 0 {
		t.Errorf("a test whose streams all arrived late should report none: %+v", res)
	}
	if strings.Contains(logs.String(), "panic") {
		t.Errorf("the endpoint panicked:\n%s", logs.String())
	}
}

// A bug in one operation must come back as an error on that request and
// leave the endpoint serving.
func TestPanicInOperationIsContained(t *testing.T) {
	sh := &fakeShaper{panic: true}
	c, logs := start(t, endpoint.Options{Shaper: sh, Policy: config.DefaultPolicy()})
	ctx := context.Background()

	err := c.Call(ctx, "shape_apply", map[string]any{"qdisc": "auto"}, nil)
	if err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("want an internal-error response, got %v", err)
	}
	if err := c.Call(ctx, "ping", nil, nil); err != nil {
		t.Fatalf("endpoint no longer answers after a panic in one operation: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "recovered from a panic") || !strings.Contains(out, "shape_apply") || !strings.Contains(out, "shaper bug") {
		t.Errorf("the panic should be logged with the operation and the cause:\n%s", out)
	}
}

// A remote client gets shaping only where the host's owner allowed it, and
// the shaper is told who is asking so it can pick the interface itself.
func TestRemoteShaping(t *testing.T) {
	ctx := context.Background()

	c, _ := start(t, endpoint.Options{Policy: config.DefaultPolicy()}) // no shaper: not allowed
	if err := c.Call(ctx, "shape_apply", map[string]any{"interface": "eth0", "force": true, "loss_pct": 100}, nil); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("shaping must be refused without a shaper, got %v", err)
	}
	var info endpoint.InfoResp
	if err := c.Call(ctx, "info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Policy.Shaping != "none" {
		t.Errorf("an endpoint without a shaper must report shaping none, got %q", info.Policy.Shaping)
	}

	sh := &fakeShaper{}
	c2, _ := start(t, endpoint.Options{Shaper: sh, Policy: config.DefaultPolicy()})
	if err := c2.Call(ctx, "shape_apply", map[string]any{"qdisc": "auto", "egress_rate_mbps": 10}, nil); err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	clients := append([]netip.Addr(nil), sh.clients...)
	sh.mu.Unlock()
	if len(clients) != 1 || clients[0] != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("the shaper should be given the client's address, got %v", clients)
	}
	// Shaping a session asked for ends with the session.
	c2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		n := sh.cleared
		sh.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shaping was not cleared when the session that asked for it ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
