// Package analysis turns raw results into findings, a health score and the
// link fingerprint (docs/PLAN.md §10). Rules are plain Go so they can be
// unit-tested with fixtures.
package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/agoodley/pathgauge/internal/hostinfo"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// Expectations are optional targets for a link (contracted speeds, SLA).
type Expectations struct {
	DownMbps  float64 `json:"down_mbps,omitempty"`
	UpMbps    float64 `json:"up_mbps,omitempty"`
	MaxRTTms  float64 `json:"max_rtt_ms,omitempty"`
	MaxLoss   float64 `json:"max_loss_pct,omitempty"`
	MaxJitter float64 `json:"max_jitter_ms,omitempty"`
}

// view gives typed access to a report's results.
type view struct {
	r *results.Report
}

func (v view) tcp(dir results.Direction, single bool) (*results.TCP, string) {
	var best *results.TCP
	id := ""
	for _, s := range v.r.StepsOf("tcp") {
		t, ok := s.Result.(*results.TCP)
		if !ok || t.Direction != dir || t.Compressible || (t.Streams == 1) != single {
			continue
		}
		if best == nil || t.Mbps > best.Mbps {
			best, id = t, s.ID
		}
	}
	return best, id
}

func first[T any](v view, test string) (*T, string) {
	for _, s := range v.r.StepsOf(test) {
		if t, ok := s.Result.(*T); ok {
			return t, s.ID
		}
	}
	return nil, ""
}

func all[T any](v view, test string) ([]*T, []string) {
	var out []*T
	var ids []string
	for _, s := range v.r.StepsOf(test) {
		if t, ok := s.Result.(*T); ok {
			out = append(out, t)
			ids = append(ids, s.ID)
		}
	}
	return out, ids
}

// dirName gives friendly direction names.
func dirName(r *results.Report, d results.Direction) string {
	c, e := r.Labels.Client, r.Labels.Endpoint
	if c == "" {
		c = "client"
	}
	if e == "" {
		e = "endpoint"
	}
	switch d {
	case results.Up:
		return fmt.Sprintf("upload (%s → %s)", c, e)
	case results.Down:
		return fmt.Sprintf("download (%s → %s)", e, c)
	}
	return string(d)
}

