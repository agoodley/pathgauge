package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAreValid(t *testing.T) {
	if err := DefaultServer().Validate(); err != nil {
		t.Errorf("server defaults: %v", err)
	}
	if err := DefaultAgent().Validate(); err != nil {
		t.Errorf("agent defaults: %v", err)
	}
	if err := DefaultPolicy().Validate(); err != nil {
		t.Errorf("policy defaults: %v", err)
	}
}

func TestPackagedConfigsLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "packaging", "config")
	if _, err := LoadServer(filepath.Join(dir, "server.yaml")); err != nil {
		t.Errorf("server.yaml: %v", err)
	}
	if _, err := LoadAgent(filepath.Join(dir, "agent.yaml")); err != nil {
		t.Errorf("agent.yaml: %v", err)
	}
	p, err := LoadPolicy(filepath.Join(dir, "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.yaml: %v", err)
	}
	if !reflect.DeepEqual(p, DefaultPolicy()) {
		t.Errorf("packaged policy.yaml drifted from DefaultPolicy:\n got %+v\nwant %+v", p, DefaultPolicy())
	}
}

func TestLoadServerOverridesDefaults(t *testing.T) {
	p := writeFile(t, "listen: \"127.0.0.1:9443\"\nlog:\n  level: debug\n")
	cfg, err := LoadServer(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9443" || cfg.Log.Level != "debug" {
		t.Errorf("override not applied: %+v", cfg)
	}
	if cfg.DataPlane.Listen != ":7400" || cfg.DataDir != DefaultDataDir {
		t.Errorf("defaults lost: %+v", cfg)
	}
}

func TestLoadEmptyFileGivesDefaults(t *testing.T) {
	cfg, err := LoadAgent(writeFile(t, "# nothing set\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPlane.Listen != DefaultAgent().DataPlane.Listen {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field": "listne: \":7443\"\n",
		"bad port":      "listen: \":99999\"\n",
		"no port":       "listen: \"localhost\"\n",
		"relative dir":  "data_dir: var/lib/x\n",
		"bad level":     "log:\n  level: loud\n",
		"bad format":    "log:\n  format: xml\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadServer(writeFile(t, content)); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if _, err := LoadServer(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestPolicyRejects(t *testing.T) {
	cases := map[string]string{
		"shaping scope": "shaping: everything\n",
		"rate zero":     "max_rate_mbps: 0\n",
		"duration":      "max_test_duration: 10ms\n",
		"window":        "allowed_hours: [\"25:00-06:00\"]\n",
		"test pattern":  "allowed_tests: [\"a*b\"]\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPolicy(writeFile(t, content)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestPolicyAllowsTest(t *testing.T) {
	p := DefaultPolicy()
	if !p.AllowsTest("throughput.tcp") {
		t.Error("empty allow-list must allow everything")
	}
	p.AllowedTests = []string{"latency.*", "path.trace"}
	for id, want := range map[string]bool{
		"latency.idle":   true,
		"path.trace":     true,
		"path.mtr":       false,
		"throughput.tcp": false,
	} {
		if got := p.AllowsTest(id); got != want {
			t.Errorf("AllowsTest(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestWindows(t *testing.T) {
	at := func(hh, mm int) time.Time { return time.Date(2026, 11, 5, hh, mm, 0, 0, time.Local) }

	night, err := ParseWindow("22:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{at(23, 30), true}, {at(2, 0), true}, {at(6, 0), false}, {at(12, 0), false}, {at(22, 0), true},
	} {
		if got := night.Contains(c.t); got != c.want {
			t.Errorf("night.Contains(%s) = %v", c.t.Format("15:04"), got)
		}
	}

	day, _ := ParseWindow("09:00 - 17:30")
	if !day.Contains(at(17, 29)) || day.Contains(at(17, 30)) {
		t.Error("day window edges wrong")
	}

	for _, bad := range []string{"", "9-5", "10:00-10:00", "10:00"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) should fail", bad)
		}
	}

	p := DefaultPolicy()
	p.AllowedHours = []string{"22:00-06:00"}
	if p.AllowsTime(at(12, 0)) || !p.AllowsTime(at(1, 0)) {
		t.Error("AllowsTime ignores windows")
	}
}

func TestErrorsNameTheField(t *testing.T) {
	_, err := LoadServer(writeFile(t, "data_plane:\n  listen: \"nope\"\n"))
	if err == nil || !strings.Contains(err.Error(), "data_plane.listen") {
		t.Errorf("error should name the field, got %v", err)
	}
}
