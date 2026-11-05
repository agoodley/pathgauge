package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runCLI(t *testing.T, ctx context.Context, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(ctx, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := runCLI(t, context.Background(), "version")
	if code != 0 || !strings.HasPrefix(out, "pathgauge ") {
		t.Errorf("code %d, out %q", code, out)
	}
	code, out, _ = runCLI(t, context.Background(), "version", "--json")
	var v map[string]string
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v["version"] == "" || v["platform"] == "" {
		t.Errorf("code %d, out %q", code, out)
	}
	code, out, _ = runCLI(t, context.Background(), "--version")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("--version: code %d, out %q", code, out)
	}
}

func TestDoctorJSON(t *testing.T) {
	code, out, errOut := runCLI(t, context.Background(), "doctor", "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	var r struct {
		Kernel string
		Checks []struct{ Name, Status string }
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Kernel == "" || len(r.Checks) == 0 {
		t.Errorf("bad report (%v): %s", err, out)
	}
}

func TestCheckConfig(t *testing.T) {
	pkg := filepath.Join("..", "..", "packaging", "config")
	code, out, errOut := runCLI(t, context.Background(), "server", "run", "--check-config", "-c", filepath.Join(pkg, "server.yaml"))
	if code != 0 || !strings.Contains(out, "OK") {
		t.Errorf("server: code %d out %q err %q", code, out, errOut)
	}

	// The packaged agent.yaml points at /etc/pathgauge/policy.yaml, so
	// rewrite it to use the packaged policy.
	agent, err := os.ReadFile(filepath.Join(pkg, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := filepath.Abs(filepath.Join(pkg, "policy.yaml"))
	tmp := filepath.Join(t.TempDir(), "agent.yaml")
	content := strings.Replace(string(agent), "/etc/pathgauge/policy.yaml", policy, 1)
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runCLI(t, context.Background(), "agent", "run", "--check-config", "-c", tmp)
	if code != 0 || strings.Count(out, "OK") != 2 {
		t.Errorf("agent: code %d out %q err %q", code, out, errOut)
	}

	code, _, errOut = runCLI(t, context.Background(), "agent", "run", "--check-config", "-c", "")
	if code != 0 {
		t.Errorf("agent defaults: code %d err %q", code, errOut)
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("listen: nope\n"), 0o600)
	code, _, errOut = runCLI(t, context.Background(), "server", "run", "--check-config", "-c", bad)
	if code != 1 || !strings.Contains(errOut, "listen") {
		t.Errorf("bad config: code %d err %q", code, errOut)
	}
}

func TestServiceStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "server.yaml")
	content := "listen: \"127.0.0.1:0\"\ndata_dir: " + dir + "\ndata_plane:\n  listen: \"127.0.0.1:0\"\nlog:\n  level: error\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _, errOut := runCLI(t, ctx, "server", "run", "-c", cfg)
		if code != 0 {
			t.Logf("stderr: %s", errOut)
		}
		done <- code
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := runCLI(t, context.Background(), "frobnicate")
	if code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("code %d err %q", code, errOut)
	}
}

func TestTuneAdvice(t *testing.T) {
	cur := map[string]string{
		"net.core.rmem_max": "212992", "net.core.wmem_max": "212992",
		"net.ipv4.tcp_rmem": "4096 131072 6291456", "net.ipv4.tcp_wmem": "4096 16384 4194304",
		"net.ipv4.tcp_mtu_probing": "0", "net.core.default_qdisc": "fq_codel", "net.ipv4.tcp_congestion_control": "cubic",
	}
	adv := tuneAdvice(1000, 100, true, func(k string) string { return cur[k] })
	got := map[string]string{}
	for _, a := range adv {
		got[a.key] = a.want
	}
	// 1 Gbit/s x 100 ms = 12.5 MB BDP -> 2x rounded up to a power of two = 32 MiB.
	if got["net.ipv4.tcp_rmem"] != "4096 131072 33554432" || got["net.core.rmem_max"] != "33554432" {
		t.Errorf("buffers: %v", got)
	}
	if got["net.ipv4.tcp_congestion_control"] != "bbr" || got["net.core.default_qdisc"] != "fq" || got["net.ipv4.tcp_mtu_probing"] != "1" {
		t.Errorf("other advice: %v", got)
	}
	cur["net.ipv4.tcp_rmem"] = "4096 131072 67108864"
	for _, a := range tuneAdvice(1000, 100, false, func(k string) string { return cur[k] }) {
		if a.key == "net.ipv4.tcp_rmem" || a.key == "net.ipv4.tcp_congestion_control" {
			t.Errorf("unexpected advice %v", a)
		}
	}
}
