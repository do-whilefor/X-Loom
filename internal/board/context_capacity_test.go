package board

import (
	"fmt"
	"strings"
	"testing"
)

func contextCapacityHistory(count, descriptionBytes int) State {
	state := overviewState()
	description := strings.Repeat("x", descriptionBytes)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("history-%04d", i)
		state.FactRecords = append(state.FactRecords, FactRecord{ID: id, Description: description, Status: "valid",
			Evidence: []EvidenceRef{{RunID: "history", Path: "evidence/observation.txt", Excerpt: description}}})
		state.Steps = append(state.Steps, Step{ID: id, GoalID: "goal", From: []string{"first"}, Description: description, Status: "completed"})
		state.Findings = append(state.Findings, Finding{ID: id, Claim: description, Sources: []string{id}, Status: "candidate", SupportValid: true})
		state.FactRelations = append(state.FactRelations, FactRelation{Kind: "supersedes", Source: id, Target: "first", Reason: description})
	}
	state.Goals = append(state.Goals, Goal{ID: "child", ParentID: "goal", Condition: "Check the bounded scope", Sources: []string{"first"}, Status: "open"})
	state.Steps = append(state.Steps, Step{ID: "active", GoalID: "child", From: []string{"first"}, Description: "Read the relevant observations", Status: "running"})
	return state
}

func TestContextCapacityMatchesFullProjection(t *testing.T) {
	legacy := contextCapacityHistory(0, 0)
	legacy.Graph.Intents = []Intent{{ID: "active", From: []string{"origin"}, Description: "Read the service", Worker: Ptr("worker")}}
	legacy.Goals, legacy.Steps, legacy.FactRecords = nil, nil, nil
	missing := contextCapacityHistory(0, 0)
	missing.Steps[0].GoalID = "missing"
	cyclic := contextCapacityHistory(0, 0)
	cyclic.Goals[1].ParentID = "child"
	for name, state := range map[string]State{
		"legacy":           legacy,
		"small":            contextCapacityHistory(0, 0),
		"large":            contextCapacityHistory(128, 8192),
		"missing_ancestry": missing,
		"cyclic_ancestry":  cyclic,
	} {
		t.Run(name, func(t *testing.T) {
			for _, step := range []string{"", "active", "unknown"} {
				for _, budget := range []int{-1, 0, 1, 1024, 2048, 4096, 8192, DefaultContextViewBytes - contextAdmissionReserve, DefaultContextViewBytes} {
					_, fullErr := ContextView(state, step, budget)
					_, capacityErr := contextView(state, step, budget, true)
					if fmt.Sprint(fullErr) != fmt.Sprint(capacityErr) {
						t.Fatalf("step=%q budget=%d: full projection=%v, capacity=%v", step, budget, fullErr, capacityErr)
					}
				}
			}
		})
	}
}

func TestContextAdmissionPreservesEncodedInputBoundary(t *testing.T) {
	for _, content := range []struct{ name, unit string }{
		{"ascii", "x"},
		{"json_escaped", "\"\\\n<>&"},
		{"unicode", "权限"},
	} {
		for _, field := range []string{"input", "hint", "step"} {
			t.Run(content.name+"/"+field, func(t *testing.T) {
				stateFor := func(count int) State {
					state := contextCapacityHistory(0, 0)
					text := strings.Repeat(content.unit, count)
					switch field {
					case "input":
						state.Graph.Facts[0].Description = text
					case "hint":
						state.Graph.Hints = []Hint{{ID: "hint", Content: text, Creator: "user"}}
					case "step":
						state.Steps[0].Description = text
					}
					return state
				}
				fullCheck := func(state State) error {
					for _, step := range []string{"", "active"} {
						if _, err := ContextView(state, step, DefaultContextViewBytes-contextAdmissionReserve); err != nil {
							return err
						}
					}
					return nil
				}
				// Find the execution projection's last admitted input; capacity
				// must retain this boundary after JSON escaping and its reserve.
				low, high := 0, DefaultContextViewBytes
				if err := fullCheck(stateFor(low)); err != nil {
					t.Fatal(err)
				}
				if err := fullCheck(stateFor(high)); err == nil {
					t.Fatal("fixture has no upper capacity bound")
				}
				for low+1 < high {
					mid := low + (high-low)/2
					if fullCheck(stateFor(mid)) == nil {
						low = mid
					} else {
						high = mid
					}
				}
				for _, count := range []int{low - 1, low, high} {
					state := stateFor(count)
					fullErr, admissionErr := fullCheck(state), ValidateContextCapacity(state)
					if (fullErr == nil) != (admissionErr == nil) {
						t.Fatalf("input units=%d: full projection=%v, admission=%v", count, fullErr, admissionErr)
					}
					if admissionErr != nil && (!strings.Contains(admissionErr.Error(), "input_context_limit") || !strings.Contains(admissionErr.Error(), fullErr.Error())) {
						t.Fatalf("capacity error lost execution diagnostic: %v", admissionErr)
					}
				}
			})
		}
	}
}

func TestContextAdmissionRetainsRetryableStepCheck(t *testing.T) {
	for _, status := range []string{"open", "running", "needs_review", "failed", "completed", "abandoned"} {
		t.Run(status, func(t *testing.T) {
			state := contextCapacityHistory(0, 0)
			state.Steps[0].Status = status
			state.Steps[0].Description = strings.Repeat("x", DefaultContextViewBytes)
			err := ValidateContextCapacity(state)
			terminal := status == "completed" || status == "abandoned"
			if (err == nil) != terminal {
				t.Fatalf("status=%s: capacity error=%v", status, err)
			}
			if err != nil && !strings.Contains(err.Error(), "mandatory input for Step active") {
				t.Fatalf("capacity error did not identify the oversized step: %v", err)
			}
		})
	}
}

func BenchmarkContextCapacity(b *testing.B) {
	state := contextCapacityHistory(128, 8192)
	for _, full := range []bool{false, true} {
		b.Run(fmt.Sprintf("full_projection=%t", full), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				if !full {
					if err := ValidateContextCapacity(state); err != nil {
						b.Fatal(err)
					}
					continue
				}
				for _, step := range []string{"", "active"} {
					if _, err := ContextView(state, step, DefaultContextViewBytes-contextAdmissionReserve); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
