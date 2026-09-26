package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// A report uses its Goal subtree and explicit source producers. The existing
// input snapshot freezes this scope; Execute's correction cursor is unrelated.
// Unknown legacy producers are conservatively included instead of guessed.
func reportScope(state State, report Step) (State, error) {
	goals := map[string]bool{report.GoalID: true}
	for changed := true; changed; {
		changed = false
		for _, goal := range state.Goals {
			if goals[goal.ParentID] && !goals[goal.ID] {
				goals[goal.ID], changed = true, true
			}
		}
	}
	steps := map[string]bool{}
	facts := map[string]bool{"origin": true, "goal": true}
	for _, id := range report.From {
		facts[id] = true
	}
	for _, fact := range state.FactRecords {
		if facts[fact.ID] && fact.SourceStepID != "" && fact.SourceStepID != report.ID {
			steps[fact.SourceStepID] = true
		}
	}
	out := State{Graph: Graph{Project: Project{ID: state.Graph.Project.ID, Generation: state.Graph.Project.Generation}, Hints: state.Graph.Hints}}
	for _, goal := range state.Goals {
		if goals[goal.ID] {
			out.Goals = append(out.Goals, Goal{ID: goal.ID, ParentID: goal.ParentID, Condition: goal.Condition})
		}
	}
	for _, step := range state.Steps {
		if step.ID == report.ID || (step.FinalReport && !steps[step.ID]) || (!goals[step.GoalID] && !steps[step.ID]) {
			continue
		}
		if step.Status != "completed" && step.Status != "abandoned" {
			return State{}, Err(409, "report_dependencies_pending: Step "+step.ID+" must complete or be explicitly abandoned before final reporting")
		}
		steps[step.ID] = true
		step.Worker, step.Priority = nil, 0
		if step.Status != "abandoned" {
			step.Reason = ""
		}
		out.Steps = append(out.Steps, step)
	}
	for _, fact := range state.FactRecords {
		if (report.ID == "" || fact.SourceStepID != report.ID) && (fact.SourceStepID == "" || steps[fact.SourceStepID]) {
			facts[fact.ID] = true
		}
	}
	// A correction can be published outside this Goal; preserve its full chain.
	for changed := true; changed; {
		changed = false
		for _, relation := range state.FactRelations {
			if facts[relation.Target] && !facts[relation.Source] {
				facts[relation.Source], changed = true, true
			}
		}
	}
	for _, fact := range state.FactRecords {
		if facts[fact.ID] {
			out.FactRecords = append(out.FactRecords, fact)
		}
	}
	for _, relation := range state.FactRelations {
		if facts[relation.Target] {
			out.FactRelations = append(out.FactRelations, relation)
		}
	}
	for _, finding := range state.Findings {
		for _, source := range finding.Sources {
			if facts[source] {
				out.Findings = append(out.Findings, finding)
				break
			}
		}
	}
	return out, nil
}

func reportReady(state State, report Step) error {
	scope, err := reportScope(state, report)
	if err != nil {
		return err
	}
	for _, fact := range scope.FactRecords {
		if fact.Status == "valid" && !slices.Contains(report.From, fact.ID) {
			return Err(409, "report_dependencies_changed: new evidence requires a new final_report Step")
		}
	}
	return nil
}

func (t *Tx) checkReportInput(state State, id string, raw json.RawMessage) error {
	var report Step
	for _, step := range state.Steps {
		if step.ID == id {
			report = step
			break
		}
	}
	if !report.FinalReport {
		return nil
	}
	var job struct {
		Version       int            `json:"result_contract_version"`
		State         *State         `json:"state"`
		InputSnapshot *InputSnapshot `json:"input_snapshot"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return err
	}
	if job.Version != 2 || (job.State == nil) == (job.InputSnapshot == nil) {
		return Err(422, "final_report requires an evidence-contract execution with one immutable input snapshot")
	}
	if job.InputSnapshot != nil {
		original, err := t.ReadInputSnapshot(state.Graph.Project.ID, job.InputSnapshot.ID)
		if err != nil {
			return err
		}
		job.State = &original
	}
	bound := false
	for _, step := range job.State.Steps {
		if step.ID == report.ID && step.FinalReport && step.GoalID == report.GoalID && slices.Equal(step.From, report.From) {
			bound = true
		}
	}
	if !bound {
		return Err(409, "final_report input does not bind this Step and its sources")
	}
	current, err := reportScope(state, report)
	if err != nil {
		return err
	}
	original, err := reportScope(*job.State, report)
	if err != nil {
		return err
	}
	if DecisionStateVersion(current) != DecisionStateVersion(original) {
		return Err(409, "report_dependencies_changed: final report no longer matches its frozen evidence; create a new final_report Step")
	}
	return nil
}

func (t *Tx) checkReportCompletion(state State, fence ExecutionFence) error {
	for _, step := range state.Steps {
		if step.ID == fence.Intent && step.FinalReport {
			var job json.RawMessage
			err := t.QueryRow("SELECT job FROM xloom_executions WHERE project_id=? AND lease=? AND intent=?", state.Graph.Project.ID, fence.Run, fence.Intent).Scan(&job)
			if errors.Is(err, sql.ErrNoRows) {
				return Err(409, "final_report requires a registered execution")
			}
			if err != nil {
				return err
			}
			return t.checkReportInput(state, fence.Intent, job)
		}
	}
	return nil
}

// Follow only the proposed completion's support. A replaced historical report
// does not block a new one unless the new result still cites its stale content.
func (t *Tx) checkCompletionReports(state State, from []string) error {
	seen := map[string]bool{}
	queue := append([]string(nil), from...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, fact := range state.FactRecords {
			if fact.ID != id || fact.SourceStepID == "" {
				continue
			}
			for _, step := range state.Steps {
				if step.ID != fact.SourceStepID {
					continue
				}
				if step.FinalReport {
					if err := t.checkReportCompletion(state, ExecutionFence{Intent: step.ID, Run: Value(step.Worker)}); err != nil {
						return err
					}
				}
				queue = append(queue, step.From...)
			}
		}
	}
	return nil
}
