package report

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// Options customise the HTML report.
type Options struct {
	Title    string
	Org      string // shown in the header (branding)
	Template string // technical (default), executive
	Notes    string // free text added by the author
}

// WriteHTML renders a self-contained HTML report.
func WriteHTML(w io.Writer, r *results.Report, o Options) error {
	if o.Title == "" {
		o.Title = "PathGauge network report"
	}
	exec := o.Template == "executive"
	var sections []template.HTML
	if !exec {
		for _, s := range r.Steps {
			sections = append(sections, section(r, s))
		}
	}
	data := map[string]any{
		"O": o, "R": r, "Exec": exec,
		"Link":     linkName(r),
		"When":     r.StartedAt.Format("2 Jan 2006 15:04 MST"),
		"Duration": r.EndedAt.Sub(r.StartedAt).Round(time.Second).String(),
		"Tiles":    tiles(r),
		"Score":    r.Summary.HealthScore,
		"ScoreCls": scoreClass(r.Summary.HealthScore),
		"Parts":    scoreParts(r),
		"Findings": r.Findings,
		"FP":       fingerprintRows(r),
		"Sections": sections,
		"Load":     loadSection(r),
		"Method":   methodology(r),
		"CSS":      template.CSS(css),
		"JS":       template.JS(js),
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, data); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func linkName(r *results.Report) string {
	c, e := r.Client.Name, r.Endpoint.Name
	if c == "" {
		c = "client"
	}
	if e == "" {
		e = r.Endpoint.Address
	}
	return c + " ↔ " + e
}

func scoreClass(s int) string {
	switch {
	case s >= 85:
		return "good"
	case s >= 65:
		return "warning"
	case s >= 45:
		return "serious"
	}
	return "critical"
}

type tile struct {
	Label, Value, Sub string
}

func tiles(r *results.Report) []tile {
	s := r.Summary
	var t []tile
	if s.DownMbps > 0 {
		t = append(t, tile{"Download", fmtMbps(s.DownMbps), singleNote(s.DownSingleMbps)})
	}
	if s.UpMbps > 0 {
		t = append(t, tile{"Upload", fmtMbps(s.UpMbps), singleNote(s.UpSingleMbps)})
	}
	if s.IdleRTTms > 0 {
		t = append(t, tile{"Idle RTT", fmt.Sprintf("%.1f ms", s.IdleRTTms), fmt.Sprintf("jitter %.2f ms · loss %.2f%%", s.JitterMs, s.LossPct)})
	}
	if s.BloatGrade != "" {
		t = append(t, tile{"Bufferbloat", s.BloatGrade, fmt.Sprintf("%.0f RPM responsiveness", s.RPM)})
	}
	if s.MOS > 0 {
		t = append(t, tile{"Voice (MOS)", fmt.Sprintf("%.1f", s.MOS), "simulated G.711 call"})
	}
	if s.PathMTU > 0 {
		t = append(t, tile{"Path MTU", fmt.Sprint(s.PathMTU), fmt.Sprintf("%d hops out · %d back", s.HopsUp, s.HopsDown)})
	}
	return t
}

func singleNote(single float64) string {
	if single <= 0 {
		return "parallel streams"
	}
	return "single stream " + fmtMbps(single)
}

func fmtMbps(m float64) string {
	switch {
	case m >= 1000:
		return fmt.Sprintf("%.2f Gbit/s", m/1000)
	case m >= 100:
		return fmt.Sprintf("%.0f Mbit/s", m)
	case m >= 10:
		return fmt.Sprintf("%.1f Mbit/s", m)
	}
	return fmt.Sprintf("%.2f Mbit/s", m)
}

func scoreParts(r *results.Report) [][2]string {
	weights := map[string]float64{"throughput": 25, "latency": 20, "loss": 20, "bufferbloat": 20, "path": 10, "integrity": 5}
	keys := []string{"throughput", "latency", "loss", "bufferbloat", "path", "integrity"}
	var out [][2]string
	for _, k := range keys {
		if v, ok := r.Summary.ScoreParts[k]; ok {
			out = append(out, [2]string{k, fmt.Sprintf("%.0f / %.0f", v, weights[k])})
		}
	}
	return out
}

func fingerprintRows(r *results.Report) [][2]string {
	var keys []string
	for k := range r.Summary.Fingerprint {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out [][2]string
	for _, k := range keys {
		out = append(out, [2]string{k, r.Summary.Fingerprint[k]})
	}
	return out
}

func methodology(r *results.Report) [][2]string {
	m := [][2]string{
		{"Software", r.Software},
		{"Profile", r.Profile},
		{"Run ID", r.ID},
		{"Started", r.StartedAt.Format(time.RFC3339)},
		{"Client", strings.TrimSpace(r.Client.Name + " " + r.Client.LocalAddr + " (seen by endpoint as " + r.Client.ObservedAddr + ")")},
		{"Endpoint", strings.TrimSpace(r.Endpoint.Name + " " + r.Endpoint.Address + " " + r.Endpoint.Software)},
	}
	for _, s := range r.StepsOf("info") {
		if in, ok := s.Result.(*results.Info); ok {
			m = append(m, [2]string{"Clocks", fmt.Sprintf("client synced=%v (±%.1f ms), endpoint synced=%v (±%.1f ms); one-way delay reported: %v",
				in.ClientClock.Synced, in.ClientClock.EstErrorMs, in.EndpointClock.Synced, in.EndpointClock.EstErrorMs, in.OWDTrustworthy)})
		}
	}
	m = append(m, [2]string{"Method", "Throughput is measured at the receiver with random (incompressible) payload; the first seconds of each TCP test are excluded as ramp-up. Latency uses timestamped UDP probes reflected by the endpoint (endpoint processing time removed). Bufferbloat grades use the 95th-percentile latency increase under load: A+ <5 ms, A <30, B <60, C <200, D <400, F ≥400. Loss interpretation on traceroutes ignores loss that does not persist to later hops (ICMP rate limiting)."})
	for _, n := range r.Notes {
		m = append(m, [2]string{"Note", n})
	}
	return m
}

func sevIcon(s string) string {
	switch s {
	case "critical":
		return "✖ Critical"
	case "warning":
		return "▲ Warning"
	case "info":
		return "ℹ Info"
	case "good":
		return "✔ Good"
	}
	return s
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{"sevIcon": sevIcon, "upper": strings.ToUpper}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.O.Title}} – {{.Link}}</title><style>{{.CSS}}</style></head>
<body><main class="viz-root">
<header><div><p class="eyebrow">{{if .O.Org}}{{.O.Org}} · {{end}}PathGauge report</p><h1>{{.Link}}</h1>
<p class="muted">{{.When}} · {{.R.Profile}} profile · {{.Duration}}</p></div>
<div class="score {{.ScoreCls}}" title="Health score"><span>{{.Score}}</span><small>health score</small></div></header>
{{if .O.Notes}}<section class="notes"><h2>Notes</h2><p>{{.O.Notes}}</p></section>{{end}}
<section class="tiles">{{range .Tiles}}<div class="tile"><small>{{.Label}}</small><strong>{{.Value}}</strong><span>{{.Sub}}</span></div>{{end}}</section>
<section><h2>Findings</h2>
{{range .Findings}}<article class="finding {{.Severity}}"><p class="sev">{{sevIcon .Severity}}</p><div><h3>{{.Title}}</h3><p>{{.Detail}}</p>{{if .Recommendation}}<p class="rec"><b>Recommendation:</b> {{.Recommendation}}</p>{{end}}{{if .Evidence}}<p class="evidence">Evidence: {{range .Evidence}}<a href="#{{.}}">{{.}}</a> {{end}}</p>{{end}}</div></article>{{end}}
</section>
{{.Load}}
<section class="two"><div><h2>Link fingerprint</h2><table>{{range .FP}}<tr><th>{{index . 0}}</th><td>{{index . 1}}</td></tr>{{end}}</table></div>
<div><h2>Score breakdown</h2><table>{{range .Parts}}<tr><th>{{index . 0}}</th><td>{{index . 1}}</td></tr>{{end}}</table></div></section>
{{if not .Exec}}<h2>Results</h2>{{range .Sections}}{{.}}{{end}}{{end}}
<section><h2>Methodology</h2><table>{{range .Method}}<tr><th>{{index . 0}}</th><td>{{index . 1}}</td></tr>{{end}}</table></section>
<footer>Generated by PathGauge. Only test networks you are authorised to test.</footer>
</main><script>{{.JS}}</script></body></html>`))

// section renders one step.
func section(r *results.Report, s results.Step) template.HTML {
	var b strings.Builder
	fmt.Fprintf(&b, `<section class="step" id="%s"><h3>%s%s <small class="muted">%s · %.1f s</small></h3>`, html.EscapeString(s.ID), html.EscapeString(s.Title), html.EscapeString(phaseLabel(r, s)), html.EscapeString(s.ID), s.Seconds)
	if s.Status != "ok" {
		fmt.Fprintf(&b, `<p class="err">%s: %s</p></section>`, html.EscapeString(s.Status), html.EscapeString(s.Error))
		return template.HTML(b.String())
	}
	switch v := s.Result.(type) {
	case *results.Info:
		infoSection(&b, v)
	case *results.Latency:
		latencySection(&b, v)
	case *results.TCP:
		tcpSection(&b, r, v, s.ID)
	case *results.UDP:
		udpSection(&b, v, s.ID)
	case *results.StreamScaling:
		var cats []string
		var vals []float64
		for _, p := range v.Points {
			cats = append(cats, fmt.Sprint(p.Streams))
			vals = append(vals, p.Mbps)
		}
		b.WriteString(string(BarChart{Title: "Throughput by number of parallel streams", YLabel: "Mbit/s", XLabel: "streams", Categories: cats, Series: []BarSeries{{Name: "throughput", Values: vals, Slot: 1}}, Unit: "Mbit/s"}.SVG()))
		fmt.Fprintf(&b, `<p>Best %s; one stream achieves %.0f%% of it.</p>`, fmtMbps(v.BestMbps), 100*v.SingleStreamEfficiency)
	case *results.Bufferbloat:
		bloatSection(&b, v, s.ID)
	case *results.Trace:
		traceSection(&b, v)
	case *results.PMTU:
		table(&b, []string{"Direction", "Path MTU", "Local MTU", "PTB received", "Black hole", "Note"},
			[][]string{pmtuRow("Outbound", v.Up), pmtuRow("Return", v.Down)})
	case *results.Integrity:
		integritySection(&b, v)
	case *results.VoIP:
		table(&b, []string{"Direction", "MOS", "R", "Loss %", "Late %", "Jitter ms", "Verdict"}, [][]string{
			{"Outbound", f2(v.Up.MOS), f1(v.Up.R), f2(v.Up.Loss.LossPct), f2(v.Up.LateLossPct), f2(v.Up.JitterMs), v.Up.Verdict},
			{"Return", f2(v.Down.MOS), f1(v.Down.R), f2(v.Down.Loss.LossPct), f2(v.Down.LateLossPct), f2(v.Down.JitterMs), v.Down.Verdict},
		})
		fmt.Fprintf(&b, `<p class="muted">%s for %.0f s%s; one-way network delay ≈ %.1f ms.</p>`, v.Codec, v.DurationS, map[bool]string{true: " while both directions were saturated", false: ""}[v.Loaded], v.OneWayMs)
	case *results.Sweep:
		sweepSection(&b, v, s.ID)
	case *results.SizeSweep:
		var cats []string
		var del []float64
		var rows [][]string
		for _, p := range v.Points {
			cats = append(cats, fmt.Sprint(p.Size))
			del = append(del, p.PPS)
			rows = append(rows, []string{fmt.Sprint(p.Size), f2(p.DeliveredMbps), fmt.Sprintf("%.0f", p.PPS), f2(p.LossPct)})
		}
		b.WriteString(string(BarChart{Title: fmt.Sprintf("Packets per second delivered at %.0f Mbit/s, by packet size", v.RateMbps), YLabel: "packets/s", XLabel: "packet size (bytes)", Categories: cats, Series: []BarSeries{{Name: "pps", Values: del, Slot: 1}}, Unit: "pps"}.SVG()))
		table(&b, []string{"Size", "Delivered Mbit/s", "Packets/s", "Loss %"}, rows)
	case *results.CCCompare:
		var rows [][]string
		for _, p := range v.Points {
			rows = append(rows, []string{p.Algorithm, f1(p.Mbps), fmt.Sprint(p.Retransmits), f2(p.RTTms), p.Error})
		}
		table(&b, []string{"Algorithm", "Mbit/s", "Retransmits", "RTT ms", "Error"}, rows)
	case *results.ShortFlow:
		rows := [][]string{{"TCP connect", f2(v.Connect.Median), f2(v.Connect.P95)}}
		for _, k := range []string{"10KB", "100KB", "1MB"} {
			if c, ok := v.Completions[k]; ok {
				rows = append(rows, []string{k + " transfer", f2(c.Median), f2(c.P95)})
			}
		}
		table(&b, []string{"Operation", "Median ms", "p95 ms"}, rows)
	case *results.Discrimination:
		var rows [][]string
		for _, p := range v.Points {
			rows = append(rows, []string{p.Label, f1(p.Mbps), p.Error})
		}
		table(&b, []string{"Traffic", "Mbit/s", "Error"}, rows)
		fmt.Fprintf(&b, `<p>%s.</p>`, html.EscapeString(v.Verdict))
	case *results.SQMTuner:
		sqmSection(&b, v)
	case *results.Mix:
		mixSection(&b, v)
	case *results.Capacity:
		capacitySection(&b, v)
	case *results.CDP:
		cdpSection(&b, v, s.ID)
	case *results.QoS:
		var rows [][]string
		var cats []string
		var idle, loaded []float64
		for _, c := range v.Classes {
			rows = append(rows, []string{c.Name, fmt.Sprint(c.DSCP), f2(c.Idle.Median), f2(c.Loaded.Median), f2(c.Loaded.P95), f1(c.Increase), f2(c.LossPct)})
			cats = append(cats, c.Name)
			idle = append(idle, c.Idle.Median)
			loaded = append(loaded, c.Loaded.P95)
		}
		b.WriteString(string(BarChart{Title: "Probe RTT by DSCP class: idle median vs p95 under load", YLabel: "RTT (ms)", Categories: cats,
			Series: []BarSeries{{Name: "idle median", Values: idle, Slot: 1}, {Name: "loaded p95", Values: loaded, Slot: 2}}, Unit: "ms"}.SVG()))
		table(&b, []string{"Class", "DSCP", "Idle median ms", "Loaded median ms", "Loaded p95 ms", "Increase ms", "Loss %"}, rows)
		fmt.Fprintf(&b, `<p>%s</p>`, html.EscapeString(v.Verdict))
	default:
		b.WriteString(`<p class="muted">No renderer for this result.</p>`)
	}
	b.WriteString(`</section>`)
	return template.HTML(b.String())
}

func f1(x float64) string { return fmt.Sprintf("%.1f", x) }
func f2(x float64) string { return fmt.Sprintf("%.2f", x) }

func table(b *strings.Builder, head []string, rows [][]string) {
	b.WriteString(`<div class="tablewrap"><table class="data"><thead><tr>`)
	for _, h := range head {
		fmt.Fprintf(b, `<th>%s</th>`, html.EscapeString(h))
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, r := range rows {
		b.WriteString(`<tr>`)
		for _, c := range r {
			fmt.Fprintf(b, `<td>%s</td>`, html.EscapeString(c))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div>`)
}

