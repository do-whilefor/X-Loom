package board

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestFinalReportWaitsForScopeAndBindsAllEvidence(t *testing.T) {
	f := newPlanFixture(t)
	dep := f.action("step", "probe", stepInput("Inspect", []string{"origin"}, "goal", 0)).ID
	input := stepInput("Final report", []string{"origin"}, "goal", 0)
	input["final_report"] = true
	raw, _ := json.Marshal(input)
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("proj_001", f.fence, StateAction{Op: "step", IdempotencyKey: "premature", Payload: raw})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "report_dependencies_pending") || len(f.state().Steps) != 1 {
		t.Fatalf("premature report was accepted or mutated state: %v", err)
	}
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": dep, "reason": "Explicitly out of scope"})
	id := f.action("step", "report", input).ID
	var report Step
	for _, step := range f.state().Steps {
		if step.ID == id {
			report = step
		}
	}
	if !report.FinalReport || !slices.Equal(report.From, []string{"origin", "f001", "f002"}) {
		t.Fatalf("report metadata or sources lost in persistence: %+v", report)
	}
	f.tx(func(tx *Tx) error { return tx.StepReady("proj_001", id) })
	f.action("step", "late-work", stepInput("New bounded check", []string{"origin"}, "goal", 0))
	err = f.store.Do(context.Background(), func(tx *Tx) error { return tx.StepReady("proj_001", id) })
	if err == nil || !strings.Contains(err.Error(), "report_dependencies_pending") {
		t.Fatalf("late work did not stop report claim: %v", err)
	}
}

func reportScopeFixture() (State, Step) {
	report := Step{ID: "report", GoalID: "audit", From: []string{"origin", "observed"}, FinalReport: true}
	state := State{
		Graph:       Graph{Project: Project{ID: "project", Generation: 2}},
		Goals:       []Goal{{ID: "goal"}, {ID: "audit", ParentID: "goal", Condition: "Audit"}, {ID: "other", ParentID: "goal"}},
		Steps:       []Step{{ID: "probe", GoalID: "audit", Status: "completed", Result: Ptr("observed")}, {ID: "other-probe", GoalID: "other", Status: "running"}, report},
		FactRecords: []FactRecord{{ID: "origin", Status: "input"}, {ID: "goal", Status: "input"}, {ID: "observed", SourceStepID: "probe", Status: "valid", Evidence: []EvidenceRef{{Path: "raw.txt", Excerpt: "original"}}}},
	}
	return state, report
}

func TestFinalReportScopeTracksEvidenceWithoutUnrelatedChurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*State)
		stale  bool
	}{
		{"own output", func(s *State) {
			s.FactRecords = append(s.FactRecords, FactRecord{ID: "report-output", SourceStepID: "report", Status: "valid"})
		}, false},
		{"unrelated fact", func(s *State) {
			s.FactRecords = append(s.FactRecords, FactRecord{ID: "other-output", SourceStepID: "other-probe", Status: "valid"})
		}, false},
		{"lease and scheduling", func(s *State) {
			s.Steps[0].Worker, s.Steps[0].Priority, s.Steps[0].Reason = Ptr("other-worker"), 100, "Schedule first"
			s.Revision++
		}, false},
		{"goal closure", func(s *State) { s.Goals[1].Status, s.Goals[1].Reason = "achieved", "Observed" }, false},
		{"late evidence", func(s *State) {
			s.FactRecords = append(s.FactRecords, FactRecord{ID: "late", SourceStepID: "probe", Status: "valid"})
		}, true},
		{"evidence bytes", func(s *State) { s.FactRecords[2].Evidence[0].Excerpt = "corrected" }, true},
		{"new relevant step", func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "late-step", GoalID: "audit", Status: "abandoned", Reason: "Not tested"})
		}, true},
		{"new descendant goal", func(s *State) {
			s.Goals = append(s.Goals, Goal{ID: "child", ParentID: "audit", Condition: "New coverage"})
		}, true},
		{"correction outside goal", func(s *State) {
			s.FactRecords = append(s.FactRecords, FactRecord{ID: "correction", SourceStepID: "other-probe", Status: "valid"})
			s.FactRelations = append(s.FactRelations, FactRelation{Kind: "refutes", Source: "correction", Target: "observed"})
		}, true},
		{"finding", func(s *State) {
			s.Findings = append(s.Findings, Finding{ID: "finding", Sources: []string{"observed"}, Claim: "Verified"})
		}, true},
		{"hint", func(s *State) { s.Graph.Hints = append(s.Graph.Hints, Hint{Content: "New requirement"}) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, report := reportScopeFixture()
			before, err := reportScope(state, report)
			if err != nil {
				t.Fatal(err)
			}
			version := DecisionStateVersion(before)
			tc.change(&state)
			after, err := reportScope(state, report)
			if err != nil {
				t.Fatal(err)
			}
			if changed := version != DecisionStateVersion(after); changed != tc.stale {
				t.Fatalf("scope changed=%v, want %v", changed, tc.stale)
			}
		})
	}
}

