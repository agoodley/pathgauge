// Package shaping applies time-limited traffic shaping and impairment with
// Linux tc (docs/PLAN.md §8.3).
//
// Safety rules:
//   - Every configuration is a lease that removes itself when it expires,
//     when the requesting session ends, or when the process stops.
//   - Leases are recorded in a file so a crashed process's shaping is
//     removed on the next start (Cleanup).
//   - An interface whose root qdisc is not a kernel default is refused
//     unless Force is set, because restoring means deleting the qdisc and
//     letting the kernel re-attach its default.
//   - Every tc command is recorded and returned, so what was done is visible.
package shaping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Handle marks qdiscs created by PathGauge (0x5047 = "PG").
const Handle = "5047:"

// IFB is the ingress mirror device PathGauge creates.
const IFB = "pg-ifb0"

// Spec describes one shaping configuration.
type Spec struct {
	Interface string `json:"interface,omitempty"` // default: route towards Target
	Target    string `json:"target,omitempty"`    // address used to pick the interface
	// Qdisc is cake, fq_codel (with htb), tbf, netem, or auto (cake, else fq_codel).
	Qdisc           string  `json:"qdisc"`
	EgressRateMbps  float64 `json:"egress_rate_mbps,omitempty"`
	IngressRateMbps float64 `json:"ingress_rate_mbps,omitempty"`
	DelayMs         float64 `json:"delay_ms,omitempty"`
	JitterMs        float64 `json:"jitter_ms,omitempty"`
	LossPct         float64 `json:"loss_pct,omitempty"`
	LeaseSeconds    int     `json:"lease_s"`
	Force           bool    `json:"force,omitempty"`
}

// Applied reports what was done.
type Applied struct {
	Interface string    `json:"interface"`
	Qdisc     string    `json:"qdisc"`
	Commands  []string  `json:"commands"`
	Expires   time.Time `json:"expires"`
	Previous  string    `json:"previous_root_qdisc"`
}

// Manager owns at most one active shaping lease on this host.
type Manager struct {
	log   *slog.Logger
	mu    sync.Mutex
	cur   *lease
	timer *time.Timer
	run   func(ctx context.Context, name string, args ...string) (string, error)
	file  string
}

type lease struct {
	Interface string    `json:"interface"`
	Ingress   bool      `json:"ingress"`
	Expires   time.Time `json:"expires"`
	Commands  []string  `json:"commands"`
}

// NewManager creates a manager and removes any shaping left by a previous
// process that crashed.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, run: runCmd, file: leaseFile()}
	m.cleanupStale()
	return m
}

func leaseFile() string {
	for _, dir := range []string{"/run/pathgauge", filepath.Join(os.TempDir(), "pathgauge")} {
		if err := os.MkdirAll(dir, 0o750); err == nil {
			return filepath.Join(dir, "shaping-lease.json")
		}
	}
	return filepath.Join(os.TempDir(), "pathgauge-shaping-lease.json")
}

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	path, err := lookPath(name)
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func lookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, d := range []string{"/usr/sbin", "/sbin"} {
		if _, err := os.Stat(filepath.Join(d, name)); err == nil {
			return filepath.Join(d, name), nil
		}
	}
	return "", fmt.Errorf("%s not found (install iproute2 / iproute-tc)", name)
}

// cleanupStale removes shaping recorded by a previous process.
func (m *Manager) cleanupStale() {
	b, err := os.ReadFile(m.file)
	if err != nil {
		return
	}
	var l lease
	if json.Unmarshal(b, &l) == nil && l.Interface != "" {
		m.log.Warn("removing shaping left by a previous run", "interface", l.Interface)
		m.remove(context.Background(), &l)
	}
	_ = os.Remove(m.file)
}

// Apply implements endpoint.Shaper.
func (m *Manager) Apply(ctx context.Context, raw json.RawMessage) (any, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return m.ApplySpec(ctx, s)
}

// Clear implements endpoint.Shaper.
func (m *Manager) Clear(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clearLocked(ctx)
}

func (m *Manager) clearLocked(ctx context.Context) error {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.cur == nil {
		return nil
	}
	m.remove(ctx, m.cur)
	m.log.Info("shaping removed", "interface", m.cur.Interface)
	m.cur = nil
	_ = os.Remove(m.file)
	return nil
}

func (m *Manager) remove(ctx context.Context, l *lease) {
	_, _ = m.run(ctx, "tc", "qdisc", "del", "dev", l.Interface, "root")
	if l.Ingress {
		_, _ = m.run(ctx, "tc", "qdisc", "del", "dev", l.Interface, "ingress")
		_, _ = m.run(ctx, "ip", "link", "del", IFB)
	}
}

