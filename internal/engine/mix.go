package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/endpoint"
	"github.com/agoodley/pathgauge/internal/flow"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// MixKind describes one kind of simulated application session. The models
// copy the timing and packet sizes of the real applications; they are not
// the real protocols, so equipment that classifies traffic by inspecting
// packets will not recognise them.
type MixKind struct {
	Name  string
	Label string
	// Class: "rt" (constant-rate UDP both ways), "video" (video frames both
	// ways), "web" (page loads), "stream" (video-on-demand chunks) or
	// "bulk" (a TCP transfer that runs for the whole phase).
	Class string
	PPS   float64 // rt: packets per second each way
	Size  int     // rt: UDP payload bytes
	TOS   int     // marking when DSCP is on (DSCP << 2); -1 for none
	Video flow.VideoModel
	// Streaming: bitrate and chunk length.
	BitrateBps float64
	ChunkS     float64
	Dir        results.Direction // bulk
	Max        int               // most sessions of this kind in one mix
}

// MixKinds lists the session kinds in display order.
var MixKinds = []MixKind{
	{Name: "voice", Label: "Voice call (G.711)", Class: "rt", PPS: 50, Size: 172, TOS: 46 << 2, Max: 256},
	{Name: "voice-opus", Label: "Voice call (Opus)", Class: "rt", PPS: 50, Size: 72, TOS: 46 << 2, Max: 256},
	{Name: "video", Label: "Video call (720p, 1.5 Mbit/s)", Class: "video", Video: flow.VideoModel{BitrateBps: 1.5e6}, TOS: 34 << 2, Max: 64},
	{Name: "video-hd", Label: "Video call (1080p, 3.8 Mbit/s)", Class: "video", Video: flow.VideoModel{BitrateBps: 3.8e6}, TOS: 34 << 2, Max: 64},
	{Name: "gaming", Label: "Online game", Class: "rt", PPS: 60, Size: 120, TOS: -1, Max: 64},
	{Name: "web", Label: "Web browsing", Class: "web", TOS: -1, Max: 32},
	{Name: "stream", Label: "Video streaming (HD, 5 Mbit/s)", Class: "stream", BitrateBps: 5e6, ChunkS: 4, TOS: -1, Max: 16},
	{Name: "stream-4k", Label: "Video streaming (4K, 16 Mbit/s)", Class: "stream", BitrateBps: 16e6, ChunkS: 4, TOS: -1, Max: 16},
	{Name: "download", Label: "File download", Class: "bulk", Dir: results.Down, TOS: -1, Max: 16},
	{Name: "upload", Label: "File upload", Class: "bulk", Dir: results.Up, TOS: -1, Max: 16},
}

// MixScenarios are named mixes.
var MixScenarios = map[string]string{
	"office":       "voice:10,video:3,web:5,download:1,upload:1",
	"home-working": "video:1,web:1,stream:1,upload:1",
	"call-centre":  "voice:30,web:5,download:1",
	"family":       "video:1,stream:2,gaming:1,web:2,download:1",
}

// DefaultMix is the scenario used when none is given.
const DefaultMix = "office"

// maxRTSessions bounds the UDP sessions (one socket each) in a mix.
const maxRTSessions = 256

// Web page model: one 80 KB document, then six 320 KB objects fetched in
// parallel over new connections (about 2 MB, a typical page weight).
const (
	webDocBytes    = 80 << 10
	webObjBytes    = 320 << 10
	webObjects     = 6
	webThinkTime   = 5 * time.Second
	voiceJitterBuf = 60 // ms: fixed buffer, only for endpoints without packet logs
)

func lookupMixKind(name string) (MixKind, bool) {
	for _, k := range MixKinds {
		if k.Name == name {
			return k, true
		}
	}
	return MixKind{}, false
}

// wireBps is a real-time session's rate in one direction, including IP and
// UDP headers.
func (k MixKind) wireBps() float64 {
	switch k.Class {
	case "rt":
		return k.PPS * float64(k.Size+28) * 8
	case "video":
		return k.Video.BitrateBps * 1.03
	}
	return 0
}

// realtime reports whether the kind is a UDP session.
func (k MixKind) realtime() bool { return k.Class == "rt" || k.Class == "video" }

// MixEntry is a session kind and how many to run.
type MixEntry struct {
	Kind  MixKind
	Count int
}