func sumRow(label string, s stats.Summary) []string {
	return []string{label, fmt.Sprint(s.N), f2(s.Min), f2(s.Median), f2(s.Mean), f2(s.P95), f2(s.P99), f2(s.Max), f2(s.StdDev)}
}

var sumHead = []string{"Measure", "Samples", "Min", "Median", "Mean", "p95", "p99", "Max", "Std dev"}

func infoSection(b *strings.Builder, v *results.Info) {
	for _, h := range []struct {
		name string
		r    *hostinfo.Report
	}{{"Client", v.ClientHost}, {"Endpoint", v.EndpointHost}} {
		if h.r == nil {
			continue
		}
		n := h.r.Counts()
		fmt.Fprintf(b, `<h4>%s: %s</h4><p class="muted">%s, kernel %s, %d CPUs%s · checks: %d ok, %d warnings, %d missing</p>`,
			h.name, html.EscapeString(h.r.Hostname), html.EscapeString(h.r.OS), html.EscapeString(h.r.Kernel), h.r.CPUs, virt(h.r.Virtualization), n[hostinfo.StatusOK], n[hostinfo.StatusWarn], n[hostinfo.StatusMissing])
		var rows [][]string
		for _, c := range h.r.Checks {
			if c.Status == hostinfo.StatusWarn || c.Status == hostinfo.StatusMissing {
				rows = append(rows, []string{c.Section, c.Name, string(c.Status), c.Detail, c.Fix})
			}
		}
		if len(rows) > 0 {
			table(b, []string{"Area", "Check", "Status", "Detail", "Fix"}, rows)
		}
	}
	fmt.Fprintf(b, `<p>Endpoint policy: up to %d Mbit/s, %d s per test, shaping %s. Estimated clock offset between hosts: %.2f ms.</p>`, v.EndpointPolicy.MaxRateMbps, v.EndpointPolicy.MaxTestDurationSec, html.EscapeString(v.EndpointPolicy.Shaping), v.ClockOffsetMs)
}

