package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// P1's fixed F2/F3 contrast: only S2 changes after the cursor. Assertions
// inspect full records, since the overview can already mention omitted nodes.
func decisionProducerState(from ...string) State {
	s := overviewState()
	s.Revision = 2
	s.Findings = nil
	s.FactRecords = []FactRecord{
		{ID: "F1", Description: "Upstream observation", Status: "valid"},
		{ID: "F2", Description: "Process observation", Status: "valid", SourceStepID: "S1"},
		{ID: "F3", Description: "Final observation", Status: "valid", SourceStepID: "S1"},
		{ID: "F4", Description: "Another process observation", Status: "valid", SourceStepID: "S1"},
	}
	s.Steps = []Step{
		{ID: "S1", GoalID: "goal", From: []string{"F1"}, Result: Ptr("F3"), Status: "completed"},
		{ID: "S2", GoalID: "goal", From: from, Status: "open"},
	}
	return s
}

func producerDecision(t *testing.T, s State, budget int) (*DecisionContext, decisionView) {
	t.Helper()
	cursor := &DecisionCursor{ProjectID: s.Graph.Project.ID, Generation: s.Graph.Project.Generation, Revision: 1}
	decision, err := BuildDecisionContextFromCursor(s, cursor, []StateEvent{{Revision: 2, Op: "step", ID: "S2"}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	var body decisionView
	if err := json.Unmarshal(decision.View, &body); err != nil {
		t.Fatal(err)
	}
	return decision, body
}

func assertProducerNodes(t *testing.T, body decisionView, steps, facts []string) {
	t.Helper()
	gotSteps, gotFacts := []string{}, []string{}
	for _, step := range body.Steps {
		gotSteps = append(gotSteps, step.ID)
	}
	for _, fact := range body.Facts {
		gotFacts = append(gotFacts, fact.ID)
	}
	if !slices.Equal(gotSteps, steps) || !slices.Equal(gotFacts, facts) {
		t.Fatalf("full records: steps=%v facts=%v; want steps=%v facts=%v", gotSteps, gotFacts, steps, facts)
	}
}

func TestDecisionProcessFactProducerClosure(t *testing.T) {
	for _, entry := range []string{"cursor", "snapshot"} {
		for _, test := range []struct {
			name   string
			from   []string
			facts  []string
			legacy bool
		}{
			{"process", []string{"F2"}, []string{"F1", "F2"}, false},
			{"final", []string{"F3"}, []string{"F1", "F3"}, false},
			{"multiple_process", []string{"F2", "F4"}, []string{"F1", "F2", "F4"}, false},
			{"legacy_result", []string{"F3"}, []string{"F1", "F3"}, true},
		} {
			t.Run(entry+"/"+test.name, func(t *testing.T) {
				s := decisionProducerState(test.from...)
				if test.legacy {
					s.FactRecords[2].SourceStepID = ""
					s.FactRecords[2].Legacy = true
				}
				original, _ := json.Marshal(s)
				var decision *DecisionContext
				var body decisionView
				if entry == "cursor" {
					decision, body = producerDecision(t, s, 0)
				} else {
					previous := s
					previous.Revision = 1
					previous.Steps = s.Steps[:1]
					var err error
					decision, err = BuildDecisionContext(s, &previous, []StateEvent{{Revision: 2, Op: "step", ID: "S2"}}, 0)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(decision.View, &body); err != nil {
						t.Fatal(err)
					}
				}
				if decision.Mode != "changes" || decision.FromRevision != 1 || decision.ToRevision != 2 {
					t.Fatalf("wrong context boundary: %+v", decision)
				}
				assertProducerNodes(t, body, []string{"S1", "S2"}, test.facts)
				if !reflect.DeepEqual(body.Steps, s.Steps) {
					t.Fatal("producer records were rewritten")
				}
				for _, fact := range body.Facts {
					for _, originalFact := range s.FactRecords {
						if fact.ID == originalFact.ID && !reflect.DeepEqual(fact, originalFact) {
							t.Fatal("fact provenance or evidence was rewritten")
						}
					}
				}
				after, _ := json.Marshal(s)
				if !bytes.Equal(original, after) {
					t.Fatal("context construction mutated authoritative state")
				}
			})
		}
	}
}

func TestDecisionProducerDoesNotGuessConflictingOrMissingSources(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*State)
		steps  []string
		facts  []string
	}{
		{"explicit_over_result", func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "old", GoalID: "goal", From: []string{"F4"}, Result: Ptr("F2"), Status: "completed"})
		}, []string{"S1", "S2"}, []string{"F1", "F2"}},
		{"missing_explicit_step", func(s *State) {
			s.FactRecords[1].SourceStepID = "missing"
			s.Steps[0].Result = Ptr("F2")
		}, []string{"S2"}, []string{"F2"}},
		{"ambiguous_legacy_result", func(s *State) {
			s.FactRecords[1].SourceStepID = ""
			s.Steps[0].Result = Ptr("F2")
			s.Steps = append(s.Steps, Step{ID: "old", GoalID: "goal", From: []string{"F4"}, Result: Ptr("F2"), Status: "completed"})
		}, []string{"S2"}, []string{"F2"}},
		{"missing_fact", func(s *State) {
			s.FactRecords = append(s.FactRecords[:1], s.FactRecords[2:]...)
			s.Steps[0].Result = Ptr("F2")
		}, []string{"S2"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := decisionProducerState("F2")
			test.change(&s)
			decision, body := producerDecision(t, s, 0)
			if decision.Mode != "changes" {
				t.Fatalf("unexpected fallback: %s", decision.Fallback)
			}
			assertProducerNodes(t, body, test.steps, test.facts)
		})
	}
}

