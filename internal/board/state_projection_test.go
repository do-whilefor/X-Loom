package board

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Every action's receipt must describe exactly what a fresh database read sees,
// including derived support, runtime failure state and invalidated sources.
func TestStateActionProjectionMatchesPersistedState(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, 1)
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		// Preserve database ordering even after a wall clock rollback.
		if _, err := tx.Exec("UPDATE intents SET created_at='2999-01-01T00:00:00Z' WHERE project_id=? AND id='i001'", stateBenchmarkProject); err != nil {
			return err
		}
		state, data, err := tx.stateAndData(stateBenchmarkProject)
		if err != nil {
			return err
		}
		actions := []struct {
			op, payload string
			fence       ExecutionFence
		}{
			{"fact", `{"description":"New observation","scope":"fixture","observed_at":"2020-01-01T00:00:00Z","evidence":[{"run_id":"worker@benchmark","path":"evidence.txt","excerpt":"retained evidence"}]}`, stateBenchmarkWorker},
			{"finding", `{"claim":"Fixture issue","scope":"fixture","status":"verified","sources":["f017"]}`, stateBenchmarkWorker},
			{"goal", `{"action":"add","condition":"Check corrected evidence"}`, stateBenchmarkPlanner},
			{"step", `{"action":"add","goal_id":"g001","from":["f017"],"description":"Check new observation"}`, stateBenchmarkPlanner},
			{"step", `{"action":"priority","id":"i017","priority":5,"reason":"important"}`, stateBenchmarkPlanner},
			{"step", `{"action":"priority","id":"i017","priority":5,"reason":"important"}`, stateBenchmarkPlanner},
			{"fact_relation", `{"kind":"refutes","source":"source001","target":"f017","reason":"corrected observation"}`, stateBenchmarkPlanner},
			{"step", `{"action":"abandon","id":"i017","reason":"invalid premise"}`, stateBenchmarkPlanner},
			{"goal", `{"action":"achieve","id":"g001","sources":["source001"],"reason":"reviewed"}`, stateBenchmarkPlanner},
		}
		for n, action := range actions {
			result, err := tx.stateAction(&state, &data, action.fence, StateAction{Op: action.op, Payload: json.RawMessage(action.payload), IdempotencyKey: fmt.Sprintf("projection:%d", n), ExpectedVersion: DecisionStateVersion(state)})
			if err != nil {
				return fmt.Errorf("action %d: %w", n, err)
			}
			persisted, err := tx.State(stateBenchmarkProject)
			if err != nil {
				return err
			}
			got, _ := json.Marshal(state)
			want, _ := json.Marshal(persisted)
			if string(got) != string(want) || result.StateVersion != DecisionStateVersion(persisted) {
				t.Fatalf("action %d (%s) projection or receipt differs from persisted state", n, action.op)
			}
			if result.Unchanged != (n == 5) {
				t.Fatalf("action %d unchanged=%v", n, result.Unchanged)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionBatchIntermediateVersionsMatchPreviewPrefixes(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, 2)
	batch := f.batch(4)
	// Include a deduplicated action: it must preserve the preceding version.
	batch.Actions[2].Payload = batch.Actions[1].Payload
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		versions := []string{}
		for n := range batch.Actions {
			prefix := DecisionBatch{ExpectedVersion: f.version, Actions: batch.Actions[:n+1]}
			preview, err := tx.PreviewDecision(stateBenchmarkProject, stateBenchmarkPlanner, prefix)
			if err != nil {
				return err
			}
			versions = append(versions, preview.StateVersion)
		}
		result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err != nil {
			return err
		}
		for n, action := range result.Results {
			if action.StateVersion != versions[n] {
				t.Fatalf("action %d lost its intermediate version", n)
			}
		}
		if versions[1] != versions[2] || result.ChangedActions != 3 {
			t.Fatal("deduplicated action changed the batch state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
