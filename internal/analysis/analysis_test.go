package analysis

import (
	"strings"
	"testing"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

func step(id, test string, r any) results.Step {
	return results.Step{ID: id, Test: test, Status: "ok", Result: r}
}

func ids(r *results.Report) map[string]results.Finding {
	out := map[string]results.Finding{}
	for _, f := range r.Findings {
		out[f.ID] = f
	}
	return out
}

func flat(mbps float64, n int, rtt float64) []results.TCPInterval {
	var iv []results.TCPInterval
	for i := 0; i < n; i++ {
		iv = append(iv, results.TCPInterval{T: float64(i+1) * 0.1, Mbps: mbps, RTTms: rtt})
	}
	return iv
}

func TestHealthyRun(t *testing.T) {
	r := &results.Report{Steps: []results.Step{
		step("l", "latency", &results.Latency{UDP: stats.Summary{N: 100, Median: 10, P99: 11}, UDPJitterMs: 0.3}),
		step("d", "tcp", &results.TCP{Direction: results.Down, Streams: 8, Mbps: 900, Intervals: flat(900, 50, 10)}),
		step("d1", "tcp", &results.TCP{Direction: results.Down, Streams: 1, Mbps: 850, Intervals: flat(850, 50, 10)}),
		step("b", "bufferbloat", &results.Bufferbloat{Grade: "A+", Grades: map[string]string{"down": "A+", "up": "A"}, IncreaseMs: map[string]float64{"down": 2, "up": 8}, RPM: 4000}),
	}}
	Analyze(r, Expectations{})
	f := ids(r)
	if _, ok := f["overall.healthy"]; !ok {
		t.Errorf("expected healthy, got %v", f)
	}
	if r.Summary.HealthScore < 95 || r.Summary.DownMbps != 900 || r.Summary.DownSingleMbps != 850 {
		t.Errorf("summary %+v", r.Summary)
	}
}

func TestBufferbloatSeverity(t *testing.T) {
	r := &results.Report{Steps: []results.Step{
		step("b", "bufferbloat", &results.Bufferbloat{Grade: "F", Grades: map[string]string{"down": "C", "up": "F"}, IncreaseMs: map[string]float64{"down": 150, "up": 600},
			Queueing: "fifo", BufferBytes: map[string]float64{"down": 900000, "up": 700000},
			Phases: []results.BloatPhase{{Name: "idle", RTT: stats.Summary{Median: 20}}, {Name: "down", DownMbps: 50}, {Name: "up", UpMbps: 10}}}),
	}}
	Analyze(r, Expectations{})
	f := ids(r)
	if f["bufferbloat.down"].Severity != "warning" || f["bufferbloat.up"].Severity != "critical" {
		t.Errorf("severities: %+v / %+v", f["bufferbloat.down"], f["bufferbloat.up"])
	}
	if !strings.Contains(f["bufferbloat.up"].Recommendation, "9 Mbit/s") {
		t.Errorf("SQM advice should be ~92%% of 10 Mbit/s: %q", f["bufferbloat.up"].Recommendation)
	}
	if _, ok := f["queue.fifo"]; !ok {
		t.Error("fifo finding missing")
	}
	if r.Summary.ScoreParts["bufferbloat"] != 0 {
		t.Errorf("grade F should zero the bufferbloat score, got %v", r.Summary.ScoreParts["bufferbloat"])
	}
}

func TestPerFlowCauses(t *testing.T) {
	multi := &results.TCP{Direction: results.Down, Streams: 8, Mbps: 800, Intervals: flat(800, 50, 40)}
	cases := []struct {
		name   string
		single *results.TCP
		want   string
	}{
		{"window", &results.TCP{Direction: results.Down, Streams: 1, Mbps: 120, RwndLimitedPct: 80, MinRTTms: 40, RTT: stats.Summary{Median: 40}, Intervals: flat(120, 50, 40)}, "perflow.window.down"},
		{"loss", &results.TCP{Direction: results.Down, Streams: 1, Mbps: 6, RetransPct: 2, MSS: 1448, MinRTTms: 40, RTT: stats.Summary{Median: 40}, Intervals: flat(6, 50, 40)}, "perflow.loss.down"},
		// Retransmits, but far faster than loss allows: bursts lost at a
		// per-flow queue, so the cap is not blamed on loss.
		{"loss-inconsistent", &results.TCP{Direction: results.Down, Streams: 1, Mbps: 963, RetransPct: 0.68, MSS: 1448, MinRTTms: 10, RTT: stats.Summary{Median: 10}, Intervals: flat(963, 50, 10)}, "perflow.policer.down"},
		{"policer", &results.TCP{Direction: results.Down, Streams: 1, Mbps: 100, RTT: stats.Summary{Median: 40}, Intervals: flat(100, 50, 40)}, "perflow.policer.down"},
		{"singlecore", &results.TCP{Direction: results.Down, Streams: 1, Mbps: 9000, RTT: stats.Summary{Median: 0.05}, Intervals: flat(9000, 50, 0.05)}, "host.singlecore.down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := *multi
			switch c.name {
			case "singlecore":
				m.Mbps = 60000
			case "loss-inconsistent":
				m.Mbps = 2047
			}
			r := &results.Report{Steps: []results.Step{step("m", "tcp", &m), step("s", "tcp", c.single)}}
			Analyze(r, Expectations{})
			if _, ok := ids(r)[c.want]; !ok {
				t.Errorf("want %s, got %v", c.want, ids(r))
			}
		})
	}
}