func TestDecisionProducerKeepsResultsOfSelectedSteps(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		event  StateEvent
		facts  []string
	}{
		{"changed_completed_step", "completed", StateEvent{Revision: 2, Op: "step_completed", ID: "S2"}, []string{"F1", "F2", "F5"}},
		{"affected_completed_step", "completed", StateEvent{Revision: 2, Op: "fact", ID: "F2"}, []string{"F1", "F2", "F5"}},
		{"active_step", "running", StateEvent{Revision: 2, Op: "fact", ID: "F4"}, []string{"F1", "F2", "F4", "F5"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := decisionProducerState("F2")
			s.Steps[1].Status, s.Steps[1].Result = test.status, Ptr("F5")
			s.FactRecords = append(s.FactRecords, FactRecord{ID: "F5", Status: "valid", SourceStepID: "S2"})
			cursor := &DecisionCursor{ProjectID: s.Graph.Project.ID, Generation: s.Graph.Project.Generation, Revision: 1}
			decision, err := BuildDecisionContextFromCursor(s, cursor, []StateEvent{test.event}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var body decisionView
			if err := json.Unmarshal(decision.View, &body); err != nil {
				t.Fatal(err)
			}
			if decision.Mode != "changes" {
				t.Fatalf("unexpected fallback: %s", decision.Fallback)
			}
			assertProducerNodes(t, body, []string{"S1", "S2"}, test.facts)
		})
	}
}

func TestDecisionProducerSupportsGraphOnlyLegacyInput(t *testing.T) {
	s := decisionProducerState("F3")
	s.Goals, s.Steps, s.FactRecords = nil, nil, nil
	s.Graph.Facts = append(s.Graph.Facts, Fact{ID: "F1", Description: "Upstream"}, Fact{ID: "F3", Description: "Final"})
	s.Graph.Intents = []Intent{
		{ID: "S1", From: []string{"F1"}, To: Ptr("F3")},
		{ID: "S2", From: []string{"F3"}},
	}
	_, body := producerDecision(t, s, 0)
	assertProducerNodes(t, body, []string{"S1", "S2"}, []string{"F1", "F3"})
	for _, fact := range body.Facts {
		if !fact.Legacy || fact.SourceStepID != "" {
			t.Fatal("legacy projection invented explicit provenance")
		}
	}
}

