package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const DefaultContextViewBytes = 32 << 10

type contextFact struct {
	FactRecord
	DetailsOmitted bool `json:"details_omitted,omitempty"`
}

// ContextView creates a bounded model projection. It never changes the
// registered graph snapshot. Original user input and the active step are
// mandatory; omitted history can be obtained through paginated graph reads.
func ContextView(state State, stepID string, maxBytes int) (json.RawMessage, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultContextViewBytes
	}
	state = contextState(state)
	view := struct {
		Version    int            `json:"version"`
		Revision   int64          `json:"revision"`
		Project    Project        `json:"project"`
		UserInputs []Fact         `json:"user_inputs"`
		Hints      []Hint         `json:"hints"`
		Goals      []Goal         `json:"goals"`
		Steps      []Step         `json:"steps"`
		Facts      []contextFact  `json:"fact_records"`
		Findings   []Finding      `json:"findings"`
		Relations  []FactRelation `json:"fact_relations"`
		Omitted    map[string]int `json:"omitted"`
	}{Version: 1, Revision: state.Revision, Project: state.Graph.Project, UserInputs: []Fact{}, Hints: append([]Hint{}, state.Graph.Hints...), Goals: []Goal{}, Steps: []Step{}, Facts: []contextFact{}, Findings: []Finding{}, Relations: []FactRelation{}, Omitted: map[string]int{"goals": len(state.Goals), "steps": len(state.Steps), "fact_records": len(state.FactRecords), "fact_details": len(state.FactRecords), "findings": len(state.Findings), "fact_relations": len(state.FactRelations)}}
	for _, f := range state.Graph.Facts {
		if f.ID == "origin" || f.ID == "goal" {
			view.UserInputs = append(view.UserInputs, f)
		}
	}
	goalByID := map[string]Goal{}
	for _, goal := range state.Goals {
		goalByID[goal.ID] = goal
	}
	relevant := map[string]bool{}
	selectedGoals := map[string]bool{}
	goalID := "goal"
	if stepID != "" {
		found := false
		for _, step := range state.Steps {
			if step.ID == stepID {
				view.Steps = append(view.Steps, step)
				view.Omitted["steps"]--
				goalID = step.GoalID
				for _, id := range step.From {
					relevant[id] = true
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("context view: unknown step %q", stepID)
		}
	}
	for goalID != "" {
		if selectedGoals[goalID] {
			return nil, errors.New("context view: cyclic goal ancestry")
		}
		goal, ok := goalByID[goalID]
		if !ok {
			return nil, fmt.Errorf("context view: missing goal %q", goalID)
		}
		selectedGoals[goalID] = true
		view.Goals = append(view.Goals, goal)
		view.Omitted["goals"]--
		for _, id := range goal.Sources {
			relevant[id] = true
		}
		goalID = goal.ParentID
	}
	if !selectedGoals["goal"] {
		if goal, ok := goalByID["goal"]; ok {
			view.Goals = append(view.Goals, goal)
			selectedGoals["goal"] = true
			view.Omitted["goals"]--
		}
	}
	base, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	used := len(base)
	if used > maxBytes {
		return nil, errors.New("context view: original user inputs, hints, goal ancestry and current step exceed the budget")
	}
	remaining := maxBytes - used
	sectionLimit := used + remaining/2
	// Omitted counts start at their largest value, so updating them never
	// grows the encoded object. Include commas in this conservative accounting.
	add := func(value any, currentCount int) bool {
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		extra := len(raw)
		if currentCount > 0 {
			extra++
		}
		if used+extra > sectionLimit {
			return false
		}
		used += extra
		return true
	}
	// Bring correction sources near the evidence they supersede/refute.
	for _, relation := range state.FactRelations {
		if relevant[relation.Target] {
			relevant[relation.Source] = true
		}
	}
	facts := append([]FactRecord{}, state.FactRecords...)
	sort.SliceStable(facts, func(i, j int) bool {
		a, b := facts[i], facts[j]
		if relevant[a.ID] != relevant[b.ID] {
			return relevant[a.ID]
		}
		if (a.Status == "valid") != (b.Status == "valid") {
			return a.Status == "valid"
		}
		return false
	})
	selectedFacts := map[string]bool{}
	for _, f := range facts {
		if f.ID == "origin" || f.ID == "goal" {
			view.Omitted["fact_records"]--
			view.Omitted["fact_details"]--
			continue
		}
		candidate := contextFact{FactRecord: f}
		full := f.Status == "valid"
		if full && add(candidate, len(view.Facts)) {
			view.Facts = append(view.Facts, candidate)
			view.Omitted["fact_records"]--
			view.Omitted["fact_details"]--
			selectedFacts[f.ID] = true
			continue
		}
		candidate = contextFact{FactRecord: FactRecord{ID: f.ID, Status: f.Status, Legacy: f.Legacy}, DetailsOmitted: true}
		if add(candidate, len(view.Facts)) {
			view.Facts = append(view.Facts, candidate)
			view.Omitted["fact_records"]--
			selectedFacts[f.ID] = true
		}
	}
	sectionLimit = used + remaining/10
	for _, relation := range state.FactRelations {
		if (selectedFacts[relation.Source] || selectedFacts[relation.Target]) && add(relation, len(view.Relations)) {
			view.Relations = append(view.Relations, relation)
			view.Omitted["fact_relations"]--
		}
	}
	sectionLimit = used + remaining/10
	for _, goal := range state.Goals {
		if !selectedGoals[goal.ID] && add(goal, len(view.Goals)) {
			view.Goals = append(view.Goals, goal)
			view.Omitted["goals"]--
		}
	}
	sectionLimit = used + remaining/5
	steps := append([]Step{}, state.Steps...)
	sort.SliceStable(steps, func(i, j int) bool {
		return (steps[i].Status == "running" || steps[i].Status == "open") && (steps[j].Status != "running" && steps[j].Status != "open")
	})
	for _, step := range steps {
		if step.ID != stepID && add(step, len(view.Steps)) {
			view.Steps = append(view.Steps, step)
			view.Omitted["steps"]--
		}
	}
	sectionLimit = maxBytes
	findings := append([]Finding{}, state.Findings...)
	sort.SliceStable(findings, func(i, j int) bool {
		related := func(f Finding) bool {
			for _, id := range f.Sources {
				if relevant[id] {
					return true
				}
			}
			return false
		}
		return related(findings[i]) && !related(findings[j])
	})
	for _, finding := range findings {
		if add(finding, len(view.Findings)) {
			view.Findings = append(view.Findings, finding)
			view.Omitted["findings"]--
		}
	}
	result, err := json.Marshal(view)
	if err == nil && len(result) > maxBytes {
		return nil, errors.New("context view exceeded its budget")
	}
	return result, err
}

// Legacy jobs contain Graph only. Project the same meaning without guessing
// new provenance or changing those jobs' immutable registration snapshots.
func contextState(s State) State {
	if len(s.Goals) == 0 {
		root := Goal{ID: "goal", Status: "open", Sources: []string{}}
		for _, f := range s.Graph.Facts {
			if f.ID == "goal" {
				root.Condition = f.Description
			}
		}
		if s.Graph.Project.Status == "completed" {
			root.Status = "achieved"
		}
		s.Goals = []Goal{root}
	}
	if len(s.Steps) == 0 {
		for _, i := range s.Graph.Intents {
			step := Step{ID: i.ID, From: i.From, GoalID: "goal", Description: i.Description, Status: "open", Result: i.To, Worker: i.Worker, CreatedAt: i.CreatedAt}
			if i.Worker != nil {
				step.Status = "running"
			}
			if i.To != nil {
				step.Status = "completed"
			}
			s.Steps = append(s.Steps, step)
		}
	}
	if len(s.FactRecords) == 0 {
		for _, f := range s.Graph.Facts {
			status := "valid"
			if f.ID == "origin" || f.ID == "goal" {
				status = "input"
			}
			s.FactRecords = append(s.FactRecords, FactRecord{ID: f.ID, Description: f.Description, Status: status, Legacy: true, Evidence: []EvidenceRef{}})
		}
	}
	return s
}
