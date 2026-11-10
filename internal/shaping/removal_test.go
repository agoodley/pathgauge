package shaping

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveTC is a fake tc that keeps qdisc state, so removal can be checked by
// what is left on the interface. Like exec.CommandContext, it refuses to
// run anything once the context it is given is done.
type liveTC struct {
	mu      sync.Mutex
	cmds    []string
	root    bool // PathGauge's root qdisc is installed
	ingress bool // the ingress redirect is installed
	stuck   bool // delete commands fail
	noShow  bool // "tc qdisc show" fails
	missing bool // the interface does not exist
}

func (f *liveTC) run(ctx context.Context, name string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)
	switch {
	case strings.HasPrefix(cmd, "ip -j route get"):
		return `[{"dst":"198.51.100.7","dev":"eth0"}]`, nil
	case strings.HasPrefix(cmd, "tc -j qdisc show"):
		if f.missing {
			return "", errors.New(`tc -j qdisc show dev eth0: exit status 1: Cannot find device "eth0"`)
		}
		if f.noShow {
			return "", errors.New("tc: not found")
		}
		out := `[{"kind":"mq","handle":"0:","root":true}`
		if f.root {
			out = `[{"kind":"cake","handle":"5047:","root":true}`
		}
		if f.ingress {
			out += `,{"kind":"ingress","handle":"ffff:","parent":"ffff:fff1"}`
		}
		return out + "]", nil
	case strings.Contains(cmd, " del "):
		if f.stuck {
			return "", errors.New("RTNETLINK answers: Operation not permitted")
		}
		switch {
		case strings.HasSuffix(cmd, " root"):
			f.root = false
		case strings.HasSuffix(cmd, " ingress"):
			f.ingress = false
		}
	case strings.Contains(cmd, "root handle "+Handle) && strings.Contains(cmd, "dev eth0"):
		f.root = true
	case strings.Contains(cmd, "handle ffff: ingress"):
		f.ingress = true
	}
	return "", nil
}

func (f *liveTC) set(fn func(*liveTC)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *liveTC) shaped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.root || f.ingress
}

