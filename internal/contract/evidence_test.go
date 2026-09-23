package contract

import (
	"testing"
)

func TestEvidenceResultRequiresOneSupportedAnchor(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"published fact", `{"fact_id":"f001"}`, true},
		{"selected evidence", `{"fact":{"description":"No authentication challenge observed","scope":"GET /fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt"}]}}`, true},
		{"bare legacy description", `{"description":"Looks done"}`, false},
		{"ambiguous anchor", `{"fact_id":"f001","fact":{}}`, false},
		{"null anchor", `{"fact_id":null}`, false},
		{"no evidence", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[]}}`, false},
		{"source identity injection", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt"}],"source_step_id":"other"}}`, false},
		{"unknown evidence field", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt","claim":"made up"}]}}`, false},
		{"null selection field", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt","start_line":null}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := `{"accepted":true,"outcome":"completed","data":` + tc.data + `}`
			for _, conclude := range []bool{false, true} {
				got, err := ParseWithPolicy(output, "explore", conclude, 1, 3, Policy{Version: 2})
				if (err == nil) != tc.valid {
					t.Fatalf("result=%+v error=%v", got, err)
				}
				if err == nil && (got.Kind != "fact" || got.Outcome != "completed" || (got.FactID == "") == (len(got.FactPayload) == 0)) {
					t.Fatalf("invalid anchor: %+v", got)
				}
			}
		})
	}
}

func TestEvidenceBootstrapRetainsProjectCompletionBoundary(t *testing.T) {
	output := `{"accepted":true,"outcome":"completed","data":{"fact_id":"f001","complete":{"description":"All requirements verified"}}}`
	for _, conclude := range []bool{false, true} {
		got, err := ParseWithPolicy(output, "bootstrap", conclude, 1, 3, Policy{Version: 2})
		if err != nil || got.FactID != "f001" || (got.Kind == "complete") == conclude {
			t.Fatalf("result=%+v error=%v", got, err)
		}
	}
	for _, output := range []string{
		`{"accepted":true,"outcome":"continue","reason":"remaining check"}`,
		`{"accepted":true,"outcome":"incomplete","reason":"missing evidence"}`,
	} {
		got, err := ParseWithPolicy(output, "explore", false, 1, 3, Policy{Version: 2})
		if err != nil || got.FactID != "" || len(got.FactPayload) != 0 {
			t.Fatalf("noncompletion acquired facts: %+v %v", got, err)
		}
	}
}
