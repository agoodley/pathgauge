package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/agoodley/pathgauge/internal/results"
)

// WriteText renders a terminal summary of a run.
func WriteText(w io.Writer, r *results.Report) error {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "\nPathGauge %s  ·  %s profile  ·  health score %d/100\n", linkName(r), r.Profile, s.HealthScore)
	fmt.Fprintln(&b, strings.Repeat("─", 72))
	row := func(label, val string) { fmt.Fprintf(&b, "  %-22s %s\n", label, val) }
	if s.DownMbps > 0 {
		row("Download", fmtMbps(s.DownMbps)+optional(" (single stream "+fmtMbps(s.DownSingleMbps)+")", s.DownSingleMbps > 0))
	}
	if s.UpMbps > 0 {
		row("Upload", fmtMbps(s.UpMbps)+optional(" (single stream "+fmtMbps(s.UpSingleMbps)+")", s.UpSingleMbps > 0))
	}
	if s.IdleRTTms > 0 {
		row("Idle latency", fmt.Sprintf("%.2f ms RTT, jitter %.2f ms, loss %.2f%%", s.IdleRTTms, s.JitterMs, s.LossPct))
	}
	if s.BloatGrade != "" {
		row("Bufferbloat", fmt.Sprintf("grade %s, %.0f RPM", s.BloatGrade, s.RPM))
	}
	if s.MOS > 0 {
		row("Voice quality", fmt.Sprintf("MOS %.2f", s.MOS))
	}
	if s.PathMTU > 0 {
		row("Path MTU", fmt.Sprint(s.PathMTU))
	}
	if s.HopsUp > 0 || s.HopsDown > 0 {
		row("Hops", fmt.Sprintf("%d out, %d back", s.HopsUp, s.HopsDown))
	}
	if s.NAT {
		row("NAT", "client is behind NAT")
	}
	fmt.Fprintln(&b, "\nFindings")
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  %-11s %s\n", sevIcon(f.Severity), f.Title)
		if f.Recommendation != "" && (f.Severity == "critical" || f.Severity == "warning") {
			fmt.Fprintf(&b, "              → %s\n", wrap(f.Recommendation, 58, "                "))
		}
	}
	var failed []string
	for _, st := range r.Steps {
		if st.Status == "error" {
			failed = append(failed, fmt.Sprintf("%s (%s)", st.Title, st.Error))
		}
	}
	if len(failed) > 0 {
		fmt.Fprintln(&b, "\nSteps that failed")
		for _, f := range failed {
			fmt.Fprintf(&b, "  ✖ %s\n", f)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func optional(s string, ok bool) string {
	if ok {
		return s
	}
	return ""
}

func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	var lines []string
	var cur string
	for _, w := range words {
		if len(cur)+len(w)+1 > width && cur != "" {
			lines = append(lines, cur)
			cur = w
			continue
		}
		if cur != "" {
			cur += " "
		}
		cur += w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n"+indent)
}