func TestDecisionProducerDoesNotExpandSiblingOutputsOrSharedOrigin(t *testing.T) {
	s := decisionProducerState("F2")
	s.Steps[0].From = []string{"F1", "origin", "goal"}
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("sibling-%03d", i)
		s.FactRecords = append(s.FactRecords, FactRecord{ID: id, Status: "valid", SourceStepID: "S1"})
		s.Steps = append(s.Steps, Step{ID: fmt.Sprintf("history-%03d", i), GoalID: "goal", From: []string{"origin"}, Status: "completed"})
	}
	// Neither immutable input is an output, even if old data claims otherwise.
	s.FactRecords = append(s.FactRecords, FactRecord{ID: "origin", SourceStepID: "input-producer"}, FactRecord{ID: "goal"})
	s.Steps = append(s.Steps,
		Step{ID: "input-producer", Result: Ptr("origin"), Status: "completed"},
		Step{ID: "goal-producer", Result: Ptr("goal"), Status: "completed"},
	)
	decision, body := producerDecision(t, s, 0)
	if decision.Mode != "changes" {
		t.Fatalf("irrelevant history expanded closure: %s", decision.Fallback)
	}
	assertProducerNodes(t, body, []string{"S1", "S2"}, []string{"F1", "F2"})
	if body.Omitted["steps"] != len(s.Steps)-2 || body.Omitted["fact_records"] != len(s.FactRecords)-4 {
		t.Fatal("omitted siblings or historical steps were not counted")
	}
	for _, section := range body.Overview.Sections {
		if section.Total != section.Omitted+len(section.Items) {
			t.Fatal("discovery overview lost its omission boundary")
		}
	}
}

func decisionProducerChain(count int, cycle bool) State {
	s := decisionProducerState("chain-000")
	s.FactRecords, s.Steps = nil, s.Steps[1:]
	for i := 0; i < count; i++ {
		id, producer := fmt.Sprintf("chain-%03d", i), fmt.Sprintf("producer-%03d", i)
		next := "origin"
		if i+1 < count {
			next = fmt.Sprintf("chain-%03d", i+1)
		} else if cycle {
			next = "chain-000"
		}
		s.FactRecords = append(s.FactRecords, FactRecord{ID: id, Status: "valid", SourceStepID: producer})
		s.Steps = append(s.Steps, Step{ID: producer, From: []string{next}, GoalID: "goal", Status: "completed"})
	}
	return s
}

func TestDecisionProducerClosureTerminatesWithStableOrder(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(fmt.Sprintf("cycle=%t", cycle), func(t *testing.T) {
			s := decisionProducerChain(96, cycle)
			first, body := producerDecision(t, s, 256<<10)
			if first.Mode != "changes" || len(body.Facts) != 96 || len(body.Steps) != 97 {
				t.Fatalf("incomplete or duplicate closure: mode=%s facts=%d steps=%d", first.Mode, len(body.Facts), len(body.Steps))
			}
			if !reflect.DeepEqual(body.Facts, s.FactRecords) || !reflect.DeepEqual(body.Steps, s.Steps) {
				t.Fatal("closure lost storage order or original records")
			}
			second, _ := producerDecision(t, s, 256<<10)
			if !bytes.Equal(first.View, second.View) {
				t.Fatal("identical state produced unstable context")
			}
		})
	}
}

func TestDecisionProducerClosureFallsBackWithinBudget(t *testing.T) {
	s := decisionProducerChain(96, false)
	const budget = 8 << 10
	decision, body := producerDecision(t, s, budget)
	if decision.Mode != "full" || decision.Fallback != "related_context_over_budget" || len(decision.View) > budget {
		t.Fatalf("closure exceeded budget without fallback: mode=%s fallback=%s bytes=%d", decision.Mode, decision.Fallback, len(decision.View))
	}
	if body.Omitted["fact_records"] == 0 || body.Omitted["steps"] == 0 || body.ReadMore == "" || len(body.UserInputs) != 2 {
		t.Fatal("fallback hid omitted support or displaced original requirements")
	}
	for _, section := range body.Overview.Sections {
		if section.Total != section.Omitted+len(section.Items) {
			t.Fatal("fallback lost graph discovery")
		}
	}
}

func TestDecisionProducerKeepsIndependentObservationAfterCorrection(t *testing.T) {
	s := decisionProducerState("F2")
	s.FactRecords[0].Status = "refuted"
	s.FactRecords = append(s.FactRecords, FactRecord{ID: "correction", Status: "valid"})
	s.FactRelations = []FactRelation{{Kind: "refutes", Source: "correction", Target: "F1", Reason: "Different environment"}}
	s.Steps[0].Status = "abandoned"
	s.Steps[0].InvalidSources = []string{"F1"}
	_, body := producerDecision(t, s, 0)
	assertProducerNodes(t, body, []string{"S1", "S2"}, []string{"F1", "F2", "correction"})
	if body.Facts[0].Status != "refuted" || body.Facts[1].Status != "valid" || len(body.Relations) != 1 || body.Steps[0].Status != "abandoned" {
		t.Fatal("producer ancestry changed independent evidence or lost its correction")
	}
}
