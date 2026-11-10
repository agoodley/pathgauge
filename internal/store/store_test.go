package store

import (
	"context"
	"encoding/json"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// FinishRun is how an agent's result reaches the database, so it is where
// "whose run is this, and may it still change" is enforced.
func TestFinishRunOwnershipAndState(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	report := func(tag string) json.RawMessage { return json.RawMessage(`{"tag":"` + tag + `"}`) }
	newRun := func(id, agent string) {
		t.Helper()
		if err := s.CreateRun(ctx, Run{ID: id, AgentID: agent, Profile: "quick", Options: json.RawMessage(`{}`), Status: "running", Trigger: "manual"}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(id, wantStatus, wantTag, wantErr string) {
		t.Helper()
		r, err := s.GetRun(ctx, id, true)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Tag string }
		_ = json.Unmarshal(r.Report, &got)
		if r.Status != wantStatus || got.Tag != wantTag || r.Error != wantErr {
			t.Errorf("run %s: status %q report %q error %q; want %q %q %q", id, r.Status, got.Tag, r.Error, wantStatus, wantTag, wantErr)
		}
	}
	finish := func(id, agent, tag string) bool {
		t.Helper()
		ok, err := s.FinishRun(ctx, id, agent, report(tag), RunSummary{HealthScore: 90, Findings: json.RawMessage(`[]`)})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// Another agent can't finish a run.
	newRun("r1", "agent-a")
	if finish("r1", "agent-b", "forged") {
		t.Error("an agent finished another agent's run")
	}
	check("r1", "running", "", "")
	// Its own agent can, once.
	if !finish("r1", "agent-a", "first") {
		t.Error("the owning agent could not finish its run")
	}
	check("r1", "done", "first", "")
	if finish("r1", "agent-a", "second") {
		t.Error("a stored report was replaced")
	}
	check("r1", "done", "first", "")

	// A cancelled run stays cancelled when its result turns up late.
	newRun("r2", "agent-a")
	if err := s.SetRunStatus(ctx, "r2", "cancelled", "cancelled by user"); err != nil {
		t.Fatal(err)
	}
	if finish("r2", "agent-a", "late") {
		t.Error("a late result reversed a cancellation")
	}
	check("r2", "cancelled", "", "cancelled by user")

	// A run failed by a disconnect is completed by its late result, and the
	// old error text goes; another agent still can't touch it.
	newRun("r3", "agent-a")
	if err := s.SetRunStatus(ctx, "r3", "failed", "agent disconnected during the run"); err != nil {
		t.Fatal(err)
	}
	if finish("r3", "agent-b", "forged") {
		t.Error("an agent finished another agent's failed run")
	}
	check("r3", "failed", "", "agent disconnected during the run")
	if !finish("r3", "agent-a", "late") {
		t.Error("the owning agent's late result was refused")
	}
	check("r3", "done", "late", "")

	if finish("no-such-run", "agent-a", "x") {
		t.Error("finishing a run that does not exist reported success")
	}
}
