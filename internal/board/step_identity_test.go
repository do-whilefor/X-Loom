package board

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type planFixture struct {
	t     *testing.T
	store *Store
	fence ExecutionFence
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	f := &planFixture{t: t, store: store, fence: ExecutionFence{Run: "planner@first", Lease: "reason"}}
	f.tx(func(tx *Tx) error {
		return tx.Save(Graph{
			Project: Project{ID: "proj_001", Title: "Plan fixture", Status: "active", CreatedAt: tx.Now, Reason: &Reason{Worker: f.fence.Run, StartedAt: tx.Now, Heartbeat: tx.Now}},
			Facts:   []Fact{{ID: "origin", Description: "Synthetic input"}, {ID: "goal", Description: "Synthetic goal"}, {ID: "f001", Description: "First observation"}, {ID: "f002", Description: "Second observation"}},
		})
	})
	return f
}

func (f *planFixture) tx(fn func(*Tx) error) {
	f.t.Helper()
	if err := f.store.Do(context.Background(), fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *planFixture) state() State {
	f.t.Helper()
	var state State
	f.tx(func(tx *Tx) (err error) { state, err = tx.State("proj_001"); return err })
	return state
}

func (f *planFixture) action(op, key string, payload any) StateActionResult {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	var result StateActionResult
	f.tx(func(tx *Tx) (err error) {
		result, err = tx.StateAction("proj_001", f.fence, StateAction{Op: op, IdempotencyKey: key, Payload: raw})
		return err
	})
	return result
}

func (f *planFixture) planner(run string) {
	f.t.Helper()
	f.fence.Run = "planner@" + run
	f.tx(func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		g.Project.Reason = &Reason{Worker: f.fence.Run, StartedAt: tx.Now, Heartbeat: tx.Now}
		return tx.Save(g)
	})
}

func stepInput(description string, from []string, goal string, priority int) map[string]any {
	return map[string]any{"action": "add", "description": description, "from": from, "goal_id": goal, "priority": priority}
}

func TestRepeatedPlanDoesNotCreateOrReprioritizeStep(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		return tx.RegisterExecution(Execution{ProjectID: "proj_001", ID: "first", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.fence.Run, RetryKey: "reason:first", Job: json.RawMessage(`{"budget":{"max_intents":1}}`)})
	})
	input := stepInput("Inspect the two observations", []string{"f001", "f002"}, "", 7)
	first := f.action("step", "first-key", input)
	before := f.state()
	// The exact request can be retried after its own mutation changed the view.
	replay := f.action("step", "first-key", input)
	if replay.ID != first.ID || replay.Revision != first.Revision {
		t.Fatalf("same-key replay changed its receipt: %+v", replay)
	}
	for _, anotherRun := range []bool{false, true} {
		if anotherRun {
			f.planner("second")
		}
		repeated := f.action("step", f.fence.Run+":repeat", stepInput("  Inspect the two observations\n", []string{"f002", "f001"}, "goal", 99))
		if repeated.ID != first.ID || !repeated.Unchanged || repeated.Revision != first.Revision || repeated.StateVersion != first.StateVersion {
			t.Fatalf("duplicate plan changed the graph: %+v", repeated)
		}
		var step Step
		if err := json.Unmarshal(repeated.Result, &step); err != nil {
			t.Fatal(err)
		}
		if step.Priority != 7 || step.Status != "open" {
			t.Fatalf("repeated add changed existing task: %+v", step)
		}
	}
	after := f.state()
	if len(after.Steps) != 1 || len(after.Graph.Intents) != 1 || after.Revision != before.Revision {
		t.Fatalf("duplicate add consumed a direction or revision: %+v", after)
	}
	f.tx(func(tx *Tx) error {
		events, err := tx.StateEvents("proj_001", 0)
		if err == nil && len(events) != 1 {
			t.Fatalf("duplicate add emitted events: %+v", events)
		}
		return err
	})
}

