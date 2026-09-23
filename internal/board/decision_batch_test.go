package board

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestDecisionBatchReusesPersistedTransitionsThroughCompletion(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, 2)
	batch := DecisionBatch{ExpectedVersion: f.version, Actions: []DecisionAction{
		{Op: "step", Payload: json.RawMessage(`{"action":"abandon","id":"active","reason":"finish this round"}`)},
		{Op: "goal", Ref: "child", Payload: json.RawMessage(`{"action":"add","condition":"Review alternate path"}`)},
		{Op: "step", Ref: "work", Payload: json.RawMessage(`{"action":"add","goal_id":"$child","from":["source000"],"description":"Check alternate path"}`)},
		{Op: "step", Payload: json.RawMessage(`{"action":"abandon","id":"$work","reason":"alternative ruled out"}`)},
		{Op: "goal", Payload: json.RawMessage(`{"action":"withdraw","id":"$child","reason":"alternative ruled out"}`)},
		{Op: "complete", Payload: json.RawMessage(`{"from":["source001"],"description":"Fixture reviewed"}`)},
	}}
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		preview, err := tx.PreviewDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err != nil {
			return err
		}
		if preview.Committed || preview.ChangedActions != 6 || preview.CompletionReview == nil {
			t.Fatalf("preview did not see preceding actions: %+v", preview)
		}
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		if DecisionStateVersion(state) != f.version || state.Revision != 0 {
			t.Fatal("preview escaped its savepoint")
		}
		result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err != nil {
			return err
		}
		if !result.Completed || !result.Committed || result.ChangedActions != 6 {
			t.Fatalf("completion did not observe the resolved Step and Goal: %+v", result)
		}
		state, err = tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		if state.Graph.Project.Reason != nil || state.Graph.Project.Status != "completed" || state.Revision != 6 || result.StateVersion != DecisionStateVersion(state) {
			t.Fatalf("batch result differs from persisted final state: %+v", result)
		}
		replay, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err == nil && (replay.StateVersion != result.StateVersion || replay.IDs["work"] != result.IDs["work"]) {
			t.Fatal("receipt replay changed batch")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionBatchLaterActionsObserveInvalidatedSources(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, 2)
	batch := DecisionBatch{ExpectedVersion: f.version, Actions: []DecisionAction{
		{Op: "fact_relation", Payload: json.RawMessage(`{"kind":"refutes","source":"source001","target":"source000","reason":"corrected observation"}`)},
		{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["source000"],"description":"Must reject invalidated input"}`)},
	}}
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 409 {
			t.Fatalf("stale batch source accepted: %v", err)
		}
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		if DecisionStateVersion(state) != f.version || len(state.FactRelations) != 0 || state.Revision != 0 {
			t.Fatal("rejected dependent action left a relation or revision")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionBatchSQLFailureDiscardsReusableStateAndCounters(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, 2)
	batch := f.batch(2)
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec(`CREATE TRIGGER reject_second_batch_event BEFORE INSERT ON xloom_state_events WHEN NEW.revision=2 BEGIN SELECT RAISE(ABORT,'event unavailable'); END`); err != nil {
			return err
		}
		if _, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch); err == nil {
			t.Fatal("injected SQL failure accepted")
		}
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM xloom_state_events)+(SELECT COUNT(*) FROM xloom_state_actions)").Scan(&count); err != nil {
			return err
		}
		if count != 0 || state.Revision != 0 || DecisionStateVersion(state) != f.version || state.Graph.Project.Reason == nil {
			t.Fatal("failure retained graph, event, receipt or released lease")
		}
		if _, err := tx.Exec("DROP TRIGGER reject_second_batch_event"); err != nil {
			return err
		}
		result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err == nil && (result.IDs["step0"] != "i017" || result.IDs["step1"] != "i018") {
			t.Fatalf("failed batch consumed counters: %+v", result)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

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
