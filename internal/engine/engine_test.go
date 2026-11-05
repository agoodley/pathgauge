package engine

import (
	"context"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/config"
	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/results"
)

var testKey = []byte("loopback-test-key")

// startEndpoint runs an endpoint on 127.0.0.1 for the test's lifetime.
func startEndpoint(t *testing.T) string {
	t.Helper()
	pol := config.DefaultPolicy()
	srv := endpoint.New(endpoint.Options{Listen: "127.0.0.1:0", Keys: endpoint.StaticKeys{"psk": testKey}, Policy: pol})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	return srv.Addr().String()
}

func dial(t *testing.T, addr string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, "psk", testKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestWrongKeyRejected(t *testing.T) {
	addr := startEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, addr, "psk", []byte("wrong")); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := Dial(ctx, addr, "nobody", testKey); err == nil {
		t.Fatal("unknown key id accepted")
	}
}

func TestLoopbackLatency(t *testing.T) {
	c := dial(t, startEndpoint(t))
	res, err := RunLatency(context.Background(), c, LatencyParams{Duration: 2 * time.Second, RateHz: 50, TCPConnect: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Loss.Received < 80 || res.Loss.LossPct > 1 {
		t.Errorf("loopback probes: %+v", res.Loss)
	}
	if res.UDP.Median <= 0 || res.UDP.Median > 5 {
		t.Errorf("loopback RTT median %v ms", res.UDP.Median)
	}
	if res.TCPConnect == nil || res.TCPConnect.N < 4 {
		t.Errorf("tcp connect samples: %+v", res.TCPConnect)
	}
	if abs(res.ClockOffsetMs) > 1 {
		t.Errorf("same-host clock offset %v ms", res.ClockOffsetMs)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestLoopbackTCP(t *testing.T) {
	c := dial(t, startEndpoint(t))
	for _, dir := range []results.Direction{results.Up, results.Down} {
		var live int
		res, err := RunTCP(context.Background(), c, TCPParams{
			Direction: dir, Streams: 2, Duration: 2 * time.Second, Omit: 500 * time.Millisecond,
			OnInterval: func(results.Direction, float64, float64) { live++ },
		})
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if res.Mbps < 100 {
			t.Errorf("%s: loopback goodput only %.1f Mbit/s", dir, res.Mbps)
		}
		if len(res.PerStreamMbps) != 2 || res.Fairness < 0.5 {
			t.Errorf("%s: per stream %v fairness %v", dir, res.PerStreamMbps, res.Fairness)
		}
		if len(res.Intervals) < 10 || live < 5 {
			t.Errorf("%s: intervals %d live %d", dir, len(res.Intervals), live)
		}
		if res.RTT.N == 0 || res.MSS == 0 {
			t.Errorf("%s: sender TCP_INFO missing: rtt %+v mss %d", dir, res.RTT, res.MSS)
		}
		t.Logf("%s: %.0f Mbit/s, %d intervals, rtt %.3f ms, retrans %d, cc %s", dir, res.Mbps, len(res.Intervals), res.RTT.Median, res.Retransmits, res.Congestion)
	}
}

func TestLoopbackStreams(t *testing.T) {
	c := dial(t, startEndpoint(t))
	res, err := RunStreams(context.Background(), c, results.Down, []int{1, 4}, time.Second, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 2 || res.BestMbps <= 0 || res.SingleStreamEfficiency <= 0 {
		t.Errorf("got %+v", res)
	}
}
