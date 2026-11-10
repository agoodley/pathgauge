package analysis

import (
	"fmt"
	"strings"

	"github.com/agoodley/pathgauge/internal/results"
)

// scoreKinds lists, for each part of the health score, the kinds of test
// whose results it is computed from.
var scoreKinds = map[string][]string{
	"throughput":  {"tcp"},
	"latency":     {"latency"},
	"loss":        {"latency", "udp"},
	"bufferbloat": {"bufferbloat"},
	"path":        {"trace"},
	"integrity":   {"pmtu", "integrity"},
}

// completeness records which tests of a run produced a result. A test that
// was asked for and failed is not evidence that the link is healthy, so it
// must never read as one that passed.
type completeness struct {
	total    int            // tests that ran to an end, with a result or an error
	measured int            // tests with a result, not counting the environment check
	failed   []results.Step // tests that ended in an error
	ok, bad  map[string]int // by test kind, among the steps the score is built from
}

// completenessOf looks at every step of r for what failed, and at the steps
// in scored (r without the steps measured under a group's load) for what
// the score parts are built from.
func completenessOf(r, scored *results.Report) completeness {
	c := completeness{ok: map[string]int{}, bad: map[string]int{}}
	for _, s := range r.Steps {
		switch s.Status {
		case "ok":
			c.total++
			if s.Result != nil && s.Test != "info" {
				c.measured++
			}
		case "error":
			c.total++
			c.failed = append(c.failed, s)
		}
	}
	for _, s := range scored.Steps {
		switch {
		case s.Status == "ok" && s.Result != nil:
			c.ok[s.Test]++
		case s.Status == "error":
			c.bad[s.Test]++
		}
	}
	return c
}

// unmeasured reports whether a part of the score has nothing behind it
// although its tests were asked for: every one of them failed, or nothing
// in the whole run was measured.
func (c completeness) unmeasured(part string) bool {
	if c.measured == 0 {
		return true
	}
	ok, bad := 0, 0
	for _, k := range scoreKinds[part] {
		ok += c.ok[k]
		bad += c.bad[k]
	}
	return bad > 0 && ok == 0
}

// finding describes the tests that did not complete, if any.
func (c completeness) finding() (results.Finding, bool) {
	var ids, names []string
	for i, s := range c.failed {
		ids = append(ids, s.ID)
		if i >= 6 {
			continue
		}
		name := s.Title
		if name == "" {
			name = s.Test
		}
		if name == "" {
			name = s.ID
		}
		if s.Error != "" {
			name += " (" + s.Error + ")"
		}
		names = append(names, name)
	}
	list := strings.Join(names, "; ")
	if n := len(c.failed) - len(names); n > 0 {
		list += fmt.Sprintf("; and %d more", n)
	}
	switch {
	case c.measured == 0 && len(c.failed) > 0:
		return results.Finding{ID: "run.nothing", Severity: "critical", Confidence: "high",
			Title:          "No test completed, so nothing was measured",
			Detail:         "Every test failed: " + list + ". The health score is 0 because there is no measurement to base one on.",
			Recommendation: "Check that the endpoint is running and that its port is reachable over both TCP and UDP, then run the tests again.",
			Evidence:       ids}, true
	case c.measured == 0:
		return results.Finding{ID: "run.nothing", Severity: "warning", Confidence: "high",
			Title:  "Nothing was measured",
			Detail: "This run has no test results: every test was skipped, or none was asked for. The health score is 0 because there is no measurement to base one on."}, true
	case len(c.failed) > 0:
		return results.Finding{ID: "run.incomplete", Severity: "warning", Confidence: "high",
			Title:          fmt.Sprintf("%d of %d tests did not complete", len(c.failed), c.total),
			Detail:         "Failed: " + list + ". The findings cover only the tests that completed, and a part of the health score whose tests all failed counts as 0.",
			Recommendation: "Run the failed tests again. If they fail the same way, the error text says what stopped them.",
			Evidence:       ids}, true
	}
	return results.Finding{}, false
}
