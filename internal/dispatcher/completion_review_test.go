package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestCompletionPreviewBridgeRetainsReviewWithinFrameBudget(t *testing.T) {
	s, runner, store, graph := automaticRetryFixture(t, 0, "")
	ctx := context.Background()
	base := projectPath(graph.Project.ID)
	var steps []board.Intent
	for n := 0; n < 5; n++ {
		var step board.Intent
		if err := s.Client.Do(ctx, "POST", base+"/intents", map[string]any{"from": []string{"origin"}, "description": fmt.Sprintf("%d%s", n, strings.Repeat("d", 16000)), "creator": "user"}, &step, nil); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, step)
	}
	fact := board.FactRecord{ID: "f001", Description: "Retained fixture observation", Status: "valid", Scope: "fixture", Evidence: []board.EvidenceRef{}}
	for n := 0; n < 4; n++ {
		fact.Evidence = append(fact.Evidence, board.EvidenceRef{RunID: "observation", Path: fmt.Sprintf("retained/%d.txt", n), Excerpt: strings.Repeat("e", 8192)})
	}
	// Seed previously retained evidence without starting an unrelated Worker;
	// the actual preview still traverses Dispatcher, HTTP, and SQLite.
	if err := store.Do(ctx, func(tx *board.Tx) error {
		current, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		current.Facts = append(current.Facts, board.Fact{ID: fact.ID, Description: fact.Description})
		if err = tx.Save(current); err != nil {
			return err
		}
		data, err := json.Marshal(map[string]any{"facts": []board.FactRecord{fact}})
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,1,1) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data", graph.Project.ID, string(data))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lease := Lease{Run: "retry-fixture@completion-review", Kind: "reason"}
	if err := s.Client.Do(ctx, "POST", base+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var state board.State
	if err := s.Client.Do(ctx, "GET", base+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if err := board.ValidateContextCapacity(state); err != nil {
		t.Fatalf("large but legitimate Steps exceeded admission: %v", err)
	}
	job := worker.Job{RunID: "completion-review", Kind: "reason", Graph: state.Graph, State: &state, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{MaxIntents: 3}, Decision: &board.DecisionContext{Version: 2, StateVersion: board.DecisionStateVersion(state)}}
	raw, _ := json.Marshal(job)
	execution := board.Execution{ProjectID: graph.Project.ID, ID: job.RunID, Namespace: "xloom", Backend: "retry-fixture", Kind: "reason", Lease: lease.Run, Job: raw, RetryKey: "reason:completion-review"}
	if err := s.Client.Do(ctx, "POST", base+"/executions", execution, nil, &lease); err != nil {
		t.Fatal(err)
	}
	batch := board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion}
	for _, step := range steps {
		payload, _ := json.Marshal(map[string]any{"action": "abandon", "id": step.ID, "reason": strings.Repeat("r", 8000)})
		batch.Actions = append(batch.Actions, board.DecisionAction{Op: "step", Payload: payload})
	}
	batch.Actions = append(batch.Actions, board.DecisionAction{Op: "complete", Payload: json.RawMessage(`{"from":["f001"],"description":"Proposed fixture proof"}`)})
	var full board.DecisionReceipt
	if err := s.Client.Do(ctx, "POST", base+"/state/decisions/preview", batch, &full, &lease); err != nil {
		t.Fatal(err)
	}
	request := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "decision_preview", Batch: &batch}
	frameSize := func(receipt board.DecisionReceipt) int {
		t.Helper()
		result, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := json.Marshal(worker.GraphResponse{RequestID: request.RequestID, Result: result})
		if err != nil {
			t.Fatal(err)
		}
		return len(frame)
	}
	if frameSize(full) <= worker.MaxGraphRPCBytes {
		t.Fatal("fixture did not expose the oversized projected abandon results")
	}
	withoutReview := full
	withoutReview.CompletionReview = nil
	if frameSize(withoutReview) > worker.MaxGraphRPCBytes {
		t.Fatal("fixture should fit before adding completion review")
	}
	result, err := runner.handler(ctx, job, request)
	if err != nil {
		t.Fatal(err)
	}
	compact := result.(board.DecisionReceipt)
	if compact.Results != nil || compact.Committed || compact.Completed || compact.ValidationScope != "protocol_only" || compact.ChangedActions != len(batch.Actions) || !reflect.DeepEqual(compact.IDs, full.IDs) || !reflect.DeepEqual(compact.CompletionReview, full.CompletionReview) || len(compact.CompletionReview.FactRecords) != 1 || !reflect.DeepEqual(compact.CompletionReview.FactRecords[0], fact) {
		t.Fatalf("bridge discarded review data or claimed acceptance: %+v", compact)
	}
	if size := frameSize(compact); size > worker.MaxGraphRPCBytes {
		t.Fatalf("completion preview remains undeliverable: %d bytes", size)
	}
	var after board.State
	if err := s.Client.Do(ctx, "GET", base+"/state", nil, &after, nil); err != nil || !reflect.DeepEqual(after, state) {
		t.Fatalf("preview published its simulated abandonment or completion: %v", err)
	}
}
