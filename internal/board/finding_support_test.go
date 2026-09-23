package board

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

type findingFixture struct {
	*planFixture
	first, second FactRecord
}

func newFindingFixture(t *testing.T) *findingFixture {
	f := &findingFixture{planFixture: newPlanFixture(t)}
	f.executor("evidence-one")
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO scoped_counters(project_id,kind,value) VALUES('proj_001','fact',2)")
		return err
	})
	for n, fact := range []*FactRecord{&f.first, &f.second} {
		ref := EvidenceRef{RunID: "evidence-one", Path: []string{"/runs/first.raw", "/runs/second.raw"}[n], Excerpt: []string{"first observation", "controlled replacement"}[n]}
		result := f.action("fact", ref.Path, map[string]any{"description": ref.Excerpt, "scope": "synthetic request", "observed_at": "2026-09-22T10:00:00Z", "evidence": []EvidenceRef{ref}})
		if err := json.Unmarshal(result.Result, fact); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *findingFixture) executor(run string) {
	f.t.Helper()
	f.fence = ExecutionFence{Run: "planner@first", Lease: "reason"}
	id := f.action("step", "plan-"+run, stepInput("Inspect evidence in "+run, []string{"origin"}, "goal", 0)).ID
	e := Execution{ProjectID: "proj_001", ID: run, Namespace: "test", Backend: "executor", Kind: "explore", Intent: id, Lease: "executor@" + run, RetryKey: "explore:" + id, Job: json.RawMessage(`{"graph":{"project":{"generation":0}}}`)}
	f.tx(func(tx *Tx) error {
		g, err := tx.Load(e.ProjectID)
		if err != nil {
			return err
		}
		for n := range g.Intents {
			if g.Intents[n].ID == id {
				g.Intents[n].Worker = Ptr(e.Lease)
			}
		}
		if err = tx.Save(g); err != nil {
			return err
		}
		if err = tx.RegisterExecution(e); err != nil {
			return err
		}
		return tx.ExecutionStatus(e, "running", nil)
	})
	f.fence = e.Fence()
}

func findingInput(facts ...FactRecord) map[string]any {
	sources, evidence := []string{}, []EvidenceRef{}
	for _, fact := range facts {
		sources = append(sources, fact.ID)
		evidence = append(evidence, fact.Evidence...)
	}
	return map[string]any{"claim": "The fixture requires authentication", "scope": "GET /private", "status": "verified", "sources": sources, "evidence": evidence, "reason": "The controlled request was rejected"}
}

func (f *findingFixture) correctFirst() {
	f.t.Helper()
	executor := f.fence
	f.fence = ExecutionFence{Run: "planner@first", Lease: "reason"}
	f.action("fact_relation", "correct-first", map[string]any{"kind": "supersedes", "source": f.second.ID, "target": f.first.ID, "reason": "The controlled request supersedes the earlier observation"})
	f.fence = executor
}

func stateEvents(f *planFixture) []StateEvent {
	f.t.Helper()
	var events []StateEvent
	f.tx(func(tx *Tx) (err error) {
		events, err = tx.StateEvents("proj_001", 0)
		return err
	})
	return events
}

func TestFindingExplicitSupportReplacementPreservesHistory(t *testing.T) {
	f := newFindingFixture(t)
	first := f.action("finding", "initial", findingInput(f.first))
	original := f.state().Findings[0]
	events := stateEvents(f.planFixture)
	f.correctFirst()
	if f.state().Findings[0].SupportValid {
		t.Fatal("superseded support remained valid")
	}
	// Existing clients retain merge semantics; replacement is never implicit.
	f.action("finding", "merge", findingInput(f.second))
	merged := f.state().Findings[0]
	if merged.SupportValid || !slices.Equal(merged.Sources, []string{f.first.ID, f.second.ID}) {
		t.Fatalf("default update discarded historical support: %+v", merged)
	}
	f.store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 1, 0, 0, time.UTC) }
	input := findingInput(f.second)
	input["replace_support"], input["reason"] = true, "Revalidated against the controlled replacement observation"
	replaced := f.action("finding", "replace", input)
	current := f.state()
	finding := current.Findings[0]
	if replaced.ID != first.ID || replaced.Unchanged || !finding.SupportValid || finding.CreatedAt != original.CreatedAt || finding.UpdatedAt == original.UpdatedAt || !slices.Equal(finding.Sources, []string{f.second.ID}) || !slices.Equal(finding.Evidence, f.second.Evidence) {
		t.Fatalf("replacement did not revalidate the existing finding: %+v %+v", replaced, finding)
	}
	var receipt Finding
	if err := json.Unmarshal(replaced.Result, &receipt); err != nil || !reflect.DeepEqual(receipt, finding) {
		t.Fatalf("receipt differs from current finding: %+v / %+v, %v", receipt, finding, err)
	}
	for _, fact := range current.FactRecords {
		if fact.ID == f.first.ID && (fact.Status != "superseded" || !slices.Equal(fact.Evidence, f.first.Evidence)) {
			t.Fatalf("support replacement rewrote the original observation: %+v", fact)
		}
	}
	afterEvents := stateEvents(f.planFixture)
	if !reflect.DeepEqual(events, afterEvents[:len(events)]) {
		t.Fatal("replacement changed immutable earlier events")
	}
	var history Finding
	if err := json.Unmarshal(events[len(events)-1].Result, &history); err != nil || !slices.Equal(history.Sources, []string{f.first.ID}) || !slices.Equal(history.Evidence, f.first.Evidence) {
		t.Fatalf("original finding support was lost from history: %+v, %v", history, err)
	}
	beforeRepeat := f.state()
	f.store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 2, 0, 0, time.UTC) }
	repeat := f.action("finding", "replace-again", input)
	if !repeat.Unchanged || !reflect.DeepEqual(beforeRepeat, f.state()) || !reflect.DeepEqual(afterEvents, stateEvents(f.planFixture)) {
		t.Fatal("identical support replacement changed state, timestamps or events")
	}
}

