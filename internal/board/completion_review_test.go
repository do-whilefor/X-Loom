package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func completionReviewFixture() (State, json.RawMessage) {
	state := State{
		Graph: Graph{
			Facts: []Fact{{ID: "origin", Description: "prod/build-42 as anonymous"}, {ID: "goal", Description: "Obtain the actual HTTP response status for /c"}},
			Hints: []Hint{{ID: "h001", Creator: "user", Content: "Preserve the required environment"}},
		},
		FactRecords: []FactRecord{{ID: "f001", Description: "/c received no response", Scope: "prod/build-42 as anonymous", Status: "valid", Evidence: []EvidenceRef{{RunID: "observe", Path: "retained/request.json", Excerpt: "{\"http_status\":null,\"response_received\":false}\n"}}}},
	}
	return state, json.RawMessage(`{"from":["f001"],"description":"All paths accounted for; /c has no status"}`)
}

func TestCompletionReviewPreservesNegativeEvidenceWithoutSemanticAcceptance(t *testing.T) {
	state, payload := completionReviewFixture()
	// Root requirements come from the original user input, not a generated
	// goal description or the model's proposed completion explanation.
	state.Goals = []Goal{{ID: "goal", Condition: "Account for /c without obtaining its status"}}
	review, err := buildCompletionReview(state, "original-version", payload, MaxCompletionReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	if review.Acceptance != "not_checked" || review.StateVersion != "original-version" || !reflect.DeepEqual(review.UserInputs, state.Graph.Facts) || !reflect.DeepEqual(review.Hints, state.Graph.Hints) || !reflect.DeepEqual(review.FactRecords, state.FactRecords) {
		t.Fatalf("review rewrote or semantically accepted the authoritative input: %+v", review)
	}
	if len(review.OmittedFactIDs) != 0 || review.ReadMore != "" || review.Description != "All paths accounted for; /c has no status" {
		t.Fatalf("review changed the proposed proof or omitted present evidence: %+v", review)
	}
}

func TestCompletionReviewKeepsWholeFactsAndBudgetedOmissionIDs(t *testing.T) {
	state, _ := completionReviewFixture()
	large := state.FactRecords[0]
	large.ID = "large"
	large.Evidence = []EvidenceRef{}
	for n := 0; n < 8; n++ {
		large.Evidence = append(large.Evidence, EvidenceRef{RunID: "observe", Path: "retained/large.json", Excerpt: strings.Repeat("\u003c", 8192)})
	}
	state.FactRecords = append([]FactRecord{large}, state.FactRecords...)
	first, err := buildCompletionReview(state, "v", json.RawMessage(`{"from":["large","f001"],"description":"proposed proof"}`), MaxCompletionReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.OmittedFactIDs, []string{"large"}) || first.ReadMore == "" || !reflect.DeepEqual(first.FactRecords, state.FactRecords[1:]) || !reflect.DeepEqual(first.UserInputs, state.Graph.Facts) {
		t.Fatalf("large evidence was truncated, omission lost, or later small evidence excluded: %+v", first)
	}
	raw, _ := json.Marshal(first)
	if len(raw) > MaxCompletionReviewBytes {
		t.Fatalf("encoded review exceeded its budget: %d", len(raw))
	}
}

func TestCompletionReviewRejectsOversizedRequirementsAndProposal(t *testing.T) {
	for _, field := range []string{"goal", "hint", "proof", "source_ids"} {
		t.Run(field, func(t *testing.T) {
			state, payload := completionReviewFixture()
			large := strings.Repeat("x", MaxCompletionReviewBytes)
			switch field {
			case "goal":
				state.Graph.Facts[1].Description = large
			case "hint":
				state.Graph.Hints[0].Content = large
			case "proof":
				payload, _ = json.Marshal(map[string]any{"from": []string{"f001"}, "description": large})
			case "source_ids":
				payload, _ = json.Marshal(map[string]any{"from": []string{large}, "description": "proof"})
			}
			if review, err := buildCompletionReview(state, "v", payload, MaxCompletionReviewBytes); err == nil || review != nil || !strings.Contains(err.Error(), "byte budget") {
				t.Fatalf("oversized mandatory %s was silently truncated: review=%+v err=%v", field, review, err)
			}
		})
	}
}