func TestFinalReportPendingStatusesAndExplicitSources(t *testing.T) {
	for _, status := range []string{"open", "running", "failed", "needs_review", "completed", "abandoned"} {
		t.Run(status, func(t *testing.T) {
			state, report := reportScopeFixture()
			state.Steps[0].Status = status
			_, err := reportScope(state, report)
			if (err == nil) != (status == "completed" || status == "abandoned") {
				t.Fatalf("wrong dependency readiness for %s: %v", status, err)
			}
		})
	}
	state, report := reportScopeFixture()
	state.FactRecords = append(state.FactRecords, FactRecord{ID: "external", SourceStepID: "other-probe", Status: "valid"})
	report.From = append(report.From, "external")
	if _, err := reportScope(state, report); err == nil || !strings.Contains(err.Error(), "other-probe") {
		t.Fatalf("explicit producer outside goal was not gated: %v", err)
	}
}

func registeredReport(t *testing.T) (*planFixture, Execution, json.RawMessage) {
	t.Helper()
	f := newPlanFixture(t)
	input := stepInput("Final report", []string{"origin"}, "goal", 0)
	input["final_report"] = true
	id := f.action("step", "report", input).ID
	e := claimedPlanExecution(f, id)
	f.tx(func(tx *Tx) error {
		state, err := tx.State(e.ProjectID)
		if err != nil {
			return err
		}
		ref, err := tx.FreezeInput(state)
		if err != nil {
			return err
		}
		e.Job, err = json.Marshal(map[string]any{"graph": state.Graph, "input_snapshot": ref, "result_contract_version": 2})
		if err != nil {
			return err
		}
		for n := 0; n < 2; n++ {
			if _, err = tx.Next(e.ProjectID, "fact"); err != nil {
				return err
			}
		}
		return tx.RegisterExecution(e)
	})
	payload, _ := json.Marshal(map[string]any{"description": "Completed report", "scope": "synthetic fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": []EvidenceRef{{RunID: e.ID, Path: "/runs/" + e.ID + "/report.json", Excerpt: "Verified report"}}})
	return f, e, payload
}

func appendReportTestFact(f *planFixture) {
	f.t.Helper()
	f.tx(func(tx *Tx) error {
		graph, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		graph.Facts = append(graph.Facts, Fact{ID: "late", Description: "New legacy observation"})
		return tx.Save(graph)
	})
}

