package board

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestCursorDecisionOrdersEventsAndDeduplicatesChangedNodes(t *testing.T) {
	state := overviewState()
	state.Revision = 4
	state.Steps = []Step{{ID: "archived", GoalID: "goal", From: []string{"origin"}, Status: "abandoned"}}
	state.FactRecords = append(state.FactRecords, FactRecord{ID: "new", Description: "The latest observation.", Status: "valid"})
	cursor := &DecisionCursor{ProjectID: state.Graph.Project.ID, Generation: state.Graph.Project.Generation, Revision: 1}
	// The Step was reprioritized and then abandoned before the new Fact arrived.
	events := []StateEvent{{Revision: 4, Op: "fact", ID: "new"}, {Revision: 2, Op: "step", ID: "archived"}, {Revision: 3, Op: "step", ID: "archived"}}
	decision, err := BuildDecisionContextFromCursor(state, cursor, events, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != "changes" || decision.FromRevision != 1 || decision.ToRevision != 4 {
		t.Fatalf("wrong revision boundary: %+v", decision)
	}
	var body decisionView
	if err := json.Unmarshal(decision.View, &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.Changed["fact_records"], []string{"new"}) || !slices.Equal(body.Changed["steps"], []string{"archived"}) || !slices.Equal(body.Changed["event_nodes"], []string{"archived", "new"}) {
		t.Fatalf("event identities were lost or duplicated: %+v", body.Changed)
	}
	if len(body.Steps) != 1 || body.Steps[0].Status != "abandoned" || len(body.Facts) != 1 || body.Facts[0].ID != "new" {
		t.Fatalf("events did not select current records: steps=%+v facts=%+v", body.Steps, body.Facts)
	}
	if body.Removed == nil || len(body.Removed) != 0 {
		t.Fatalf("removed field lost its empty-object compatibility: %+v", body.Removed)
	}
}

func TestCursorDecisionUsesCurrentGraphAndKeepsOlderKnowledgeDiscoverable(t *testing.T) {
	current := overviewState()
	current.Revision = 3
	current.FactRecords[0].Status = "superseded"
	current.FactRecords = append(current.FactRecords, FactRecord{ID: "new", Description: "A corrected observation", Status: "valid"})
	current.FactRelations = []FactRelation{{Kind: "supersedes", Source: "new", Target: "first", Reason: "scope corrected"}}
	current.Steps = []Step{{ID: "waiting", GoalID: "goal", From: []string{"first"}, Status: "needs_review", InvalidSources: []string{"first"}}}
	cursor := &DecisionCursor{ProjectID: current.Graph.Project.ID, Generation: current.Graph.Project.Generation, Revision: 1}
	view, err := BuildDecisionContextFromCursor(current, cursor, []StateEvent{{Revision: 2, Op: "fact", ID: "new"}, {Revision: 3, Op: "fact_relation", ID: "first"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if view.Mode != "changes" || view.FromRevision != 1 || view.ToRevision != 3 {
		t.Fatalf("wrong revision boundary: %+v", view)
	}
	var body decisionView
	if err := json.Unmarshal(view.View, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Relations) != 1 || len(body.Steps) != 1 || body.Steps[0].Status != "needs_review" {
		t.Fatalf("correction lost its causal closure: %+v", body)
	}
	if len(body.Facts) != 2 || body.Facts[0].Status != "superseded" {
		t.Fatalf("old belief replaced current graph: %+v", body.Facts)
	}
	overview := decodeOverview(t, view.View)
	if overviewSection(t, overview, "facts").Total != 3 {
		t.Fatal("unrelated historical fact disappeared from discovery")
	}
}

func TestCursorDecisionFallsBackWithoutInventingMissingHistory(t *testing.T) {
	state := overviewState()
	state.Revision = 3
	for _, test := range []struct {
		name     string
		cursor   *DecisionCursor
		events   []StateEvent
		fallback string
	}{
		{"initial", nil, nil, "no_baseline"},
		{"missing", &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 1}, nil, "event_gap"},
		{"different_round", &DecisionCursor{ProjectID: "project", Generation: 0, Revision: 1}, nil, "generation_changed"},
		{"unknown", &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 2}, []StateEvent{{Revision: 3, Op: "future_operation", ID: "first"}}, "unknown_event"},
		{"wrong_project", &DecisionCursor{ProjectID: "other", Generation: 1, Revision: 3}, nil, "project_changed"},
		{"future", &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 4}, nil, "invalid_cursor"},
		{"negative", &DecisionCursor{ProjectID: "project", Generation: 1, Revision: -1}, nil, "invalid_cursor"},
		{"duplicate", &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 1}, []StateEvent{{Revision: 2, Op: "fact", ID: "first"}, {Revision: 2, Op: "fact", ID: "first"}}, "event_gap"},
		{"no_identity", &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 2}, []StateEvent{{Revision: 3, Op: "fact"}}, "event_without_identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			view, err := BuildDecisionContextFromCursor(state, test.cursor, test.events, 0)
			if err != nil {
				t.Fatal(err)
			}
			if view.Mode != "full" || view.Fallback != test.fallback {
				t.Fatalf("unexpected fallback: %+v", view)
			}
			if overviewSection(t, decodeOverview(t, view.View), "facts").Total != 2 {
				t.Fatal("fallback lost current FGS")
			}
			var body struct {
				UserInputs     []Fact          `json:"user_inputs"`
				Removed        decisionChanges `json:"removed"`
				RemovedOmitted map[string]int  `json:"removed_omitted"`
			}
			if err := json.Unmarshal(view.View, &body); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(body.UserInputs, state.Graph.Facts) {
				t.Fatal("fallback lost the original scope or root requirement")
			}
			if body.Removed == nil || len(body.Removed) != 0 || body.RemovedOmitted == nil || len(body.RemovedOmitted) != 0 {
				t.Fatal("fallback removal fields lost their empty-object compatibility")
			}
		})
	}
}

func TestCursorDecisionFallsBackWhenChangeIndexLimitIsReached(t *testing.T) {
	state := overviewState()
	state.Revision = 1001
	events := make([]StateEvent, 1000)
	for n := range events {
		events[n] = StateEvent{Revision: int64(n + 1), Op: "fact", ID: "first"}
	}
	view, err := BuildDecisionContextFromCursor(state, &DecisionCursor{ProjectID: "project", Generation: 1}, events, DefaultContextViewBytes)
	if err != nil || view.Fallback != "event_limit" || view.Mode != "full" || view.ToRevision != 1001 {
		t.Fatalf("truncated index was treated as complete: view=%+v err=%v", view, err)
	}
	if len(view.View) > DefaultContextViewBytes || overviewSection(t, decodeOverview(t, view.View), "facts").Total != 2 {
		t.Fatal("fallback lost bounded current graph discovery")
	}
}
