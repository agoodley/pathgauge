// Package tlsprofile changes how PathGauge's TLS clients open connections.
//
// Every profile is encrypted and verified in exactly the same way; they
// differ only in the TLS ClientHello. Some firewalls, intrusion-prevention
// systems and endpoint-security products silently drop TLS hellos they
// don't recognise (the connection opens, then nothing comes back), and
// Go's default hello is one of the less common ones. A different profile
// often gets through.
package tlsprofile

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Profile is one way of opening a TLS connection.
type Profile struct {
	Name        string
	Description string
	apply       func(*tls.Config)
}

func classic(c *tls.Config) {
	c.CurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}
}

// All lists the profiles in the order automatic selection tries them.
var All = []Profile{
	{Name: "default", Description: "TLS 1.3 with post-quantum key exchange (Go's defaults)", apply: func(*tls.Config) {}},
	{Name: "classic", Description: "TLS 1.3 with classic key exchange only (a smaller hello)", apply: classic},
	{Name: "browser", Description: "TLS 1.3 shaped like a typical HTTPS client: classic key exchange, ALPN http/1.1, session tickets", apply: func(c *tls.Config) {
		classic(c)
		c.NextProtos = []string{"http/1.1"}
		c.SessionTicketsDisabled = false
		c.ClientSessionCache = tls.NewLRUClientSessionCache(8)
	}},
	{Name: "tls12", Description: "TLS 1.2 only, with ALPN http/1.1 (the oldest, most widely recognised hello)", apply: func(c *tls.Config) {
		c.MaxVersion = tls.VersionTLS12
		c.CurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256}
		c.NextProtos = []string{"http/1.1"}
		c.CipherSuites = []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		}
	}},
}

// Auto means: try every profile in order and use the first that works.
const Auto = "auto"

// Names lists the profile names.
func Names() []string {
	var n []string
	for _, p := range All {
		n = append(n, p.Name)
	}
	return n
}

// Get returns a profile by name ("" is the default profile).
func Get(name string) (Profile, error) {
	if name == "" {
		name = "default"
	}
	for _, p := range All {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("unknown TLS profile %q (choose %s, or %s)", name, strings.Join(Names(), ", "), Auto)
}

// Apply adjusts c for the named profile.
func Apply(name string, c *tls.Config) error {
	p, err := Get(name)
	if err != nil {
		return err
	}
	p.apply(c)
	return nil
}

// Stage names where a connection attempt stopped.
const (
	StageConnect = "connect"
	StageTLS     = "tls"
	StageDone    = "done"
)

// Result is one probe of a profile.
type Result struct {
	Profile   string
	Stage     string        // where it stopped: connect, tls or done
	Connect   time.Duration // TCP connection time
	Handshake time.Duration // TLS handshake time (or how long it waited)
	Version   string        // negotiated TLS version
	Stalled   bool          // the TLS hello got no answer before the timeout
	Err       error
}

// OK reports whether the TLS handshake completed.
func (r Result) OK() bool { return r.Stage == StageDone }

// Summary is a one-line description of the result.
func (r Result) Summary() string {
	switch {
	case r.OK():
		return fmt.Sprintf("ok: %s handshake in %s", r.Version, ms(r.Handshake))
	case r.Stage == StageConnect:
		return "could not connect: " + r.Err.Error()
	case r.Stalled:
		return fmt.Sprintf("no answer to the TLS hello after %s (dropped on the way?)", ms(r.Handshake))
	}
	return "TLS handshake failed: " + r.Err.Error()
}

func ms(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}

// Probe opens a TCP connection to addr and completes a TLS handshake with
// base (cloned) adjusted for the profile, waiting at most timeout for the
// handshake. It returns the open connection when the handshake succeeds.
func Probe(ctx context.Context, addr, profile string, base *tls.Config, timeout time.Duration) (Result, *tls.Conn) {
	r := Result{Profile: profile, Stage: StageConnect}
	cfg := base.Clone()
	if err := Apply(profile, cfg); err != nil {
		r.Err = err
		return r, nil
	}
	start := time.Now()
	d := net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	r.Connect = time.Since(start)
	if err != nil {
		r.Err = err
		return r, nil
	}
	r.Stage = StageTLS
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host) == nil {
			cfg.ServerName = host
		}
	}
	tc := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hs := time.Now()
	err = tc.HandshakeContext(hctx)
	r.Handshake = time.Since(hs)
	if err != nil {
		r.Err = err
		r.Stalled = errors.Is(err, context.DeadlineExceeded) || errors.Is(hctx.Err(), context.DeadlineExceeded)
		_ = raw.Close()
		return r, nil
	}
	r.Stage = StageDone
	r.Version = tls.VersionName(tc.ConnectionState().Version)
	return r, tc
}