// ParseMix parses a scenario name ("office") or a list such as
// "voice:10,video:2,download:1". It returns the entries in display order.
func ParseMix(spec string) ([]MixEntry, string, error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" {
		spec = DefaultMix
	}
	scenario := ""
	if s, ok := MixScenarios[spec]; ok {
		scenario, spec = spec, s
	}
	counts := map[string]int{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, n, hasN := strings.Cut(part, ":")
		count := 1
		if hasN {
			v, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil || v < 1 {
				return nil, "", fmt.Errorf("mix: bad count in %q", part)
			}
			count = v
		}
		name = strings.TrimSpace(name)
		if _, ok := lookupMixKind(name); !ok {
			return nil, "", fmt.Errorf("mix: unknown session kind %q (use a scenario: %s; or kinds: %s)", name, strings.Join(mixScenarioNames(), ", "), strings.Join(mixKindNames(), ", "))
		}
		counts[name] += count
	}
	var out []MixEntry
	rt := 0
	for _, k := range MixKinds {
		n := counts[k.Name]
		if n == 0 {
			continue
		}
		if n > k.Max {
			return nil, "", fmt.Errorf("mix: at most %d %s sessions", k.Max, k.Name)
		}
		if k.realtime() {
			rt += n
		}
		out = append(out, MixEntry{Kind: k, Count: n})
	}
	if len(out) == 0 {
		return nil, "", errors.New("mix: no sessions")
	}
	if rt > maxRTSessions {
		return nil, "", fmt.Errorf("mix: at most %d call and game sessions in total", maxRTSessions)
	}
	return out, scenario, nil
}

func mixKindNames() []string {
	var n []string
	for _, k := range MixKinds {
		n = append(n, k.Name)
	}
	return n
}

func mixScenarioNames() []string {
	var n []string
	for k := range MixScenarios {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// FormatMix renders entries back into the "kind:count" form.
func FormatMix(entries []MixEntry) string {
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s:%d", e.Kind.Name, e.Count))
	}
	return strings.Join(parts, ",")
}

// MixParams controls a traffic-mix run.
type MixParams struct {
	Spec    string
	Phase   time.Duration // length of each phase
	RateCap float64       // bit/s per direction available to real-time sessions (0: no cap)
	DSCP    bool          // mark calls as applications do (EF voice, AF41 video)
	// OnRTT receives latency-probe samples for live progress.
	OnRTT func(phase string, t, rttMs float64)
}

// mixBaseline is the idle path, measured before the mix starts.
type mixBaseline struct {
	rttMs   float64 // minimum round-trip time
	upOWD   float64 // minimum raw one-way delay (includes any clock offset), ms
	downOWD float64
}

func (b mixBaseline) oneWay() float64 { return b.rttMs / 2 }

// RunMix runs a traffic mix.
func RunMix(ctx context.Context, c *Client, mp MixParams) (*results.Mix, error) {
	entries, scenario, err := ParseMix(mp.Spec)
	if err != nil {
		return nil, err
	}
	if mp.Phase <= 0 {
		mp.Phase = 30 * time.Second
	}
	if err := checkMixRate(entries, mp.RateCap); err != nil {
		return nil, err
	}
	res := &results.Mix{Spec: FormatMix(entries), Scenario: scenario, DSCP: mp.DSCP}
	var interactive, bulk []MixEntry
	for _, e := range entries {
		res.Counts = append(res.Counts, results.MixCount{Kind: e.Kind.Name, Label: e.Kind.Label, Count: e.Count})
		if e.Kind.Class == "bulk" {
			bulk = append(bulk, e)
		} else {
			interactive = append(interactive, e)
		}
	}
	base, err := mixCalibrate(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("measuring the idle path: %w", err)
	}
	res.BaseRTTms = stats.Round(base.rttMs, 2)

	type phaseDef struct {
		name, label string
		entries     []MixEntry
		withBulk    bool
	}
	var phases []phaseDef
	switch {
	case len(bulk) > 0 && len(interactive) > 0:
		phases = []phaseDef{
			{"interactive", "Without transfers", interactive, false},
			{"with-transfers", "With file transfers", entries, true},
		}
	default:
		phases = []phaseDef{{"all", "All sessions", entries, len(bulk) > 0}}
	}
	for i, ph := range phases {
		if i > 0 {
			sleepCtx(ctx, 2*time.Second) // let queues drain between phases
		}
		p, err := runMixPhase(ctx, c, ph.entries, mp.Phase, base, mp, ph.name)
		if err != nil {
			return nil, err
		}
		p.Name, p.Label, p.WithTransfers = ph.name, ph.label, ph.withBulk
		res.Phases = append(res.Phases, p)
		if ctx.Err() != nil {
			break
		}
	}
	for _, p := range res.Phases {
		res.Verdict = worseOf(res.Verdict, p.Verdict)
	}
	for _, e := range entries {
		if e.Kind.Class == "video" {
			res.Notes = append(res.Notes, "Video calls use a fixed bitrate. Real applications lower their bitrate when the network is congested, so these results are the worst case for the chosen quality.")
			break
		}
	}
	if !mp.DSCP {
		res.Notes = append(res.Notes, "Calls were sent without DSCP markings.")
	}
	return res, nil
}

func checkMixRate(entries []MixEntry, rateCap float64) error {
	var rt float64
	for _, e := range entries {
		rt += e.Kind.wireBps() * float64(e.Count)
	}
	if rateCap > 0 && rt > rateCap {
		return fmt.Errorf("mix: the calls and games need %.1f Mbit/s each way, more than the %.1f Mbit/s this run may use", rt/1e6, rateCap/1e6)
	}
	return nil
}

// mixCalibrate measures the idle path: minimum RTT, and the minimum raw
// one-way delay each way, against which queueing delay is measured.
func mixCalibrate(ctx context.Context, c *Client) (mixBaseline, error) {
	k := MixKind{Name: "calibration", Class: "rt", PPS: 50, Size: 100, TOS: -1}
	s := &rtSession{id: "calibration", kind: k}
	if err := s.setup(ctx, c, false, false); err != nil {
		return mixBaseline{}, err
	}
	defer s.cleanup(c)
	p := newProber(c, 0)
	defer p.close()
	d := 3 * time.Second
	pctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.run(pctx, 20) }()
	go func() { defer wg.Done(); s.run(ctx, c, d, 0, -1) }()
	wg.Wait()
	sleepCtx(ctx, 300*time.Millisecond)
	if err := s.collect(ctx, c); err != nil {
		return mixBaseline{}, err
	}
	if s.err != nil {
		return mixBaseline{}, s.err
	}
	b := mixBaseline{rttMs: math.Inf(1), upOWD: math.NaN(), downOWD: math.NaN()}
	for _, ps := range p.samples() {
		if ps.got {
			b.rttMs = math.Min(b.rttMs, ps.rttMs)
		}
	}
	if math.IsInf(b.rttMs, 1) {
		return mixBaseline{}, errors.New("no replies to latency probes")
	}
	if s.up.OWDRaw.N > 0 {
		b.upOWD = s.up.OWDRaw.Min
	}
	if s.down.OWDRaw.N > 0 {
		b.downOWD = s.down.OWDRaw.Min
	}
	return b, nil
}

