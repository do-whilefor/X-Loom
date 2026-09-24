package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/worker"
)

func TestDecisionObservationIsIndependentOfImmutableBusinessResult(t *testing.T) {
	f := newDecisionBatchFixture(t)
	path := f.base() + "/executions/" + f.run + "/observation"
	body := map[string]any{"metrics": worker.DecisionMetrics{Version: 1, ModelCalls: 2, Committed: true, Outcome: "actions_committed"}}
	f.request("POST", path, body, true, http.StatusConflict, nil)
	f.decision("commit", f.batch(batchAction("step", "next", `{"action":"add","from":["origin"],"description":"A fixture check"}`)), http.StatusOK)
	before := f.state()
	var runs []board.Execution
	runs = f.executionRecords()
	original := runs[0]
	f.request("POST", path, body, false, http.StatusForbidden, nil)
	f.request("POST", path, body, true, http.StatusOK, nil)
	f.request("POST", path, body, true, http.StatusOK, nil)
	f.request("POST", path, map[string]any{"metrics": worker.DecisionMetrics{Version: 1, ModelCalls: 99}}, true, http.StatusConflict, nil)
	f.request("POST", path, map[string]any{"metrics": body["metrics"], "status": "failed"}, true, http.StatusUnprocessableEntity, nil)
	runs = f.executionRecords()
	var oldResult, newResult map[string]json.RawMessage
	if json.Unmarshal(original.Result, &oldResult) != nil || json.Unmarshal(runs[0].Result, &newResult) != nil {
		t.Fatal("invalid stored result")
	}
	var metrics worker.DecisionMetrics
	if json.Unmarshal(newResult["metrics"], &metrics) != nil || metrics.ModelCalls != 2 {
		t.Fatal("observation was not persisted")
	}
	delete(newResult, "metrics")
	if !reflect.DeepEqual(oldResult, newResult) || runs[0].Status != original.Status || runs[0].UpdatedAt != original.UpdatedAt || !reflect.DeepEqual(before, f.state()) {
		t.Fatal("observation changed authoritative result or graph")
	}
	f.decisionReceipt(http.StatusOK)
	f.apply(http.StatusOK)
}

func TestDecisionChangesReturnOnlyBoundedGraphIndex(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	f.action("fact", "observation", evidenceFixtureFact(f.run))
	state := f.state()
	var changes []board.StateChange
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		var err error
		changes, err = tx.StateChanges(f.project, 0, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Revision != 1 || strings.Contains(string(raw), `"payload"`) || strings.Contains(string(raw), `"result"`) {
		t.Fatalf("index leaked event bodies: %+v", changes)
	}
	if state.Revision < 2 {
		t.Fatal("fixture must contain information newer than the query boundary")
	}
	err = f.store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.StateChanges(f.project, 2, 1)
		return err
	})
	var apiErr *board.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid revision interval: %v", err)
	}
}