// Analyze fills r.Findings and r.Summary.
func Analyze(r *results.Report, exp Expectations) {
	// Steps run together in a group were measured under load: the ordinary
	// rules ignore them, and loadComparisons covers them instead.
	v := view{withoutLoaded(r)}
	var f []results.Finding
	add := func(x results.Finding) { f = append(f, x) }
	sum := results.Summary{ScoreParts: map[string]float64{}, Fingerprint: map[string]string{}}

	// ---- headline numbers ----
	for _, d := range []results.Direction{results.Down, results.Up} {
		multi, _ := v.tcp(d, false)
		single, _ := v.tcp(d, true)
		var m, s float64
		if multi != nil {
			m = multi.Mbps
		}
		if single != nil {
			s = single.Mbps
		}
		if d == results.Down {
			sum.DownMbps, sum.DownSingleMbps = math.Max(m, s), s
		} else {
			sum.UpMbps, sum.UpSingleMbps = math.Max(m, s), s
		}
	}
	lat, latID := first[results.Latency](v, "latency")
	if lat != nil {
		sum.IdleRTTms, sum.JitterMs, sum.LossPct = lat.UDP.Median, lat.UDPJitterMs, stats.Round(lat.Loss.LossPct, 3)
	}
	bloat, bloatID := first[results.Bufferbloat](v, "bufferbloat")
	if bloat != nil {
		sum.BloatGrade, sum.RPM = bloat.Grade, bloat.RPM
	}
	voips, voipIDs := all[results.VoIP](v, "voip")
	for _, vp := range voips {
		if !vp.Loaded {
			sum.MOS = math.Min(vp.Up.MOS, vp.Down.MOS)
		}
	}
	pm, pmID := first[results.PMTU](v, "pmtu")
	if pm != nil {
		sum.PathMTU = minPositive(pm.Up.MTU, pm.Down.MTU)
	}
	integ, integID := first[results.Integrity](v, "integrity")
	if integ != nil {
		sum.NAT = integ.NAT
		sum.HopsUp, sum.HopsDown = integ.HopsUp, integ.HopsDown
	}
	traces, traceIDs := all[results.Trace](v, "trace")
	for _, t := range traces {
		n := len(t.Hops)
		if t.Direction == results.Up && n > 0 {
			sum.HopsUp = n
		}
		if t.Direction == results.Down && n > 0 {
			sum.HopsDown = n
		}
	}
	info, _ := first[results.Info](v, "info")

	// ---- bufferbloat ----
	if bloat != nil {
		for _, ph := range []string{"down", "up"} {
			g, ok := bloat.Grades[ph]
			if !ok {
				continue
			}
			d := results.Direction(ph)
			inc := bloat.IncreaseMs[ph]
			rate := phaseRate(bloat, ph)
			capacity := math.Max(rate, tcpMbps(sum, d))
			partial := ""
			if rate > 0 && rate < 0.5*capacity {
				partial = fmt.Sprintf(" The busy phase reached only %.0f of the %.0f Mbit/s measured with parallel streams, so latency at full load may be higher still.", rate, capacity)
			}
			switch stats.GradeRank(g) {
			case 0, 1:
			case 2:
				add(results.Finding{ID: "bufferbloat." + ph, Severity: "info", Confidence: "high",
					Title:          fmt.Sprintf("Mild bufferbloat on %s (grade %s)", dirName(r, d), g),
					Detail:         fmt.Sprintf("Latency rises by %.0f ms (95th percentile) while the %s is busy.%s", inc, dirName(r, d), partial),
					Recommendation: sqmAdvice(capacity), Evidence: []string{bloatID}})
			default:
				sev := "warning"
				if stats.GradeRank(g) >= 4 {
					sev = "critical"
				}
				add(results.Finding{ID: "bufferbloat." + ph, Severity: sev, Confidence: "high",
					Title: fmt.Sprintf("Bufferbloat on %s (grade %s)", dirName(r, d), g),
					Detail: fmt.Sprintf("While the %s is busy, latency rises by %.0f ms at the 95th percentile (idle median %.1f ms). A buffer of roughly %s fills at the bottleneck, so calls, games and interactive apps stall whenever the link is in use.%s",
						dirName(r, d), inc, idleMedian(bloat), human(bloat.BufferBytes[ph]), partial),
					Recommendation: sqmAdvice(capacity), Evidence: []string{bloatID}})
			}
		}
		if g := bloat.Grade; g != "" && stats.GradeRank(g) <= 1 {
			add(results.Finding{ID: "bufferbloat.good", Severity: "good", Confidence: "high",
				Title:  fmt.Sprintf("Latency stays low under load (grade %s, %.0f RPM)", g, bloat.RPM),
				Detail: "Saturating the link barely affects latency, so the bottleneck queue is well managed.", Evidence: []string{bloatID}})
		}
		switch bloat.Queueing {
		case "fifo":
			if stats.GradeRank(bloat.Grade) >= 2 {
				add(results.Finding{ID: "queue.fifo", Severity: "info", Confidence: "medium",
					Title:          "The bottleneck uses a single shared queue",
					Detail:         "Latency of a separate light flow rose as much as latency inside the loaded flows, so every flow waits in the same FIFO queue.",
					Recommendation: "A flow-queuing AQM (fq_codel or CAKE) at the bottleneck isolates light flows from bulk transfers.", Evidence: []string{bloatID}})
			}
		case "flow-queued":
			add(results.Finding{ID: "queue.flow-queued", Severity: "good", Confidence: "medium",
				Title:  "Flow queuing detected at the bottleneck",
				Detail: "Bulk flows queued, but a separate light flow stayed fast: something like fq_codel or CAKE is isolating flows.", Evidence: []string{bloatID}})
		}
	}

	// ---- per-flow limits and TCP behaviour ----
	for _, d := range []results.Direction{results.Down, results.Up} {
		multi, mid := v.tcp(d, false)
		single, sid := v.tcp(d, true)
		if multi != nil && single != nil && multi.Mbps > 0 && stillRamping(single) {
			add(results.Finding{ID: "perflow.ramp." + string(d), Severity: "info", Confidence: "medium",
				Title:          fmt.Sprintf("The single-stream %s test ended before TCP reached full speed", dirName(r, d)),
				Detail:         fmt.Sprintf("Its throughput was still rising at the end (%.0f ms RTT over %.0f s, about %.0f round trips), so %.1f Mbit/s understates what one connection can reach on this path.", single.RTT.Median, single.DurationS, single.DurationS*1000/math.Max(single.RTT.Median, 1), single.Mbps),
				Recommendation: "Run a longer test (for example --scale 2) to measure single-stream throughput on long paths.", Evidence: []string{sid}})
		} else if multi != nil && single != nil && multi.Mbps > 0 {
			ratio := single.Mbps / multi.Mbps
			if ratio < 0.5 {
				fd := results.Finding{ID: "perflow.limit." + string(d), Severity: "warning", Confidence: "high",
					Title:    fmt.Sprintf("One connection gets only %.0f%% of the %s capacity", 100*ratio, dirName(r, d)),
					Detail:   fmt.Sprintf("A single TCP stream reached %.1f Mbit/s, but %d streams together reached %.1f Mbit/s. Single downloads, backups and file copies will be slower than the link allows.", single.Mbps, multi.Streams, multi.Mbps),
					Evidence: []string{sid, mid}}
				rtt := single.MinRTTms
				if rtt <= 0 {
					rtt = single.RTT.Median
				}
				bdp := multi.Mbps * 1e6 / 8 * rtt / 1000
				switch {
				case single.RwndLimitedPct > 30 || single.SndbufLimitedPct > 30:
					fd.Detail += fmt.Sprintf(" The sending socket was limited by the receive window %.0f%% and by its send buffer %.0f%% of the time.", single.RwndLimitedPct, single.SndbufLimitedPct)
					fd.Recommendation = fmt.Sprintf("Socket buffers are the limit, not the network. The bandwidth-delay product is about %s at %.1f ms; raise the third value of net.ipv4.tcp_rmem (receiver) and net.ipv4.tcp_wmem (sender) to at least %s on both hosts, for example with: sudo pathgauge tune --rate %.0f --rtt %.0f --apply (or sysctl -w net.ipv4.tcp_rmem='4096 131072 %d' net.ipv4.tcp_wmem='4096 16384 %d').",
						human(bdp), rtt, human(2*bdp), multi.Mbps, math.Ceil(rtt), nextPow2(int64(2*bdp)), nextPow2(int64(2*bdp)))
					fd.ID = "perflow.window." + string(d)
				case single.RetransPct > 0.5 && single.Mbps <= mathisSlack*stats.Mathis(float64(max(single.MSS, 1200)), rtt/1000, single.RetransPct/100)/1e6:
					// Loss only explains the cap when the stream's speed is
					// consistent with TCP's loss response. A stream running
					// far above the Mathis bound is losing bursts at a queue
					// it fills itself (a per-flow shaper or policer), not
					// suffering random loss, so it falls through.
					bound := stats.Mathis(float64(max(single.MSS, 1200)), rtt/1000, single.RetransPct/100) / 1e6
					fd.Detail += fmt.Sprintf(" The stream retransmitted %.2f%% of its segments; at that loss rate TCP theory (Mathis) caps one stream near %.0f Mbit/s.", single.RetransPct, bound)
					fd.Recommendation = "Loss limits single streams. Find its source (see the loss and path findings); BBR congestion control is less sensitive to random loss than CUBIC."
					fd.ID = "perflow.loss." + string(d)
				case single.Mbps > 5000:
					fd.Severity, fd.Confidence = "info", "medium"
					fd.Title = fmt.Sprintf("One connection reaches %s on %s; more streams reach %s", fmtRate(single.Mbps), dirName(r, d), fmtRate(multi.Mbps))
					fd.Detail += " At these rates one TCP stream is usually limited by a single CPU core on the hosts rather than by the network."
					fd.Recommendation = "Expected on fast hosts. Use parallel streams for bulk transfers; check per-core CPU and IRQ affinity if single-stream speed matters."
					fd.ID = "host.singlecore." + string(d)
				default:
					fd.Recommendation = "The per-stream cap points to per-flow policing or shaping in the path (common on VPNs, cloud egress and some ISPs). Use parallel transfers, or ask the provider about per-flow limits."
					fd.ID = "perflow.policer." + string(d)
				}
				add(fd)
			}
		}
		if multi != nil && multi.RetransPct > 2 {
			add(results.Finding{ID: "tcp.retrans." + string(d), Severity: "warning", Confidence: "medium",
				Title:          fmt.Sprintf("High TCP retransmissions on %s (%.1f%%)", dirName(r, d), multi.RetransPct),
				Detail:         "Retransmissions at this level mean packets are being lost or badly reordered under load.",
				Recommendation: "Compare with the UDP loss and rate-sweep results: loss only near capacity is normal congestion; loss at low rates points to a faulty link.", Evidence: []string{mid}})
		}
		if multi != nil && multi.RampS > 3 && multi.DurationS >= 8 {
			add(results.Finding{ID: "tcp.slowramp." + string(d), Severity: "info", Confidence: "medium",
				Title:  fmt.Sprintf("TCP takes %.1f s to reach full speed on %s", multi.RampS, dirName(r, d)),
				Detail: "Short transfers will not reach the link's full rate. This is typical of long round-trip times or early loss during slow start.", Evidence: []string{mid}})
		}
		if exp.DownMbps > 0 && d == results.Down && multi != nil && multi.Mbps < 0.85*exp.DownMbps {
			add(results.Finding{ID: "sla.down", Severity: "warning", Confidence: "high",
				Title:  fmt.Sprintf("Download below expectation: %.1f of %.1f Mbit/s (%.0f%%)", multi.Mbps, exp.DownMbps, 100*multi.Mbps/exp.DownMbps),
				Detail: "Measured with parallel TCP streams, which normally reach the contracted rate.", Evidence: []string{mid}})
		}
		if exp.UpMbps > 0 && d == results.Up && multi != nil && multi.Mbps < 0.85*exp.UpMbps {
			add(results.Finding{ID: "sla.up", Severity: "warning", Confidence: "high",
				Title:  fmt.Sprintf("Upload below expectation: %.1f of %.1f Mbit/s (%.0f%%)", multi.Mbps, exp.UpMbps, 100*multi.Mbps/exp.UpMbps),
				Detail: "Measured with parallel TCP streams, which normally reach the contracted rate.", Evidence: []string{mid}})
		}
	}
	if sum.DownMbps > 0 && sum.UpMbps > 0 {
		ratio := sum.DownMbps / sum.UpMbps
		if ratio > 8 || ratio < 0.125 {
			add(results.Finding{ID: "asymmetric.capacity", Severity: "info", Confidence: "high",
				Title:  fmt.Sprintf("Strongly asymmetric capacity (%.1f down / %.1f up Mbit/s)", sum.DownMbps, sum.UpMbps),
				Detail: "Typical of ADSL, DOCSIS cable and some wireless links. The slower direction is where queues and bufferbloat usually build."})
		}
	}

	// ---- host limits ----
	if info != nil {
		hostFindings(r, info, sum, add)
	}

	// ---- loss, jitter, reordering ----
	if lat != nil {
		if lat.Loss.LossPct > 0.5 {
			add(results.Finding{ID: "loss.idle", Severity: severity(lat.Loss.LossPct, 2), Confidence: "high",
				Title:          fmt.Sprintf("Packet loss on an idle path (%.2f%%)", lat.Loss.LossPct),
				Detail:         fmt.Sprintf("%d of %d light probes were lost while nothing else was running (%d separate loss bursts, longest %d packets). Loss without load is not congestion.", lat.Loss.Lost, lat.Loss.Sent, lat.Loss.Bursts, lat.Loss.MaxBurst),
				Recommendation: "Check for physical-layer problems: cabling, optics, Wi-Fi signal, or a faulty link (see the path and interface findings).", Evidence: []string{latID}})
		}
		if lat.UDPJitterMs > 10 {
			add(results.Finding{ID: "jitter.high", Severity: severity(lat.UDPJitterMs, 30), Confidence: "high",
				Title:  fmt.Sprintf("High jitter on an idle path (%.1f ms)", lat.UDPJitterMs),
				Detail: "Round-trip times vary a lot even without load, which hurts voice and video. Common on Wi-Fi, cellular and congested shared links.", Evidence: []string{latID}})
		}
		if lat.ICMP != nil && lat.UDP.Median > 0 && lat.ICMP.Median > 1.5*lat.UDP.Median+2 {
			add(results.Finding{ID: "icmp.deprioritised", Severity: "info", Confidence: "medium",
				Title:  "ICMP (ping) is slower than other traffic",
				Detail: fmt.Sprintf("Ping median %.1f ms vs UDP %.1f ms: something on the path deprioritises ICMP, so ping alone would overstate latency.", lat.ICMP.Median, lat.UDP.Median), Evidence: []string{latID}})
		}
		if info != nil && !info.OWDTrustworthy {
			add(results.Finding{ID: "clock.owd", Severity: "info", Confidence: "high",
				Title:          "One-way delay not reported",
				Detail:         "At least one host's clock is not synchronised closely enough, so only round-trip times are shown. Jitter and queueing measurements are unaffected.",
				Recommendation: "Enable chrony (or PTP) on both hosts to get one-way delay per direction."})
		}
	}
	udps, udpIDs := all[results.UDP](v, "udp")
	for i, u := range udps {
		if u.Loss.ReorderPct > 1 {
			add(results.Finding{ID: "reorder." + string(u.Direction), Severity: "warning", Confidence: "high",
				Title:          fmt.Sprintf("Packet reordering on %s (%.1f%%)", dirName(r, u.Direction), u.Loss.ReorderPct),
				Detail:         "Packets arrive out of order, which TCP can mistake for loss. Usually caused by per-packet load balancing or link bonding.",
				Recommendation: "Ask the provider whether per-packet load balancing is used; per-flow hashing avoids reordering.", Evidence: []string{udpIDs[i]}})
		}
		if u.Loss.LossPct > 1 && u.TargetMbps < 0.6*tcpMbps(sum, u.Direction) {
			fd := results.Finding{ID: "loss.udp." + string(u.Direction), Severity: severity(u.Loss.LossPct, 3), Confidence: "medium",
				Title:  fmt.Sprintf("UDP loss of %.1f%% on %s well below capacity", u.Loss.LossPct, dirName(r, u.Direction)),
				Detail: fmt.Sprintf("A %.1f Mbit/s UDP stream lost packets although TCP reaches %.1f Mbit/s: UDP may be policed, or the link drops packets without congestion.", u.TargetMbps, tcpMbps(sum, u.Direction)), Evidence: []string{udpIDs[i]}}
			if u.Loss.Bursts == 1 {
				// One burst is a transient (a queue overflowing as the
				// stream started, a Wi-Fi hiccup) rather than steady loss.
				fd.Severity, fd.Confidence = "warning", "low"
				fd.Detail = fmt.Sprintf("A %.1f Mbit/s UDP stream lost %d packets in a single burst. One burst points to a transient event (a buffer overflowing as the stream started, or a Wi-Fi interruption) rather than steady loss; repeat the test to confirm.", u.TargetMbps, u.Loss.MaxBurst)
			}
			add(fd)
		}
	}

	// ---- sweeps ----
	sweeps, sweepIDs := all[results.Sweep](v, "sweep")
	for i, s := range sweeps {
		sev, title := "info", ""
		switch s.Signature {
		case "policer":
			title = fmt.Sprintf("A policer limits %s to about %.0f Mbit/s", dirName(r, s.Direction), s.KneeMbps)
		case "shaper":
			title = fmt.Sprintf("A shaper limits %s to about %.0f Mbit/s", dirName(r, s.Direction), s.CapacityMbps)
		case "bufferbloat":
			title, sev = fmt.Sprintf("Deep buffer at the %s bottleneck (%.0f ms of queue)", dirName(r, s.Direction), s.MaxQueueMs), "warning"
		case "burst-allowance":
			title = fmt.Sprintf("Burst allowance on %s, then about %.0f Mbit/s sustained", dirName(r, s.Direction), s.CapacityMbps)
		case "non-congestive-loss":
			title, sev = fmt.Sprintf("Loss at every rate on %s (not congestion)", dirName(r, s.Direction)), "warning"
		default:
			continue
		}
		add(results.Finding{ID: "rate." + s.Signature + "." + string(s.Direction), Severity: sev, Confidence: "medium", Title: title, Detail: s.SignatureDetail, Evidence: []string{sweepIDs[i]}})
	}
	sizes, sizeIDs := all[results.SizeSweep](v, "sizesweep")
	for i, s := range sizes {
		if len(s.Points) >= 2 {
			small, large := s.Points[0], s.Points[len(s.Points)-1]
			if small.LossPct > 2 && large.LossPct < 0.5 {
				add(results.Finding{ID: "pps.limit." + string(s.Direction), Severity: "warning", Confidence: "medium",
					Title:          fmt.Sprintf("Small packets are dropped on %s (packet-rate limit near %.0f pps)", dirName(r, s.Direction), s.MaxPPS),
					Detail:         fmt.Sprintf("At the same bit rate, %d-byte packets lost %.1f%% while %d-byte packets lost %.1f%%. A device on the path is limited by packets per second rather than bandwidth (common with firewalls and small routers).", small.Size, small.LossPct, large.Size, large.LossPct),
					Recommendation: "This affects VoIP, gaming and DNS-heavy traffic. Check firewall/router CPU and fast-path offload settings.", Evidence: []string{sizeIDs[i]}})
			}
		}
	}

	// ---- path ----
	// End-to-end loss from PathGauge's own probes decides whether hop loss
	// is real: routers (and the destination) rate-limit ICMP.
	type rlAgg struct {
		hops     map[int]bool
		evidence []string
	}
	rateLimited := map[results.Direction]*rlAgg{}
	e2eLoss := -1.0
	if lat != nil {
		e2eLoss = lat.Loss.LossPct
	}
	for _, u := range udps {
		e2eLoss = math.Max(e2eLoss, u.Loss.LossPct)
	}
	for i, t := range traces {
		if t.LossFromHop > 0 && e2eLoss >= 0 && e2eLoss < 0.3 {
			// No loss end to end: treat the hop loss as rate limiting.
			t.RateLimitedHops = append(t.RateLimitedHops, t.LossFromHop)
			t.LossFromHop = 0
		}
		if t.LossFromHop > 0 {
			h := t.Hops[t.LossFromHop-1]
			who := h.Addr
			if h.Name != "" {
				who = h.Name + " (" + h.Addr + ")"
			}
			if h.Org != "" {
				who += ", " + h.Org
			}
			add(results.Finding{ID: "path.loss." + string(t.Direction), Severity: "warning", Confidence: "medium",
				Title:          fmt.Sprintf("Loss starts at hop %d on the %s path", t.LossFromHop, pathName(t.Direction)),
				Detail:         fmt.Sprintf("Loss first appears at hop %d (%s, %.1f%%) and continues to the end of the path, so it is real loss rather than routers ignoring probes.", t.LossFromHop, who, h.LossPct),
				Recommendation: "Raise it with whoever operates that hop, including this report and its timestamps as evidence.", Evidence: []string{traceIDs[i]}})
		}
		if len(t.RateLimitedHops) > 0 {
			rl := rateLimited[t.Direction]
			if rl == nil {
				rl = &rlAgg{hops: map[int]bool{}}
				rateLimited[t.Direction] = rl
			}
			for _, h := range t.RateLimitedHops {
				rl.hops[h] = true
			}
			rl.evidence = append(rl.evidence, traceIDs[i])
		}
		for _, h := range t.Hops {
			if h.CGNAT {
				add(results.Finding{ID: "nat.cgnat", Severity: "info", Confidence: "medium",
					Title:  "Carrier-grade NAT (CGNAT) on the path",
					Detail: fmt.Sprintf("Hop %d uses the shared address space 100.64.0.0/10, typical of carrier-grade NAT. Inbound connections and some peer-to-peer apps will not work.", h.TTL), Evidence: []string{traceIDs[i]}})
				break
			}
		}
	}
	// One "harmless loss" finding per direction, merging traceroute and MTR.
	for _, d := range []results.Direction{results.Up, results.Down} {
		rl := rateLimited[d]
		if rl == nil {
			continue
		}
		var hops []int
		for h := range rl.hops {
			hops = append(hops, h)
		}
		sort.Ints(hops)
		word := "hop"
		if len(hops) > 1 {
			word = "hops"
		}
		add(results.Finding{ID: "path.ratelimit." + string(d), Severity: "info", Confidence: "high",
			Title:  fmt.Sprintf("Apparent loss at %s %s on %s is harmless", word, joinInts(hops), pathName(d)),
			Detail: "These routers drop some trace probes, but later hops show no matching loss, so traffic passing through is unaffected. Routers commonly rate-limit the ICMP replies traceroute relies on.", Evidence: rl.evidence})
	}
	var fwd, rev *results.Trace
	for _, t := range traces {
		if t.Direction == results.Up {
			fwd = t
		} else {
			rev = t
		}
	}
	if fwd != nil && rev != nil && len(fwd.Hops) > 0 && len(rev.Hops) > 0 {
		if d := len(fwd.Hops) - len(rev.Hops); d > 2 || d < -2 {
			add(results.Finding{ID: "path.asymmetric", Severity: "info", Confidence: "medium",
				Title:  fmt.Sprintf("Asymmetric routing (%d hops out, %d hops back)", len(fwd.Hops), len(rev.Hops)),
				Detail: "Traffic takes different routes in each direction. This is common on the internet, but it means problems can affect one direction only."})
		}
	}

	// ---- MTU and middleboxes ----
	if pm != nil {
		for _, d := range []results.PMTUDir{pm.Up, pm.Down} {
			if d.Blackhole {
				add(results.Finding{ID: "mtu.blackhole." + string(d.Direction), Severity: "critical", Confidence: "medium",
					Title:          fmt.Sprintf("Path MTU black hole on %s (MTU %d)", pathName(d.Direction), d.MTU),
					Detail:         fmt.Sprintf("Packets larger than %d bytes are silently dropped, with no 'fragmentation needed' message. Some connections will hang after connecting (web pages that never load, stalled VPNs and transfers).", d.MTU),
					Recommendation: "Allow ICMP type 3 code 4 (and ICMPv6 Packet Too Big) through firewalls, or clamp TCP MSS on the router at the tunnel or link with the smaller MTU.", Evidence: []string{pmID}})
			} else if d.MTU > 0 && d.MTU < 1500 && d.LocalMTU >= 1500 {
				add(results.Finding{ID: "mtu.reduced." + string(d.Direction), Severity: "info", Confidence: "high",
					Title:  fmt.Sprintf("Reduced path MTU on %s (%d bytes)", pathName(d.Direction), d.MTU),
					Detail: "Something on the path (a tunnel, VPN, PPPoE or similar) carries smaller packets than Ethernet's 1500 bytes. It works because MTU discovery messages get through.", Evidence: []string{pmID}})
			}
		}
	}
	if integ != nil {
		if integ.MSSClampedUp || integ.MSSClampedDown {
			add(results.Finding{ID: "mss.clamp", Severity: "info", Confidence: "medium",
				Title:  "TCP MSS is rewritten in transit",
				Detail: fmt.Sprintf("Each end advertised an MSS of about %d, but the other end was told less (%d / %d). A router or VPN clamps MSS, usually to fit a smaller MTU.", integ.ClientAdvMSS, integ.EndpointSeesMSS, integ.ClientSeesMSS), Evidence: []string{integID}})
		}
		if len(integ.StrippedOptions) > 0 {
			add(results.Finding{ID: "tcp.options.stripped", Severity: "warning", Confidence: "medium",
				Title:          "TCP options are removed in transit: " + strings.Join(integ.StrippedOptions, ", "),
				Detail:         "Both hosts enable these options, but the connection did not negotiate them, so a middlebox strips them. Losing window scaling or SACK severely limits throughput on long paths.",
				Recommendation: "Check firewalls and load balancers for TCP normalisation or option-stripping features.", Evidence: []string{integID}})
		}
		for _, dd := range []struct {
			dir results.Direction
			res []results.DSCPResult
		}{{results.Up, integ.DSCPUp}, {results.Down, integ.DSCPDown}} {
			var bleached, remarked []string
			for _, x := range dd.res {
				switch x.Status {
				case "bleached":
					bleached = append(bleached, x.Name)
				case "remarked":
					remarked = append(remarked, fmt.Sprintf("%s→%d", x.Name, x.Received))
				}
			}
			if len(bleached) > 0 {
				add(results.Finding{ID: "qos.bleached." + string(dd.dir), Severity: "info", Confidence: "high",
					Title:          fmt.Sprintf("DSCP QoS markings are cleared on %s", pathName(dd.dir)),
					Detail:         fmt.Sprintf("%d of %d markings arrive as best effort (%s). Any QoS policy that relies on DSCP after this point will not see voice or video markings.", len(bleached), len(dd.res)-1, strings.Join(bleached, ", ")),
					Recommendation: "Normal across the public internet. On private WANs, check the provider's QoS trust settings.", Evidence: []string{integID}})
			}
			if len(remarked) > 0 {
				add(results.Finding{ID: "qos.remarked." + string(dd.dir), Severity: "info", Confidence: "high",
					Title:  fmt.Sprintf("DSCP markings are rewritten on %s", pathName(dd.dir)),
					Detail: "Rewritten: " + strings.Join(remarked, ", ") + ". A QoS policy on the path maps classes.", Evidence: []string{integID}})
			}
		}
		if integ.NAT {
			add(results.Finding{ID: "nat.detected", Severity: "info", Confidence: "high",
				Title:  "The client is behind NAT",
				Detail: fmt.Sprintf("The client's address %s appears to the endpoint as %s. Tests worked because the client opens every connection.", integ.ClientLocal, integ.ClientObserved), Evidence: []string{integID}})
		}
	}

	// ---- voice ----
	for i, vp := range voips {
		label := "Voice calls"
		if vp.Loaded {
			label = "Voice calls under load"
		}
		worst := vp.Up
		wd := results.Up
		if vp.Down.MOS < vp.Up.MOS {
			worst, wd = vp.Down, results.Down
		}
		switch vp.Verdict {
		case "good":
			if !vp.Loaded {
				add(results.Finding{ID: "voip.good", Severity: "good", Confidence: "high",
					Title:  fmt.Sprintf("%s: good quality (MOS %.1f)", label, math.Min(vp.Up.MOS, vp.Down.MOS)),
					Detail: "A simulated G.711 call in both directions scores as good quality.", Evidence: []string{voipIDs[i]}})
			}
		default:
			sev := "warning"
			if vp.Verdict == "acceptable" {
				sev = "info"
			}
			add(results.Finding{ID: "voip." + vp.Verdict + map[bool]string{true: ".loaded", false: ""}[vp.Loaded], Severity: sev, Confidence: "high",
				Title:          fmt.Sprintf("%s: %s quality (MOS %.1f on %s)", label, vp.Verdict, worst.MOS, pathName(wd)),
				Detail:         fmt.Sprintf("Loss %.2f%%, late packets %.1f%%, jitter %.1f ms, one-way delay ~%.0f ms.", worst.Loss.LossPct, worst.LateLossPct, worst.JitterMs, vp.OneWayMs),
				Recommendation: "Prioritise voice (DSCP EF) and fix bufferbloat; voice suffers most from queues building behind bulk traffic.", Evidence: []string{voipIDs[i]}})
		}
	}

	// ---- traffic mix and call capacity ----
	mixes, mixIDs := all[results.Mix](v, "mix")
	for i, m := range mixes {
		for _, x := range mixFindings(m, mixIDs[i]) {
			add(x)
		}
	}
	cdps, cdpIDs := all[results.CDP](v, "cdp")
	for i, c := range cdps {
		for _, x := range cdpFindings(c, cdpIDs[i]) {
			add(x)
		}
	}
	caps, capIDs := all[results.Capacity](v, "capacity")
	for i, cp := range caps {
		for _, x := range capacityFindings(cp, capIDs[i]) {
			add(x)
		}
	}

	// ---- comparisons ----
	if cc, ccID := first[results.CCCompare](v, "cc"); cc != nil {
		var cubic, bbr float64
		for _, p := range cc.Points {
			switch p.Algorithm {
			case "cubic":
				cubic = p.Mbps
			case "bbr":
				bbr = p.Mbps
			}
		}
		if cubic > 0 && bbr > 1.3*cubic {
			add(results.Finding{ID: "cc.bbr", Severity: "info", Confidence: "medium",
				Title:          fmt.Sprintf("BBR is %.0f%% faster than CUBIC on this path", 100*(bbr/cubic-1)),
				Detail:         fmt.Sprintf("Single-stream upload: BBR %.1f vs CUBIC %.1f Mbit/s. Loss-based CUBIC backs off on packet loss that is not congestion.", bbr, cubic),
				Recommendation: "Consider net.ipv4.tcp_congestion_control=bbr (with net.core.default_qdisc=fq) on hosts that send over this path.", Evidence: []string{ccID}})
		}
	}
	if q, qID := first[results.QoS](v, "qos"); q != nil && len(q.Classes) == 3 && q.Classes[2].Increase >= 10 {
		if q.Effective {
			add(results.Finding{ID: "qos.priority", Severity: "good", Confidence: "medium", Title: "Voice priority (DSCP EF) works under load", Detail: q.Verdict, Evidence: []string{qID}})
		} else {
			add(results.Finding{ID: "qos.ineffective", Severity: "warning", Confidence: "medium", Title: "Voice priority (DSCP EF) has no effect under load", Detail: q.Verdict,
				Recommendation: "If calls matter, configure priority queuing for EF on the bottleneck router (or trust DSCP from the provider), or remove the bufferbloat that delays everything.", Evidence: []string{qID}})
		}
	}
	if disc, dID := first[results.Discrimination](v, "discrimination"); disc != nil && !strings.HasPrefix(disc.Verdict, "no discrimination") {
		add(results.Finding{ID: "throttle", Severity: "warning", Confidence: "medium", Title: "Traffic is treated differently by type",
			Detail: disc.Verdict + ".", Evidence: []string{dID}})
	}
	if sqm, sqmID := first[results.SQMTuner](v, "sqm"); sqm != nil && sqm.Recommended != nil && stats.GradeRank(sqm.BaselineGrade) <= 1 {
		add(results.Finding{ID: "sqm.notneeded", Severity: "good", Confidence: "high",
			Title:  fmt.Sprintf("SQM is not needed on this link (already grade %s)", sqm.BaselineGrade),
			Detail: fmt.Sprintf("Latency under load is already low without shaping; shaping to %.0f%% of line rate would only cost throughput.", sqm.Recommended.Percent), Evidence: []string{sqmID}})
	} else if sqm != nil && sqm.Recommended != nil {
		add(results.Finding{ID: "sqm.recommended", Severity: "info", Confidence: "high",
			Title:          fmt.Sprintf("SQM at %.0f%% of line rate gives grade %s", sqm.Recommended.Percent, sqm.Recommended.Grade),
			Detail:         fmt.Sprintf("Shaping to %.1f down / %.1f up Mbit/s with %s brought latency under load to grade %s (baseline grade %s).", sqm.Recommended.DownMbps, sqm.Recommended.UpMbps, sqm.Recommended.Qdisc, sqm.Recommended.Grade, sqm.BaselineGrade),
			Recommendation: "Apply these settings on the router at this link's bottleneck: " + strings.Join(sqm.TCCommands, "; "), Evidence: []string{sqmID}})
	}

	// ---- score ----
	score(&sum, f, bloat, lat, traces, pm, integ, udps)
	fingerprint(&sum, v, bloat, sweeps, pm, integ, traces)
	if len(f) == 0 || countSev(f, "warning", "critical") == 0 {
		f = append(f, results.Finding{ID: "overall.healthy", Severity: "good", Confidence: "medium",
			Title: "No problems detected", Detail: "Every test completed without finding a problem."})
	}
	sort.SliceStable(f, func(i, j int) bool { return sevRank(f[i].Severity) < sevRank(f[j].Severity) })
	lc, lf := loadComparisons(r)
	r.LoadComparisons = lc
	f = append(f, lf...)
	r.Findings, r.Summary = f, sum
}

