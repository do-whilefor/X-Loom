package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"xloom/internal/board"
)

func TestCompletionPreviewReturnsAuthoritativeReviewWithoutAcceptanceOrWrites(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Completion review", "origin": "Request /c on prod/build-42 as anonymous", "goal": "Obtain the actual HTTP response status for /c"}, false, http.StatusCreated, &graph)
	f.project = graph.Project.ID
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "Use the specified deployment and identity"}, false, http.StatusCreated, nil)
	live := true
	f.register("explore", &live, 2)
	negative := evidenceFixtureFact(f.run)
	negative["description"] = "/c produced no response"
	negative["scope"] = "prod/build-42 as anonymous"
	negative["evidence"] = []board.EvidenceRef{{RunID: f.run, Path: "/run/evidence/no-response.json", Excerpt: "{\"http_status\":null,\"response_received\":false,\"client_exit_code\":0}\n"}}
	fact := f.action("fact", "negative-observation", negative)
	conclusion, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact_id": fact.ID}})
	f.pending(string(conclusion))
	f.apply(http.StatusOK)
	f.run, f.lease, f.intent = "completion-review", "planner@completion-review", ""
	f.registerWithFields("reason", &live, 2, map[string]any{"decision": map[string]any{"version": 2, "state_version": board.DecisionStateVersion(f.state())}}, http.StatusCreated)
	payload, _ := json.Marshal(map[string]any{"from": []string{fact.ID}, "description": "No response means that /c has been accounted for"})
	before := f.state()
	batch := f.batch(board.DecisionAction{Op: "complete", Payload: payload})
	preview := f.decision("preview", batch, http.StatusOK)
	review := preview.CompletionReview
	if preview.Committed || preview.Completed || preview.ValidationScope != "protocol_only" || review == nil || review.Acceptance != "not_checked" || review.StateVersion != batch.ExpectedVersion {
		t.Fatalf("protocol preview was presented as semantic acceptance: %+v", preview)
	}
	if !reflect.DeepEqual(review.UserInputs, graph.Facts) || !reflect.DeepEqual(review.Hints, before.Graph.Hints) || len(review.FactRecords) != 1 {
		t.Fatalf("authoritative requirements or support missing: %+v", review)
	}
	for _, original := range before.FactRecords {
		if original.ID == fact.ID && !reflect.DeepEqual(original, review.FactRecords[0]) {
			t.Fatalf("negative evidence was summarized, rewritten or lost: %+v", review.FactRecords[0])
		}
	}
	if !reflect.DeepEqual(before, f.state()) || f.state().Graph.Project.Status != "active" {
		t.Fatal("preview persisted simulated root completion")
	}
	if saved := f.decisionReceipt(http.StatusOK); saved.Committed || saved.CompletionReview != nil || saved.ValidationScope != "" {
		t.Fatalf("preview persisted a review or receipt: %+v", saved)
	}
	// A review only applies to its exact original state, including user hints.
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "Include a later user requirement"}, false, http.StatusCreated, nil)
	f.decision("preview", batch, http.StatusConflict)
	f.decision("commit", batch, http.StatusConflict)
	if f.state().Graph.Project.Status != "active" {
		t.Fatal("stale reviewed proposal completed the project")
	}
}