func TestFindingSupportReplacementRejectsInvalidRequestsAtomically(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		edit   func(map[string]any, *findingFixture)
	}{
		{"missing_reason", 422, func(p map[string]any, _ *findingFixture) { p["reason"] = " " }},
		{"missing_sources", 422, func(p map[string]any, _ *findingFixture) { delete(p, "sources") }},
		{"candidate_without_sources", 422, func(p map[string]any, _ *findingFixture) { p["status"], p["sources"] = "candidate", []string{} }},
		{"superseded_source", 409, func(p map[string]any, f *findingFixture) { p["sources"] = []string{f.first.ID} }},
		{"mixed_invalid_source", 409, func(p map[string]any, f *findingFixture) { p["sources"] = []string{f.first.ID, f.second.ID} }},
		{"unknown_source", 404, func(p map[string]any, _ *findingFixture) { p["sources"] = []string{"missing"} }},
		{"forged_evidence", 403, func(p map[string]any, _ *findingFixture) {
			p["evidence"] = []EvidenceRef{{RunID: "other-run", Path: "/unknown.raw", Excerpt: "unowned evidence"}}
		}},
		{"unknown_finding", 409, func(p map[string]any, _ *findingFixture) { p["claim"] = "A different finding" }},
		{"invalid_flag_type", 422, func(p map[string]any, _ *findingFixture) { p["replace_support"] = "true" }},
		{"null_flag", 422, func(p map[string]any, _ *findingFixture) { p["replace_support"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindingFixture(t)
			f.action("finding", "initial", findingInput(f.first))
			f.correctFirst()
			input := findingInput(f.second)
			input["replace_support"] = true
			tc.edit(input, f)
			before, events := f.state(), stateEvents(f.planFixture)
			raw, _ := json.Marshal(input)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("proj_001", f.fence, StateAction{Op: "finding", IdempotencyKey: "invalid", Payload: raw})
				return err
			})
			var api *APIError
			if !errors.As(err, &api) || api.Status != tc.status {
				t.Fatalf("got %v, want status %d", err, tc.status)
			}
			if !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, stateEvents(f.planFixture)) {
				t.Fatal("invalid replacement changed state or history")
			}
		})
	}
}

func TestRepeatedFindingPreservesTimeVersionAndEventsAcrossRuns(t *testing.T) {
	f := newFindingFixture(t)
	first := f.action("finding", "initial", findingInput(f.first, f.second))
	for _, anotherRun := range []bool{false, true} {
		if anotherRun {
			f.executor("evidence-two")
		}
		before, events := f.state(), stateEvents(f.planFixture)
		f.store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 2, 0, 0, time.UTC) }
		input := findingInput(f.second, f.first)
		if anotherRun {
			input["replace_support"] = false
		}
		result := f.action("finding", f.fence.Run+":repeat", input)
		if !result.Unchanged || result.ID != first.ID || result.Revision != before.Revision || result.StateVersion != DecisionStateVersion(before) || !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, stateEvents(f.planFixture)) {
			t.Fatalf("duplicate finding changed state or triggered a decision: %+v", result)
		}
	}
	before := f.state()
	changed := findingInput(f.first, f.second)
	changed["reason"] = "A newly reviewed explanation of the same retained observations"
	result := f.action("finding", "new-reason", changed)
	if result.Unchanged || f.state().DecisionRevision != before.DecisionRevision+1 || f.state().Findings[0].Reason != changed["reason"] {
		t.Fatal("a meaningful finding update was suppressed")
	}
}

func TestDuplicateFactRelationPreservesOriginalProvenanceAcrossPlannerRuns(t *testing.T) {
	f := newFindingFixture(t)
	f.fence = ExecutionFence{Run: "planner@first", Lease: "reason"}
	input := map[string]any{"kind": "supersedes", "source": f.second.ID, "target": f.first.ID, "reason": "Controlled evidence replaces the original observation"}
	first := f.action("fact_relation", "first-relation", input)
	for _, anotherRun := range []bool{false, true} {
		if anotherRun {
			f.planner("second")
		}
		before, events := f.state(), stateEvents(f.planFixture)
		f.store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 2, 0, 0, time.UTC) }
		result := f.action("fact_relation", f.fence.Run+":repeat-relation", input)
		if !result.Unchanged || string(result.Result) != string(first.Result) || result.Revision != before.Revision || result.StateVersion != DecisionStateVersion(before) || !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, stateEvents(f.planFixture)) {
			t.Fatalf("duplicate correction changed provenance or shared state: %+v", result)
		}
	}
	input["reason"] = "Additional independently reviewed rationale"
	changed := f.action("fact_relation", "new-relation-reason", input)
	if changed.Unchanged || len(f.state().FactRelations) != 2 {
		t.Fatal("different correction rationale was silently discarded")
	}
	// Deduplication never grants a new request permission to cite invalid evidence.
	f.action("fact_relation", "invalidate-correction", map[string]any{"kind": "refutes", "source": "f001", "target": f.second.ID, "reason": "A later independent observation rejects that premise"})
	before := f.state()
	raw, _ := json.Marshal(input)
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("proj_001", f.fence, StateAction{Op: "fact_relation", IdempotencyKey: "invalid-duplicate", Payload: raw})
		return err
	})
	var api *APIError
	if !errors.As(err, &api) || api.Status != 409 || !reflect.DeepEqual(before, f.state()) {
		t.Fatalf("duplicate bypassed effective-source validation: %v", err)
	}
}