// rtSession is one simulated real-time session: a UDP stream each way on
// its own client socket.
type rtSession struct {
	id         string
	kind       MixKind
	sock       *UDPSock // nil: the client's main socket
	upStream   uint32   // endpoint sink
	downStream uint32   // local sink
	downSink   *flow.UDPSink
	upSent     flow.UDPSendResult
	downSent   flow.UDPSendResult
	up, down   flow.SinkResult
	err        error
	mu         sync.Mutex
}

func (s *rtSession) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// setup registers both streams. ownSocket gives the session its own source
// port; logPackets keeps per-packet logs (video).
func (s *rtSession) setup(ctx context.Context, c *Client, ownSocket, logPackets bool) error {
	var ready endpoint.UDPSinkReady
	if err := c.Call(ctx, "udp_sink", endpoint.UDPSinkReq{Log: logPackets}, &ready); err != nil {
		return err
	}
	s.upStream = ready.Stream
	s.downStream = c.LocalStream()
	s.downSink = flow.NewUDPSink(0)
	if logPackets {
		s.downSink.EnableLog()
	}
	c.AddSink(s.downStream, s.downSink)
	if ownSocket {
		sock, err := c.OpenUDP()
		if err != nil {
			return err
		}
		s.sock = sock
		sock.Punch(s.downStream)
	} else {
		c.punch()
	}
	return nil
}

func (s *rtSession) cleanup(c *Client) {
	if s.downSink != nil {
		c.AddSink(s.downStream, nil)
	}
	if s.sock != nil {
		s.sock.Close()
	}
}

