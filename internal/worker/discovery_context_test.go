package worker

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
)

// Exercise the production selector before following its discovery entry point.
// Handwritten model views can test a model's choices, but cannot establish that
// a real full, incremental, or fallback view leaves old branches discoverable.
func TestProductionDecisionContextKeepsCrossBranchEvidenceDiscoverable(t *testing.T) {
	for _, mode := range []string{"initial", "incremental", "closure_over_budget"} {
		t.Run(mode, func(t *testing.T) {
			current := discoveryContextState()
			var cursor *board.DecisionCursor
			var events []board.StateEvent
			wantMode, wantFallback := "full", "no_baseline"
			if mode != "initial" {
				cursor = &board.DecisionCursor{ProjectID: current.Graph.Project.ID, Generation: current.Graph.Project.Generation, Revision: current.Revision - 1}
				if mode == "incremental" {
					// The needed facts are unchanged history in other branches.
					events = []board.StateEvent{{Revision: current.Revision, Op: "fact", ID: current.FactRecords[90].ID}}
					wantMode, wantFallback = "changes", ""
				} else {
					from := make([]string, 0, len(current.FactRecords)-2)
					for _, fact := range current.FactRecords[2:] {
						from = append(from, fact.ID)
					}
					current.Steps = append(current.Steps, board.Step{ID: "review", GoalID: "goal", From: from, Description: "Review the collected inventory.", Status: "open"})
					events = []board.StateEvent{{Revision: current.Revision, Op: "step", ID: "review"}}
					wantFallback = "related_context_over_budget"
				}
			}
			decision, err := board.BuildDecisionContextFromCursor(current, cursor, events, board.DefaultContextViewBytes)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Mode != wantMode || decision.Fallback != wantFallback {
				t.Fatalf("selector path = %s/%s, want %s/%s", decision.Mode, decision.Fallback, wantMode, wantFallback)
			}
			if len(decision.View) > board.DefaultContextViewBytes {
				t.Fatal("discovery expanded the initial context beyond its budget")
			}
			var view struct {
				UserInputs []board.Fact          `json:"user_inputs"`
				Facts      []struct{ ID string } `json:"fact_records"`
				Overview   struct {
					Sections []struct {
						Section                       string
						Total, Omitted, Offset, Limit int
						Items                         []struct{ ID string }
					}
				} `json:"overview"`
			}
			if err := json.Unmarshal(decision.View, &view); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(view.UserInputs, current.Graph.Facts[:2]) {
				t.Fatal("production projection lost the original scope or root requirement")
			}
			const hiddenID = "f180"
			for _, fact := range view.Facts {
				if fact.ID == hiddenID {
					t.Fatal("fixture no longer exercises discovery of a fact outside the initial body")
				}
			}
			request := GraphRequest{Op: "read_graph", ExpectedVersion: decision.StateVersion}
			for _, section := range view.Overview.Sections {
				if section.Section != "facts" {
					continue
				}
				if section.Total != len(current.FactRecords) || section.Total != len(section.Items)+section.Omitted || section.Omitted == 0 {
					t.Fatal("production discovery index hid or misstated omitted facts")
				}
				for _, fact := range section.Items {
					if fact.ID == hiddenID {
						t.Fatal("fixture no longer exercises a fact absent from the initial discovery index")
					}
				}
				request.Section, request.Offset, request.Limit = section.Section, section.Offset, section.Limit
			}
			if request.Section == "" || request.Offset != 0 || request.Limit < 1 || request.Limit > 50 {
				t.Fatal("omitted history has no usable discovery entry point")
			}
			var recovered []board.FactRecord
			pages := 0
			for {
				page := checkedGraphPage(t, current, request)
				pages++
				if page.Offset != len(recovered) || page.Total != len(current.FactRecords) {
					t.Fatal("graph discovery skipped or changed the selected history")
				}
				for _, raw := range page.Items {
					var fact board.FactRecord
					if err := json.Unmarshal(raw, &fact); err != nil {
						t.Fatal(err)
					}
					recovered = append(recovered, fact)
				}
				if page.NextOffset == nil {
					break
				}
				if len(page.Items) == 0 || *page.NextOffset <= request.Offset || pages > len(current.FactRecords) {
					t.Fatal("discovery pagination did not make bounded progress")
				}
				request.Offset = *page.NextOffset
			}
			if pages < 2 || !reflect.DeepEqual(recovered, current.FactRecords) {
				t.Fatal("production discovery did not recover complete facts and retained evidence across pages")
			}
			// Discovered IDs also support focused reads under the selector's version.
			page := checkedGraphPage(t, current, GraphRequest{Section: "facts", IDs: []string{"f001", hiddenID}, Limit: 20, ExpectedVersion: decision.StateVersion})
			if len(page.Items) != 2 || page.NextOffset != nil || len(page.MissingIDs) != 0 {
				t.Fatal("discovered cross-branch fact IDs cannot retrieve their full support")
			}
		})
	}
}

func discoveryContextState() board.State {
	inputs := []board.Fact{
		{ID: "origin", Description: "Inspect the existing records for local deployment build-42; do not change the deployment."},
		{ID: "goal", Description: "Identify the host and port serving route R17 from the route record and backend catalog for build-42."},
	}
	state := board.State{
		Graph: board.Graph{Project: board.Project{ID: "discovery", Status: "active", Generation: 1}, Facts: inputs},
		Goals: []board.Goal{{ID: "goal", Condition: inputs[1].Description, Status: "open"}},
		Steps: []board.Step{
			{ID: "route-branch", GoalID: "goal", From: []string{"origin"}, Description: "Read the route configuration.", Status: "completed", Result: board.Ptr("f001")},
			{ID: "catalog-branch", GoalID: "goal", From: []string{"origin"}, Description: "Read the backend catalog.", Status: "completed", Result: board.Ptr("f180")},
		},
		Revision: 2,
	}
	for _, input := range inputs {
		state.FactRecords = append(state.FactRecords, board.FactRecord{ID: input.ID, Description: input.Description, Status: "input"})
	}
	for n := 1; n <= 180; n++ {
		fact := board.FactRecord{ID: fmt.Sprintf("f%03d", n), Description: strings.Repeat("Unrelated archived inventory observation. ", 32), Scope: "local build-42", Status: "valid", Evidence: []board.EvidenceRef{{RunID: "retained-run", Path: fmt.Sprintf("retained/%03d.json", n), Excerpt: strings.Repeat("Archived inventory entry. ", 40)}}}
		if n == 1 {
			fact.SourceStepID, fact.Description = "route-branch", "Route R17 selects backend slot beta in build-42."
			fact.Evidence[0].Excerpt = `{"build":"build-42","route":"R17","slot":"beta"}`
		}
		if n == 180 {
			fact.SourceStepID, fact.Description = "catalog-branch", "Backend slot beta is worker-7:4317 in build-42. "+strings.Repeat("This observation came from the backend catalog. ", 8)
			fact.Evidence[0].Excerpt = `{"build":"build-42","backends":{"beta":"worker-7:4317"}}`
		}
		state.FactRecords = append(state.FactRecords, fact)
		state.Graph.Facts = append(state.Graph.Facts, board.Fact{ID: fact.ID, Description: fact.Description})
	}
	return state
}
