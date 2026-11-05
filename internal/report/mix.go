package report

import (
	"fmt"
	"html"
	"html/template"
	"sort"
	"strings"

	"github.com/agoodley/pathgauge/internal/results"
)

// verdictLabel shows a quality verdict with an icon and a word, never
// colour alone.
func verdictLabel(v string) string {
	switch v {
	case "good":
		return "✔ Good"
	case "acceptable":
		return "▲ Acceptable"
	case "poor":
		return "✖ Poor"
	}
	return "—"
}

// flowText summarises one direction of a real-time session.
func flowText(kind string, f *results.MixFlow) string {
	if f == nil {
		return "—"
	}
	switch {
	case strings.HasPrefix(kind, "voice"):
		return fmt.Sprintf("MOS %.2f · loss %.1f%% · late %.1f%% · +%.0f ms delay", f.MOS, f.LossPct, f.LatePct, f.PlayoutMs)
	case f.Frames != nil:
		return fmt.Sprintf("%.1f%% frames on time · %d freezes · +%.0f ms delay", f.Frames.OKPct, f.Frames.Freezes, f.Frames.PlayoutMs)
	}
	return fmt.Sprintf("+%.0f ms p95 · loss %.1f%% · jitter %.1f ms", f.AddedP95Ms, f.LossPct, f.JitterMs)
}

// sessionText summarises a TCP session.
func sessionText(s results.MixSession) string {
	switch {
	case s.Error != "":
		return s.Error
	case s.Times != nil && s.TargetMs > 0:
		return fmt.Sprintf("%d chunks · p95 %.1f s for %.0f s of video", s.Count, s.Times.P95/1000, s.TargetMs/1000)
	case s.Times != nil:
		return fmt.Sprintf("%d pages · median %.1f s · p95 %.1f s", s.Count, s.Times.Median/1000, s.Times.P95/1000)
	case s.Mbps > 0:
		return fmt.Sprintf("%.1f Mbit/s", s.Mbps)
	}
	return "—"
}

func mixSection(b *strings.Builder, v *results.Mix) {
	var counts []string
	for _, c := range v.Counts {
		counts = append(counts, fmt.Sprintf("%d × %s", c.Count, strings.ToLower(c.Label)))
	}
	dscp := "calls marked EF (voice) and AF41 (video)"
	if !v.DSCP {
		dscp = "calls unmarked"
	}
	scen := ""
	if v.Scenario != "" {
		scen = fmt.Sprintf("Scenario <b>%s</b>: ", html.EscapeString(v.Scenario))
	}
	fmt.Fprintf(b, `<p>%s%s; %s. Idle round trip %.1f ms. Overall: <b>%s</b>.</p>`, scen, html.EscapeString(strings.Join(counts, ", ")), dscp, v.BaseRTTms, verdictLabel(v.Verdict))

	// Share of good sessions per kind and phase: one unit, one axis.
	var cats []string
	var series []BarSeries
	for pi, p := range v.Phases {
		var vals []float64
		for _, k := range p.Kinds {
			if k.Verdict == "" || k.Sessions == 0 {
				continue
			}
			if pi == 0 {
				cats = append(cats, k.Label)
			}
			vals = append(vals, 100*float64(k.Good)/float64(k.Sessions))
		}
		series = append(series, BarSeries{Name: p.Label, Values: vals, Slot: pi + 1})
	}
	if len(cats) > 0 && len(series) > 0 && len(series[len(series)-1].Values) == len(cats) {
		b.WriteString(string(BarChart{Title: "Sessions with good quality, by kind", YLabel: "% of sessions", Categories: cats, Series: series, Unit: "%"}.SVG()))
	}
	for _, p := range v.Phases {
		fmt.Fprintf(b, `<h4>%s</h4><p class="muted">%.0f s; latency probes: median %.1f ms, p95 %.1f ms.</p>`, html.EscapeString(p.Label), p.DurationS, p.ProbeRTT.Median, p.ProbeRTT.P95)
		var rows [][]string
		for _, k := range p.Kinds {
			head := fmt.Sprintf("%g %s", k.Value, k.Unit)
			rows = append(rows, []string{k.Label, fmt.Sprint(k.Sessions), fmt.Sprintf("%d / %d / %d", k.Good, k.Acceptable, k.Poor), k.Metric + ": " + strings.TrimSpace(head), verdictLabel(k.Verdict)})
		}
		table(b, []string{"Sessions", "Count", "Good / acceptable / poor", "Headline", "Verdict"}, rows)
		var srows [][]string
		for _, s := range p.Sessions {
			if s.Up != nil || s.Down != nil {
				srows = append(srows, []string{s.ID, verdictLabel(s.Verdict), flowText(s.Kind, s.Up), flowText(s.Kind, s.Down)})
			} else {
				srows = append(srows, []string{s.ID, verdictLabel(s.Verdict), sessionText(s), ""})
			}
		}
		b.WriteString(`<details><summary>Every session</summary>`)
		table(b, []string{"Session", "Verdict", "Outbound (or result)", "Return"}, srows)
		b.WriteString(`</details>`)
	}
	for _, n := range v.Notes {
		fmt.Fprintf(b, `<p class="muted">%s</p>`, html.EscapeString(n))
	}
}

