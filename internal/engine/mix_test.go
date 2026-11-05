package engine

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/proto"
	"github.com/agoodley/pathgauge/internal/results"
)

func TestParseMix(t *testing.T) {
	e, scen, err := ParseMix("office")
	if err != nil || scen != "office" || FormatMix(e) != "voice:10,video:3,web:5,download:1,upload:1" {
		t.Fatalf("office: %v %q %q", err, scen, FormatMix(e))
	}
	e, scen, err = ParseMix(" upload , voice:2, video-hd:1, voice:3 ")
	if err != nil || scen != "" || FormatMix(e) != "voice:5,video-hd:1,upload:1" {
		t.Fatalf("list: %v %q", err, FormatMix(e))
	}
	if e, _, err := ParseMix(""); err != nil || FormatMix(e) != MixScenarios[DefaultMix] {
		t.Errorf("empty spec should be the default scenario: %v", err)
	}
	for _, bad := range []string{"telepathy:2", "voice:0", "voice:x", "voice:300", "video:65", ",", "voice:200,gaming:60"} {
		if _, _, err := ParseMix(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	for name, spec := range MixScenarios {
		if _, _, err := ParseMix(spec); err != nil {
			t.Errorf("scenario %s: %v", name, err)
		}
	}
}

func TestCheckMixRate(t *testing.T) {
	e, _, _ := ParseMix("voice:10,video:2,download:1")
	// 10 x 80 kbit/s + 2 x ~1.55 Mbit/s = ~3.9 Mbit/s; transfers don't count.
	if err := checkMixRate(e, 5e6); err != nil {
		t.Errorf("fits in 5 Mbit/s: %v", err)
	}
	if err := checkMixRate(e, 3e6); err == nil || !strings.Contains(err.Error(), "Mbit/s") {
		t.Errorf("should not fit in 3 Mbit/s: %v", err)
	}
	if err := checkMixRate(e, 0); err != nil {
		t.Errorf("no cap: %v", err)
	}
}

func TestTCPSessionVerdicts(t *testing.T) {
	web, _ := lookupMixKind("web")
	st, _ := lookupMixKind("stream")
	cases := []struct {
		k     MixKind
		times []float64
		errs  int
		want  string
	}{
		{web, []float64{800, 900, 1200}, 0, "good"},
		{web, []float64{800, 4000}, 0, "acceptable"},
		{web, []float64{9000}, 0, "poor"},
		{web, []float64{800}, 1, "poor"}, // half the loads failed
		{web, nil, 2, "poor"},
		{st, []float64{900, 1100}, 0, "good"}, // 4 s chunks in ~1 s
		{st, []float64{3000, 3500}, 0, "acceptable"},
		{st, []float64{3000, 5000}, 0, "poor"}, // slower than real time
	}
	for i, c := range cases {
		got := evalTCPSession("x", c.k, &tcpSessionResult{times: c.times, errors: c.errs})
		if got.Verdict != c.want {
			t.Errorf("case %d (%s %v): got %s want %s", i, c.k.Name, c.times, got.Verdict, c.want)
		}
	}
}

func TestWorseOf(t *testing.T) {
	if worseOf("", "good") != "good" || worseOf("good", "poor") != "poor" || worseOf("acceptable", "good") != "acceptable" || worseOf("poor", "") != "poor" {
		t.Error("verdict ordering")
	}
}

// A stream punched from a session's own socket must come back to that
// socket, so each simulated call is a separate flow on the path.
func TestStreamReturnsToSessionSocket(t *testing.T) {
	c := dial(t, startEndpoint(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: c.EndpointIP.AsSlice(), Port: c.Port})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	stream := c.LocalStream()
	b := make([]byte, proto.UDPHeaderLen)
	c.Signer().Encode(b, &proto.UDPHeader{Type: proto.UDPPunch, Session: c.Session(), Stream: stream, T1: time.Now().UnixNano()})
	if _, err := raw.Write(b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := c.Call(ctx, "udp_send", endpoint.UDPSendReq{Stream: stream, RateBps: 50 * 200 * 8, Size: 200, DurationMs: 300, TOS: -1}, nil); err != nil {
		t.Fatal(err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := raw.Read(buf)
	if err != nil {
		t.Fatalf("no packet on the session socket: %v", err)
	}
	h, err := proto.DecodeUDP(buf[:n])
	if err != nil || h.Stream != stream || h.Type != proto.UDPData {
		t.Fatalf("unexpected packet %+v %v", h, err)
	}
}

func TestLoopbackMix(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for ~10 s")
	}
	c := dial(t, startEndpoint(t))
	res, err := RunMix(context.Background(), c, MixParams{Spec: "voice:3,video:1,gaming:1,web:1,stream:1,download:1,upload:1", Phase: 3 * time.Second, DSCP: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Phases) != 2 || res.Phases[0].Name != "interactive" || !res.Phases[1].WithTransfers {
		t.Fatalf("phases: %+v", res.Phases)
	}
	for _, ph := range res.Phases {
		kinds := map[string]results.MixKind{}
		for _, k := range ph.Kinds {
			kinds[k.Kind] = k
		}
		for _, s := range ph.Sessions {
			if s.Error != "" {
				t.Errorf("%s %s: %s", ph.Name, s.ID, s.Error)
			}
		}
		if v := kinds["voice"]; v.Sessions != 3 || v.Value < 4 {
			t.Errorf("%s voice over loopback: %+v", ph.Name, v)
		}
		if v := kinds["video"]; v.Sessions != 1 || v.Value < 95 {
			t.Errorf("%s video over loopback: %+v", ph.Name, v)
		}
		for _, s := range ph.Sessions {
			if s.Kind == "video" && (s.Up == nil || s.Up.Frames == nil || s.Up.Frames.Frames < 60 || s.Down.Frames.Frames < 60) {
				t.Errorf("%s video frames missing: %+v", ph.Name, s)
			}
			if s.Kind == "web" && s.Count == 0 {
				t.Errorf("%s: no pages loaded", ph.Name)
			}
		}
		if _, ok := kinds["download"]; ok != ph.WithTransfers {
			t.Errorf("%s: transfers present=%v", ph.Name, ok)
		}
	}
	if res.Phases[1].Kinds[len(res.Phases[1].Kinds)-1].Value <= 0 {
		t.Errorf("upload throughput missing: %+v", res.Phases[1].Kinds)
	}
}

func TestLoopbackCapacityStopsAtRateCap(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for ~12 s")
	}
	c := dial(t, startEndpoint(t))
	voice, _ := lookupMixKind("voice")
	var steps []int
	res, err := RunCapacity(context.Background(), c, CapacityParams{Kind: "voice", Step: 1500 * time.Millisecond, RateCap: 4.5 * voice.wireBps(),
		OnStep: func(_ string, s results.CapacityStep) { steps = append(steps, s.Sessions) }})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Alone
	if a.Acceptable != 4 || a.Good != 4 || a.Limit != 4 || a.LimitReason != "rate cap" || res.WithTransfers != nil {
		t.Errorf("search: %+v", a)
	}
	if len(steps) != 3 || steps[0] != 1 || steps[1] != 2 || steps[2] != 4 {
		t.Errorf("steps %v, want 1 2 4", steps)
	}
	if _, err := RunCapacity(context.Background(), c, CapacityParams{Kind: "web"}); err == nil {
		t.Error("web is not a call kind")
	}
}

// The dashboard keeps its own copy of the mix catalogue
// (web/src/lib/mix.ts); it must match the engine's.
func TestDashboardMixCatalogueMatches(t *testing.T) {
	src, err := os.ReadFile("../../web/src/lib/mix.ts")
	if err != nil {
		t.Skip("dashboard source not available:", err)
	}
	kindRe := regexp.MustCompile(`\{ name: '([a-z0-9-]+)', label: '([^']+)',.*?cls: '([a-z]+)', max: (\d+)`)
	var got []string
	for _, m := range kindRe.FindAllStringSubmatch(string(src), -1) {
		got = append(got, strings.Join(m[1:], "|"))
	}
	var want []string
	for _, k := range MixKinds {
		want = append(want, fmt.Sprintf("%s|%s|%s|%d", k.Name, k.Label, k.Class, k.Max))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("web/src/lib/mix.ts kinds differ from MixKinds:\n got %v\nwant %v", got, want)
	}
	scenRe := regexp.MustCompile(`\{ id: '([a-z-]+)',.*?spec: '([^']+)' \}`)
	found := map[string]string{}
	for _, m := range scenRe.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = m[2]
	}
	if len(found) != len(MixScenarios) {
		t.Errorf("scenarios: dashboard has %v, engine %v", found, MixScenarios)
	}
	for id, spec := range MixScenarios {
		if found[id] != spec {
			t.Errorf("scenario %s: dashboard %q, engine %q", id, found[id], spec)
		}
	}
	if !strings.Contains(string(src), fmt.Sprintf("MAX_RT_SESSIONS = %d", maxRTSessions)) {
		t.Error("MAX_RT_SESSIONS differs from maxRTSessions")
	}
}

func TestLoopbackCDPBothDirections(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for ~12 s")
	}
	c := dial(t, startEndpoint(t))
	for _, dir := range []results.Direction{results.Up, results.Down} {
		r, err := RunCDP(context.Background(), c, CDPParams{Direction: dir, Settings: results.CDPSettings{RateMbps: 80, BurstS: 1, PeriodS: 2, DurationS: 4}})
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if r.Verdict != "good" || r.SentMB <= 0 || len(r.Points) < 3 || r.Settings.Streams != 4 || r.Settings.RPOs != 15 {
			t.Errorf("%s: %+v", dir, r)
		}
		if r.IdleRTT.N == 0 || r.LoadedRTT.N == 0 {
			t.Errorf("%s: latency probes missing", dir)
		}
	}
}
