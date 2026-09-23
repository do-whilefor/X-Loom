package board

import "encoding/json"

const MaxCompletionReviewBytes = 48 << 10

// CompletionReview presents the original requirements and cited observations.
// It is a review input, not a machine judgment that the proof satisfies them.
type CompletionReview struct {
	StateVersion   string       `json:"state_version"`
	Acceptance     string       `json:"acceptance"`
	UserInputs     []Fact       `json:"user_inputs"`
	Hints          []Hint       `json:"hints"`
	From           []string     `json:"from"`
	Description    string       `json:"description"`
	FactRecords    []FactRecord `json:"fact_records"`
	OmittedFactIDs []string     `json:"omitted_fact_ids,omitempty"`
	ReadMore       string       `json:"read_more,omitempty"`
}

const completionReviewReadMore = "Read omitted Fact records and evidence using read_graph with these IDs before accepting the completion proof; omission is not acceptance."

func buildCompletionReview(state State, version string, payload json.RawMessage, maxBytes int) (*CompletionReview, error) {
	var proposed struct {
		From        []string `json:"from"`
		Description string   `json:"description"`
	}
	if err := decodeAction(payload, &proposed); err != nil {
		return nil, err
	}
	review := &CompletionReview{
		StateVersion: version, Acceptance: "not_checked",
		UserInputs: []Fact{}, Hints: append([]Hint{}, state.Graph.Hints...),
		From: append([]string{}, proposed.From...), Description: proposed.Description,
		FactRecords: []FactRecord{}, OmittedFactIDs: append([]string{}, proposed.From...),
		ReadMore: completionReviewReadMore,
	}
	for _, fact := range state.Graph.Facts {
		if fact.ID == "origin" || fact.ID == "goal" {
			review.UserInputs = append(review.UserInputs, fact)
		}
	}
	// Reserve all omitted IDs up front. No original requirement, proof or
	// citation can silently disappear when individual observations are large.
	if raw, err := json.Marshal(review); err != nil {
		return nil, err
	} else if len(raw) > maxBytes {
		return nil, Err(422, "completion review requirements and proposal exceed the review byte budget")
	}
	facts := make(map[string]FactRecord, len(state.FactRecords))
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	for _, id := range proposed.From {
		fact, ok := facts[id]
		if !ok {
			return nil, Err(404, "completion review Fact "+id+" not found")
		}
		review.FactRecords = append(review.FactRecords, fact)
		// Conservatively retain the reserved omissions while testing capacity;
		// a successful inclusion may only reduce the final encoded size.
		raw, err := json.Marshal(review)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			review.FactRecords = review.FactRecords[:len(review.FactRecords)-1]
			continue
		}
		for n, omitted := range review.OmittedFactIDs {
			if omitted == id {
				review.OmittedFactIDs = append(review.OmittedFactIDs[:n], review.OmittedFactIDs[n+1:]...)
				break
			}
		}
	}
	if len(review.OmittedFactIDs) == 0 {
		review.ReadMore = ""
	}
	return review, nil
}