func TestFinalReportCompletionChecksFrozenInputAndOwnOutput(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "late evidence"}[stale], func(t *testing.T) {
			f, e, payload := registeredReport(t)
			if stale {
				appendReportTestFact(f)
			}
			before := f.state()
			var result Conclusion
			err := f.store.Do(context.Background(), func(tx *Tx) (err error) {
				result, err = tx.ConcludeEvidenceStep(e.ProjectID, e.Fence(), "", payload)
				return err
			})
			if stale {
				if err == nil || !strings.Contains(err.Error(), "report_dependencies_changed") || len(f.state().FactRecords) != len(before.FactRecords) {
					t.Fatalf("stale report completed or leaked final fact: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f.tx(func(tx *Tx) error { return tx.ValidateStateCompletion(e.ProjectID, []string{result.Fact.ID}) })
			appendReportTestFact(f)
			err = f.store.Do(context.Background(), func(tx *Tx) error { return tx.ValidateStateCompletion(e.ProjectID, []string{result.Fact.ID}) })
			if err == nil || !strings.Contains(err.Error(), "report_dependencies_changed") {
				t.Fatalf("project accepted stale completed report: %v", err)
			}
		})
	}
}

func TestFinalReportStartRejectsChangedFinding(t *testing.T) {
	f, e, _ := registeredReport(t)
	f.tx(func(tx *Tx) error {
		data, revision, decision, err := tx.stateData(e.ProjectID)
		if err != nil {
			return err
		}
		data.Findings = append(data.Findings, Finding{ID: "late-finding", Claim: "Corrected finding", Sources: []string{"f001"}})
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE xloom_state SET data=?,revision=?,decision_revision=? WHERE project_id=?", string(raw), revision, decision, e.ProjectID)
		return err
	})
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.ExecutionStatus(e, "running", nil) })
	if err == nil || !strings.Contains(err.Error(), "report_dependencies_changed") {
		t.Fatalf("obsolete report started: %v", err)
	}
	f.tx(func(tx *Tx) error {
		stored, err := tx.Execution(e.ProjectID, e.ID)
		if err == nil && stored.Status != "prepared" {
			t.Errorf("start mutated execution: %s", stored.Status)
		}
		return err
	})
}

func TestFinalReportRequiresRegisteredEvidenceContract(t *testing.T) {
	f := newPlanFixture(t)
	input := stepInput("Final report", []string{"origin"}, "goal", 0)
	input["final_report"] = true
	id := f.action("step", "report", input).ID
	e := claimedPlanExecution(f, id)
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(e) })
	if err == nil || !strings.Contains(err.Error(), "immutable input snapshot") {
		t.Fatalf("legacy registration bypassed report barrier: %v", err)
	}
	err = f.store.Do(context.Background(), func(tx *Tx) error { return tx.CheckLegacyConclusion(e.ProjectID, e.Lease) })
	if err == nil || !strings.Contains(err.Error(), "registered evidence-contract") {
		t.Fatalf("legacy conclusion bypassed report barrier: %v", err)
	}
}

func TestFinalReportOverviewRetainsDesignation(t *testing.T) {
	state, _ := reportScopeFixture()
	view, err := buildContextOverview(state, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range view.Sections {
		for _, item := range section.Items {
			if item.ID == "report" {
				if !item.FinalReport {
					t.Fatal("compact discovery context hid report designation")
				}
				return
			}
		}
	}
	t.Fatal("report omitted from small fixture")
}

func TestCompletionChecksOnlyCitedReportSupportClosure(t *testing.T) {
	f, e, payload := registeredReport(t)
	var result Conclusion
	f.tx(func(tx *Tx) (err error) {
		result, err = tx.ConcludeEvidenceStep(e.ProjectID, e.Fence(), "", payload)
		return err
	})
	appendReportTestFact(f)
	state := f.state()
	state.Steps = append(state.Steps, Step{ID: "consumer", GoalID: "other", From: []string{result.Fact.ID}, Status: "completed"})
	state.FactRecords = append(state.FactRecords, FactRecord{ID: "derived", SourceStepID: "consumer", Status: "valid"})
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.checkCompletionReports(state, []string{"derived"}) })
	if err == nil || !strings.Contains(err.Error(), "report_dependencies_changed") {
		t.Fatalf("derived completion hid stale report support: %v", err)
	}
	f.tx(func(tx *Tx) error { return tx.checkCompletionReports(state, []string{"f001"}) })
}