func virt(v string) string {
	if v == "" {
		return ""
	}
	return ", " + v
}

func latencySection(b *strings.Builder, v *results.Latency) {
	var x, y []float64
	interval := 1 / math.Max(v.RateHz, 1)
	for i, r := range v.Series {
		x = append(x, stats.Round(float64(i)*interval, 3))
		y = append(y, r)
	}
	b.WriteString(string(LineChart{Title: "Round-trip time of each probe (gaps are lost probes)", XLabel: "seconds", YLabel: "RTT (ms)", Series: []Series{{Name: "UDP RTT", X: x, Y: y, Slot: 1}}, YZero: true}.SVG()))
	rows := [][]string{sumRow("UDP round trip (ms)", v.UDP)}
	if v.ICMP != nil {
		rows = append(rows, sumRow("ICMP ping (ms)", *v.ICMP))
	}
	if v.TCPConnect != nil {
		rows = append(rows, sumRow("TCP connect (ms)", *v.TCPConnect))
	}
	if v.OWDUp != nil {
		rows = append(rows, sumRow("One-way outbound (ms)", *v.OWDUp), sumRow("One-way return (ms)", *v.OWDDown))
	}
	table(b, sumHead, rows)
	fmt.Fprintf(b, `<p>Loss %.2f%% (%d of %d), %d reordered, %d duplicated. Jitter (RFC 3550) %.2f ms round trip, %.2f ms outbound, %.2f ms return.%s</p>`,
		v.Loss.LossPct, v.Loss.Lost, v.Loss.Sent, v.Loss.Reordered, v.Loss.Duplicates, v.UDPJitterMs, v.UpJitterMs, v.DownJitterMs, noteStr(v.ICMPNote))
}