// RouteInterface returns the interface used to reach an address.
func (m *Manager) RouteInterface(ctx context.Context, target string) (string, error) {
	out, err := m.run(ctx, "ip", "-j", "route", "get", target)
	if err != nil {
		return "", err
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil || len(routes) == 0 || routes[0].Dev == "" {
		return "", fmt.Errorf("cannot find the interface towards %s", target)
	}
	return routes[0].Dev, nil
}

// defaultRoots are root qdiscs the kernel attaches by itself.
var defaultRoots = []string{"mq", "fq", "fq_codel", "pfifo_fast", "noqueue", "pfifo", "sfq"}

// rootQdisc returns the kind and handle of an interface's root qdisc.
func (m *Manager) rootQdisc(ctx context.Context, iface string) (kind, handle string, hasIngress bool, err error) {
	out, err := m.run(ctx, "tc", "-j", "qdisc", "show", "dev", iface)
	if err != nil {
		return "", "", false, err
	}
	var qs []struct {
		Kind   string `json:"kind"`
		Handle string `json:"handle"`
		Root   bool   `json:"root"`
		Parent string `json:"parent"`
	}
	if err := json.Unmarshal([]byte(out), &qs); err != nil {
		return "", "", false, err
	}
	for _, q := range qs {
		if q.Root {
			kind, handle = q.Kind, q.Handle
		}
		if q.Kind == "ingress" || q.Kind == "clsact" {
			hasIngress = true
		}
	}
	return kind, handle, hasIngress, nil
}

// ApplySpec applies a configuration, replacing any active PathGauge lease.
func (m *Manager) ApplySpec(ctx context.Context, s Spec) (Applied, error) {
	if s.LeaseSeconds <= 0 {
		s.LeaseSeconds = 60
	}
	if s.LeaseSeconds > 3600 {
		return Applied{}, errors.New("lease longer than one hour refused")
	}
	if s.EgressRateMbps < 0 || s.IngressRateMbps < 0 || s.LossPct < 0 || s.LossPct > 100 || s.DelayMs < 0 || s.DelayMs > 10000 {
		return Applied{}, errors.New("invalid shaping values")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.clearLocked(ctx); err != nil {
		return Applied{}, err
	}
	iface := s.Interface
	if iface == "" {
		if s.Target == "" {
			return Applied{}, errors.New("an interface or target address is required")
		}
		var err error
		if iface, err = m.RouteInterface(ctx, s.Target); err != nil {
			return Applied{}, err
		}
	}
	if strings.ContainsAny(iface, " /\t\n") || len(iface) > 15 {
		return Applied{}, fmt.Errorf("invalid interface name %q", iface)
	}
	kind, handle, hasIngress, err := m.rootQdisc(ctx, iface)
	if err != nil {
		return Applied{}, err
	}
	if !s.Force && handle != Handle && !slices.Contains(defaultRoots, kind) {
		return Applied{}, fmt.Errorf("interface %s already uses a %s qdisc; PathGauge will not replace a custom configuration without force (it would be restored to the kernel default afterwards)", iface, kind)
	}
	if s.IngressRateMbps > 0 && hasIngress && !s.Force {
		return Applied{}, fmt.Errorf("interface %s already has an ingress qdisc; refusing without force", iface)
	}
	l := &lease{Interface: iface, Expires: time.Now().Add(time.Duration(s.LeaseSeconds) * time.Second)}
	// Record the lease before changing anything, so a crash mid-way is cleaned up.
	m.cur = l
	m.writeLease()
	cmdErr := func(err error) (Applied, error) {
		m.remove(context.Background(), l)
		m.cur = nil
		_ = os.Remove(m.file)
		return Applied{}, err
	}
	exec := func(name string, args ...string) error {
		l.Commands = append(l.Commands, name+" "+strings.Join(args, " "))
		_, err := m.run(ctx, name, args...)
		return err
	}
	qdisc := s.Qdisc
	if qdisc == "" || qdisc == "auto" {
		qdisc = "cake"
	}
	used, err := m.applyRoot(exec, iface, qdisc, s.EgressRateMbps, s, false)
	if err != nil && s.Qdisc == "auto" && qdisc == "cake" {
		// EL8 kernels have no CAKE: fall back to fq_codel under htb.
		l.Commands = nil
		_, _ = m.run(ctx, "tc", "qdisc", "del", "dev", iface, "root")
		used, err = m.applyRoot(exec, iface, "fq_codel", s.EgressRateMbps, s, false)
	}
	if err != nil {
		return cmdErr(err)
	}
	if s.IngressRateMbps > 0 {
		l.Ingress = true
		m.writeLease()
		_, _ = m.run(ctx, "ip", "link", "del", IFB)
		steps := [][]string{
			{"ip", "link", "add", IFB, "type", "ifb"},
			{"ip", "link", "set", IFB, "up"},
			{"tc", "qdisc", "replace", "dev", iface, "handle", "ffff:", "ingress"},
			{"tc", "filter", "replace", "dev", iface, "parent", "ffff:", "protocol", "all", "matchall", "action", "mirred", "egress", "redirect", "dev", IFB},
		}
		for _, st := range steps {
			if err := exec(st[0], st[1:]...); err != nil {
				return cmdErr(err)
			}
		}
		if _, err := m.applyRoot(exec, IFB, used, s.IngressRateMbps, s, true); err != nil {
			return cmdErr(err)
		}
	}
	m.writeLease()
	m.timer = time.AfterFunc(time.Until(l.Expires), func() {
		m.log.Info("shaping lease expired", "interface", iface)
		_ = m.Clear(context.Background())
	})
	m.log.Info("shaping applied", "interface", iface, "qdisc", used, "egress_mbps", s.EgressRateMbps, "ingress_mbps", s.IngressRateMbps, "lease", time.Until(l.Expires).Round(time.Second))
	return Applied{Interface: iface, Qdisc: used, Commands: append([]string(nil), l.Commands...), Expires: l.Expires, Previous: kind}, nil
}

// applyRoot installs the root qdisc on dev and returns the qdisc used.
func (m *Manager) applyRoot(exec func(string, ...string) error, dev, qdisc string, rate float64, s Spec, ingress bool) (string, error) {
	rateArg := fmt.Sprintf("%.3fmbit", rate)
	netem := !ingress && (s.DelayMs > 0 || s.LossPct > 0)
	switch {
	case netem:
		args := []string{"qdisc", "replace", "dev", dev, "root", "handle", Handle, "netem", "limit", "100000"}
		if s.DelayMs > 0 {
			args = append(args, "delay", fmt.Sprintf("%.1fms", s.DelayMs))
			if s.JitterMs > 0 {
				args = append(args, fmt.Sprintf("%.1fms", s.JitterMs), "distribution", "normal")
			}
		}
		if s.LossPct > 0 {
			args = append(args, "loss", fmt.Sprintf("%.3f%%", s.LossPct))
		}
		if rate > 0 {
			args = append(args, "rate", rateArg)
		}
		return "netem", exec("tc", args...)
	case rate <= 0:
		return qdisc, nil
	case qdisc == "cake":
		args := []string{"qdisc", "replace", "dev", dev, "root", "handle", Handle, "cake", "bandwidth", rateArg}
		if ingress {
			args = append(args, "ingress")
		}
		return "cake", exec("tc", args...)
	case qdisc == "tbf":
		burst := max(int(rate*1e6/8/1000), 6056) // ~1 ms at line rate
		return "tbf", exec("tc", "qdisc", "replace", "dev", dev, "root", "handle", Handle, "tbf", "rate", rateArg, "burst", fmt.Sprint(burst), "latency", "50ms")
	default: // fq_codel under htb
		burst := max(int(rate*1e6/8/1000), 6056)
		if err := exec("tc", "qdisc", "replace", "dev", dev, "root", "handle", Handle, "htb", "default", "1"); err != nil {
			return "", err
		}
		if err := exec("tc", "class", "replace", "dev", dev, "parent", Handle, "classid", Handle+"1", "htb", "rate", rateArg, "ceil", rateArg, "burst", fmt.Sprint(burst), "quantum", "1514"); err != nil {
			return "", err
		}
		return "fq_codel", exec("tc", "qdisc", "replace", "dev", dev, "parent", Handle+"1", "fq_codel")
	}
}

func (m *Manager) writeLease() {
	if m.cur == nil {
		return
	}
	b, _ := json.Marshal(m.cur)
	_ = os.WriteFile(m.file, b, 0o600)
}

// Active reports the current lease, if any.
func (m *Manager) Active() (iface string, expires time.Time, commands []string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return "", time.Time{}, nil, false
	}
	return m.cur.Interface, m.cur.Expires, append([]string(nil), m.cur.Commands...), true
}
