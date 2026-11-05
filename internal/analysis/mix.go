package analysis

import (
	"fmt"
	"strings"

	"github.com/agoodley/pathgauge/internal/results"
)

var verdictRank = map[string]int{"good": 0, "acceptable": 1, "poor": 2}

// kindNoun names a session kind in findings.
func kindNoun(kind string) string {
	switch {
	case strings.HasPrefix(kind, "voice"):
		return "Voice calls"
	case strings.HasPrefix(kind, "video"):
		return "Video calls"
	case kind == "gaming":
		return "Online gaming"
	case kind == "web":
		return "Web browsing"
	case strings.HasPrefix(kind, "stream"):
		return "Video streaming"
	}
	return kind
}

// metricText renders a kind's headline metric, e.g. "MOS 4.3".
func metricText(k results.MixKind) string {
	switch {
	case strings.HasPrefix(k.Kind, "voice"):
		return fmt.Sprintf("MOS %.1f", k.Value)
	case strings.HasPrefix(k.Kind, "video"):
		return fmt.Sprintf("%.1f%% of frames on time", k.Value)
	case k.Kind == "gaming":
		return fmt.Sprintf("%.0f ms added delay", k.Value)
	case k.Kind == "web":
		return fmt.Sprintf("%.1f s page loads (p95)", k.Value/1000)
	case strings.HasPrefix(k.Kind, "stream"):
		return fmt.Sprintf("chunks take %.0f%% of play time", k.Value)
	}
	return fmt.Sprintf("%g %s", k.Value, k.Unit)
}

func sevFor(verdict string) string {
	if verdict == "poor" {
		return "warning"
	}
	return "info"
}

// realtimeKind reports whether the kind is a call or game.
func realtimeKind(kind string) bool {
	return strings.HasPrefix(kind, "voice") || strings.HasPrefix(kind, "video") || kind == "gaming"
}

