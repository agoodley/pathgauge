package analysis

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/agoodley/pathgauge/internal/results"
)

// withoutLoaded returns a view of the report without the steps run
// together in a group: they were measured under the other tests' load, so
// the ordinary rules (idle latency, throughput, ...) must not use them.
func withoutLoaded(r *results.Report) *results.Report {
	vr := *r
	vr.Steps = nil
	for _, s := range r.Steps {
		if s.Phase != "together" {
			vr.Steps = append(vr.Steps, s)
		}
	}
	return &vr
}

// keyMetric is one comparable result of a test.
type keyMetric struct {
	name, unit, better string
	v                  float64
	// worse reports whether together is noticeably worse than alone.
	worse func(alone, together float64) bool
}

func risesBy(abs, ratio float64) func(a, t float64) bool {
	return func(a, t float64) bool { return t-a > abs && t > a*ratio }
}

func fallsBy(abs float64) func(a, t float64) bool {
	return func(a, t float64) bool { return a-t >= abs }
}

func fallsTo(frac float64) func(a, t float64) bool {
	return func(a, t float64) bool { return a > 0 && t < a*frac }
}

// keyMetrics picks out the results worth comparing for each test type.
func keyMetrics(res any) []keyMetric {
	switch v := res.(type) {
	case *results.Latency:
		return []keyMetric{
			{"round trip (p95)", "ms", "lower", v.UDP.P95, risesBy(20, 1.5)},
			{"packet loss", "%", "lower", v.Loss.LossPct, risesBy(1, 1)},
		}
	case *results.VoIP:
		return []keyMetric{{"call quality (worst MOS)", "", "higher", math.Min(v.Up.MOS, v.Down.MOS), fallsBy(0.3)}}
	case *results.TCP:
		return []keyMetric{{"throughput", "Mbit/s", "higher", v.Mbps, fallsTo(0.7)}}
	case *results.UDP:
		return []keyMetric{
			{"packet loss", "%", "lower", v.Loss.LossPct, risesBy(1, 1)},
			{"jitter", "ms", "lower", v.JitterMs, risesBy(10, 1.5)},
		}
	case *results.CDP:
		return []keyMetric{
			{"worst replication lag", "s", "lower", v.MaxLagS, risesBy(1, 1.5)},
			{"time within the RPO", "%", "higher", v.WithinRPOPct, fallsBy(5)},
		}
	case *results.ShortFlow:
		if c, ok := v.Completions["1MB"]; ok {
			return []keyMetric{{"1 MB transfer (p95)", "ms", "lower", c.P95, risesBy(100, 1.5)}}
		}
	case *results.Mix:
		if len(v.Phases) > 0 {
			p := v.Phases[len(v.Phases)-1]
			good, n := 0, 0
			for _, k := range p.Kinds {
				if k.Verdict != "" {
					good += k.Good
					n += k.Sessions
				}
			}
			if n > 0 {
				return []keyMetric{{"sessions with good quality", "%", "higher", 100 * float64(good) / float64(n), fallsBy(10)}}
			}
		}
	case *results.Bufferbloat:
		worst := 0.0
		for _, x := range v.IncreaseMs {
			worst = math.Max(worst, x)
		}
		return []keyMetric{{"latency rise under load (p95)", "ms", "lower", worst, risesBy(30, 1.5)}}
	}
	return nil
}

// stepFor finds a grouped test's step in a phase.
func stepFor(r *results.Report, group, phase, test string) *results.Step {
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.Group == group && s.Phase == phase && s.Status == "ok" && strings.HasSuffix(s.ID, "-"+test) {
			return s
		}
	}
	return nil
}

// loadComparisons compares each grouped test alone and together, and
// returns the comparisons with a finding per group.
func loadComparisons(r *results.Report) ([]results.LoadComparison, []results.Finding) {
	var out []results.LoadComparison
	var findings []results.Finding
	for _, g := range r.Groups {
		if !g.Baseline {
			continue
		}
		var worse []string
		var ids []string
		for _, t := range g.Tests {
			alone, together := stepFor(r, g.ID, "alone", t), stepFor(r, g.ID, "together", t)
			if alone == nil || together == nil {
				continue
			}
			am, tm := keyMetrics(alone.Result), keyMetrics(together.Result)
			if len(am) == 0 || len(am) != len(tm) {
				continue
			}
			ids = append(ids, alone.ID, together.ID)
			others := slices.DeleteFunc(slices.Clone(g.Tests), func(x string) bool { return x == t })
			for i, m := range am {
				lc := results.LoadComparison{Group: g.ID, Test: t, Title: together.Title, Others: others, Metric: m.name, Unit: m.unit,
					Alone: round2(m.v), Together: round2(tm[i].v), Better: m.better, Worse: m.worse(m.v, tm[i].v)}
				out = append(out, lc)
				if lc.Worse {
					worse = append(worse, fmt.Sprintf("%s: %s %s → %s", together.Title, m.name, fmtVal(lc.Alone, m.unit), fmtVal(lc.Together, m.unit)))
				}
			}
		}
		if len(ids) == 0 {
			continue
		}
		names := strings.Join(g.Tests, " + ")
		if len(worse) == 0 {
			findings = append(findings, results.Finding{ID: "group." + g.ID + ".ok", Severity: "good", Confidence: "medium",
				Title:  fmt.Sprintf("Running %s together didn't noticeably affect them", names),
				Detail: fmt.Sprintf("Each test ran for %.0f s on its own and then alongside the others; no key result got noticeably worse.", g.DurationS), Evidence: ids})
			continue
		}
		findings = append(findings, results.Finding{ID: "group." + g.ID + ".worse", Severity: "warning", Confidence: "medium",
			Title:          fmt.Sprintf("Under load (%s together), %s", names, plural(len(worse), "result got worse", "results got worse")),
			Detail:         strings.Join(worse, "; ") + ".",
			Recommendation: "These tests compete for the same bottleneck. Queue management (CAKE or fq_codel, see the sqm test), QoS for the traffic that matters, or a throttle on bulk and replication traffic keeps one from hurting the others.",
			Evidence:       ids})
	}
	return out, findings
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func fmtVal(v float64, unit string) string {
	switch unit {
	case "":
		return fmt.Sprintf("%.2f", v)
	case "%":
		return fmt.Sprintf("%.1f%%", v)
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
