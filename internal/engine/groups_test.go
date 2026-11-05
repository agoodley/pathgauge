package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/agoodley/pathgauge/internal/results"
)

func TestGroupValidation(t *testing.T) {
	if _, err := (Options{Tests: []string{"info", "voip+cdp-up+tcp-down"}}).TestList(); err != nil {
		t.Fatalf("valid group refused: %v", err)
	}
	for _, bad := range []string{"voip+sqm", "voip+capacity-voice", "voip+nonsense", "voip+voip", "voip+"} {
		if _, err := (Options{Tests: []string{bad}}).TestList(); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestLoopbackGroupRunsAloneThenTogether(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for ~20 s")
	}
	c := dial(t, startEndpoint(t))
	rep, err := Run(context.Background(), c, Options{Tests: []string{"latency+voip+cdp-up"}, GroupDurationS: 3,
		CDP: results.CDPSettings{RateMbps: 50, BurstS: 1, PeriodS: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Groups) != 1 || rep.Groups[0].ID != "g1" || !rep.Groups[0].Baseline || len(rep.Groups[0].Tests) != 3 {
		t.Fatalf("groups: %+v", rep.Groups)
	}
	phases := map[string]int{}
	for _, s := range rep.Steps {
		if s.Status != "ok" {
			t.Errorf("%s: %s %s", s.ID, s.Status, s.Error)
		}
		if s.Group != "g1" {
			t.Errorf("%s not marked as grouped", s.ID)
		}
		phases[s.Phase]++
		if s.Test == "cdp" && s.Result.(*results.CDP).Settings.DurationS != 3 {
			t.Errorf("CDP in a group must run for the group duration")
		}
	}
	if phases["alone"] != 3 || phases["together"] != 3 {
		t.Errorf("phases %v", phases)
	}
	if !strings.Contains(rep.Steps[3].ID, "g1-together") {
		t.Errorf("step ids: %s", rep.Steps[3].ID)
	}
}