func fmtRate(m float64) string {
	if m >= 1000 {
		return fmt.Sprintf("%.1f Gbit/s", m/1000)
	}
	return fmt.Sprintf("%.0f Mbit/s", m)
}

// mathisSlack is how far above the Mathis bound a single stream may run and
// still be called loss-limited. CUBIC with SACK beats Reno's response
// function a little, and segment retransmissions overstate the loss-event
// rate, so the bound is loose; real loss-limited streams land within about
// 1-3x of it, while streams that fill a per-flow queue run 20x or more above.
const mathisSlack = 4

// stillRamping reports whether a TCP test's throughput was still clearly
// rising at the end (it never left slow start / window growth).
func stillRamping(t *results.TCP) bool {
	n := len(t.Intervals)
	if n < 8 {
		return false
	}
	avg := func(ivs []results.TCPInterval) float64 {
		var s float64
		for _, iv := range ivs {
			s += iv.Mbps
		}
		return s / float64(len(ivs))
	}
	mid := avg(t.Intervals[n/3 : 2*n/3])
	last := avg(t.Intervals[3*n/4:])
	return last > 1.25*mid && t.RetransPct < 0.1
}

func pathName(d results.Direction) string {
	if d == results.Up {
		return "the outbound path"
	}
	return "the return path"
}