func TestRepeatedPlanPreservesExistingStepState(t *testing.T) {
	for _, status := range []string{"running", "completed", "abandoned", "failed"} {
		t.Run(status, func(t *testing.T) {
			f := newPlanFixture(t)
			input := stepInput("Observe fixture", []string{"origin"}, "goal", 0)
			first := f.action("step", "add", input)
			if status == "abandoned" {
				f.action("step", "abandon", map[string]any{"action": "abandon", "id": first.ID, "reason": "No longer needed"})
			} else {
				f.tx(func(tx *Tx) error {
					g, err := tx.Load("proj_001")
					if err != nil {
						return err
					}
					g.Intents[0].Worker = Ptr("executor@attempt")
					if status == "completed" {
						g.Intents[0].To, g.Intents[0].ConcludedAt = Ptr("f001"), Ptr(tx.Now)
					}
					if err = tx.Save(g); err != nil || status != "failed" {
						return err
					}
					e := Execution{ProjectID: "proj_001", ID: "attempt", Namespace: "test", Backend: "executor", Kind: "explore", Intent: first.ID, Lease: "executor@attempt", RetryKey: "explore:" + first.ID, Job: json.RawMessage(`{}`)}
					if err = tx.RegisterExecution(e); err != nil {
						return err
					}
					return tx.ExecutionStatus(e, "failed", json.RawMessage(`{"error":"incomplete fixture"}`))
				})
			}
			before := f.state()
			f.planner("next")
			repeated := f.action("step", "repeated-add", input)
			var step Step
			if err := json.Unmarshal(repeated.Result, &step); err != nil {
				t.Fatal(err)
			}
			if !repeated.Unchanged || repeated.ID != first.ID || repeated.Revision != before.Revision || step.Status != status || len(f.state().Steps) != 1 {
				t.Fatalf("repeated plan restarted %s step: %+v %+v", status, repeated, step)
			}
			if status != "failed" {
				return
			}
			// Deduplication must preserve the existing one-use human retry path.
			f.tx(func(tx *Tx) error {
				g, err := tx.Load("proj_001")
				if err != nil {
					return err
				}
				g.Intents[0].Worker = Ptr("executor@retry")
				if err = tx.Save(g); err != nil {
					return err
				}
				e := Execution{ProjectID: "proj_001", ID: "retry", Namespace: "test", Backend: "executor", Kind: "explore", Intent: first.ID, Lease: "executor@retry", RetryKey: "explore:" + first.ID, Job: json.RawMessage(`{}`)}
				var api *APIError
				if err = tx.RegisterExecution(e); !errors.As(err, &api) || api.Status != 409 {
					t.Fatalf("retry without authorization: %v", err)
				}
				previous, err := tx.Execution("proj_001", "attempt")
				if err != nil {
					return err
				}
				if err = tx.ExecutionStatus(previous, "retry_requested", previous.Result); err != nil {
					return err
				}
				e.Job = json.RawMessage(`{"previous_run_id":"attempt"}`)
				return tx.RegisterExecution(e)
			})
		})
	}
}

func TestPlanIdentityAllowsNewEvidenceGoalsAndRounds(t *testing.T) {
	f := newPlanFixture(t)
	first := f.action("step", "first", stepInput("Observe fixture", []string{"f001"}, "goal", 0))
	goal := f.action("goal", "child", map[string]any{"action": "add", "condition": "Verify another objective"})
	for key, input := range map[string]any{
		"new-evidence": stepInput("Observe fixture", []string{"f001", "f002"}, "goal", 0),
		"new-goal":     stepInput("Observe fixture", []string{"f001"}, goal.ID, 0),
		"new-task":     stepInput("Observe a different fixture", []string{"f001"}, "goal", 0),
	} {
		if added := f.action("step", key, input); added.ID == first.ID || added.Unchanged {
			t.Fatalf("distinct task %s was suppressed: %+v", key, added)
		}
	}
	input := stepInput("Observe initial input", []string{"origin"}, "goal", 0)
	old := f.action("step", "before-restart", input)
	f.tx(func(tx *Tx) error { _, err := tx.RestartProject("proj_001", nil); return err })
	f.planner("new-round")
	added := f.action("step", "after-restart", input)
	if added.ID == old.ID || added.Unchanged || len(f.state().Steps) != 1 || f.state().Graph.Project.Generation != 1 {
		t.Fatalf("previous round blocked fresh planning: %+v", added)
	}
}

func TestRepeatedGoalDoesNotDuplicateItsSteps(t *testing.T) {
	f := newPlanFixture(t)
	first := f.action("goal", "goal-first", map[string]any{"action": "add", "condition": "Verify fixture"})
	step := f.action("step", "step-first", stepInput("Observe fixture", []string{"origin"}, first.ID, 4))
	f.planner("next")
	repeated := f.action("goal", "goal-next", map[string]any{"action": "add", "condition": " Verify fixture\n", "parent_id": "goal"})
	if repeated.ID != first.ID || !repeated.Unchanged {
		t.Fatalf("repeated goal was recreated: %+v", repeated)
	}
	repeatedStep := f.action("step", "step-next", stepInput("Observe fixture", []string{"origin"}, repeated.ID, 5))
	if repeatedStep.ID != step.ID || !repeatedStep.Unchanged || len(f.state().Goals) != 2 || len(f.state().Steps) != 1 {
		t.Fatalf("repeated nested plan created another task: %+v", repeatedStep)
	}
	otherParent := f.action("goal", "other-parent", map[string]any{"action": "add", "condition": "Another branch"})
	child := f.action("goal", "other-child", map[string]any{"action": "add", "condition": "Verify fixture", "parent_id": otherParent.ID})
	if child.ID == first.ID || child.Unchanged {
		t.Fatalf("same condition under a different parent was suppressed: %+v", child)
	}
}
