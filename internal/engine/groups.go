package engine

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
)

// A test-list entry joining tests with "+" (for example
// "voip+cdp-up+tcp-down") is a group: its tests run at the same time, each
// for the group duration, so the report shows what happens to each under
// the others' load. Unless NoBaseline is set, each test first runs on its
// own for the same time, so the two can be compared.

// notInGroups lists tests that can't share the path with others: the SQM
// tuner changes the interface's shaping, and the capacity searches run
// their own sequence of loads.
var notInGroups = []string{"sqm", "capacity-voice", "capacity-video"}

// GroupMembers splits a test-list entry into its tests.
func GroupMembers(item string) []string {
	return strings.Split(item, "+")
}

// IsGroup reports whether a test-list entry is a group.
func IsGroup(item string) bool { return strings.Contains(item, "+") }

func checkGroup(item string) error {
	m := GroupMembers(item)
	if len(m) < 2 {
		return fmt.Errorf("group %q needs at least two tests", item)
	}
	seen := map[string]bool{}
	for _, t := range m {
		switch {
		case !slices.Contains(AllTests, t):
			return fmt.Errorf("unknown test %q in group %q (available: %s)", t, item, strings.Join(AllTests, ", "))
		case slices.Contains(notInGroups, t):
			return fmt.Errorf("%s can't run in a group (it changes shaping or runs its own loads)", t)
		case seen[t]:
			return fmt.Errorf("%s appears twice in group %q", t, item)
		}
		seen[t] = true
	}
	return nil
}

// groupDuration is how long each test in a group runs.
func groupDuration(o Options) time.Duration {
	if o.GroupDurationS > 0 {
		return time.Duration(o.GroupDurationS * float64(time.Second))
	}
	return max(15*time.Second, time.Duration(float64(30*time.Second)*o.Scale))
}

// runGroup runs one group: optionally each test alone, then all together.
func runGroup(ctx context.Context, c *Client, item, gid string, o Options, st *state, rep *results.Report, index, total int) {
	members := GroupMembers(item)
	d := groupDuration(o)
	og := o
	og.groupDur = d
	// Progress may now come from several goroutines at once.
	var pmu sync.Mutex
	progress := o.Progress
	og.Progress = func(e Event) {
		pmu.Lock()
		defer pmu.Unlock()
		progress(e)
	}
	rep.Groups = append(rep.Groups, results.Group{ID: gid, Tests: members, DurationS: d.Seconds(), Baseline: !o.NoBaseline})

	if !o.NoBaseline {
		for _, t := range members {
			if ctx.Err() != nil {
				return
			}
			steps := runStep(ctx, c, t, og, st, rep, index, total, fmt.Sprintf("%s-alone", gid))
			for i := range steps {
				steps[i].Group, steps[i].Phase = gid, "alone"
			}
			rep.Steps = append(rep.Steps, steps...)
			sleepCtx(ctx, time.Second)
		}
	}
	if ctx.Err() != nil {
		return
	}
	// Together: each test gets its own copy of the run state.
	out := make([][]results.Step, len(members))
	copies := make([]*state, len(members))
	var wg sync.WaitGroup
	for i, t := range members {
		cp := *st
		cp.tcpMbps = maps.Clone(st.tcpMbps)
		copies[i] = &cp
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			out[i] = runStep(ctx, c, t, og, copies[i], rep, index, total, fmt.Sprintf("%s-together", gid))
		}(i, t)
	}
	wg.Wait()
	for i := range members {
		for j := range out[i] {
			out[i][j].Group, out[i][j].Phase = gid, "together"
		}
		rep.Steps = append(rep.Steps, out[i]...)
	}
}

// runStep runs one test and returns its step (two for trace/mtr, which
// report both directions). prefix, if set, goes before the step's name.
func runStep(ctx context.Context, c *Client, t string, o Options, st *state, rep *results.Report, index, total int, prefix string) []results.Step {
	id := fmt.Sprintf("s%02d-%s", index, t)
	if prefix != "" {
		id = fmt.Sprintf("s%02d-%s-%s", index, prefix, t)
	}
	step := results.Step{ID: id, Test: testKind(t), Title: Titles[t], StartedAt: time.Now().UTC()}
	o.Progress(Event{Type: "step_start", RunID: rep.ID, Step: step.ID, Test: t, Title: step.Title, Index: index, Total: total})
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
	var steps []results.Step
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
			steps = append(steps, s)
		}
	} else {
		steps = append(steps, step)
	}
	o.Progress(Event{Type: "step_done", RunID: rep.ID, Step: step.ID, Test: t, Title: step.Title, Index: index, Total: total, Status: step.Status, Error: step.Error, Seconds: step.Seconds, Data: step.Result})
	return steps
}