func tcpMbps(s results.Summary, d results.Direction) float64 {
	if d == results.Up {
		return s.UpMbps
	}
	return s.DownMbps
}

func phaseRate(b *results.Bufferbloat, ph string) float64 {
	for _, p := range b.Phases {
		if p.Name == ph {
			if ph == "up" {
				return p.UpMbps
			}
			return p.DownMbps
		}
	}
	return 0
}

func idleMedian(b *results.Bufferbloat) float64 {
	for _, p := range b.Phases {
		if p.Name == "idle" {
			return p.RTT.Median
		}
	}
	return 0
}

func sqmAdvice(rate float64) string {
	if rate <= 0 {
		return "Enable smart queue management (CAKE or fq_codel) on the router at the bottleneck, shaped slightly below the line rate."
	}
	return fmt.Sprintf("Enable smart queue management (CAKE, or fq_codel) on the router at the bottleneck, shaped to about %.0f Mbit/s (90-95%% of the measured %.0f Mbit/s). Run the SQM tuner to confirm the best rate.", rate*0.92, rate)
}

func severity(x, critical float64) string {
	if x >= critical {
		return "critical"
	}
	return "warning"
}

func sevRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	case "info":
		return 2
	}
	return 3
}

func countSev(f []results.Finding, sev ...string) int {
	n := 0
	for _, x := range f {
		for _, s := range sev {
			if x.Severity == s {
				n++
			}
		}
	}
	return n
}