func TestStillRampingIsNotBlamedOnTheNetwork(t *testing.T) {
	var iv []results.TCPInterval
	for i := 0; i < 40; i++ {
		iv = append(iv, results.TCPInterval{T: float64(i) * 0.1, Mbps: float64(i * i)}) // still growing
	}
	r := &results.Report{Steps: []results.Step{
		step("m", "tcp", &results.TCP{Direction: results.Up, Streams: 8, Mbps: 46, Intervals: flat(46, 40, 250)}),
		step("s", "tcp", &results.TCP{Direction: results.Up, Streams: 1, Mbps: 4, DurationS: 5, RTT: stats.Summary{Median: 250}, Intervals: iv}),
	}}
	Analyze(r, Expectations{})
	f := ids(r)
	if _, ok := f["perflow.ramp.up"]; !ok {
		t.Errorf("expected ramp finding, got %v", f)
	}
	for id := range f {
		if strings.HasPrefix(id, "perflow.policer") {
			t.Errorf("a ramping stream must not be called a policer: %v", f)
		}
	}
}

func TestPathLossNeedsEndToEndEvidence(t *testing.T) {
	tr := &results.Trace{Direction: results.Up, LossFromHop: 2, Hops: []results.Hop{{TTL: 1, Received: 10, Sent: 10}, {TTL: 2, Addr: "192.0.2.1", LossPct: 30, Received: 7, Sent: 10}, {TTL: 3, LossPct: 30, Received: 7, Sent: 10}}}
	clean := &results.Latency{UDP: stats.Summary{N: 100, Median: 10}}
	r := &results.Report{Steps: []results.Step{step("l", "latency", clean), step("t", "trace", tr)}}
	Analyze(r, Expectations{})
	if _, ok := ids(r)["path.loss.up"]; ok {
		t.Error("hop loss without end-to-end loss must be treated as ICMP rate limiting")
	}
	tr2 := *tr
	tr2.LossFromHop, tr2.RateLimitedHops = 2, nil
	lossy := &results.Latency{UDP: stats.Summary{N: 100, Median: 10}, Loss: stats.LossStats{Sent: 100, Lost: 5, LossPct: 5}}
	r = &results.Report{Steps: []results.Step{step("l", "latency", lossy), step("t", "trace", &tr2)}}
	Analyze(r, Expectations{})
	if f, ok := ids(r)["path.loss.up"]; !ok || !strings.Contains(f.Detail, "192.0.2.1") {
		t.Errorf("real loss should be located at hop 2: %v", ids(r))
	}
}

func TestMiddleboxFindings(t *testing.T) {
	r := &results.Report{Steps: []results.Step{
		step("p", "pmtu", &results.PMTU{Up: results.PMTUDir{Direction: results.Up, MTU: 1400, LocalMTU: 1500, Blackhole: true}, Down: results.PMTUDir{Direction: results.Down, MTU: 1400, LocalMTU: 1500, PTBSeen: true}}),
		step("i", "integrity", &results.Integrity{MSSClampedUp: true, StrippedOptions: []string{"sack"}, NAT: true,
			DSCPUp: []results.DSCPResult{{Name: "CS0", Status: "preserved"}, {Name: "EF", Sent: 46, Status: "bleached"}}}),
	}}
	Analyze(r, Expectations{})
	f := ids(r)
	for _, want := range []string{"mtu.blackhole.up", "mtu.reduced.down", "mss.clamp", "tcp.options.stripped", "nat.detected", "qos.bleached.up"} {
		if _, ok := f[want]; !ok {
			t.Errorf("missing %s in %v", want, f)
		}
	}
	if f["mtu.blackhole.up"].Severity != "critical" {
		t.Error("black hole must be critical")
	}
	if r.Findings[0].Severity != "critical" {
		t.Error("findings must be sorted most severe first")
	}
}

func TestUDPBurstIsTransient(t *testing.T) {
	r := &results.Report{Steps: []results.Step{
		step("d", "tcp", &results.TCP{Direction: results.Down, Streams: 8, Mbps: 250, Intervals: flat(250, 20, 250)}),
		step("u", "udp", &results.UDP{Direction: results.Down, TargetMbps: 50, Loss: stats.LossStats{Sent: 1000, Lost: 49, LossPct: 4.9, Bursts: 1, MaxBurst: 49}}),
	}}
	Analyze(r, Expectations{})
	f := ids(r)["loss.udp.down"]
	if f.Severity != "warning" || !strings.Contains(f.Detail, "single burst") {
		t.Errorf("single burst should be a warning with context: %+v", f)
	}
}

func TestExpectations(t *testing.T) {
	r := &results.Report{Steps: []results.Step{step("d", "tcp", &results.TCP{Direction: results.Down, Streams: 8, Mbps: 400, Intervals: flat(400, 20, 5)})}}
	Analyze(r, Expectations{DownMbps: 1000})
	if _, ok := ids(r)["sla.down"]; !ok {
		t.Error("SLA finding missing")
	}
}
