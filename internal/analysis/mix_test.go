package analysis

import (
	"strings"
	"testing"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

func mixPhase(name string, transfers bool, voiceMOS float64, voiceVerdict string, probeP95 float64) results.MixPhase {
	p := results.MixPhase{Name: name, WithTransfers: transfers, ProbeRTT: stats.Summary{P95: probeP95}, Verdict: voiceVerdict,
		Kinds: []results.MixKind{
			{Kind: "voice", Label: "Voice call (G.711)", Sessions: 10, Verdict: voiceVerdict, Metric: "worst call quality (MOS)", Value: voiceMOS},
			{Kind: "web", Label: "Web browsing", Sessions: 2, Verdict: "good", Metric: "page load (p95)", Value: 1200, Unit: "ms"},
		}}
	if transfers {
		p.Kinds = append(p.Kinds, results.MixKind{Kind: "download", Label: "File download", Sessions: 1, Metric: "throughput", Value: 92, Unit: "Mbit/s"})
	}
	return p
}

func TestMixDegradedUnderTransfers(t *testing.T) {
	m := &results.Mix{DSCP: true, Verdict: "poor", Phases: []results.MixPhase{
		mixPhase("interactive", false, 4.4, "good", 22),
		mixPhase("with-transfers", true, 2.6, "poor", 410),
	}}
	r := &results.Report{Steps: []results.Step{step("mix", "mix", m)}}
	Analyze(r, Expectations{})
	f := ids(r)
	d, ok := f["mix.voice.degraded"]
	if !ok || d.Severity != "warning" {
		t.Fatalf("want a voice degraded warning, got %v", f)
	}
	if !strings.Contains(d.Title, "MOS 4.4 → MOS 2.6") || !strings.Contains(d.Detail, "file download at 92 Mbit/s") || !strings.Contains(d.Recommendation, "not prioritising") {
		t.Errorf("finding text: %q / %q / %q", d.Title, d.Detail, d.Recommendation)
	}
	if _, ok := f["mix.web.degraded"]; ok {
		t.Error("web did not degrade")
	}
	if _, ok := f["mix.good"]; ok {
		t.Error("not all good")
	}
}

func TestMixAllGoodAndPoorOnItsOwn(t *testing.T) {
	good := &results.Mix{Verdict: "good", Counts: []results.MixCount{{Kind: "voice", Label: "Voice call (G.711)", Count: 10}},
		Phases: []results.MixPhase{mixPhase("interactive", false, 4.4, "good", 20), mixPhase("with-transfers", true, 4.3, "good", 25)}}
	r := &results.Report{Steps: []results.Step{step("mix", "mix", good)}}
	Analyze(r, Expectations{})
	if g, ok := ids(r)["mix.good"]; !ok || !strings.Contains(g.Title, "with and without") {
		t.Errorf("want mix.good, got %v", ids(r))
	}
	poor := &results.Mix{Verdict: "poor", Phases: []results.MixPhase{mixPhase("all", false, 3.1, "poor", 20)}}
	r = &results.Report{Steps: []results.Step{step("mix", "mix", poor)}}
	Analyze(r, Expectations{})
	if p, ok := ids(r)["mix.voice.poor"]; !ok || !strings.Contains(p.Title, "in this mix") {
		t.Errorf("want mix.voice.poor, got %v", ids(r))
	}
}

func TestCapacityFindings(t *testing.T) {
	c := &results.Capacity{Kind: "voice", PerSessionKbps: 80, Alone: results.CapacitySearch{Acceptable: 48, Good: 40},
		WithTransfers: &results.CapacitySearch{Acceptable: 0}}
	r := &results.Report{Steps: []results.Step{step("cap", "capacity", c)}}
	Analyze(r, Expectations{})
	f := ids(r)
	if x, ok := f["capacity.voice"]; !ok || !strings.Contains(x.Title, "48 simultaneous voice calls; 0 while transfers run") {
		t.Errorf("capacity finding: %v", x.Title)
	}
	if x, ok := f["capacity.voice.transfers"]; !ok || x.Severity != "warning" {
		t.Errorf("want a transfers warning: %v", f)
	}
	c2 := &results.Capacity{Kind: "video", PerSessionKbps: 1545, Alone: results.CapacitySearch{Acceptable: 64, Good: 64, Limit: 64, LimitReason: "session cap"}}
	r = &results.Report{Steps: []results.Step{step("cap", "capacity", c2)}}
	Analyze(r, Expectations{})
	if x := ids(r)["capacity.video"]; !strings.Contains(x.Title, "at least 64") {
		t.Errorf("limit wording: %q", x.Title)
	}
}

func TestCDPFindings(t *testing.T) {
	bad := &results.CDP{Settings: results.CDPSettings{RateMbps: 30, BurstS: 10, PeriodS: 60, RPOs: 15, LimitMbps: 40}, PeakMbps: 90,
		Verdict: "poor", MaxLagS: 42.3, WithinRPOPct: 40, OverRPOS: 33, SustainedMbps: 40, Backlogged: true,
		IdleRTT: stats.Summary{N: 20, Median: 12}, LoadedRTT: stats.Summary{N: 100, P95: 230}}
	r := &results.Report{Steps: []results.Step{step("cdp", "cdp", bad)}}
	Analyze(r, Expectations{})
	f := ids(r)
	m, ok := f["cdp.rpo.missed"]
	if !ok || m.Severity != "critical" || !strings.Contains(m.Recommendation, "40 Mbit/s throttle") || !strings.Contains(m.Recommendation, "43 s") {
		t.Errorf("missed RPO finding: %+v", m)
	}
	if l, ok := f["cdp.latency"]; !ok || !strings.Contains(l.Title, "218 ms") {
		t.Errorf("latency finding: %+v", l)
	}
	good := &results.CDP{Settings: results.CDPSettings{RateMbps: 5, RPOs: 15, BurstS: 10, PeriodS: 60}, Verdict: "good", MaxLagS: 0.4}
	r = &results.Report{Steps: []results.Step{step("cdp", "cdp", good)}}
	Analyze(r, Expectations{})
	if _, ok := ids(r)["cdp.rpo.ok"]; !ok {
		t.Errorf("want cdp.rpo.ok: %v", ids(r))
	}
}

func TestLoadComparisons(t *testing.T) {
	lat := func(p95 float64) *results.Latency {
		return &results.Latency{UDP: stats.Summary{N: 100, Median: p95 - 2, P95: p95}}
	}
	voip := func(mos float64) *results.VoIP {
		return &results.VoIP{Up: results.VoIPDir{MOS: mos}, Down: results.VoIPDir{MOS: mos}, Verdict: "good"}
	}
	grouped := func(s results.Step, phase string) results.Step {
		s.Group, s.Phase = "g1", phase
		s.ID = "s01-g1-" + phase + "-" + s.ID
		return s
	}
	r := &results.Report{
		Groups: []results.Group{{ID: "g1", Tests: []string{"latency", "voip", "cdp-up"}, DurationS: 30, Baseline: true}},
		Steps: []results.Step{
			grouped(step("latency", "latency", lat(22)), "alone"),
			grouped(step("voip", "voip", voip(4.4)), "alone"),
			grouped(step("latency", "latency", lat(340)), "together"),
			grouped(step("voip", "voip", voip(3.1)), "together"),
		},
	}
	Analyze(r, Expectations{})
	f := ids(r)
	w, ok := f["group.g1.worse"]
	if !ok || !strings.Contains(w.Detail, "round trip (p95) 22.0 ms → 340.0 ms") || !strings.Contains(w.Detail, "4.40 → 3.10") {
		t.Fatalf("worse finding: %+v", w)
	}
	worse := 0
	for _, c := range r.LoadComparisons {
		if c.Worse {
			worse++
		}
	}
	if len(r.LoadComparisons) != 3 || worse != 2 {
		t.Errorf("comparisons: %+v", r.LoadComparisons)
	}
	// The 340 ms measured under load must not count as the link's idle latency.
	if r.Summary.IdleRTTms > 30 {
		t.Errorf("idle RTT taken from a loaded step: %v", r.Summary.IdleRTTms)
	}
}