func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, ", ")
}

func human(b float64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f bytes", b)
}

func nextPow2(n int64) int64 {
	p := int64(1 << 20)
	for p < n {
		p <<= 1
	}
	return p
}

// hostFindings flags host limits so the network is not blamed for them.
func hostFindings(r *results.Report, info *results.Info, sum results.Summary, add func(results.Finding)) {
	for _, h := range []struct {
		name string
		rep  *hostinfo.Report
	}{{"client", info.ClientHost}, {"endpoint", info.EndpointHost}} {
		if h.rep == nil {
			continue
		}
		for _, ifc := range h.rep.Interfaces {
			if ifc.OperState != "up" || ifc.Virtual {
				continue
			}
			if ifc.Wireless && h.name == "client" {
				add(results.Finding{ID: "host.wifi", Severity: "info", Confidence: "medium",
					Title:  fmt.Sprintf("The %s is on Wi-Fi (%s)", h.name, ifc.Name),
					Detail: "Results include the wireless hop, which often limits throughput and adds jitter. Test from a wired connection to isolate the wider network."})
			}
			if ifc.SpeedMbps > 0 {
				limit := float64(ifc.SpeedMbps) * 0.94
				if sum.DownMbps > 0.9*limit || sum.UpMbps > 0.9*limit {
					add(results.Finding{ID: "host.nic", Severity: "info", Confidence: "medium",
						Title:  fmt.Sprintf("Throughput is at the %s's %d Mbit/s link speed", h.name, ifc.SpeedMbps),
						Detail: fmt.Sprintf("Interface %s negotiated %d Mbit/s, so the network may be capable of more than measured.", ifc.Name, ifc.SpeedMbps)})
				}
				if ifc.SpeedMbps < 1000 && ifc.SpeedMbps > 0 && !ifc.Wireless {
					add(results.Finding{ID: "host.linkspeed", Severity: "warning", Confidence: "medium",
						Title:          fmt.Sprintf("The %s's interface %s runs at only %d Mbit/s", h.name, ifc.Name, ifc.SpeedMbps),
						Detail:         "Gigabit Ethernet normally negotiates 1000 Mbit/s; a lower speed often means a damaged cable, a two-pair cable or a port problem.",
						Recommendation: "Check the cable and switch port, and the negotiated speed with: ethtool " + ifc.Name})
				}
			}
		}
	}
}

