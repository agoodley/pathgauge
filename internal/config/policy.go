package config

import (
	"fmt"
	"strings"
	"time"
)

// ShapingScope says how much traffic an agent may shape (plan §8.3).
type ShapingScope string

// Shaping scopes. Test-traffic-only scope arrives in Phase 7.
const (
	ShapingNone      ShapingScope = "none"
	ShapingInterface ShapingScope = "interface"
)

// Policy is the agent-local safety policy (/etc/pathgauge/policy.yaml).
// The server can read it but never override it.
type Policy struct {
	// MaxRateMbps caps any test's offered load, per direction.
	MaxRateMbps int `yaml:"max_rate_mbps" json:"max_rate_mbps"`
	// MaxTestDuration caps a single test step.
	MaxTestDuration time.Duration `yaml:"max_test_duration" json:"max_test_duration_ns"`
	// AllowedTests lists test IDs or prefixes ending in "*". Empty means all.
	AllowedTests []string `yaml:"allowed_tests" json:"allowed_tests"`
	// AllowedHours lists local-time windows such as "22:00-06:00".
	// Empty means any time.
	AllowedHours []string `yaml:"allowed_hours" json:"allowed_hours"`
	// Shaping is the largest shaping scope this agent accepts.
	Shaping ShapingScope `yaml:"shaping" json:"shaping"`
	// AllowedPeers lists extra endpoints (names or addresses) this agent may
	// test against. Empty means only its enrolled server.
	AllowedPeers []string `yaml:"allowed_peers" json:"allowed_peers"`
}

// DefaultPolicy returns the policy written by `agent join` (decision
// 2026-11-05: whole-interface shaping is acceptable by default).
func DefaultPolicy() Policy {
	return Policy{
		MaxRateMbps:     10000,
		MaxTestDuration: 30 * time.Minute,
		Shaping:         ShapingInterface,
	}
}

// LoadPolicy reads a policy file over the defaults. An empty path returns
// the defaults.
func LoadPolicy(path string) (Policy, error) {
	p := DefaultPolicy()
	if err := decodeFile(path, &p); err != nil {
		return Policy{}, err
	}
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Validate reports the first policy error.
func (p Policy) Validate() error {
	if p.MaxRateMbps < 1 || p.MaxRateMbps > 100000 {
		return fmt.Errorf("max_rate_mbps: %d is outside 1-100000", p.MaxRateMbps)
	}
	if p.MaxTestDuration < time.Second {
		return fmt.Errorf("max_test_duration: %s is shorter than 1s", p.MaxTestDuration)
	}
	switch p.Shaping {
	case ShapingNone, ShapingInterface:
	default:
		return fmt.Errorf("shaping: %q is not none or interface", p.Shaping)
	}
	for _, w := range p.AllowedHours {
		if _, err := ParseWindow(w); err != nil {
			return fmt.Errorf("allowed_hours: %w", err)
		}
	}
	for _, t := range p.AllowedTests {
		if t == "" || strings.Contains(strings.TrimSuffix(t, "*"), "*") {
			return fmt.Errorf("allowed_tests: %q must be a test ID or a prefix ending in *", t)
		}
	}
	return nil
}

// AllowsTest reports whether the policy permits the given test ID.
func (p Policy) AllowsTest(id string) bool {
	if len(p.AllowedTests) == 0 {
		return true
	}
	for _, pattern := range p.AllowedTests {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(id, prefix) {
				return true
			}
		} else if id == pattern {
			return true
		}
	}
	return false
}

// AllowsTime reports whether t falls inside an allowed window.
func (p Policy) AllowsTime(t time.Time) bool {
	if len(p.AllowedHours) == 0 {
		return true
	}
	for _, s := range p.AllowedHours {
		w, err := ParseWindow(s)
		if err == nil && w.Contains(t) {
			return true
		}
	}
	return false
}

// Window is a daily local-time window in minutes since midnight. A window
// whose end is not after its start wraps past midnight.
type Window struct {
	Start, End int
}

// ParseWindow parses "HH:MM-HH:MM".
func ParseWindow(s string) (Window, error) {
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("%q is not HH:MM-HH:MM", s)
	}
	start, err := parseClock(strings.TrimSpace(from))
	if err != nil {
		return Window{}, fmt.Errorf("%q: %w", s, err)
	}
	end, err := parseClock(strings.TrimSpace(to))
	if err != nil {
		return Window{}, fmt.Errorf("%q: %w", s, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("%q: start and end are equal", s)
	}
	return Window{Start: start, End: end}, nil
}

// Contains reports whether t's local wall-clock time is inside the window.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return m >= w.Start && m < w.End
	}
	return m >= w.Start || m < w.End
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}