func noteStr(s string) string {
	if s == "" {
		return ""
	}
	return " " + html.EscapeString(s) + "."
}

func tcpSection(b *strings.Builder, r *results.Report, v *results.TCP, id string) {
	var x, y, rtt []float64
	for _, iv := range v.Intervals {
		x = append(x, iv.T)
		y = append(y, iv.Mbps)
		rtt = append(rtt, iv.RTTms)
	}
	b.WriteString(string(LineChart{ID: id, Title: fmt.Sprintf("Throughput, %d stream(s), receiver side", v.Streams), XLabel: "seconds", YLabel: "Mbit/s", Series: []Series{{Name: "throughput", X: x, Y: y, Slot: 1}}, YZero: true, Height: 200}.SVG()))
	b.WriteString(string(LineChart{Title: "Sender's smoothed RTT during the test (queueing shows as a rise)", XLabel: "seconds", YLabel: "RTT (ms)", Series: []Series{{Name: "RTT", X: x, Y: rtt, Slot: 2}}, YZero: true, Height: 170}.SVG()))
	table(b, []string{"Goodput", "Peak", "Retransmits", "Retrans %", "RTT median", "RTT p95", "Window-limited", "Send-buffer-limited", "MSS", "Ramp", "CC", "Fairness"},
		[][]string{{fmtMbps(v.Mbps), fmtMbps(v.PeakMbps), fmt.Sprint(v.Retransmits), fmt.Sprintf("%.3f", v.RetransPct), f2(v.RTT.Median) + " ms", f2(v.RTT.P95) + " ms",
			f1(v.RwndLimitedPct) + "%", f1(v.SndbufLimitedPct) + "%", fmt.Sprint(v.MSS), f1(v.RampS) + " s", v.Congestion, fmt.Sprintf("%.3f", v.Fairness)}})
}