// score computes the 0-100 health score from weighted components.
func score(sum *results.Summary, f []results.Finding, bloat *results.Bufferbloat, lat *results.Latency, traces []*results.Trace, pm *results.PMTU, integ *results.Integrity, udps []*results.UDP) {
	parts := map[string]float64{}
	// Throughput (25): full marks unless per-flow, retransmission or SLA findings.
	tp := 25.0
	for _, x := range f {
		switch {
		case strings.HasPrefix(x.ID, "perflow.") && x.Severity != "info":
			tp -= 6
		case strings.HasPrefix(x.ID, "tcp.retrans."):
			tp -= 5
		case strings.HasPrefix(x.ID, "sla."):
			tp -= 8
		}
	}
	parts["throughput"] = math.Max(0, tp)
	// Latency and jitter (20).
	lj := 20.0
	if lat != nil {
		lj -= math.Min(12, math.Max(0, lat.UDPJitterMs-2)*0.6)
		if lat.UDP.Median > 0 {
			spread := lat.UDP.P99 - lat.UDP.Median
			lj -= math.Min(8, math.Max(0, spread-5)*0.2)
		}
	}
	parts["latency"] = math.Max(0, lj)
	// Loss (20).
	loss := 0.0
	if lat != nil {
		loss = lat.Loss.LossPct
	}
	for _, u := range udps {
		loss = math.Max(loss, u.Loss.LossPct)
	}
	parts["loss"] = math.Max(0, 20*(1-loss/2))
	// Bufferbloat (20).
	bb := 20.0
	if bloat != nil {
		bb = map[int]float64{0: 20, 1: 18, 2: 14, 3: 9, 4: 4, 5: 0}[stats.GradeRank(bloat.Grade)]
	}
	parts["bufferbloat"] = bb
	// Path (10).
	path := 10.0
	for _, t := range traces {
		if t.LossFromHop > 0 {
			path -= 5
		}
	}
	parts["path"] = math.Max(0, path)
	// Integrity (5).
	in := 5.0
	if pm != nil && (pm.Up.Blackhole || pm.Down.Blackhole) {
		in -= 3
	}
	if integ != nil && len(integ.StrippedOptions) > 0 {
		in -= 2
	}
	parts["integrity"] = math.Max(0, in)
	total := 0.0
	for k, x := range parts {
		parts[k] = stats.Round(x, 1)
		total += x
	}
	sum.ScoreParts = parts
	sum.HealthScore = int(math.Round(total))
}

