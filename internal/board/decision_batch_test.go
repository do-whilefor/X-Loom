package board

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestDecisionBatchSavepointProtectsCallerThatHandlesFailure(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.Do(context.Background(), func(tx *Tx) error {
		fence := ExecutionFence{Run: "planner@batch-run", Lease: "reason"}
		g := Graph{Project: Project{ID: "project", Title: "Fixture", Status: "active", CreatedAt: tx.Now, Reason: &Reason{Worker: fence.Run, Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}}, Facts: []Fact{{ID: "origin", Description: "Fixture input"}, {ID: "goal", Description: "Inspect fixture"}}, Intents: []Intent{}, Hints: []Hint{}}
		if err := tx.Save(g); err != nil {
			return err
		}
		state, err := tx.State(g.Project.ID)
		if err != nil {
			return err
		}
		job, _ := json.Marshal(map[string]any{"kind": "reason", "run_id": "batch-run", "graph": g, "state": state, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(state)}, "budget": map[string]int{"max_intents": 3}})
		if err = tx.RegisterExecution(Execution{ProjectID: g.Project.ID, ID: "batch-run", Namespace: "fixture", Backend: "planner", Kind: "reason", Lease: fence.Run, Job: job, RetryKey: "reason:fixture"}); err != nil {
			return err
		}
		batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(state), Actions: []DecisionAction{
			{Op: "goal", Ref: "temporary", Payload: json.RawMessage(`{"action":"add","condition":"Temporary condition"}`)},
			{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["missing"],"description":"Invalid source"}`)},
		}}
		if _, err = tx.CommitDecision(g.Project.ID, fence, batch); err == nil {
			t.Fatal("invalid batch succeeded")
		}
		if tx.inDecisionBatch {
			t.Fatal("batch authorization escaped its call")
		}
		current, err := tx.State(g.Project.ID)
		if err != nil {
			return err
		}
		if DecisionStateVersion(current) != batch.ExpectedVersion || current.Revision != state.Revision {
			t.Fatal("handled failure left partial changes inside caller transaction")
		}
		batch.Actions = batch.Actions[:1]
		batch.Actions = append(batch.Actions, DecisionAction{Op: "goal", Ref: "same", Payload: batch.Actions[0].Payload})
		preview, err := tx.PreviewDecision(g.Project.ID, fence, batch)
		if err != nil {
			return err
		}
		if preview.Committed || preview.ChangedActions != 1 || len(preview.Results) != 2 || !preview.Results[1].Unchanged {
			t.Fatalf("preview changed-action count includes a duplicate: %+v", preview)
		}
		current, err = tx.State(g.Project.ID)
		if err != nil {
			return err
		}
		if current.Revision != state.Revision || len(current.Goals) != 1 {
			t.Fatal("preview left staged goals or events")
		}
		result, err := tx.CommitDecision(g.Project.ID, fence, batch)
		if err != nil {
			return err
		}
		if !result.Committed || result.IDs["temporary"] != "g001" || result.ChangedActions != 1 {
			t.Fatalf("rollback consumed alias ID allocation: %+v", result)
		}
		saved, err := tx.DecisionReceipt(g.Project.ID, fence)
		if err != nil {
			return err
		}
		if !saved.Committed || saved.ChangedActions != result.ChangedActions || len(saved.Results) != 2 {
			t.Fatalf("durable receipt lost changed-action count: %+v", saved)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
