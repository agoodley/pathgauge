package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

func TestMonitorSample(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	ok := &results.Latency{UDP: stats.Summary{N: 49, Min: 9, Median: 10, P95: 14, Max: 20}, UDPJitterMs: 1.5}
	ok.Loss.Sent, ok.Loss.LossPct = 50, 2
	s := monitorSample(now, ok)
	if s.RTTP95 == nil || *s.RTTP95 != 14 || *s.RTTMin != 9 || *s.RTTP50 != 10 || *s.RTTMax != 20 || *s.Jitter != 1.5 || s.LossPct != 2 || s.Sent != 50 {
		t.Errorf("answered interval: %+v", s)
	}

	// Nothing answered: there is no latency, and the sample must say so
	// rather than report 0 ms.
	dead := &results.Latency{}
	dead.Loss.Sent, dead.Loss.LossPct = 50, 100
	s = monitorSample(now, dead)
	if s.RTTMin != nil || s.RTTP50 != nil || s.RTTP95 != nil || s.RTTMax != nil || s.Jitter != nil {
		t.Errorf("an outage has no latency: %+v", s)
	}
	if s.LossPct != 100 || s.Sent != 50 || s.Answered() {
		t.Errorf("an outage is 100%% loss: %+v", s)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"rtt_p95":null`) {
		t.Errorf("want null latency on the wire, got %s", b)
	}

	// One answer in very many is still an answer.
	one := &results.Latency{UDP: stats.Summary{N: 1, Min: 9, Median: 9, P95: 9, Max: 9}}
	one.Loss.Sent, one.Loss.LossPct = 1_000_000, 99.9999
	if s = monitorSample(now, one); !s.Answered() || s.RTTP95 == nil {
		t.Errorf("an interval with an answer must keep its latency: %+v", s)
	}
}
