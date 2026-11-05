package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/pki"
)

// dropBigHellos proxies to target but silently swallows connections whose
// TLS hello record is over 1000 bytes, like a firewall or IPS that drops
// hellos it doesn't recognise.
func dropBigHellos(t *testing.T, target string) string {
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
					_, _ = io.Copy(io.Discard, c)
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				_, _ = up.Write(append(hdr, body...))
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestDiagnoseFindsAWorkingProfile(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServerCert(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(ca.CertPEM) }))
	}()
	old := diagnoseTimeout
	diagnoseTimeout = 500 * time.Millisecond
	defer func() { diagnoseTimeout = old }()

	var out bytes.Buffer
	if err := Diagnose(context.Background(), &out, "https://"+ln.Addr().String(), 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Every TLS profile works") || !strings.Contains(out.String(), "HTTPS request: HTTP/1.1 200 OK") {
		t.Errorf("direct path:\n%s", out.String())
	}

	out.Reset()
	if err := Diagnose(context.Background(), &out, "https://"+dropBigHellos(t, ln.Addr().String()), 0); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "✖ TLS profile default: no answer to the TLS hello") || !strings.Contains(s, "--tls-profile classic") {
		t.Errorf("behind a hello-dropping firewall:\n%s", s)
	}
	t.Logf("\n%s", s)
}