// run sends both directions for d, starting after offset.
func (s *rtSession) run(ctx context.Context, c *Client, d, offset time.Duration, tos int) {
	if offset > 0 {
		sleepCtx(ctx, offset)
	}
	send := c.SendUDP
	if s.sock != nil {
		send = s.sock.Send
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p := flow.UDPSendParams{Session: c.Session(), Stream: s.upStream, Duration: d, TOS: tos}
		if s.kind.Class == "video" {
			s.upSent = flow.SendVideo(ctx, c.Signer(), p, s.kind.Video, send)
			return
		}
		p.RateBps, p.Size = s.kind.PPS*float64(s.kind.Size)*8, s.kind.Size
		s.upSent = flow.SendUDP(ctx, c.Signer(), p, send)
	}()
	go func() {
		defer wg.Done()
		req := endpoint.UDPSendReq{Stream: s.downStream, DurationMs: int(d / time.Millisecond), TOS: tos}
		if s.kind.Class == "video" {
			v := s.kind.Video
			req.Video = &v
		} else {
			req.RateBps, req.Size = s.kind.PPS*float64(s.kind.Size)*8, s.kind.Size
		}
		var started map[string]any
		if err := c.Call(ctx, "udp_send", req, &started); err != nil {
			s.fail(err)
			return
		}
		if s.kind.Class == "video" && started["pattern"] != "video" {
			s.fail(errors.New("the endpoint's PathGauge is too old for video sessions; upgrade it"))
		}
		var sent flow.UDPSendResult
		if err := c.Call(ctx, "udp_send_result", endpoint.UDPSendResultReq{Stream: s.downStream}, &sent); err != nil {
			s.fail(err)
			return
		}
		s.downSent = sent
	}()
	wg.Wait()
}

// collect fetches the endpoint's view of the upstream and closes the
// downstream sink.
func (s *rtSession) collect(ctx context.Context, c *Client) error {
	var up flow.SinkResult
	err := c.Call(ctx, "udp_sink_result", endpoint.UDPSinkResultReq{Stream: s.upStream, Sent: s.upSent.Sent, Close: true}, &up)
	s.up = up
	s.down = s.downSink.Result(s.downSent.Sent)
	return err
}

