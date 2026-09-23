package server

import (
	"encoding/json"
	"net/http"
	"reflect"
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
	f.request("GET", "/executions?namespace=protocol-test", nil, false, http.StatusOK, &runs)
	original := runs[0]
	f.request("POST", path, body, false, http.StatusForbidden, nil)
	f.request("POST", path, body, true, http.StatusOK, nil)
	f.request("POST", path, body, true, http.StatusOK, nil)
	f.request("POST", path, map[string]any{"metrics": worker.DecisionMetrics{Version: 1, ModelCalls: 99}}, true, http.StatusConflict, nil)
	f.request("POST", path, map[string]any{"metrics": body["metrics"], "status": "failed"}, true, http.StatusUnprocessableEntity, nil)
	f.request("GET", "/executions?namespace=protocol-test", nil, false, http.StatusOK, &runs)
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
	var changes []board.StateEvent
	f.request("GET", f.base()+"/state/changes?after=0&through=1", nil, false, http.StatusOK, &changes)
	if len(changes) != 1 || changes[0].Revision != 1 || len(changes[0].Payload) != 0 || len(changes[0].Result) != 0 {
		t.Fatalf("index leaked event bodies: %+v", changes)
	}
	if state.Revision < 2 {
		t.Fatal("fixture must contain information newer than the query boundary")
	}
	f.request("GET", f.base()+"/state/changes?after=2&through=1", nil, false, http.StatusUnprocessableEntity, nil)
}