func capacitySection(b *strings.Builder, v *results.Capacity) {
	count := func(s results.CapacitySearch) string {
		if s.Limit > 0 {
			return fmt.Sprintf("at least %d (stopped at the %s)", s.Acceptable, s.LimitReason)
		}
		return fmt.Sprint(s.Acceptable)
	}
	fmt.Fprintf(b, `<p>%s, about %.0f kbit/s each way per session; %.0f s per step. On their own: <b>%s</b> sessions at acceptable quality or better (%d with every session good).`,
		html.EscapeString(v.Label), v.PerSessionKbps, v.StepS, count(v.Alone), v.Alone.Good)
	if w := v.WithTransfers; w != nil {
		fmt.Fprintf(b, ` While a download and an upload run: <b>%s</b> (%d all good).`, count(*w), w.Good)
	}
	b.WriteString(`</p>`)
	worst := "Worst MOS"
	switch {
	case strings.HasPrefix(v.Kind, "video"):
		worst = "Worst frames on time %"
	case v.Kind == "gaming":
		worst = "Worst added delay ms"
	}
	render := func(title string, s results.CapacitySearch) {
		steps := append([]results.CapacityStep(nil), s.Steps...)
		sort.Slice(steps, func(i, j int) bool { return steps[i].Sessions < steps[j].Sessions })
		var rows [][]string
		for _, st := range steps {
			rows = append(rows, []string{fmt.Sprint(st.Sessions), fmt.Sprintf("%d / %d / %d", st.Good, st.Acceptable, st.Poor), f2(st.Worst), f2(st.LossPct), f1(st.AddedP95Ms), verdictLabel(st.Verdict)})
		}
		fmt.Fprintf(b, `<h4>%s</h4>`, html.EscapeString(title))
		table(b, []string{"Sessions", "Good / acceptable / poor", worst, "Worst loss %", "Worst added delay p95 ms", "Verdict"}, rows)
	}
	render("On their own", v.Alone)
	if v.WithTransfers != nil {
		render("While a download and an upload run", *v.WithTransfers)
	}
}

