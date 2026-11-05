package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/tlsprofile"
)

// diagnoseTimeout is how long Diagnose waits for each TLS answer.
var diagnoseTimeout = 6 * time.Second

// Diagnose checks the path to a server step by step and prints what it
// finds: name resolution, the TCP connection, a TLS handshake with each
// TLS profile, an HTTPS request, and the test-traffic port. It sends no
// secrets, so it needs no join token.
func Diagnose(ctx context.Context, w io.Writer, serverURL string, dataPort int) error {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("the server URL must look like https://server:7443")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(config.DefaultControlPort))
	}
	host := u.Hostname()
	fmt.Fprintf(w, "Checking the path to %s\n\n", u.Host)
	if net.ParseIP(host) == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			fmt.Fprintf(w, "  ✖ DNS: %s does not resolve: %v\n", host, err)
			return nil
		}
		fmt.Fprintf(w, "  ✔ DNS: %s is %s\n", host, strings.Join(addrs, ", "))
	}
	// Diagnosis only: no secrets are sent, so the certificate isn't checked.
	base := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // diagnostic probe, sends nothing secret
	var working, stalled []string
	connected := false
	for _, p := range tlsprofile.All {
		r, conn := tlsprofile.Probe(ctx, u.Host, p.Name, base, diagnoseTimeout)
		if r.Stage == tlsprofile.StageConnect {
			fmt.Fprintf(w, "  ✖ TCP connect to %s: %v\n", u.Host, r.Err)
			fmt.Fprintf(w, "\nThe server's control port can't be reached. Check that the server is running (systemctl status pathgauge-server), that it listens on this port, and that every firewall on the way allows TCP %s.\n", u.Port())
			return nil
		}
		if !connected {
			fmt.Fprintf(w, "  ✔ TCP connect to %s: %.1f ms\n", u.Host, float64(r.Connect.Microseconds())/1000)
			connected = true
		}
		mark := "✔"
		if !r.OK() {
			mark = "✖"
		}
		fmt.Fprintf(w, "  %s TLS profile %-8s %s\n", mark, p.Name+":", r.Summary())
		if conn == nil {
			if r.Stalled {
				stalled = append(stalled, p.Name)
			}
			continue
		}
		if len(working) == 0 {
			fmt.Fprintf(w, "  %s\n", httpsCheck(conn, u.Host))
		}
		_ = conn.Close()
		working = append(working, p.Name)
	}
	if dataPort > 0 {
		addr := net.JoinHostPort(host, strconv.Itoa(dataPort))
		start := time.Now()
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			fmt.Fprintf(w, "  ✖ test-traffic port %s (TCP): %v\n", addr, err)
		} else {
			_ = c.Close()
			fmt.Fprintf(w, "  ✔ test-traffic port %s (TCP): %.1f ms (UDP %d is checked when tests run)\n", addr, float64(time.Since(start).Microseconds())/1000, dataPort)
		}
	}
	fmt.Fprintln(w)
	switch {
	case len(working) == len(tlsprofile.All):
		fmt.Fprintf(w, "Every TLS profile works, so the path is fine for TLS. If 'pathgauge agent join' still fails, run it with --debug and check the server's log.\n")
	case len(working) > 0:
		fmt.Fprintf(w, "Some TLS hellos get no answer (%s), so something on the path drops them. Join with the first profile that works:\n\n  pathgauge agent join %s --token TOKEN --tls-profile %s\n\n(--tls-profile auto, the default, picks it too.)\n",
			strings.Join(stalled, ", "), u.String(), working[0])
	default:
		fmt.Fprintf(w, "The TCP connection opens, but no TLS hello gets an answer: something on the path (a firewall, IPS or endpoint-security product on either host) drops them. "+
			"Ask for an exception for this traffic (TCP %s, and TCP and UDP %d for tests), or use peer mode, which doesn't use TLS:\n\n"+
			"  on the server:  pathgauge endpoint --listen :%d --key-file /root/pg-peer.key\n  here:           pathgauge test %s --key KEY\n",
			u.Port(), dataPort, dataPort, host)
	}
	return nil
}

// httpsCheck fetches /ca.crt over an established TLS connection.
func httpsCheck(conn *tls.Conn, host string) string {
	start := time.Now()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /ca.crt HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host); err != nil {
		return "✖ HTTPS request: " + err.Error()
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Sprintf("✖ HTTPS request: sent, but no answer: %v", err)
	}
	return fmt.Sprintf("✔ HTTPS request: %s in %.1f ms", strings.TrimSpace(line), float64(time.Since(start).Microseconds())/1000)
}