func udpSection(b *strings.Builder, v *results.UDP, id string) {
	var x, y, q []float64
	for _, bin := range v.Bins {
		x = append(x, bin.T)
		y = append(y, bin.Mbps)
		q = append(q, bin.QueueDelayMs)
	}
	b.WriteString(string(LineChart{Title: fmt.Sprintf("Delivered rate of a %.1f Mbit/s UDP stream", v.TargetMbps), XLabel: "seconds", YLabel: "Mbit/s", Series: []Series{{Name: "delivered", X: x, Y: y, Slot: 1}}, YZero: true, Height: 180}.SVG()))
	b.WriteString(string(LineChart{Title: "Queueing delay (one-way delay above its minimum)", XLabel: "seconds", YLabel: "ms", Series: []Series{{Name: "queue delay", X: x, Y: q, Slot: 2}}, YZero: true, Height: 160}.SVG()))
	table(b, []string{"Target", "Delivered", "Packets/s", "Loss %", "Bursts (max)", "Reordered %", "Jitter ms", "Queue p50/p95 ms"},
		[][]string{{fmtMbps(v.TargetMbps), fmtMbps(v.Mbps), fmt.Sprintf("%.0f", v.PPS), fmt.Sprintf("%.3f", v.Loss.LossPct), fmt.Sprintf("%d (%d)", v.Loss.Bursts, v.Loss.MaxBurst),
			f2(v.Loss.ReorderPct), f2(v.JitterMs), f2(v.QueueDelay.Median) + " / " + f2(v.QueueDelay.P95)}})
}

func bloatSection(b *strings.Builder, v *results.Bufferbloat, id string) {
	var x, y []float64
	var bands []Band
	phaseStart := map[string]float64{}
	phaseEnd := map[string]float64{}
	for _, ph := range v.Phases {
		for _, s := range ph.Series {
			x = append(x, s.T)
			y = append(y, s.RTTms)
			if _, ok := phaseStart[ph.Name]; !ok || s.T < phaseStart[ph.Name] {
				phaseStart[ph.Name] = s.T
			}
			phaseEnd[ph.Name] = math.Max(phaseEnd[ph.Name], s.T)
		}
	}
	idx := make([]int, len(x))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return x[idx[i]] < x[idx[j]] })
	xs, ys := make([]float64, len(x)), make([]float64, len(x))
	for i, k := range idx {
		xs[i], ys[i] = x[k], y[k]
	}
	labels := map[string]string{"idle": "idle", "down": "download busy", "up": "upload busy", "bidir": "both busy"}
	for _, ph := range v.Phases {
		if ph.Name != "idle" {
			bands = append(bands, Band{From: phaseStart[ph.Name], To: phaseEnd[ph.Name], Label: labels[ph.Name]})
		}
	}
	b.WriteString(string(LineChart{Title: "Latency of a separate light flow while the link is idle and busy", XLabel: "seconds", YLabel: "RTT (ms)", Series: []Series{{Name: "probe RTT", X: xs, Y: ys, Slot: 1, Points: true}}, Bands: bands, YZero: true}.SVG()))
	var rows [][]string
	for _, ph := range v.Phases {
		g := v.Grades[ph.Name]
		inc := ""
		if ph.Name != "idle" {
			inc = f1(v.IncreaseMs[ph.Name])
		}
		rows = append(rows, []string{labels[ph.Name], f2(ph.RTT.Median), f2(ph.RTT.P95), f2(ph.InFlowRTT.Median), f2(ph.Connect.Median), inc, g, fmtMbps(ph.DownMbps), fmtMbps(ph.UpMbps)})
	}
	table(b, []string{"Phase", "Probe RTT median", "Probe RTT p95", "In-flow RTT median", "TCP connect", "Increase (p95)", "Grade", "Download", "Upload"}, rows)
	fmt.Fprintf(b, `<p>Overall grade <b>%s</b>; responsiveness %.0f RPM (working latency %.1f ms); queue behaviour: %s.</p>`, v.Grade, v.RPM, v.WorkingLatency, html.EscapeString(v.Queueing))
}