func cdpSection(b *strings.Builder, v *results.CDP, id string) {
	set := v.Settings
	dir := "from this host to the other"
	if v.Direction == results.Down {
		dir = "from the other host to this one"
	}
	limit := "no throttle"
	if set.LimitMbps > 0 {
		limit = fmt.Sprintf("throttled to %.0f Mbit/s", set.LimitMbps)
	}
	fmt.Fprintf(b, `<p>Replication %s: %.0f Mbit/s of changed data on average, %.0f Mbit/s between bursts and %.0f Mbit/s during %.0f s bursts every %.0f s; %d connections, %s, target RPO %.0f s, %.0f s run. Overall: <b>%s</b>.</p>`,
		dir, set.RateMbps, v.BaseMbps, v.PeakMbps, set.BurstS, set.PeriodS, set.Streams, limit, set.RPOs, set.DurationS, verdictLabel(v.Verdict))
	var t, lag, rpo, gen, sent []float64
	for _, p := range v.Points {
		t = append(t, p.T)
		lag = append(lag, p.LagS)
		rpo = append(rpo, set.RPOs)
		gen = append(gen, p.GenMbps)
		sent = append(sent, p.SentMbps)
	}
	if len(t) > 1 {
		b.WriteString(string(LineChart{ID: id + "-lag", Title: "Replication lag (the RPO the replica had) against the target", XLabel: "seconds", YLabel: "lag (s)", YZero: true,
			Series: []Series{{Name: "lag", X: t, Y: lag, Slot: 1}, {Name: "target RPO", X: t, Y: rpo, Slot: 2}}}.SVG()))
		b.WriteString(string(LineChart{ID: id + "-rate", Title: "Changed data produced and sent", XLabel: "seconds", YLabel: "Mbit/s", YZero: true,
			Series: []Series{{Name: "produced", X: t, Y: gen, Slot: 1}, {Name: "sent", X: t, Y: sent, Slot: 2}}}.SVG()))
	}
	sustained := "never had to wait"
	if v.Backlogged {
		sustained = fmt.Sprintf("%.1f Mbit/s", v.SustainedMbps)
	}
	table(b, []string{"Measure", "Value"}, [][]string{
		{"Worst lag", fmt.Sprintf("%.1f s", v.MaxLagS)},
		{"Lag p95 / mean / at the end", fmt.Sprintf("%.1f / %.1f / %.1f s", v.P95LagS, v.MeanLagS, v.EndLagS)},
		{"Time within the RPO", fmt.Sprintf("%.0f%% (over it for %.0f s)", v.WithinRPOPct, v.OverRPOS)},
		{"Changed data produced / sent", fmt.Sprintf("%.0f / %.0f MB", v.GeneratedMB, v.SentMB)},
		{"Average sent", fmt.Sprintf("%.1f Mbit/s", v.ThroughputMbps)},
		{"Path carried while changes waited", sustained},
		{"Retransmitted", fmt.Sprintf("%.2f%%", v.RetransPct)},
		{"Round trip idle / during (p95)", fmt.Sprintf("%.1f / %.1f ms", v.IdleRTT.Median, v.LoadedRTT.P95)},
	})
	for _, n := range v.Notes {
		fmt.Fprintf(b, `<p class="muted">%s</p>`, html.EscapeString(n))
	}
}

// phaseLabel marks a grouped step as run alone or together.
func phaseLabel(r *results.Report, s results.Step) string {
	switch s.Phase {
	case "alone":
		return " (on its own, for comparison)"
	case "together":
		for _, g := range r.Groups {
			if g.ID == s.Group {
				return " (together with " + strings.Join(othersThan(g.Tests, s.ID), ", ") + ")"
			}
		}
		return " (together with the group)"
	}
	return ""
}

func othersThan(tests []string, stepID string) []string {
	// Trace and MTR steps end in -fwd / -rev (one per direction).
	stepID = strings.TrimSuffix(strings.TrimSuffix(stepID, "-fwd"), "-rev")
	var out []string
	for _, t := range tests {
		if !strings.HasSuffix(stepID, "-"+t) {
			out = append(out, t)
		}
	}
	return out
}

// loadSection compares grouped tests alone and together.
func loadSection(r *results.Report) template.HTML {
	if len(r.Groups) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<section><h2>Tests run together</h2>`)
	for _, g := range r.Groups {
		fmt.Fprintf(&b, `<p><b>%s</b> ran at the same time, each for %.0f s.`, html.EscapeString(strings.Join(g.Tests, " + ")), g.DurationS)
		if !g.Baseline {
			b.WriteString(` They didn't run on their own first, so there is nothing to compare with.</p>`)
			continue
		}
		b.WriteString(` Each first ran on its own for the same time.</p>`)
		var rows [][]string
		for _, c := range r.LoadComparisons {
			if c.Group != g.ID {
				continue
			}
			mark := "✔ Similar"
			if c.Worse {
				mark = "▲ Worse"
			}
			rows = append(rows, []string{c.Title, c.Metric, loadVal(c.Alone, c.Unit), loadVal(c.Together, c.Unit), mark})
		}
		if len(rows) > 0 {
			table(&b, []string{"Test", "Measure", "On its own", "Together", "Change"}, rows)
		}
	}
	b.WriteString(`</section>`)
	return template.HTML(b.String())
}

func loadVal(v float64, unit string) string {
	switch unit {
	case "":
		return f2(v)
	case "%":
		return f1(v) + "%"
	}
	return f1(v) + " " + unit
}
