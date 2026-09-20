package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestContextViewPreservesUserInputsAndCurrentStep(t *testing.T) {
	s := State{Graph: Graph{Project: Project{ID: "p", Title: "project", Status: "active"}, Facts: []Fact{{ID: "origin", Description: "authorized local environment"}, {ID: "goal", Description: "preserve exact constraints"}, {ID: "proof", Description: "observed proof"}}, Hints: []Hint{{ID: "h1", Content: "Do not change files"}}, Intents: []Intent{{ID: "s1", From: []string{"proof"}, Description: "read-only verify"}}}}
	before, _ := json.Marshal(s)
	raw, err := ContextView(s, "s1", DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		UserInputs []Fact        `json:"user_inputs"`
		Hints      []Hint        `json:"hints"`
		Steps      []Step        `json:"steps"`
		Facts      []contextFact `json:"fact_records"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.UserInputs) != 2 || view.UserInputs[1].Description != "preserve exact constraints" || !reflect.DeepEqual(view.Hints, s.Graph.Hints) || len(view.Steps) != 1 || view.Steps[0].ID != "s1" || len(view.Facts) != 1 || view.Facts[0].Description != "observed proof" {
		t.Fatal(string(raw))
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("context projection modified immutable graph")
	}
}

func TestContextViewBoundsLargeGraphAndMarksOmissions(t *testing.T) {
	s := State{Graph: Graph{Project: Project{ID: "p"}, Facts: []Fact{{ID: "origin", Description: "origin"}, {ID: "goal", Description: "goal"}}}, Goals: []Goal{{ID: "goal", Condition: "goal", Status: "open"}}, Steps: []Step{{ID: "current", GoalID: "goal", From: []string{"important", "outdated"}, Description: "current task", Status: "running"}}, FactRecords: []FactRecord{{ID: "important", Description: "unique required proof", Status: "valid"}, {ID: "outdated", Description: "DO_NOT_PRESENT_AS_CURRENT", Status: "refuted"}}, FactRelations: []FactRelation{{Kind: "refutes", Source: "important", Target: "outdated", Reason: "new evidence"}}, Findings: []Finding{{ID: "finding", Claim: "independent partial result", Status: "candidate", Sources: []string{"important"}}}}
	for i := 0; i < 2000; i++ {
		s.FactRecords = append(s.FactRecords, FactRecord{ID: fmt.Sprintf("f%d", i), Description: strings.Repeat("large history ", 100), Status: "valid"})
	}
	raw, err := ContextView(s, "current", 6000)
	if err != nil || len(raw) > 6000 {
		t.Fatal(len(raw), err)
	}
	var view struct {
		Facts     []contextFact  `json:"fact_records"`
		Findings  []Finding      `json:"findings"`
		Omitted   map[string]int `json:"omitted"`
		Relations []FactRelation `json:"fact_relations"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Facts[0].ID != "important" || view.Omitted["fact_records"] == 0 || view.Omitted["fact_details"] == 0 || strings.Contains(string(raw), "DO_NOT_PRESENT_AS_CURRENT") || len(view.Findings) != 1 || len(view.Relations) != 1 {
		t.Fatal(string(raw))
	}
	found := false
	for _, f := range view.Facts {
		if f.ID == "outdated" {
			found = f.DetailsOmitted && f.Status == "refuted"
		}
	}
	if !found {
		t.Fatal("referenced invalid fact lost its explicit status")
	}
	second, err := ContextView(s, "current", 6000)
	if err != nil || string(second) != string(raw) {
		t.Fatal("projection is not deterministic", err)
	}
}

func TestContextViewDoesNotSilentlyDropMandatoryConstraints(t *testing.T) {
	for _, where := range []string{"origin", "hint", "step"} {
		t.Run(where, func(t *testing.T) {
			s := State{Graph: Graph{Project: Project{ID: "p"}, Facts: []Fact{{ID: "origin", Description: "origin"}, {ID: "goal", Description: "goal"}}, Intents: []Intent{{ID: "s", Description: "step"}}, Hints: []Hint{{ID: "h", Content: "hint"}}}}
			switch where {
			case "origin":
				s.Graph.Facts[0].Description = strings.Repeat("constraint", 1000)
			case "hint":
				s.Graph.Hints[0].Content = strings.Repeat("constraint", 1000)
			case "step":
				s.Graph.Intents[0].Description = strings.Repeat("constraint", 1000)
			}
			if _, err := ContextView(s, "s", 2000); err == nil {
				t.Fatal("mandatory constraint silently omitted")
			}
		})
	}
}

func TestContextViewRejectsMissingStepAndGoalCycle(t *testing.T) {
	s := State{Graph: Graph{Project: Project{ID: "p"}}}
	if _, err := ContextView(s, "missing", 0); err == nil {
		t.Fatal("missing step accepted")
	}
	s.Goals = []Goal{{ID: "goal", ParentID: "child"}, {ID: "child", ParentID: "goal"}}
	if _, err := ContextView(s, "", 0); err == nil {
		t.Fatal("cyclic goal accepted")
	}
}