func traceSection(b *strings.Builder, v *results.Trace) {
	var rows [][]string
	for _, h := range v.Hops {
		addr := h.Addr
		if addr == "" {
			addr = "*"
		}
		name := h.Name
		if h.Org != "" {
			name = strings.TrimSpace(name + " " + h.ASN + " " + h.Org)
		}
		flags := []string{}
		if h.Private {
			flags = append(flags, "private")
		}
		if h.CGNAT {
			flags = append(flags, "CGNAT")
		}
		if len(h.Addrs) > 1 {
			flags = append(flags, fmt.Sprintf("%d responders (ECMP)", len(h.Addrs)))
		}
		if len(h.MPLS) > 0 {
			flags = append(flags, "MPLS "+strings.Join(h.MPLS, "/"))
		}
		if h.Dest {
			flags = append(flags, "destination")
		}
		rows = append(rows, []string{fmt.Sprint(h.TTL), addr, name, fmt.Sprintf("%.1f", h.LossPct), f2(h.RTT.Min), f2(h.RTT.Median), f2(h.RTT.Max), strings.Join(flags, ", ")})
	}
	fmt.Fprintf(b, `<p class="muted">From %s to %s, method %s, %d cycle(s), fingerprint %s.</p>`, html.EscapeString(v.From), html.EscapeString(v.Target), html.EscapeString(v.Method), v.Cycles, v.Fingerprint)
	table(b, []string{"Hop", "Address", "Name / network", "Loss %", "Best ms", "Median ms", "Worst ms", "Notes"}, rows)
	if v.Note != "" {
		fmt.Fprintf(b, `<p class="muted">%s.</p>`, html.EscapeString(v.Note))
	}
}

func pmtuRow(label string, d results.PMTUDir) []string {
	return []string{label, fmt.Sprint(d.MTU), fmt.Sprint(d.LocalMTU), fmt.Sprint(d.PTBSeen), fmt.Sprint(d.Blackhole), d.Note}
}

func integritySection(b *strings.Builder, v *results.Integrity) {
	var rows [][]string
	for i, d := range v.DSCPUp {
		down := results.DSCPResult{}
		if i < len(v.DSCPDown) {
			down = v.DSCPDown[i]
		}
		rows = append(rows, []string{d.Name, fmt.Sprint(d.Sent), recv(d), d.Status, recv(down), down.Status})
	}
	table(b, []string{"Marking", "DSCP sent", "Received out", "Outbound", "Received back", "Return"}, rows)
	table(b, []string{"Check", "Result"}, [][]string{
		{"ECN (ECT(0) marks)", "outbound " + v.ECNUp + ", return " + v.ECNDown},
		{"TCP MSS", fmt.Sprintf("client advertises %d (endpoint uses %d); endpoint advertises %d (client uses %d); clamped out=%v back=%v", v.ClientAdvMSS, v.EndpointSeesMSS, v.EndpointAdvMSS, v.ClientSeesMSS, v.MSSClampedUp, v.MSSClampedDown)},
		{"TCP options negotiated", strings.Join(v.ClientOptions, ", ") + stripped(v.StrippedOptions)},
		{"NAT", fmt.Sprintf("%v (client %s seen as %s; port preserved %v; CGNAT %v)", v.NAT, v.ClientLocal, v.ClientObserved, v.PortPreserved, v.CGNAT)},
		{"Hops from TTL", fmt.Sprintf("outbound %d (TTL %d), return %d (TTL %d)", v.HopsUp, v.TTLUp, v.HopsDown, v.TTLDown)},
	})
}

func recv(d results.DSCPResult) string {
	if d.Received < 0 {
		return "lost"
	}
	return fmt.Sprint(d.Received)
}

func stripped(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return " (stripped: " + strings.Join(s, ", ") + ")"
}

func sweepSection(b *strings.Builder, v *results.Sweep, id string) {
	var off, del, loss, q50, q95 []float64
	var rows [][]string
	for _, s := range v.Steps {
		off = append(off, s.OfferedMbps)
		del = append(del, s.DeliveredMbps)
		loss = append(loss, s.LossPct)
		q50 = append(q50, s.QueueP50ms)
		q95 = append(q95, s.QueueP95ms)
		rows = append(rows, []string{f1(s.OfferedMbps), f1(s.DeliveredMbps), f2(s.LossPct), f1(s.QueueP50ms), f1(s.QueueP95ms), f2(s.JitterMs), f1(s.FirstHalfMbps) + " / " + f1(s.SecondHalfMbps)})
	}
	fmt.Fprintf(b, `<p><b>%s</b>: %s</p>`, html.EscapeString(v.Signature), html.EscapeString(v.SignatureDetail))
	b.WriteString(`<div class="multiples">`)
	b.WriteString(string(LineChart{Title: "Delivered vs offered rate", XLabel: "offered Mbit/s", YLabel: "delivered Mbit/s", Series: []Series{{Name: "delivered", X: off, Y: del, Slot: 1}, {Name: "offered (ideal)", X: off, Y: off, Slot: 3}}, YZero: true, Height: 200}.SVG()))
	b.WriteString(string(LineChart{Title: "Loss vs offered rate", XLabel: "offered Mbit/s", YLabel: "loss %", Series: []Series{{Name: "loss", X: off, Y: loss, Slot: 2}}, YZero: true, Height: 200}.SVG()))
	b.WriteString(string(LineChart{Title: "Queueing delay vs offered rate", XLabel: "offered Mbit/s", YLabel: "ms", Series: []Series{{Name: "median", X: off, Y: q50, Slot: 1}, {Name: "p95", X: off, Y: q95, Slot: 2}}, YZero: true, Height: 200}.SVG()))
	b.WriteString(`</div>`)
	table(b, []string{"Offered Mbit/s", "Delivered", "Loss %", "Queue p50 ms", "Queue p95 ms", "Jitter ms", "1st/2nd half Mbit/s"}, rows)
	fmt.Fprintf(b, `<p class="muted">Capacity %.1f Mbit/s, knee %.1f Mbit/s, queue onset %.1f Mbit/s, max queue %.1f ms, estimated buffer %.0f bytes.</p>`, v.CapacityMbps, v.KneeMbps, v.QueueOnsetMbps, v.MaxQueueMs, v.BufferBytes)
}

