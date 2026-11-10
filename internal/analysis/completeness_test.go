package analysis

import (
	"strings"
	"testing"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

func failed(id, test, title, msg string) results.Step {
	return results.Step{ID: id, Test: test, Title: title, Status: "error", Error: msg}
}

// A run in which nothing worked used to score 100 with "No problems
// detected", so a dead link looked like a perfect one.
func TestAllTestsFailedIsNotHealthy(t *testing.T) {
	r := &results.Report{Steps: []results.Step{
		step("i", "info", &results.Info{}), // the environment check alone measures nothing
		failed("l", "latency", "Idle latency, jitter and loss", "no UDP path to the endpoint"),
		failed("d", "tcp", "TCP download, parallel streams", "dial data: connection refused"),
		failed("u", "tcp", "TCP upload, parallel streams", "dial data: connection refused"),
		failed("b", "bufferbloat", "Bufferbloat (latency under load)", "dial data: connection refused"),
	}}
	Analyze(r, Expectations{})
	f := ids(r)
	if _, ok := f["overall.healthy"]; ok {
		t.Error("a run with no measurements must not be reported healthy")
	}
	n, ok := f["run.nothing"]
	if !ok || n.Severity != "critical" {
		t.Fatalf("want a critical run.nothing finding, got %+v", r.Findings)
	}
	if !strings.Contains(n.Detail, "connection refused") || len(n.Evidence) != 4 {
		t.Errorf("the finding should name what failed: %+v", n)
	}
	if r.Summary.HealthScore != 0 {
		t.Errorf("health score %d; nothing measured must score 0 so health alerts fire", r.Summary.HealthScore)
	}
	for k, v := range r.Summary.ScoreParts {
		if v != 0 {
			t.Errorf("score part %s = %v with nothing measured", k, v)
		}
	}
	if r.Findings[0].ID != "run.nothing" {
		t.Errorf("the critical finding should come first, got %q", r.Findings[0].ID)
	}
}

func TestEmptyRunIsNotHealthy(t *testing.T) {
	for name, r := range map[string]*results.Report{
		"no steps":    {},
		"all skipped": {Steps: []results.Step{{ID: "a", Test: "tcp", Status: "skipped"}, {ID: "b", Test: "latency", Status: "skipped"}}},
	} {
		Analyze(r, Expectations{})
		f := ids(r)
		if _, ok := f["overall.healthy"]; ok {
			t.Errorf("%s: reported healthy", name)
		}
		if f["run.nothing"].Severity != "warning" || r.Summary.HealthScore != 0 {
			t.Errorf("%s: want a warning and score 0, got %+v score %d", name, r.Findings, r.Summary.HealthScore)
		}
	}
}

// Tests that failed lose their part of the score; the parts that were
// measured keep theirs.
func TestPartlyFailedRun(t *testing.T) {
	r := &results.Report{Steps: []results.Step{
		step("l", "latency", &results.Latency{UDP: stats.Summary{N: 100, Median: 10, P99: 11}, UDPJitterMs: 0.3}),
		failed("d", "tcp", "TCP download, parallel streams", "dial data: connection refused"),
		failed("u", "tcp", "", "dial data: connection refused"),
		step("b", "bufferbloat", &results.Bufferbloat{Grade: "A", Grades: map[string]string{"down": "A", "up": "A"}, IncreaseMs: map[string]float64{"down": 8, "up": 8}}),
	}}
	Analyze(r, Expectations{})
	f := ids(r)
	inc, ok := f["run.incomplete"]
	if !ok || inc.Severity != "warning" || !strings.HasPrefix(inc.Title, "2 of 4 tests") {
		t.Fatalf("want a warning naming 2 of 4 failed tests, got %+v", r.Findings)
	}
	if _, ok := f["overall.healthy"]; ok {
		t.Error("a run with failed tests must not be reported healthy")
	}
	p := r.Summary.ScoreParts
	if p["throughput"] != 0 {
		t.Errorf("throughput part %v; every throughput test failed, so it has earned nothing", p["throughput"])
	}
	if p["latency"] != 20 || p["bufferbloat"] != 18 {
		t.Errorf("measured parts should keep their marks: %v", p)
	}
	if p["path"] != 10 || p["integrity"] != 5 {
		t.Errorf("parts whose tests were not part of the run keep full marks: %v", p)
	}
	if r.Summary.HealthScore > 75 {
		t.Errorf("health score %d should reflect the failed throughput tests", r.Summary.HealthScore)
	}
}

// One of two tests of a kind failing leaves the part to the one that
// worked, and a failure inside a group's loaded phase is still reported.
func TestFailedAlongsideWorkingTestOfTheSameKind(t *testing.T) {
	together := failed("g", "udp", "UDP download (loss and jitter)", "timed out")
	together.Phase = "together"
	r := &results.Report{Steps: []results.Step{
		step("l", "latency", &results.Latency{UDP: stats.Summary{N: 100, Median: 10, P99: 11}}),
		step("d", "tcp", &results.TCP{Direction: results.Down, Streams: 8, Mbps: 900, Intervals: flat(900, 50, 10)}),
		failed("u", "tcp", "TCP upload, parallel streams", "reset by peer"),
		together,
	}}
	Analyze(r, Expectations{})
	if got := r.Summary.ScoreParts["throughput"]; got != 25 {
		t.Errorf("throughput part %v; one throughput test did complete", got)
	}
	if got := r.Summary.ScoreParts["loss"]; got != 20 {
		t.Errorf("loss part %v; latency measured loss, and the failed UDP test ran under a group's load", got)
	}
	inc := ids(r)["run.incomplete"]
	if len(inc.Evidence) != 2 {
		t.Errorf("both failed tests should be listed: %+v", inc)
	}
}
