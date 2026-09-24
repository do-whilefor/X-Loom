package board

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func invalidatePlanSource(f *planFixture) {
	f.action("fact_relation", "refute-first", map[string]any{"kind": "refutes", "source": "f002", "target": "f001", "reason": "The independent second observation disproves the initial premise"})
}

func claimedPlanExecution(f *planFixture, step string) Execution {
	e := Execution{ProjectID: "proj_001", ID: "execute-one", Namespace: "test", Backend: "runner", Kind: "explore", Intent: step, Lease: "runner@execute-one", RetryKey: "explore:" + step, Job: json.RawMessage(`{"graph":{"project":{"generation":0}}}`)}
	f.tx(func(tx *Tx) error {
		g, err := tx.Load(e.ProjectID)
		if err != nil {
			return err
		}
		for n := range g.Intents {
			if g.Intents[n].ID == step {
				g.Intents[n].Worker = Ptr(e.Lease)
			}
		}
		return tx.Save(g)
	})
	return e
}

func TestInvalidPremiseBlocksQueuedStepAndRegistration(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "plan", stepInput("Inspect one bounded premise", []string{"f001"}, "goal", 0)).ID
	invalidatePlanSource(f)
	s := f.state()
	if s.Steps[0].Status != "needs_review" || len(s.Steps[0].InvalidSources) != 1 || s.Steps[0].InvalidSources[0] != "f001" {
		t.Fatalf("missing invalidation: %+v", s.Steps[0])
	}
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.StepReady("proj_001", id) })
	if err == nil || !strings.Contains(err.Error(), "not effective evidence") {
		t.Fatalf("claim accepted invalid premise: %v", err)
	}
	e := claimedPlanExecution(f, id)
	err = f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(e) })
	if err == nil || !strings.Contains(err.Error(), "not effective evidence") {
		t.Fatalf("registration accepted invalid premise: %v", err)
	}
}

func TestStepReadyPreservesAbandonedAndMissingErrors(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "plan", stepInput("Inspect a valid premise", []string{"f001"}, "goal", 0)).ID
	f.tx(func(tx *Tx) error { return tx.StepReady("proj_001", id) })
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "This direction is no longer needed"})
	invalidatePlanSource(f)
	for _, test := range []struct {
		step   string
		status int
		detail string
	}{
		{id, 409, "Step was abandoned"},
		{"missing", 404, "Step not found"},
	} {
		err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.StepReady("proj_001", test.step) })
		var api *APIError
		if !errors.As(err, &api) || api.Status != test.status || api.Error() != test.detail {
			t.Fatalf("step %s: expected %d %q, got %v", test.step, test.status, test.detail, err)
		}
	}
}

func TestInvalidationBetweenRegistrationAndStartBlocksProcess(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "plan", stepInput("Inspect the premise", []string{"f001"}, "goal", 0)).ID
	e := claimedPlanExecution(f, id)
	f.tx(func(tx *Tx) error { return tx.RegisterExecution(e) })
	invalidatePlanSource(f)
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.ExecutionStatus(e, "running", nil) })
	if err == nil || !strings.Contains(err.Error(), "not effective evidence") {
		t.Fatalf("start accepted invalid premise: %v", err)
	}
	f.tx(func(tx *Tx) error {
		stored, err := tx.Execution(e.ProjectID, e.ID)
		if err == nil && stored.Status != "prepared" {
			t.Errorf("failed start changed status: %s", stored.Status)
		}
		return err
	})
}

func TestRunningInvalidatedStepRetainsIndependentObservationUntilCancelled(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "plan", stepInput("Observe an independent result", []string{"f001"}, "goal", 0)).ID
	e := claimedPlanExecution(f, id)
	f.tx(func(tx *Tx) error { return tx.RegisterExecution(e) })
	f.tx(func(tx *Tx) error { return tx.ExecutionStatus(e, "running", nil) })
	invalidatePlanSource(f)
	step := f.state().Steps[0]
	if step.Status != "running" || len(step.InvalidSources) != 1 {
		t.Fatalf("running inputs were hidden or silently cancelled: %+v", step)
	}
	var observed StateActionResult
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO scoped_counters(project_id,kind,value) VALUES('proj_001','fact',2) ON CONFLICT(project_id,kind) DO UPDATE SET value=MAX(value,2)")
		return err
	})
	payload := json.RawMessage(`{"description":"An independent observation remains reproducible","scope":"synthetic fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"run_id":"execute-one","path":"/runs/execute-one/evidence.txt","excerpt":"independent original evidence"}]}`)
	f.tx(func(tx *Tx) (err error) {
		observed, err = tx.StateAction(e.ProjectID, e.Fence(), StateAction{Op: "fact", IdempotencyKey: "independent", Payload: payload})
		return err
	})
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "Stop this now superseded direction"})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction(e.ProjectID, e.Fence(), StateAction{Op: "fact", IdempotencyKey: "late", Payload: payload})
		return err
	})
	if err == nil {
		t.Fatal("late evidence accepted after cancellation")
	}
	if err := f.state().ValidateFactSources([]string{observed.ID}, true); err != nil {
		t.Fatalf("independent observation was recursively invalidated: %v", err)
	}
}