func sqmSection(b *strings.Builder, v *results.SQMTuner) {
	var rows [][]string
	var pct, inc []float64
	for _, s := range v.Steps {
		rows = append(rows, []string{f1(s.Percent) + "%", s.Qdisc, f1(s.DownMbps) + " / " + f1(s.UpMbps), f1(s.DownMeasured) + " / " + f1(s.UpMeasured), f1(s.IncreaseMs), s.Grade, s.Error})
		if s.Error == "" {
			pct = append(pct, s.Percent)
			inc = append(inc, s.IncreaseMs)
		}
	}
	b.WriteString(string(LineChart{Title: "Latency increase under load at each shaping rate", XLabel: "shaped rate (% of measured capacity)", YLabel: "ms (p95 increase)", Series: []Series{{Name: "latency increase", X: pct, Y: inc, Slot: 1}}, YZero: true, Height: 200}.SVG()))
	table(b, []string{"Shaped to", "Qdisc", "Shaped Mbit/s (down / up)", "Measured Mbit/s", "Increase ms", "Grade", "Error"}, rows)
	if v.Recommended != nil {
		fmt.Fprintf(b, `<p>Recommended: <b>%.0f%%</b> (%.1f / %.1f Mbit/s) with %s, grade %s.</p>`, v.Recommended.Percent, v.Recommended.DownMbps, v.Recommended.UpMbps, html.EscapeString(v.Recommended.Qdisc), v.Recommended.Grade)
	}
	if len(v.TCCommands) > 0 {
		fmt.Fprintf(b, `<pre>%s</pre>`, html.EscapeString(strings.Join(v.TCCommands, "\n")))
	}
	if v.Note != "" {
		fmt.Fprintf(b, `<p class="muted">%s</p>`, html.EscapeString(v.Note))
	}
}

