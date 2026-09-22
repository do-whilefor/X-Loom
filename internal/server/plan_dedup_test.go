package server

import (
	"net/http"
	"testing"

	"xloom/internal/board"
)

func TestLegacyFinalPlanReusesToolAndBatchDuplicates(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("reason", nil, 0)
	toolStep := f.action("step", "already-added", map[string]any{
		"action": "add", "from": []string{"origin"}, "description": "Observe the fixture",
	})
	f.pending(`{"accepted":true,"data":{"intents":[
		{"from":["origin"],"description":" Observe the fixture "},
		{"from":["origin"],"description":"Observe a second fixture"},
		{"from":["origin"],"description":"Observe a second fixture"}
	]}}`)
	f.apply(http.StatusOK)
	f.apply(http.StatusOK)
	state := f.state()
	if len(state.Steps) != 2 || len(state.Graph.Intents) != 2 || state.Steps[0].ID != toolStep.ID {
		t.Fatalf("final plan duplicated a tool-created or repeated batch task: %+v", state.Steps)
	}
}

func TestFencedLegacyPlanReusesExistingStepAcrossDecisions(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	old := f.newIntent()
	f.register("reason", nil, 0)
	var repeated board.Intent
	f.request("POST", f.base()+"/intents", map[string]any{
		"from": []string{"origin"}, "description": "  " + old.Description + "\n", "creator": f.lease,
	}, true, http.StatusOK, &repeated)
	if repeated.ID != old.ID || repeated.Creator != old.Creator || repeated.Worker != nil || len(f.state().Steps) != 1 {
		t.Fatalf("new planning lease recreated or claimed existing task: %+v", repeated)
	}
	f.pending(`{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"Observe the synthetic fixture"}]}}`)
	f.apply(http.StatusOK)
	if len(f.state().Steps) != 1 {
		t.Fatal("final plan recreated existing task")
	}
}

func TestUnfencedIntentCreationKeepsExplicitCairnSemantics(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	first, second := f.newIntent(), f.newIntent()
	if first.ID == second.ID || len(f.state().Steps) != 2 {
		t.Fatal("deduplication changed the unfenced compatibility API")
	}
}
