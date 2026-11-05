package analysis

import (
	"fmt"
	"math"

	"github.com/agoodley/pathgauge/internal/results"
)

func cdpFindings(c *results.CDP, id string) []results.Finding {
	set := c.Settings
	rate := fmt.Sprintf("%.0f Mbit/s of changed data (bursts of %.0f Mbit/s for %.0f s every %.0f s)", set.RateMbps, c.PeakMbps, set.BurstS, set.PeriodS)
	var out []results.Finding
	switch c.Verdict {
	case "good":
		out = append(out, results.Finding{ID: "cdp.rpo.ok", Severity: "good", Confidence: "high",
			Title:    fmt.Sprintf("CDP replication keeps well within a %.0f s RPO (worst lag %.1f s)", set.RPOs, c.MaxLagS),
			Detail:   fmt.Sprintf("Replicating %s, the replica was never more than %.1f s behind.", rate, c.MaxLagS),
			Evidence: []string{id}})
	case "acceptable":
		out = append(out, results.Finding{ID: "cdp.rpo.tight", Severity: "info", Confidence: "high",
			Title:          fmt.Sprintf("CDP meets the %.0f s RPO, with little margin (worst lag %.1f s)", set.RPOs, c.MaxLagS),
			Detail:         fmt.Sprintf("Replicating %s, the lag peaked at %.1f s during write bursts. Busier periods or a slower link would break the RPO.", rate, c.MaxLagS),
			Recommendation: "Leave headroom: plan for bursts above the average change rate, or set the RPO with margin over the worst lag.",
			Evidence:       []string{id}})
	default:
		sev := "warning"
		if c.WithinRPOPct < 50 {
			sev = "critical"
		}
		carried := ""
		if c.SustainedMbps > 0 {
			carried = fmt.Sprintf(" While changes were waiting, the path carried about %.0f Mbit/s.", c.SustainedMbps)
		}
		rec := fmt.Sprintf("Give CDP at least the burst rate (%.0f Mbit/s) between the sites, raise the RPO to at least %.0f s, or reduce the change rate (exclude busy disks or VMs from the policy).",
			c.PeakMbps, math.Ceil(c.MaxLagS))
		if set.LimitMbps > 0 && c.SustainedMbps >= set.LimitMbps*0.9 {
			rec = fmt.Sprintf("The %.0f Mbit/s throttle is the limit: raise it, or raise the RPO to at least %.0f s.", set.LimitMbps, math.Ceil(c.MaxLagS))
		}
		out = append(out, results.Finding{ID: "cdp.rpo.missed", Severity: sev, Confidence: "high",
			Title: fmt.Sprintf("CDP falls behind: the replica lagged up to %.1f s, past the %.0f s RPO for %.0f s", c.MaxLagS, set.RPOs, c.OverRPOS),
			Detail: fmt.Sprintf("Replicating %s, the lag stayed within the RPO only %.0f%% of the time.%s",
				rate, c.WithinRPOPct, carried),
			Recommendation: rec, Evidence: []string{id}})
	}
	if c.IdleRTT.N > 0 && c.LoadedRTT.N > 0 {
		if rise := c.LoadedRTT.P95 - c.IdleRTT.Median; rise > 50 {
			out = append(out, results.Finding{ID: "cdp.latency", Severity: "warning", Confidence: "medium",
				Title:          fmt.Sprintf("CDP replication slows other traffic: latency rose by %.0f ms (p95) while it ran", rise),
				Detail:         fmt.Sprintf("Round trip %.0f ms idle, %.0f ms (p95) during replication: CDP fills the bottleneck queue, so calls and interactive traffic wait behind it.", c.IdleRTT.Median, c.LoadedRTT.P95),
				Recommendation: "Set a replication throttle (a Veeam network traffic rule) a little below the link rate, prioritise interactive traffic, or enable smart queue management on the link (see the sqm test).",
				Evidence:       []string{id}})
		}
	}
	return out
}