// parallel runs fn for each index with at most n running at once.
func parallel(count, n int, fn func(i int)) {
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// tcpSessionResult is what a web or streaming session measured.
type tcpSessionResult struct {
	times  []float64 // ms
	errors int
}

// runMixPhase runs one phase of a mix and evaluates every session.
func runMixPhase(ctx context.Context, c *Client, entries []MixEntry, dur time.Duration, base mixBaseline, mp MixParams, name string) (results.MixPhase, error) {
	ph := results.MixPhase{DurationS: dur.Seconds()}

	// Real-time sessions, each on its own socket.
	var rts []*rtSession
	for _, e := range entries {
		if !e.Kind.realtime() {
			continue
		}
		for i := 1; i <= e.Count; i++ {
			rts = append(rts, &rtSession{id: fmt.Sprintf("%s-%d", e.Kind.Name, i), kind: e.Kind})
		}
	}
	defer func() {
		for _, s := range rts {
			s.cleanup(c)
		}
	}()
	var setupErr error
	var emu sync.Mutex
	parallel(len(rts), 8, func(i int) {
		// Calls keep per-packet logs so their jitter buffers can be simulated.
		logs := rts[i].kind.Class == "video" || isVoice(rts[i].kind)
		if err := rts[i].setup(ctx, c, true, logs); err != nil {
			emu.Lock()
			if setupErr == nil {
				setupErr = fmt.Errorf("setting up %s: %w", rts[i].id, err)
			}
			emu.Unlock()
		}
	})
	if setupErr != nil {
		return ph, setupErr
	}
	sleepCtx(ctx, 100*time.Millisecond) // let the punches arrive

	start := time.Now()
	until := start.Add(dur)
	var wg sync.WaitGroup

	// Latency probes on the main socket, for the overall picture.
	p := newProber(c, 0)
	defer p.close()
	if mp.OnRTT != nil {
		p.onRTT = func(s *probeSample) { mp.OnRTT(name, time.Since(start).Seconds(), s.rttMs) }
	}
	pctx, pcancel := context.WithDeadline(ctx, until)
	defer pcancel()
	wg.Add(1)
	go func() { defer wg.Done(); p.run(pctx, 10) }()

	for _, s := range rts {
		tos := -1
		if mp.DSCP {
			tos = s.kind.TOS
		}
		// Start sessions at random points within one packet or frame
		// interval, as independent calls would.
		var spread time.Duration
		switch s.kind.Class {
		case "video":
			spread = s.kind.Video.FrameInterval()
		default:
			spread = time.Duration(float64(time.Second) / s.kind.PPS)
		}
		offset := time.Duration(rand.Int64N(int64(spread)))
		wg.Add(1)
		go func(s *rtSession) { defer wg.Done(); s.run(ctx, c, dur, offset, tos) }(s)
	}

	// Web and streaming sessions stop starting requests at the end of the
	// phase; requests in flight get a short grace period.
	tctx, tcancel := context.WithDeadline(ctx, until.Add(10*time.Second))
	defer tcancel()
	tcpRes := map[string]*tcpSessionResult{}
	var tmu sync.Mutex
	for _, e := range entries {
		if e.Kind.Class != "web" && e.Kind.Class != "stream" {
			continue
		}
		for i := 1; i <= e.Count; i++ {
			id := fmt.Sprintf("%s-%d", e.Kind.Name, i)
			r := &tcpSessionResult{}
			tmu.Lock()
			tcpRes[id] = r
			tmu.Unlock()
			k := e.Kind
			// Spread the first requests so sessions don't start in step.
			offset := time.Duration(rand.Int64N(int64(2 * time.Second)))
			wg.Add(1)
			go func() {
				defer wg.Done()
				sleepCtx(tctx, offset)
				if k.Class == "web" {
					runWebUser(tctx, c, until, r)
				} else {
					runStreamViewer(tctx, c, k, until, r)
				}
			}()
		}
	}

	// Bulk transfers: one TCP test per direction, one stream per transfer.
	bulk := map[results.Direction]*results.TCP{}
	bulkErr := map[results.Direction]error{}
	var bmu sync.Mutex
	for _, e := range entries {
		if e.Kind.Class != "bulk" {
			continue
		}
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := RunTCP(ctx, c, TCPParams{Direction: e.Kind.Dir, Streams: e.Count, Duration: dur})
			bmu.Lock()
			bulk[e.Kind.Dir], bulkErr[e.Kind.Dir] = r, err
			bmu.Unlock()
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ph, ctx.Err()
	}
	// The other sessions' results are "with transfers" only if the
	// transfers ran; without them they were measured on a quieter link.
	for _, e := range entries {
		if e.Kind.Class != "bulk" {
			continue
		}
		if err := loadErr("the "+strings.ToLower(e.Kind.Label), bulk[e.Kind.Dir], bulkErr[e.Kind.Dir]); err != nil {
			return ph, fmt.Errorf("%w, so the other sessions were not measured alongside it", err)
		}
	}

	// Collect the endpoint's side of every real-time session.
	sleepCtx(ctx, 300*time.Millisecond)
	parallel(len(rts), 8, func(i int) {
		if err := rts[i].collect(ctx, c); err != nil {
			rts[i].fail(err)
		}
	})

	rtts := []float64{}
	for _, s := range p.samples() {
		if s.got {
			rtts = append(rtts, s.rttMs)
		}
	}
	ph.ProbeRTT = roundSum(stats.Summarize(rtts))

	// Evaluate sessions, in mix order.
	byKind := map[string][]results.MixSession{}
	ri := 0
	for _, e := range entries {
		k := e.Kind
		switch k.Class {
		case "rt", "video":
			for i := 0; i < e.Count; i++ {
				s := rts[ri]
				ri++
				byKind[k.Name] = append(byKind[k.Name], evalRT(s, base, dur))
			}
		case "web", "stream":
			for i := 1; i <= e.Count; i++ {
				id := fmt.Sprintf("%s-%d", k.Name, i)
				byKind[k.Name] = append(byKind[k.Name], evalTCPSession(id, k, tcpRes[id]))
			}
		case "bulk":
			r, err := bulk[k.Dir], bulkErr[k.Dir]
			for i := 0; i < e.Count; i++ {
				ms := results.MixSession{ID: fmt.Sprintf("%s-%d", k.Name, i+1), Kind: k.Name}
				switch {
				case err != nil:
					ms.Error = err.Error()
				case r != nil && i < len(r.PerStreamMbps):
					ms.Mbps = stats.Round(r.PerStreamMbps[i], 2)
				}
				byKind[k.Name] = append(byKind[k.Name], ms)
			}
		}
	}
	for _, e := range entries {
		sess := byKind[e.Kind.Name]
		ph.Sessions = append(ph.Sessions, sess...)
		kind := summarizeMixKind(e.Kind, sess, bulk[e.Kind.Dir])
		ph.Kinds = append(ph.Kinds, kind)
		ph.Verdict = worseOf(ph.Verdict, kind.Verdict)
	}
	return ph, nil
}

// runWebUser loads a page, waits, and repeats until the phase ends.
func runWebUser(ctx context.Context, c *Client, until time.Time, r *tcpSessionResult) {
	for ctx.Err() == nil && time.Now().Before(until) {
		start := time.Now()
		err := fetchParallel(ctx, c, 1, webDocBytes)
		if err == nil {
			err = fetchParallel(ctx, c, webObjects, webObjBytes)
		}
		if err != nil {
			if ctx.Err() == nil || time.Since(start) > webThinkTime {
				r.errors++
			}
		} else {
			r.times = append(r.times, float64(time.Since(start).Microseconds())/1000)
		}
		next := start.Add(webThinkTime)
		if !next.Before(until) {
			return
		}
		sleepCtx(ctx, time.Until(next))
	}
}

// runStreamViewer downloads one chunk per chunk length until the phase ends.
func runStreamViewer(ctx context.Context, c *Client, k MixKind, until time.Time, r *tcpSessionResult) {
	chunk := uint64(k.BitrateBps * k.ChunkS / 8)
	every := time.Duration(k.ChunkS * float64(time.Second))
	for ctx.Err() == nil && time.Now().Before(until) {
		start := time.Now()
		if err := fetchParallel(ctx, c, 1, chunk); err != nil {
			if ctx.Err() == nil || time.Since(start) > every {
				r.errors++
			}
		} else {
			r.times = append(r.times, float64(time.Since(start).Microseconds())/1000)
		}
		next := start.Add(every)
		if !next.Before(until) {
			return
		}
		sleepCtx(ctx, time.Until(next))
	}
}

// fetchParallel downloads n objects of size bytes over n new connections.
func fetchParallel(ctx context.Context, c *Client, n int, size uint64) error {
	var prep endpoint.TCPPrepared
	if err := c.Call(ctx, "tcp_prepare", endpoint.TCPPrepare{Role: "send", Streams: n, DurationMs: 60000, Bytes: size}, &prep); err != nil {
		return err
	}
	defer func() {
		// Release the endpoint's state without delaying the caller.
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = c.Call(cctx, "tcp_result", map[string]any{"test_id": prep.TestID, "wait_ms": 5000}, nil)
		}()
	}()
	errs := make(chan error, len(prep.Streams))
	for _, id := range prep.Streams {
		go func(id uint32) {
			conn, err := c.DialData(ctx, id, DialOptions{})
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			got, err := io.Copy(io.Discard, conn)
			if err == nil && uint64(got) < size {
				err = fmt.Errorf("transfer ended after %d of %d bytes", got, size)
			}
			errs <- err
		}(id)
	}
	var first error
	for range prep.Streams {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// evalRT turns a real-time session's measurements into a result.
func evalRT(s *rtSession, base mixBaseline, dur time.Duration) results.MixSession {
	ms := results.MixSession{ID: s.id, Kind: s.kind.Name}
	if s.err != nil {
		ms.Error, ms.Verdict = s.err.Error(), "poor"
		return ms
	}
	up := evalRTFlow(s.kind, s.up, s.upSent.Sent, base.upOWD, base.oneWay(), dur)
	down := evalRTFlow(s.kind, s.down, s.downSent.Sent, base.downOWD, base.oneWay(), dur)
	ms.Up, ms.Down = &up, &down
	ms.Verdict = worseOf(up.Verdict, down.Verdict)
	if s.kind.Class == "video" && (s.up.Packets == nil || s.down.Packets == nil) {
		// Without packet logs frames can't be rebuilt.
		ms.Error, ms.Verdict = "no packet log (the endpoint's PathGauge is too old for video sessions)", "poor"
	}
	return ms
}

// evalRTFlow evaluates one direction. baseOWD is the idle path's minimum
// raw one-way delay in this direction (NaN if unknown).
func evalRTFlow(k MixKind, s flow.SinkResult, sent uint64, baseOWD, baseOneWay float64, dur time.Duration) results.MixFlow {
	f := results.MixFlow{LossPct: stats.Round(s.Loss.LossPct, 2), JitterMs: stats.Round(s.JitterMs, 2)}
	if s.OWDRaw.N == 0 {
		f.LossPct, f.Verdict = 100, "poor"
		return f
	}
	ref := s.OWDRaw.Min
	if !math.IsNaN(baseOWD) {
		// Use the idle baseline, unless this flow saw a lower delay (clock
		// drift since the baseline was taken).
		ref = math.Min(baseOWD, s.OWDRaw.Min)
	}
	f.AddedP50Ms = stats.Round(math.Max(0, s.OWDRaw.Median-ref), 2)
	f.AddedP95Ms = stats.Round(math.Max(0, s.OWDRaw.P95-ref), 2)
	f.DelayMs = stats.Round(baseOneWay+f.AddedP50Ms, 2)
	switch {
	case isVoice(k):
		// Mouth-to-ear delay: the idle path, plus what queues and the
		// adaptive jitter buffer add, plus ~20 ms of codec and packetisation.
		var late, playout float64
		if s.Packets != nil {
			po := flow.VoicePlayout.PlayoutLog(s.Packets)
			// Received-packet share to share of all packets sent.
			late = po.LatePct * (100 - s.Loss.LossPct) / 100
			playout = math.Max(0, po.MeanTarget-ref)
		} else {
			late = s.LatePct[voiceJitterBuf]
			playout = f.AddedP50Ms + voiceJitterBuf/2
		}
		mos, r := stats.MOS(baseOneWay+playout+20, s.JitterMs, s.Loss.LossPct+late)
		f.LatePct, f.PlayoutMs, f.MOS, f.R = stats.Round(late, 2), stats.Round(playout, 1), stats.Round(mos, 2), stats.Round(r, 1)
		f.Verdict = mosVerdict(mos)
	case k.Class == "video":
		fs := flow.AnalyzeFrames(k.Video, s.Packets, sent, ref, 2*baseOneWay)
		f.Frames = &results.MixFrames{Frames: fs.Frames, OKPct: stats.Round(fs.OKPct, 2), LatePct: stats.Round(fs.LatePct, 2), LostPct: stats.Round(fs.LostPct, 2), RepairedPct: stats.Round(fs.RepairedPct, 2),
			Freezes: fs.Freezes, FreezeS: stats.Round(fs.FreezeS, 2), DelayP50Ms: stats.Round(fs.DelayP50Ms, 2), DelayP95Ms: stats.Round(fs.DelayP95Ms, 2), PlayoutMs: stats.Round(fs.PlayoutMs, 1)}
		f.Verdict = videoVerdict(fs, dur)
	default: // gaming
		f.Verdict = gamingVerdict(f.AddedP95Ms, f.LossPct)
	}
	return f
}

// videoVerdict grades a video direction: frames in time for playout, no
// visible freezes, and added latency (queueing plus buffering) within what
// conversation tolerates (ITU-T G.114: under 150 ms ideal, 400 ms limit).
func videoVerdict(fs flow.FrameStats, dur time.Duration) string {
	switch {
	case fs.Frames == 0:
		return "poor"
	case fs.OKPct >= 99 && fs.Freezes == 0 && fs.PlayoutMs <= 150:
		return "good"
	case fs.OKPct >= 95 && fs.FreezeS <= 0.02*dur.Seconds() && fs.PlayoutMs <= 300:
		return "acceptable"
	}
	return "poor"
}

func gamingVerdict(addedP95, lossPct float64) string {
	switch {
	case addedP95 <= 30 && lossPct <= 0.5:
		return "good"
	case addedP95 <= 75 && lossPct <= 2:
		return "acceptable"
	}
	return "poor"
}

// evalTCPSession evaluates a web or streaming session.
func evalTCPSession(id string, k MixKind, r *tcpSessionResult) results.MixSession {
	ms := results.MixSession{ID: id, Kind: k.Name, Count: len(r.times), Errors: r.errors}
	if len(r.times) == 0 {
		ms.Verdict = "poor"
		ms.Error = "no request completed"
		return ms
	}
	sum := roundSum(stats.Summarize(r.times))
	ms.Times = &sum
	failed := float64(r.errors) / float64(r.errors+len(r.times))
	switch k.Class {
	case "web":
		switch {
		case sum.P95 <= 3000 && failed == 0:
			ms.Verdict = "good"
		case sum.P95 <= 6000 && failed <= 0.1:
			ms.Verdict = "acceptable"
		default:
			ms.Verdict = "poor"
		}
	case "stream":
		ms.TargetMs = k.ChunkS * 1000
		ratio := sum.P95 / ms.TargetMs
		switch {
		case ratio <= 0.5 && failed == 0:
			ms.Verdict = "good"
		case ratio <= 1 && failed <= 0.1:
			ms.Verdict = "acceptable"
		default:
			ms.Verdict = "poor"
		}
	}
	return ms
}

// summarizeMixKind summarises the sessions of one kind.
func summarizeMixKind(k MixKind, sess []results.MixSession, bulk *results.TCP) results.MixKind {
	out := results.MixKind{Kind: k.Name, Label: k.Label, Sessions: len(sess)}
	for _, s := range sess {
		switch s.Verdict {
		case "good":
			out.Good++
		case "acceptable":
			out.Acceptable++
		case "poor":
			out.Poor++
		}
		if k.Class != "bulk" {
			out.Verdict = worseOf(out.Verdict, s.Verdict)
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	flows := func(s results.MixSession) []*results.MixFlow {
		var f []*results.MixFlow
		if s.Up != nil {
			f = append(f, s.Up)
		}
		if s.Down != nil {
			f = append(f, s.Down)
		}
		return f
	}
	switch {
	case isVoice(k):
		worst, mos := 5.0, []float64{}
		for _, s := range sess {
			for _, f := range flows(s) {
				worst = math.Min(worst, f.MOS)
				mos = append(mos, f.MOS)
			}
			if s.Error != "" {
				worst = math.Min(worst, 1)
			}
		}
		out.Metric, out.Value, out.Unit = "worst call quality (MOS)", stats.Round(worst, 2), ""
		out.Summary = fmt.Sprintf("%s: worst MOS %.2f, median %.2f.", plural(len(sess), "call", "calls"), worst, stats.Summarize(mos).Median)
	case k.Class == "video":
		worst, freezes, p95 := 100.0, 0, 0.0
		for _, s := range sess {
			for _, f := range flows(s) {
				if f.Frames != nil {
					worst = math.Min(worst, f.Frames.OKPct)
					freezes += f.Frames.Freezes
					p95 = math.Max(p95, f.Frames.DelayP95Ms)
				}
			}
			if s.Error != "" {
				worst = 0
			}
		}
		out.Metric, out.Value, out.Unit = "worst frames on time", stats.Round(worst, 2), "%"
		out.Summary = fmt.Sprintf("%s: the worst direction delivered %.1f%% of frames on time; %s; added frame delay up to %.0f ms (p95).", plural(len(sess), "call", "calls"), worst, plural(freezes, "freeze", "freezes"), p95)
	case k.Class == "rt": // gaming
		worst, loss := 0.0, 0.0
		for _, s := range sess {
			for _, f := range flows(s) {
				worst = math.Max(worst, f.AddedP95Ms)
				loss = math.Max(loss, f.LossPct)
			}
		}
		out.Metric, out.Value, out.Unit = "worst added delay (p95)", stats.Round(worst, 1), "ms"
		out.Summary = fmt.Sprintf("%s: up to %.0f ms of added delay (p95) and %.1f%% loss.", plural(len(sess), "session", "sessions"), worst, loss)
	case k.Class == "web":
		var all []float64
		pages, errs := 0, 0
		for _, s := range sess {
			if s.Times != nil {
				all = append(all, s.Times.P95)
			}
			pages += s.Count
			errs += s.Errors
		}
		p95 := 0.0
		if len(all) > 0 {
			p95 = slices.Max(all)
		}
		out.Metric, out.Value, out.Unit = "page load (p95)", stats.Round(p95, 0), "ms"
		out.Summary = fmt.Sprintf("%s loaded (%d failed); the slowest user's p95 load time was %s.", plural(pages, "page", "pages"), errs, fmtMs(p95))
	case k.Class == "stream":
		worst := 0.0
		chunks := 0
		for _, s := range sess {
			if s.Times != nil && s.TargetMs > 0 {
				worst = math.Max(worst, s.Times.P95/s.TargetMs*100)
			}
			chunks += s.Count
		}
		out.Metric, out.Unit = "chunk download time (p95)", "% of play time"
		pct := fmt.Sprintf("%.0f%%", worst)
		if worst < 10 {
			pct = fmt.Sprintf("%.1f%%", worst)
		}
		out.Value = stats.Round(worst, 1)
		out.Summary = fmt.Sprintf("%s fetched; the slowest viewer's p95 chunk took %s of its play time (over 100%% means rebuffering).", plural(chunks, "chunk", "chunks"), pct)
	case k.Class == "bulk":
		total := 0.0
		var each []string
		for _, s := range sess {
			total += s.Mbps
			each = append(each, fmtMbps(s.Mbps))
		}
		if bulk != nil {
			total = bulk.Mbps
		}
		out.Metric, out.Value, out.Unit = "throughput", stats.Round(total, 1), "Mbit/s"
		out.Summary = fmt.Sprintf("%s, %s in total (%s).", plural(len(sess), strings.ToLower(k.Label), strings.ToLower(k.Label)+"s"), fmtMbps(total), strings.Join(each, ", "))
	}
	return out
}

// fmtMs shows a duration in ms below a second and in seconds above.
func fmtMs(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0f ms", ms)
	}
	return fmt.Sprintf("%.1f s", ms/1000)
}

func fmtMbps(m float64) string {
	if m >= 1000 {
		return fmt.Sprintf("%.2f Gbit/s", m/1000)
	}
	return fmt.Sprintf("%.1f Mbit/s", m)
}

// worseOf returns the worse of two verdicts ("" counts as unknown/best).
func worseOf(a, b string) string {
	rank := map[string]int{"": -1, "good": 0, "acceptable": 1, "poor": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