func mixFindings(m *results.Mix, id string) []results.Finding {
	var out []results.Finding
	if len(m.Phases) == 0 {
		return nil
	}
	first := m.Phases[0]
	byKind := func(p results.MixPhase) map[string]results.MixKind {
		out := map[string]results.MixKind{}
		for _, k := range p.Kinds {
			out[k.Kind] = k
		}
		return out
	}
	var loaded map[string]results.MixKind
	var loadedPhase *results.MixPhase
	if len(m.Phases) > 1 {
		loadedPhase = &m.Phases[len(m.Phases)-1]
		loaded = byKind(*loadedPhase)
	}
	transfers := func() string {
		var parts []string
		for _, k := range loadedPhase.Kinds {
			if k.Verdict == "" && k.Sessions > 0 { // bulk transfers
				parts = append(parts, fmt.Sprintf("%s at %.0f Mbit/s", strings.ToLower(k.Label), k.Value))
			}
		}
		return strings.Join(parts, " and ")
	}
	problems := 0
	for _, k := range first.Kinds {
		if k.Verdict == "" {
			continue
		}
		l, inLoaded := loaded[k.Kind]
		if verdictRank[k.Verdict] >= 1 {
			problems++
			rec := "Check the loss, jitter and capacity findings for this path."
			switch {
			case realtimeKind(k.Kind):
				rec = "Calls suffer from loss, jitter and queueing delay even without other traffic: check the loss and latency findings, and whether the path has the capacity for this many calls."
			case k.Kind == "web":
				rec = "Pages load slowly even without bulk transfers: check the round-trip time, short-transfer and capacity findings."
			case strings.HasPrefix(k.Kind, "stream"):
				rec = "The path does not have enough spare capacity for this many streams at this quality."
			}
			label := "on their own"
			if first.Name == "all" {
				label = "in this mix"
			}
			out = append(out, results.Finding{ID: "mix." + k.Kind + "." + k.Verdict, Severity: sevFor(k.Verdict), Confidence: "high",
				Title:          fmt.Sprintf("%s are %s %s (%s)", kindNoun(k.Kind), k.Verdict, label, metricText(k)),
				Detail:         k.Summary,
				Recommendation: rec, Evidence: []string{id}})
		}
		if loaded == nil || !inLoaded || verdictRank[l.Verdict] <= verdictRank[k.Verdict] {
			continue
		}
		problems++
		detail := fmt.Sprintf("Without transfers: %s. While %s ran: %s. Latency probes: p95 %.0f ms without transfers, %.0f ms with them.",
			metricText(k), transfers(), metricText(l), first.ProbeRTT.P95, loadedPhase.ProbeRTT.P95)
		rec := "The transfers fill a queue at the bottleneck that everything else waits behind. Enable smart queue management (CAKE or fq_codel set slightly below the link rate; the sqm test finds the rate)"
		if realtimeKind(k.Kind) {
			if m.DSCP {
				rec += ". The calls were marked (EF for voice, AF41 for video), so the bottleneck is not prioritising them either: honour those markings there."
			} else {
				rec += ", or prioritise calls with DSCP (EF for voice, AF41 for video) at the bottleneck."
			}
		} else {
			rec += "."
		}
		out = append(out, results.Finding{ID: "mix." + k.Kind + ".degraded", Severity: sevFor(l.Verdict), Confidence: "high",
			Title:          fmt.Sprintf("%s degrade while file transfers run (%s → %s)", kindNoun(k.Kind), metricText(k), metricText(l)),
			Detail:         detail,
			Recommendation: rec, Evidence: []string{id}})
	}
	if problems == 0 && m.Verdict == "good" {
		title := "Every simulated session had good quality"
		if loadedPhase != nil {
			title += ", with and without file transfers"
		}
		var kinds []string
		for _, c := range m.Counts {
			kinds = append(kinds, fmt.Sprintf("%d × %s", c.Count, strings.ToLower(c.Label)))
		}
		out = append(out, results.Finding{ID: "mix.good", Severity: "good", Confidence: "high", Title: title,
			Detail: "Mix: " + strings.Join(kinds, ", ") + ".", Evidence: []string{id}})
	}
	return out
}

func capacityFindings(c *results.Capacity, id string) []results.Finding {
	noun := strings.ToLower(kindNoun(c.Kind))
	count := func(s results.CapacitySearch) string {
		if s.Limit > 0 {
			return fmt.Sprintf("at least %d (the test stopped at its %s)", s.Acceptable, s.LimitReason)
		}
		return fmt.Sprint(s.Acceptable)
	}
	detail := fmt.Sprintf("On their own: %s %s at acceptable quality or better, %d with every call good. Each uses about %.0f kbit/s in each direction.",
		count(c.Alone), noun, c.Alone.Good, c.PerSessionKbps)
	title := fmt.Sprintf("The path carries %s simultaneous %s", count(c.Alone), noun)
	var out []results.Finding
	if w := c.WithTransfers; w != nil {
		detail += fmt.Sprintf(" While a download and an upload run: %s (%d all good).", count(*w), w.Good)
		title += fmt.Sprintf("; %s while transfers run", count(*w))
		if w.Acceptable == 0 && c.Alone.Acceptable > 0 {
			out = append(out, results.Finding{ID: "capacity." + c.Kind + ".transfers", Severity: "warning", Confidence: "high",
				Title:          fmt.Sprintf("Not even one of the %s stays acceptable while file transfers run", noun),
				Detail:         detail,
				Recommendation: "Bulk transfers fill the bottleneck queue. Enable smart queue management (CAKE or fq_codel slightly below the link rate) or prioritise calls with DSCP at the bottleneck.",
				Evidence:       []string{id}})
		}
	}
	out = append([]results.Finding{{ID: "capacity." + c.Kind, Severity: "info", Confidence: "medium", Title: title, Detail: detail, Evidence: []string{id}}}, out...)
	return out
}