func fingerprint(sum *results.Summary, v view, bloat *results.Bufferbloat, sweeps []*results.Sweep, pm *results.PMTU, integ *results.Integrity, traces []*results.Trace) {
	fp := sum.Fingerprint
	if sum.DownMbps > 0 {
		fp["Capacity down"] = fmt.Sprintf("%.1f Mbit/s", sum.DownMbps)
	}
	if sum.UpMbps > 0 {
		fp["Capacity up"] = fmt.Sprintf("%.1f Mbit/s", sum.UpMbps)
	}
	if sum.IdleRTTms > 0 {
		fp["Base RTT"] = fmt.Sprintf("%.2f ms", sum.IdleRTTms)
	}
	if bloat != nil {
		for _, ph := range []string{"down", "up"} {
			if ms, ok := bloat.BufferMs[ph]; ok {
				fp["Queue under load "+ph] = fmt.Sprintf("%.0f ms (grade %s)", ms, bloat.Grades[ph])
			}
		}
		fp["Queue type"] = bloat.Queueing
	}
	for _, s := range sweeps {
		fp["Bottleneck "+string(s.Direction)] = fmt.Sprintf("%s near %.0f Mbit/s", s.Signature, s.CapacityMbps)
	}
	if sum.DownSingleMbps > 0 && sum.DownMbps > 0 {
		fp["Single-stream share down"] = fmt.Sprintf("%.0f%%", 100*sum.DownSingleMbps/sum.DownMbps)
	}
	if sum.UpSingleMbps > 0 && sum.UpMbps > 0 {
		fp["Single-stream share up"] = fmt.Sprintf("%.0f%%", 100*sum.UpSingleMbps/sum.UpMbps)
	}
	if pm != nil {
		fp["Path MTU"] = fmt.Sprintf("%d out / %d back", pm.Up.MTU, pm.Down.MTU)
	}
	if len(traces) > 0 {
		fp["Hops"] = fmt.Sprintf("%d out / %d back", sum.HopsUp, sum.HopsDown)
	}
	if integ != nil {
		fp["NAT"] = map[bool]string{true: "yes", false: "no"}[integ.NAT]
		preserved := 0
		for _, d := range integ.DSCPUp {
			if d.Status == "preserved" {
				preserved++
			}
		}
		fp["DSCP preserved (out)"] = fmt.Sprintf("%d of %d", preserved, len(integ.DSCPUp))
		fp["ECN"] = integ.ECNUp
	}
	if cc, _ := first[results.CCCompare](v, "cc"); cc != nil && cc.Best != "" {
		fp["Best congestion control"] = cc.Best
	}
}