const css = `
.viz-root{color-scheme:light;--surface-0:#f4f3f0;--surface-1:#fcfcfb;--text-primary:#0b0b0b;--text-secondary:#52514e;--text-muted:#6f6e69;--line:#dedcd6;--grid:#e9e7e2;
--s1:#2a78d6;--s2:#eb6834;--s3:#1baf7a;--s4:#eda100;--good:#0ca30c;--warning:#fab219;--serious:#ec835a;--critical:#d03b3b;--band:rgba(42,120,214,.07)}
@media (prefers-color-scheme:dark){:root:where(:not([data-theme="light"])) .viz-root{color-scheme:dark;--surface-0:#121211;--surface-1:#1a1a19;--text-primary:#f0efec;--text-secondary:#c3c2b7;--text-muted:#9b9a92;--line:#383835;--grid:#2a2a28;
--s1:#3987e5;--s2:#d95926;--s3:#199e70;--s4:#c98500;--band:rgba(57,135,229,.10)}}
*{box-sizing:border-box}body{margin:0;background:var(--surface-0,#f4f3f0);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
.viz-root{max-width:1100px;margin:0 auto;padding:28px;color:var(--text-primary);background:var(--surface-0)}
h1{margin:.1em 0;font-size:28px}h2{margin:32px 0 12px;font-size:20px}h3{margin:0 0 6px;font-size:16px}h4{margin:14px 0 2px}
.eyebrow{margin:0;text-transform:uppercase;letter-spacing:.08em;font-size:12px;color:var(--text-secondary)}.muted{color:var(--text-muted)}
header{display:flex;justify-content:space-between;align-items:center;gap:24px;border-bottom:1px solid var(--line);padding-bottom:16px}
.score{text-align:center;border-radius:12px;padding:10px 18px;border:2px solid var(--line);background:var(--surface-1)}.score span{display:block;font-size:40px;font-weight:700;line-height:1}
.score small{color:var(--text-secondary)}.score.good{border-color:var(--good)}.score.warning{border-color:var(--warning)}.score.serious{border-color:var(--serious)}.score.critical{border-color:var(--critical)}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px;margin-top:20px}
.tile{background:var(--surface-1);border:1px solid var(--line);border-radius:10px;padding:12px 14px}.tile small{color:var(--text-secondary);display:block}
.tile strong{display:block;font-size:24px;margin:2px 0}.tile span{color:var(--text-muted);font-size:13px}
.finding{display:grid;grid-template-columns:110px 1fr;gap:12px;background:var(--surface-1);border:1px solid var(--line);border-left:4px solid var(--line);border-radius:8px;padding:12px 14px;margin:8px 0}
.finding .sev{margin:0;font-weight:600;font-size:13px;color:var(--text-secondary)}.finding p{margin:4px 0}
.finding.critical{border-left-color:var(--critical)}.finding.warning{border-left-color:var(--warning)}.finding.info{border-left-color:var(--s1)}.finding.good{border-left-color:var(--good)}
.rec{color:var(--text-secondary)}.evidence{font-size:12px;color:var(--text-muted)}a{color:var(--s1)}
.two{display:grid;grid-template-columns:1fr 1fr;gap:24px}
table{border-collapse:collapse;width:100%;font-size:13px;background:var(--surface-1)}th,td{text-align:left;padding:5px 8px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--text-secondary);font-weight:600}.tablewrap{overflow-x:auto;margin:8px 0}
.step{background:var(--surface-1);border:1px solid var(--line);border-radius:10px;padding:16px;margin:14px 0}.err{color:var(--critical)}
.chart{margin:8px 0;position:relative}.chart figcaption{font-size:13px;color:var(--text-secondary);margin-bottom:2px}.chart svg{width:100%;height:auto;display:block}
.legend{display:flex;gap:14px;font-size:12px;color:var(--text-secondary);margin:2px 0}.sw{display:inline-block;width:14px;height:3px;border-radius:2px;vertical-align:middle;margin-right:5px}
.sw.dot{width:8px;height:8px;border-radius:50%}
.sw.s1,.pt.s1{background:var(--s1);fill:var(--s1)}.sw.s2,.pt.s2{background:var(--s2);fill:var(--s2)}.sw.s3,.pt.s3{background:var(--s3);fill:var(--s3)}.sw.s4,.pt.s4{background:var(--s4);fill:var(--s4)}
.line{fill:none;stroke-width:2;stroke-linejoin:round;stroke-linecap:round}.line.s1{stroke:var(--s1)}.line.s2{stroke:var(--s2)}.line.s3{stroke:var(--s3)}.line.s4{stroke:var(--s4)}
.bar.s1{fill:var(--s1)}.bar.s2{fill:var(--s2)}.bar.s3{fill:var(--s3)}.bar.s4{fill:var(--s4)}
.grid{stroke:var(--grid);stroke-width:1}.axis{stroke:var(--line);stroke-width:1}.tick,.vlabel,.bandlabel{font-size:11px;fill:var(--text-muted)}.axislabel{font-size:11px;fill:var(--text-secondary)}
.band{fill:var(--band)}.xhair{stroke:var(--text-muted);stroke-width:1;stroke-dasharray:3 3}
.tip{position:absolute;pointer-events:none;background:var(--surface-1);border:1px solid var(--line);border-radius:6px;padding:4px 8px;font-size:12px;box-shadow:0 2px 8px rgba(0,0,0,.12);white-space:nowrap}
.multiples{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:8px}
pre{background:var(--surface-0);padding:10px;border-radius:6px;overflow-x:auto;font-size:12px}footer{margin:32px 0 8px;color:var(--text-muted);font-size:12px}
@media print{body{background:#fff}.viz-root{max-width:none;padding:0}.step,.finding{break-inside:avoid}.tip,.xhair{display:none}}
@media (max-width:720px){.two{grid-template-columns:1fr}.finding{grid-template-columns:1fr}}
`

const js = `
document.querySelectorAll('figure.chart[data-chart=line]').forEach(function(fig){
 var dataEl=fig.querySelector('.hoverdata'); if(!dataEl) return; var d=JSON.parse(dataEl.textContent);
 var svg=fig.querySelector('svg'), tip=fig.querySelector('.tip'), xh=svg.querySelector('.xhair');
 var pl=d.pad[0], pr=d.pad[1];
 function toX(px){var f=(px-pl)/(d.w-pl-pr); if(d.log){return Math.pow(10,Math.log10(d.x[0])+f*(Math.log10(d.x[1])-Math.log10(d.x[0])));} return d.x[0]+f*(d.x[1]-d.x[0]);}
 function fx(v){return Math.abs(v)>=100?v.toFixed(0):Math.abs(v)>=10?v.toFixed(1):v.toFixed(2);}
 svg.addEventListener('mousemove',function(ev){
  var r=svg.getBoundingClientRect(), sx=(ev.clientX-r.left)*d.w/r.width; if(sx<pl||sx>d.w-pr){tip.hidden=true;xh.setAttribute('visibility','hidden');return;}
  var x=toX(sx), lines=[], bestX=null;
  d.s.forEach(function(s){var best=null,bd=Infinity; s.p.forEach(function(p){var dd=Math.abs(p[0]-x); if(dd<bd){bd=dd;best=p;}}); if(best){lines.push(s.n+': '+fx(best[1])); if(bestX===null)bestX=best[0];}});
  if(!lines.length)return; xh.setAttribute('x1',sx);xh.setAttribute('x2',sx);xh.setAttribute('visibility','visible');
  tip.textContent=''; var h=document.createElement('b'); h.textContent=(d.xl||'x')+' '+fx(bestX); tip.appendChild(h);
  lines.forEach(function(l){var div=document.createElement('div'); div.textContent=l; tip.appendChild(div);});
  tip.hidden=false; var left=(ev.clientX-fig.getBoundingClientRect().left)+12; if(left>fig.clientWidth-160)left-=180; tip.style.left=left+'px'; tip.style.top='28px';
 });
 svg.addEventListener('mouseleave',function(){tip.hidden=true;xh.setAttribute('visibility','hidden');});
});
`