func (f *liveTC) ran(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func liveManager(t *testing.T, f *liveTC) *Manager {
	t.Helper()
	m := newTestManager(t, &fakeTC{})
	m.run = f.run
	t.Cleanup(func() {
		m.mu.Lock()
		if m.timer != nil {
			m.timer.Stop()
		}
		m.mu.Unlock()
	})
	return m
}

func apply(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := m.ApplySpec(context.Background(), Spec{Interface: "eth0", Qdisc: "auto", EgressRateMbps: 90, IngressRateMbps: 450, LeaseSeconds: 120}); err != nil {
		t.Fatal(err)
	}
}

func leaseExists(m *Manager) bool {
	_, err := os.Stat(m.file)
	return err == nil
}

// The tuner clears shaping with the run's context, which is already
// cancelled when a run is cancelled. Shaping must come off all the same.
func TestClearWithCancelledContextRemovesShaping(t *testing.T) {
	f := &liveTC{}
	m := liveManager(t, f)
	apply(t, m)
	if !f.shaped() {
		t.Fatal("test setup: nothing was shaped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Clear(ctx); err != nil {
		t.Fatalf("Clear with a cancelled context: %v", err)
	}
	if f.shaped() {
		t.Error("shaping is still on the interface")
	}
	if _, _, _, ok := m.Active(); ok || leaseExists(m) {
		t.Error("the lease should be gone once the shaping is")
	}
}

// Shaping that could not be removed stays recorded, in memory and on
// disk, and is removed by a later attempt.
func TestFailedRemovalKeepsTheLeaseAndRetries(t *testing.T) {
	old := retryDelay
	retryDelay = 50 * time.Millisecond
	defer func() { retryDelay = old }()

	f := &liveTC{}
	m := liveManager(t, f)
	apply(t, m)
	f.set(func(f *liveTC) { f.stuck = true })
	err := m.Clear(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still applied on eth0") {
		t.Fatalf("Clear must report shaping it could not remove, got %v", err)
	}
	if _, _, _, ok := m.Active(); !ok || !leaseExists(m) {
		t.Fatal("a lease whose shaping is still applied must not be forgotten")
	}
	// New shaping is refused while the old one can't be removed.
	if _, err := m.ApplySpec(context.Background(), Spec{Interface: "eth0", EgressRateMbps: 10}); err == nil {
		t.Error("applying over shaping that could not be removed should fail")
	}
	// Once tc works again the retry removes it without being asked.
	f.set(func(f *liveTC) { f.stuck = false })
	deadline := time.Now().Add(5 * time.Second)
	for f.shaped() || leaseExists(m) {
		if time.Now().After(deadline) {
			t.Fatal("removal was not retried")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, _, ok := m.Active(); ok {
		t.Error("the lease should be gone after the retry")
	}
}

// Not being able to see what is on the interface is not proof it is clean.
func TestRemovalThatCannotBeCheckedKeepsTheLease(t *testing.T) {
	f := &liveTC{}
	m := liveManager(t, f)
	apply(t, m)
	f.set(func(f *liveTC) { f.noShow = true })
	if err := m.Clear(context.Background()); err == nil {
		t.Fatal("Clear must fail when it cannot check the interface")
	}
	if !leaseExists(m) {
		t.Error("the lease file must stay")
	}
	// An interface that no longer exists has no qdiscs left to remove.
	f.set(func(f *liveTC) { f.noShow, f.missing = false, true })
	if err := m.Clear(context.Background()); err != nil {
		t.Fatalf("a vanished interface counts as clean: %v", err)
	}
	if leaseExists(m) {
		t.Error("the lease file should be gone")
	}
}

// A lease left by a crashed process is kept until its shaping is removed.
func TestStaleLeaseSurvivesFailedCleanup(t *testing.T) {
	f := &liveTC{root: true, ingress: true, stuck: true}
	m := liveManager(t, f)
	if err := os.WriteFile(m.file, []byte(`{"interface":"eth0","ingress":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cleanupStale()
	if !leaseExists(m) {
		t.Fatal("the stale lease was deleted although its shaping is still applied")
	}
	if iface, _, _, ok := m.Active(); !ok || iface != "eth0" {
		t.Error("the stale lease should be adopted so removal is retried")
	}
	f.set(func(f *liveTC) { f.stuck = false })
	if err := m.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.shaped() || leaseExists(m) {
		t.Error("stale shaping not removed")
	}
	// A lease file that is not a lease is just removed.
	if err := os.WriteFile(m.file, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cleanupStale()
	if leaseExists(m) {
		t.Error("an unreadable lease file should be removed")
	}
}

// Without a recorded lease there is no cleanup after a crash, so there is
// no shaping either.
func TestNoShapingWithoutALeaseRecord(t *testing.T) {
	f := &liveTC{}
	m := liveManager(t, f)
	m.file = filepath.Join(t.TempDir(), "no-such-dir", "lease.json")
	_, err := m.ApplySpec(context.Background(), Spec{Interface: "eth0", EgressRateMbps: 50})
	if err == nil || !strings.Contains(err.Error(), "cannot record the shaping lease") {
		t.Fatalf("want a lease-record error, got %v", err)
	}
	if f.ran("qdisc replace") || f.shaped() {
		t.Error("nothing may be changed when the lease can't be recorded")
	}
	if _, _, _, ok := m.Active(); ok {
		t.Error("no lease may remain")
	}
}

// A request from a remote client never chooses the interface and never
// forces the replacement of the owner's own qdisc.
func TestRemoteRequestCannotChooseInterfaceOrForce(t *testing.T) {
	f := &liveTC{}
	m := liveManager(t, f)
	raw, _ := json.Marshal(Spec{Interface: "wan0", Force: true, Qdisc: "auto", EgressRateMbps: 20})
	res, err := m.Apply(context.Background(), raw, netip.MustParseAddr("198.51.100.7"))
	if err != nil {
		t.Fatal(err)
	}
	if ap := res.(Applied); ap.Interface != "eth0" {
		t.Errorf("shaped %q; a remote request must shape the interface that routes to the client", ap.Interface)
	}
	if !f.ran("ip -j route get 198.51.100.7") || f.ran("wan0") {
		t.Errorf("the client's interface choice was used: %v", f.cmds)
	}
	if _, err := m.Apply(context.Background(), raw, netip.Addr{}); err == nil {
		t.Error("a request with no client address must be refused")
	}

	// force from a remote client does not override the custom-qdisc guard.
	custom := &fakeTC{rootKind: "cake"}
	m2 := newTestManager(t, custom)
	if _, err := m2.Apply(context.Background(), raw, netip.MustParseAddr("198.51.100.7")); err == nil || !strings.Contains(err.Error(), "will not replace") {
		t.Errorf("remote force must be ignored, got %v", err)
	}
}
