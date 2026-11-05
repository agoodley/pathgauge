package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// Every result type renders (even empty), failed steps render their
// error, and user-controlled text is escaped.
func TestWriteHTMLAllTypes(t *testing.T) {
	r := &results.Report{
		Schema: 1, ID: "abcdef123456", Profile: "full", StartedAt: time.Now(), EndedAt: time.Now().Add(time.Minute),
		Client:   results.Peer{Name: `<script>alert("x")</script>`},
		Endpoint: results.Peer{Name: "endpoint", Address: "192.0.2.1:7400"},
		Findings: []results.Finding{{ID: "x", Severity: "warning", Title: "T <b>", Detail: "D", Recommendation: "R", Evidence: []string{"s01"}}},
		Summary:  results.Summary{HealthScore: 77, DownMbps: 940, UpMbps: 40, IdleRTTms: 12.3, BloatGrade: "C", ScoreParts: map[string]float64{"loss": 20}, Fingerprint: map[string]string{"Base RTT": "12 ms"}},
	}
	tcp := &results.TCP{Direction: results.Down, Streams: 4, Mbps: 900, Intervals: []results.TCPInterval{{T: 0.1, Mbps: 800, RTTms: 12}, {T: 0.2, Mbps: 900, RTTms: 14}}}
	for i, res := range []any{
		&results.Info{}, &results.Latency{Series: []float64{1, -1, 2}, UDP: stats.Summary{N: 2}}, tcp, &results.UDP{Bins: []results.UDPBin{{T: 0.1, Mbps: 5}}},
		&results.StreamScaling{Points: []results.StreamPoint{{Streams: 1, Mbps: 100}}}, &results.Bufferbloat{Phases: []results.BloatPhase{{Name: "idle", Series: []results.BloatSample{{T: 1, RTTms: 10}}}}},
		&results.Trace{Hops: []results.Hop{{TTL: 1, Addr: "192.0.2.1", MPLS: []string{"16"}}}}, &results.PMTU{}, &results.Integrity{DSCPUp: []results.DSCPResult{{Name: "EF", Received: -1}}},
		&results.VoIP{}, &results.Sweep{Steps: []results.SweepStep{{OfferedMbps: 1, DeliveredMbps: 1}}}, &results.SizeSweep{Points: []results.SizePoint{{Size: 64}}},
		&results.CCCompare{Points: []results.CCPoint{{Algorithm: "bbr"}}}, &results.ShortFlow{}, &results.Discrimination{}, &results.SQMTuner{Steps: []results.SQMStep{{Percent: 90}}},
		&results.QoS{Classes: []results.QoSClass{{Name: "EF"}}},
	} {
		r.Steps = append(r.Steps, results.Step{ID: "s" + string(rune('a'+i)), Test: "x", Title: "step", Status: "ok", Result: res})
	}
	r.Steps = append(r.Steps, results.Step{ID: "failed", Title: "broken", Status: "error", Error: "boom <i>"})
	var b bytes.Buffer
	if err := WriteHTML(&b, r, Options{Org: "Example & Co"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, `<script>alert("x")`) || strings.Contains(out, "T <b>") || strings.Contains(out, "boom <i>") {
		t.Error("user-controlled text must be escaped")
	}
	for _, want := range []string{"No renderer", "<svg", "Example &amp; Co", "boom &lt;i&gt;", "77"} {
		if want == "No renderer" {
			if strings.Contains(out, want) {
				t.Error("a result type has no renderer")
			}
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// Never two y-axes: each chart has one y-axis label.
	if n := strings.Count(out, `rotate(-90)`); n != strings.Count(out, `<svg viewBox`) {
		t.Errorf("charts %d vs y-axis labels %d", strings.Count(out, `<svg viewBox`), n)
	}
	var e bytes.Buffer
	if err := WriteHTML(&e, r, Options{Template: "executive"}); err != nil || strings.Contains(e.String(), "Results</h2>") {
		t.Error("executive template should omit per-step results")
	}
	var txt bytes.Buffer
	if err := WriteText(&txt, r); err != nil || !strings.Contains(txt.String(), "health score 77/100") || !strings.Contains(txt.String(), "Steps that failed") {
		t.Errorf("text summary:\n%s", txt.String())
	}
}

func TestNiceTicks(t *testing.T) {
	ticks := niceTicks(0, 97, 5)
	if ticks[0] != 0 || ticks[len(ticks)-1] < 80 || ticks[len(ticks)-1] > 97 || len(ticks) > 7 {
		t.Errorf("ticks %v", ticks)
	}
	if fmtTick(1500000) != "1.5M" || fmtTick(25000) != "25k" || fmtTick(0.25) != "0.25" || fmtTick(12.5) != "12.5" {
		t.Errorf("fmtTick: %s %s %s %s", fmtTick(1500000), fmtTick(25000), fmtTick(0.25), fmtTick(12.5))
	}
}
