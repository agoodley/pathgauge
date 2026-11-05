package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/stats"
)

// CapacityParams controls a call-capacity search.
type CapacityParams struct {
	Kind    string        // a real-time mix kind: voice, voice-opus, video, video-hd, gaming
	Step    time.Duration // how long each count of sessions runs
	RateCap float64       // bit/s per direction (0: no cap)
	DSCP    bool
	// WithTransfers repeats the search while a download and an upload run.
	WithTransfers bool
	OnRTT         func(phase string, t, rttMs float64)
	OnStep        func(search string, s results.CapacityStep)
}

// maxCapacitySteps bounds one search.
const maxCapacitySteps = 12

// RunCapacity finds how many simultaneous sessions of one kind the path
// carries: it doubles the count until a session's quality turns poor, then
// narrows the gap.
func RunCapacity(ctx context.Context, c *Client, cp CapacityParams) (*results.Capacity, error) {
	k, ok := lookupMixKind(cp.Kind)
	if !ok || !k.realtime() {
		return nil, fmt.Errorf("capacity: %q is not a call or game session kind", cp.Kind)
	}
	if cp.Step <= 0 {
		cp.Step = 8 * time.Second
	}
	per := k.wireBps()
	res := &results.Capacity{Kind: k.Name, Label: k.Label, PerSessionKbps: stats.Round(per/1000, 1), StepS: cp.Step.Seconds()}
	maxN, reason := k.Max, "session cap"
	if cp.RateCap > 0 {
		if byRate := int(cp.RateCap / per); byRate < maxN {
			maxN, reason = byRate, "rate cap"
		}
	}
	if maxN < 1 {
		return nil, errors.New("capacity: the rate limit for this run is too low for even one session")
	}
	base, err := mixCalibrate(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("measuring the idle path: %w", err)
	}
	dl, _ := lookupMixKind("download")
	ul, _ := lookupMixKind("upload")

	search := func(name string, withBulk bool) (results.CapacitySearch, error) {
		var cs results.CapacitySearch
		test := func(n int) (results.CapacityStep, error) {
			entries := []MixEntry{{Kind: k, Count: n}}
			if withBulk {
				entries = append(entries, MixEntry{Kind: dl, Count: 1}, MixEntry{Kind: ul, Count: 1})
			}
			label := fmt.Sprintf("%d sessions", n)
			if n == 1 {
				label = "1 session"
			}
			if withBulk {
				label += " with transfers"
			}
			ph, err := runMixPhase(ctx, c, entries, cp.Step, base, MixParams{DSCP: cp.DSCP, OnRTT: cp.OnRTT}, label)
			if err != nil {
				return results.CapacityStep{}, err
			}
			st := capacityStep(k, n, ph)
			cs.Steps = append(cs.Steps, st)
			if cp.OnStep != nil {
				cp.OnStep(name, st)
			}
			sleepCtx(ctx, time.Second) // let queues drain
			return st, ctx.Err()
		}
		pass := func(s results.CapacityStep) bool { return s.Poor == 0 }
		lastPass, firstFail := 0, 0
		for n := 1; ; n *= 2 {
			if n > maxN {
				n = maxN
			}
			st, err := test(n)
			if err != nil {
				return cs, err
			}
			if !pass(st) {
				firstFail = n
				break
			}
			lastPass = n
			if n == maxN {
				cs.Limit, cs.LimitReason = n, reason
				break
			}
		}
		for firstFail > 0 && firstFail-lastPass > max(1, lastPass/8) && len(cs.Steps) < maxCapacitySteps {
			mid := (lastPass + firstFail) / 2
			st, err := test(mid)
			if err != nil {
				return cs, err
			}
			if pass(st) {
				lastPass = mid
			} else {
				firstFail = mid
			}
		}
		cs.Acceptable = lastPass
		for _, s := range cs.Steps {
			if s.Sessions <= lastPass && s.Poor == 0 && s.Acceptable == 0 && s.Sessions > cs.Good {
				cs.Good = s.Sessions
			}
		}
		return cs, nil
	}

	if res.Alone, err = search("alone", false); err != nil {
		return nil, err
	}
	if cp.WithTransfers {
		sleepCtx(ctx, 2*time.Second)
		ws, err := search("with-transfers", true)
		if err != nil {
			return nil, err
		}
		res.WithTransfers = &ws
	}
	return res, nil
}

// capacityStep summarises one count of sessions.
func capacityStep(k MixKind, n int, ph results.MixPhase) results.CapacityStep {
	st := results.CapacityStep{Sessions: n, Worst: math.Inf(1)}
	if k.Class == "rt" && !isVoice(k) {
		st.Worst = 0 // gaming: worst added delay, higher is worse
	}
	for _, s := range ph.Sessions {
		if s.Kind != k.Name {
			continue
		}
		switch s.Verdict {
		case "good":
			st.Good++
		case "acceptable":
			st.Acceptable++
		default:
			st.Poor++
		}
		for _, f := range []*results.MixFlow{s.Up, s.Down} {
			if f == nil {
				if s.Error != "" {
					st.Worst = worstFor(k, st.Worst, 0)
				}
				continue
			}
			st.LossPct = math.Max(st.LossPct, f.LossPct)
			st.AddedP95Ms = math.Max(st.AddedP95Ms, f.AddedP95Ms)
			switch {
			case isVoice(k):
				st.Worst = math.Min(st.Worst, f.MOS)
			case k.Class == "video" && f.Frames != nil:
				st.Worst = math.Min(st.Worst, f.Frames.OKPct)
			default:
				st.Worst = math.Max(st.Worst, f.AddedP95Ms)
			}
		}
	}
	if math.IsInf(st.Worst, 1) {
		st.Worst = 0
	}
	st.Worst = stats.Round(st.Worst, 2)
	switch {
	case st.Poor > 0:
		st.Verdict = "poor"
	case st.Acceptable > 0:
		st.Verdict = "acceptable"
	default:
		st.Verdict = "good"
	}
	return st
}

func isVoice(k MixKind) bool { return len(k.Name) >= 5 && k.Name[:5] == "voice" }

func worstFor(k MixKind, cur, v float64) float64 {
	if k.Class == "rt" && !isVoice(k) {
		return math.Max(cur, v)
	}
	return math.Min(cur, v)
}
