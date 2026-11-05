package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/agent"
	"github.com/agoodley/pathgauge/internal/config"
)

// helloFirewall is a TCP proxy that behaves like a firewall or IPS that
// silently drops TLS hellos it doesn't like: here any hello record over
// 1000 bytes (Go's default hello, with post-quantum key exchange, is about
// 1.4 KB). The connection opens, the hello vanishes, nothing comes back.
// Every other connection is passed through untouched.
func helloFirewall(t *testing.T, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func(c net.Conn) {
				defer c.Close()
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
				if _, err := io.ReadFull(c, body); err != nil {
					return
				}
				if len(body) > 1000 {
					_, _ = io.Copy(io.Discard, c) // swallow it; answer nothing
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				if _, err := up.Write(append(hdr, body...)); err != nil {
					return
				}
				go func() { _, _ = io.Copy(up, c); _ = up.(*net.TCPConn).CloseWrite() }()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestJoinAndConnectThroughHelloDroppingFirewall(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	cfg := config.DefaultServer()
	cfg.Listen, cfg.DataDir = "127.0.0.1:0", t.TempDir()
	cfg.DataPlane.Listen = "127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := Open(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	<-srv.Ready()
	fw := "https://" + helloFirewall(t, srv.Addr().String())

	// Asking for the profile the firewall drops fails, saying why.
	tok1, _ := srv.CreateJoinToken(ctx, "test", "", time.Hour)
	_, err = agent.JoinWith(ctx, agent.JoinOptions{URL: fw, Token: tok1, Name: "x", DataDir: t.TempDir(), TLSProfile: "default", HandshakeTimeout: 500 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "never answered the TLS hello") {
		t.Fatalf("want a dropped-hello error, got %v", err)
	}

	// Automatic selection falls back to a profile that gets through.
	tok2, _ := srv.CreateJoinToken(ctx, "test", "", time.Hour)
	agentDir := t.TempDir()
	var dbg bytes.Buffer
	id, err := agent.JoinWith(ctx, agent.JoinOptions{URL: fw, Token: tok2, Name: "behind-firewall", DataDir: agentDir, HandshakeTimeout: time.Second, Debug: &dbg})
	if err != nil {
		t.Fatalf("join through the firewall: %v\n%s", err, dbg.String())
	}
	if id.TLSProfile != "classic" || !strings.Contains(dbg.String(), `using TLS profile "classic"`) {
		t.Errorf("profile %q; debug:\n%s", id.TLSProfile, dbg.String())
	}

	// The agent service opens its connection the same way and comes online
	// through the firewall.
	acfg := config.DefaultAgent()
	acfg.DataDir = agentDir
	ag, err := agent.New(acfg, config.DefaultPolicy(), log)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ag.Run(ctx) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if online, _ := srv.hub.Online(id.AgentID); online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never came online through the firewall")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
