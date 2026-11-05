package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/version"
)

// Event reports run progress (for the CLI, the agent and the dashboard).
type Event struct {
	Type    string  `json:"type"` // run_start, step_start, progress, step_done, run_done
	RunID   string  `json:"run_id,omitempty"`
	Step    string  `json:"step,omitempty"`
	Test    string  `json:"test,omitempty"`
	Title   string  `json:"title,omitempty"`
	Index   int     `json:"index,omitempty"`
	Total   int     `json:"total,omitempty"`
	Status  string  `json:"status,omitempty"`
	Error   string  `json:"error,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Data    any     `json:"data,omitempty"`
}

// Options controls a run.
type Options struct {
	Profile string   // quick, standard, full
	Tests   []string // explicit test list overrides the profile
	Skip    []string
	// Scale multiplies every test duration (e.g. 0.5 for a faster run).
	Scale float64
	// MaxRateMbps caps UDP rates and sweeps (0: derive from TCP results,
	// limited by the endpoint's policy).
	MaxRateMbps float64
	TraceCycles int
	Resolve     bool
	Streams     int
	Labels      results.LinkLabels
	Annotate    func(string) (string, string)
	Progress    func(Event)
	// Shaper enables the SQM tuner (local shaping) when non-nil.
	SQM SQMHooks
}

// SQMHooks lets the runner drive local shaping without importing it.
type SQMHooks interface {
	Run(ctx context.Context, c *Client, baselineDown, baselineUp float64, progress func(Event)) (*results.SQMTuner, error)
}

// Profiles lists the built-in profiles and their tests, in run order.
var Profiles = map[string][]string{
	"quick":    {"info", "latency", "tcp-down", "tcp-up", "bufferbloat"},
	"standard": {"info", "latency", "trace", "tcp-down-1", "tcp-up-1", "tcp-down", "tcp-up", "udp-down", "udp-up", "bufferbloat", "pmtu", "integrity", "voip"},
	"full": {"info", "latency", "trace", "tcp-down-1", "tcp-up-1", "tcp-down", "tcp-up", "streams-down", "streams-up", "udp-down", "udp-up",
		"bufferbloat", "sweep-down", "sweep-up", "sizesweep-up", "pmtu", "integrity", "voip", "voip-loaded", "qos", "cc", "shortflow", "discrimination", "mtr"},
}

// AllTests lists every runnable test ID.
var AllTests = []string{"info", "latency", "trace", "mtr", "tcp-down", "tcp-up", "tcp-down-1", "tcp-up-1", "streams-down", "streams-up",
	"udp-down", "udp-up", "bufferbloat", "sweep-down", "sweep-up", "sizesweep-up", "sizesweep-down", "pmtu", "integrity", "voip", "voip-loaded",
	"qos", "cc", "shortflow", "discrimination", "sqm"}

// Titles are human names for test IDs.
var Titles = map[string]string{
	"info": "Environment and clocks", "latency": "Idle latency, jitter and loss", "trace": "Path (traceroute, both directions)",
	"mtr": "Path statistics (MTR, both directions)", "tcp-down": "TCP download, parallel streams", "tcp-up": "TCP upload, parallel streams",
	"tcp-down-1": "TCP download, single stream", "tcp-up-1": "TCP upload, single stream", "streams-down": "Stream scaling, download",
	"streams-up": "Stream scaling, upload", "udp-down": "UDP download (loss and jitter)", "udp-up": "UDP upload (loss and jitter)",
	"bufferbloat": "Bufferbloat (latency under load)", "sweep-down": "Rate sweep, download", "sweep-up": "Rate sweep, upload",
	"sizesweep-up": "Packet-size sweep, upload", "sizesweep-down": "Packet-size sweep, download", "pmtu": "Path MTU, both directions",
	"integrity": "Middlebox checks (DSCP, ECN, MSS, NAT)", "voip": "Voice call simulation", "voip-loaded": "Voice call under load",
	"cc": "Congestion-control comparison", "shortflow": "Short transfers and connection setup", "discrimination": "Throttling and WAN optimiser checks",
	"sqm": "SQM tuner (shaping sweep)", "qos": "QoS priority under load (EF vs best effort)",
}

// NewRunID returns a random run ID.
func NewRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// TestList resolves the tests a run will execute.
func (o Options) TestList() ([]string, error) {
	tests := o.Tests
	if len(tests) == 0 {
		p := o.Profile
		if p == "" {
			p = "standard"
		}
		var ok bool
		if tests, ok = Profiles[p]; !ok {
			return nil, fmt.Errorf("unknown profile %q (want quick, standard or full)", p)
		}
	}
	var out []string
	for _, t := range tests {
		if !slices.Contains(AllTests, t) {
			return nil, fmt.Errorf("unknown test %q (available: %s)", t, strings.Join(AllTests, ", "))
		}
		if !slices.Contains(o.Skip, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// state carries results between steps.
type state struct {
	trustOWD   bool
	tcpMbps    map[results.Direction]float64
	policyMbps float64
	rttMs      float64 // idle RTT from the latency step
}

// tcpDuration lengthens TCP tests on long paths: a TCP flow needs a few
// dozen round trips to leave slow start, so at least 40 RTTs are allowed
// (capped at 30 s). Real-world test UK-Japan (250 ms) showed 5 s tests
// understate single-stream throughput.
func tcpDuration(o Options, st *state, base time.Duration) (d, omit time.Duration) {
	d = scaled(o, base)
	if st.rttMs > 0 {
		need := time.Duration(40 * st.rttMs * float64(time.Millisecond))
		d = max(d, min(need, 30*time.Second))
	}
	omit = max(scaled(o, base/5), time.Duration(8*st.rttMs*float64(time.Millisecond)))
	return d, min(omit, d/3)
}

// Run executes a profile against a connected endpoint.
func Run(ctx context.Context, c *Client, o Options) (*results.Report, error) {
	tests, err := o.TestList()
	if err != nil {
		return nil, err
	}
	if o.Scale <= 0 {
		o.Scale = 1
	}
	if o.Streams <= 0 {
		o.Streams = 8
	}
	if o.Progress == nil {
		o.Progress = func(Event) {}
	}
	rep := &results.Report{
		Schema: results.SchemaVersion, ID: NewRunID(), Software: version.Get().Version, Profile: o.Profile,
		StartedAt: time.Now().UTC(), Labels: o.Labels,
		Client:   results.Peer{Name: o.Labels.Client, LocalAddr: c.Local, ObservedAddr: c.Observed, Software: version.Get().Version},
		Endpoint: results.Peer{Name: o.Labels.Endpoint, Address: c.Addr, Software: c.EndpointSoftware},
	}
	// A profile's own test list (as agents send it) keeps the profile name.
	if rep.Profile == "" {
		rep.Profile = "standard"
	}
	if len(o.Tests) > 0 && !slices.Equal(o.Tests, Profiles[o.Profile]) {
		rep.Profile = "custom"
	}
	st := &state{tcpMbps: map[results.Direction]float64{}}
	o.Progress(Event{Type: "run_start", RunID: rep.ID, Total: len(tests), Data: tests})
	for i, t := range tests {
		if ctx.Err() != nil {
			break
		}
		step := results.Step{ID: fmt.Sprintf("s%02d-%s", i+1, t), Test: testKind(t), Title: Titles[t], StartedAt: time.Now().UTC()}
		o.Progress(Event{Type: "step_start", RunID: rep.ID, Step: step.ID, Test: t, Title: step.Title, Index: i + 1, Total: len(tests)})
		progress := func(data any) {
			o.Progress(Event{Type: "progress", RunID: rep.ID, Step: step.ID, Test: t, Data: data})
		}
		res, err := runOne(ctx, c, t, o, st, rep, progress)
		step.Seconds = math.Round(time.Since(step.StartedAt).Seconds()*10) / 10
		switch {
		case err != nil:
			step.Status, step.Error = "error", err.Error()
		case res == nil:
			step.Status = "skipped"
		default:
			step.Status, step.Result = "ok", res
		}
		// trace/mtr return both directions as two steps.
		if pair, ok := res.([2]*results.Trace); ok {
			for j, tr := range pair {
				s := step
				s.ID = fmt.Sprintf("%s-%s", step.ID, []string{"fwd", "rev"}[j])
				if tr == nil {
					s.Status, s.Result = "error", nil
					s.Title = step.Title + " - " + []string{"outbound", "return"}[j]
					if s.Error == "" {
						s.Error = "no result"
					}
				} else {
					s.Status, s.Error, s.Result = "ok", "", tr
					s.Title = step.Title + " - " + map[results.Direction]string{results.Up: "outbound", results.Down: "return"}[tr.Direction]
				}
				rep.Steps = append(rep.Steps, s)
			}
		} else {
			rep.Steps = append(rep.Steps, step)
		}
		o.Progress(Event{Type: "step_done", RunID: rep.ID, Step: step.ID, Test: t, Title: step.Title, Index: i + 1, Total: len(tests), Status: step.Status, Error: step.Error, Seconds: step.Seconds, Data: step.Result})
		sleepCtx(ctx, 500*time.Millisecond)
	}
	rep.EndedAt = time.Now().UTC()
	if ctx.Err() != nil {
		rep.Notes = append(rep.Notes, "run cancelled before completion")
	}
	return rep, nil
}

// testKind maps test IDs to result types.
func testKind(t string) string {
	switch {
	case strings.HasPrefix(t, "tcp-"):
		return "tcp"
	case strings.HasPrefix(t, "udp-"):
		return "udp"
	case strings.HasPrefix(t, "streams-"):
		return "streams"
	case strings.HasPrefix(t, "sweep-"):
		return "sweep"
	case strings.HasPrefix(t, "sizesweep-"):
		return "sizesweep"
	case strings.HasPrefix(t, "voip"):
		return "voip"
	case t == "mtr":
		return "trace"
	}
	return t
}

func dirOf(t string) results.Direction {
	if strings.Contains(t, "-up") {
		return results.Up
	}
	return results.Down
}

func scaled(o Options, d time.Duration) time.Duration {
	return time.Duration(float64(d) * o.Scale)
}

// rateCap returns the highest rate (bit/s) UDP tests may use.
func rateCap(o Options, st *state, dir results.Direction) float64 {
	limit := math.Inf(1)
	if st.policyMbps > 0 {
		limit = st.policyMbps * 1e6
	}
	if o.MaxRateMbps > 0 {
		limit = math.Min(limit, o.MaxRateMbps*1e6)
	}
	return limit
}

func runOne(ctx context.Context, c *Client, t string, o Options, st *state, rep *results.Report, progress func(any)) (any, error) {
	live := func(dir results.Direction, tt, mbps float64) {
		progress(map[string]any{"kind": "throughput", "direction": dir, "t": tt, "mbps": math.Round(mbps*10) / 10})
	}
	switch t {
	case "info":
		var info endpoint.InfoResp
		if err := c.Call(ctx, "info", nil, &info); err != nil {
			return nil, err
		}
		local := hostinfo.Collect(hostinfo.SystemSources())
		res := &results.Info{ClientHost: &local, EndpointHost: info.Host, ClientClock: endpoint.CurrentClock(), EndpointClock: info.Clock,
			EndpointPolicy: info.Policy, ObservedAddr: c.Observed, ClientLocalAddr: c.Local}
		res.OWDTrustworthy = res.ClientClock.Synced && res.EndpointClock.Synced && res.ClientClock.EstErrorMs+res.EndpointClock.EstErrorMs < 5
		st.trustOWD = res.OWDTrustworthy
		st.policyMbps = float64(info.Policy.MaxRateMbps)
		rep.Client.Host, rep.Endpoint.Host = &local, info.Host
		if info.Host != nil {
			rep.Endpoint.Name = firstNonEmpty(rep.Endpoint.Name, info.Host.Hostname)
		}
		rep.Client.Name = firstNonEmpty(rep.Client.Name, local.Hostname)
		return res, nil
	case "latency":
		r, err := RunLatency(ctx, c, LatencyParams{Duration: scaled(o, 15*time.Second), RateHz: 20, ICMP: true, TCPConnect: true, TrustOWD: st.trustOWD})
		if err == nil && r.UDP.N > 0 {
			st.rttMs = r.UDP.Median
		}
		return r, err
	case "trace", "mtr":
		cycles := o.TraceCycles
		if cycles <= 0 {
			cycles = 3
			if t == "mtr" {
				cycles = 20
			}
		}
		fwd, rev, err := RunTrace(ctx, c, TraceParams{Cycles: cycles, Resolve: o.Resolve, Annotate: o.Annotate})
		if fwd == nil && err != nil {
			return nil, err
		}
		return [2]*results.Trace{fwd, rev}, err
	case "tcp-down", "tcp-up", "tcp-down-1", "tcp-up-1":
		streams := o.Streams
		if strings.HasSuffix(t, "-1") {
			streams = 1
		}
		dir := dirOf(t)
		d, omit := tcpDuration(o, st, 10*time.Second)
		r, err := RunTCP(ctx, c, TCPParams{Direction: dir, Streams: streams, Duration: d, Omit: omit, OnInterval: live})
		if err == nil && r.Mbps > st.tcpMbps[dir] {
			st.tcpMbps[dir] = r.Mbps
		}
		return r, err
	case "streams-down", "streams-up":
		d, _ := tcpDuration(o, st, 5*time.Second)
		return RunStreams(ctx, c, dirOf(t), []int{1, 2, 4, 8, 16}, d, "", live)
	case "udp-down", "udp-up":
		dir := dirOf(t)
		rate := 50e6
		if m := st.tcpMbps[dir]; m > 0 {
			rate = math.Min(rate, m*0.5*1e6)
		}
		rate = math.Max(1e6, math.Min(rate, rateCap(o, st, dir)))
		return RunUDP(ctx, c, UDPParams{Direction: dir, RateBps: rate, Size: 1200, Duration: scaled(o, 10*time.Second), TOS: -1}, st.trustOWD)
	case "bufferbloat":
		phase, _ := tcpDuration(o, st, 10*time.Second)
		streams := 4
		if st.rttMs > 100 {
			streams = 8 // long paths need more flows to fill the link quickly
		}
		return RunBufferbloat(ctx, c, BloatParams{Idle: scaled(o, 5*time.Second), Phase: phase, Streams: streams,
			OnSample: func(ph string, tt, rtt float64) {
				progress(map[string]any{"kind": "latency", "phase": ph, "t": tt, "rtt_ms": math.Round(rtt*100) / 100})
			},
			OnInterval: live})
	case "sweep-down", "sweep-up":
		dir := dirOf(t)
		maxRate := st.tcpMbps[dir] * 1.3 * 1e6
		if maxRate <= 0 {
			maxRate = 100e6
		}
		maxRate = math.Min(maxRate, rateCap(o, st, dir))
		return RunSweep(ctx, c, SweepParams{Direction: dir, MaxBps: maxRate, Steps: 10, StepTime: scaled(o, 3*time.Second), Size: 1200,
			OnStep: func(s results.SweepStep) { progress(map[string]any{"kind": "sweep", "step": s}) }})
	case "sizesweep-up", "sizesweep-down":
		dir := dirOf(t)
		rate := 20e6
		if m := st.tcpMbps[dir]; m > 0 {
			rate = math.Min(m*0.3*1e6, 200e6)
		}
		rate = math.Max(1e6, math.Min(rate, rateCap(o, st, dir)))
		return RunSizeSweep(ctx, c, dir, rate, nil, scaled(o, 2*time.Second))
	case "pmtu":
		return RunPMTU(ctx, c)
	case "integrity":
		return RunIntegrity(ctx, c)
	case "voip":
		return RunVoIP(ctx, c, VoIPParams{Duration: scaled(o, 15*time.Second)})
	case "voip-loaded":
		return RunVoIP(ctx, c, VoIPParams{Duration: scaled(o, 15*time.Second), Load: true})
	case "cc":
		d, _ := tcpDuration(o, st, 8*time.Second)
		return RunCC(ctx, c, LocalCongestionAlgorithms(), d)
	case "shortflow":
		return RunShortFlows(ctx, c, 10)
	case "discrimination":
		d, _ := tcpDuration(o, st, 6*time.Second)
		return RunDiscrimination(ctx, c, results.Down, d)
	case "qos":
		d, _ := tcpDuration(o, st, 10*time.Second)
		return RunQoS(ctx, c, scaled(o, 5*time.Second), d, 4)
	case "sqm":
		if o.SQM == nil {
			return nil, fmt.Errorf("the SQM tuner needs local shaping permission (run with --allow-shaping as root or via the agent service)")
		}
		down, up := st.tcpMbps[results.Down], st.tcpMbps[results.Up]
		return o.SQM.Run(ctx, c, down, up, func(e Event) { progress(e.Data) })
	}
	return nil, fmt.Errorf("test %q not implemented", t)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Concurrency guard for callers sharing a client.
var runMu sync.Mutex

// RunExclusive runs while holding a process-wide lock, so two heavy tests
// never overlap on one host.
func RunExclusive(ctx context.Context, c *Client, o Options) (*results.Report, error) {
	runMu.Lock()
	defer runMu.Unlock()
	return Run(ctx, c, o)
}
