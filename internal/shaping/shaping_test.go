package shaping

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTC records commands and simulates qdisc state.
type fakeTC struct {
	mu       sync.Mutex
	cmds     []string
	rootKind string
	ingress  bool
	noCake   bool
	failOn   string
}

func (f *fakeTC) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)
	switch {
	case f.failOn != "" && strings.Contains(cmd, f.failOn):
		return "", errors.New("simulated failure")
	case strings.HasPrefix(cmd, "tc -j qdisc show"):
		out := `[{"kind":"` + f.rootKind + `","handle":"0:","root":true}`
		if f.ingress {
			out += `,{"kind":"ingress","handle":"ffff:","parent":"ffff:fff1"}`
		}
		return out + "]", nil
	case strings.HasPrefix(cmd, "ip -j route get"):
		return `[{"dst":"192.0.2.1","dev":"eth0"}]`, nil
	case strings.Contains(cmd, " cake ") && f.noCake:
		return "", errors.New("Error: Specified qdisc kind is unknown.")
	}
	return "", nil
}

func (f *fakeTC) has(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func newTestManager(t *testing.T, f *fakeTC) *Manager {
	t.Helper()
	return &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil)), run: f.run, file: filepath.Join(t.TempDir(), "lease.json")}
}

func TestApplyAndClear(t *testing.T) {
	f := &fakeTC{rootKind: "mq"}
	m := newTestManager(t, f)
	ap, err := m.ApplySpec(context.Background(), Spec{Target: "192.0.2.1", Qdisc: "auto", EgressRateMbps: 90, IngressRateMbps: 450, LeaseSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if ap.Interface != "eth0" || ap.Qdisc != "cake" || ap.Previous != "mq" {
		t.Errorf("applied %+v", ap)
	}
	for _, want := range []string{"root handle 5047: cake bandwidth 90.000mbit", "ip link add pg-ifb0 type ifb", "mirred egress redirect dev pg-ifb0", "dev pg-ifb0 root handle 5047: cake bandwidth 450.000mbit ingress"} {
		if !f.has(want) {
			t.Errorf("missing command %q in %v", want, f.cmds)
		}
	}
	if _, err := os.Stat(m.file); err != nil {
		t.Error("lease file must exist while shaping is active")
	}
	if err := m.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.has("tc qdisc del dev eth0 root") || !f.has("tc qdisc del dev eth0 ingress") || !f.has("ip link del pg-ifb0") {
		t.Errorf("clear did not remove everything: %v", f.cmds)
	}
	if _, err := os.Stat(m.file); !os.IsNotExist(err) {
		t.Error("lease file must be removed on clear")
	}
}

func TestRefusesCustomQdisc(t *testing.T) {
	f := &fakeTC{rootKind: "cake"} // someone's own SQM
	m := newTestManager(t, f)
	_, err := m.ApplySpec(context.Background(), Spec{Interface: "eth0", Qdisc: "auto", EgressRateMbps: 50})
	if err == nil || !strings.Contains(err.Error(), "will not replace") {
		t.Fatalf("custom root qdisc must be refused, got %v", err)
	}
	if f.has("qdisc replace") {
		t.Error("nothing may be changed when refusing")
	}
	f2 := &fakeTC{rootKind: "fq_codel", ingress: true}
	if _, err := newTestManager(t, f2).ApplySpec(context.Background(), Spec{Interface: "eth0", IngressRateMbps: 50}); err == nil {
		t.Error("an existing ingress qdisc must be refused")
	}
	if _, err := newTestManager(t, &fakeTC{rootKind: "cake"}).ApplySpec(context.Background(), Spec{Interface: "eth0", EgressRateMbps: 50, Force: true}); err != nil {
		t.Errorf("force should override: %v", err)
	}
}

func TestCakeFallsBackToFqCodel(t *testing.T) {
	f := &fakeTC{rootKind: "fq_codel", noCake: true} // EL8 kernels
	ap, err := newTestManager(t, f).ApplySpec(context.Background(), Spec{Interface: "eth0", Qdisc: "auto", EgressRateMbps: 100})
	if err != nil {
		t.Fatal(err)
	}
	if ap.Qdisc != "fq_codel" || !f.has("htb rate 100.000mbit") || !f.has("parent 5047:1 fq_codel") {
		t.Errorf("fallback: %+v %v", ap, f.cmds)
	}
}

func TestFailureRollsBack(t *testing.T) {
	f := &fakeTC{rootKind: "mq", failOn: "mirred"}
	m := newTestManager(t, f)
	if _, err := m.ApplySpec(context.Background(), Spec{Interface: "eth0", EgressRateMbps: 50, IngressRateMbps: 50}); err == nil {
		t.Fatal("expected failure")
	}
	if !f.has("tc qdisc del dev eth0 root") || !f.has("ip link del pg-ifb0") {
		t.Errorf("partial shaping must be rolled back: %v", f.cmds)
	}
	if _, _, _, ok := m.Active(); ok {
		t.Error("no lease may remain after a failure")
	}
}

func TestLeaseExpiresAndStaleCleanup(t *testing.T) {
	f := &fakeTC{rootKind: "mq"}
	m := newTestManager(t, f)
	if _, err := m.ApplySpec(context.Background(), Spec{Interface: "eth0", EgressRateMbps: 10, LeaseSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, _, ok := m.Active(); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A crash leaves a lease file: the next manager removes the shaping.
	if err := os.WriteFile(m.file, []byte(`{"interface":"eth9","ingress":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeTC{}
	m2 := &Manager{log: m.log, run: f2.run, file: m.file}
	m2.cleanupStale()
	if !f2.has("tc qdisc del dev eth9 root") || !f2.has("ip link del pg-ifb0") {
		t.Errorf("stale lease not cleaned: %v", f2.cmds)
	}
}

func TestValidation(t *testing.T) {
	m := newTestManager(t, &fakeTC{rootKind: "mq"})
	for _, s := range []Spec{
		{Interface: "eth0", LeaseSeconds: 7200},
		{Interface: "eth0", LossPct: 150},
		{Interface: "eth0; rm -rf /", EgressRateMbps: 1},
		{EgressRateMbps: 1}, // no interface or target
	} {
		if _, err := m.ApplySpec(context.Background(), s); err == nil {
			t.Errorf("spec %+v should be refused", s)
		}
	}
}
