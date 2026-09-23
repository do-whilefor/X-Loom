package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"xloom/internal/board"
)

func evidenceFixtureFact(run string) map[string]any {
	return map[string]any{"description": "GET /fixture returned 401 without credentials", "scope": "one unauthenticated fixture request", "observed_at": "2026-09-22T10:00:00Z", "evidence": []board.EvidenceRef{{RunID: run, Path: "/run/evidence/frozen.raw", Excerpt: "HTTP/1.1 401 Unauthorized\n"}}}
}

func TestEvidenceCompletionReusesPublishedFactAndBindsSources(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "final observation", true: "published observation"}[published], func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.register("explore", &live, 2)
			data := map[string]any{"fact": evidenceFixtureFact(f.run)}
			var publishedID string
			if published {
				receipt := f.action("fact", "observation", evidenceFixtureFact(f.run))
				publishedID = receipt.ID
				data = map[string]any{"fact_id": publishedID}
			}
			raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": data})
			f.pending(string(raw))
			f.apply(http.StatusOK)
			first := f.state()
			f.apply(http.StatusOK)
			after := f.state()
			if len(after.Graph.Facts) != 3 || after.Graph.Project.Status != "active" || after.Steps[0].Status != "completed" || after.Revision != first.Revision {
				t.Fatalf("wrong completion or replay: %+v", after)
			}
			resultID := *after.Steps[0].Result
			if published && resultID != publishedID {
				t.Fatal("published observation was duplicated")
			}
			for _, fact := range after.FactRecords {
				if fact.ID == resultID && (fact.Legacy || fact.SourceStepID != f.intent || fact.RunID != f.lease || len(fact.Evidence) != 1 || fact.Evidence[0].RunID != f.run) {
					t.Fatalf("lost runtime provenance: %+v", fact)
				}
			}
		})
	}
}

func TestEvidenceCompletionCannotDowngradeOrForgeProvenance(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func(*executionProtocolFixture) map[string]any
		want int
	}{
		{"bare description", func(f *executionProtocolFixture) map[string]any { return map[string]any{"description": "Done"} }, 422},
		{"input as observation", func(f *executionProtocolFixture) map[string]any { return map[string]any{"fact_id": "origin"} }, 409},
		{"unknown fact", func(f *executionProtocolFixture) map[string]any { return map[string]any{"fact_id": "missing"} }, 404},
		{"foreign run", func(f *executionProtocolFixture) map[string]any {
			return map[string]any{"fact": evidenceFixtureFact("other-run")}
		}, 403},
		{"model step identity", func(f *executionProtocolFixture) map[string]any {
			fact := evidenceFixtureFact(f.run)
			fact["source_step_id"] = "other"
			return map[string]any{"fact": fact}
		}, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.register("explore", &live, 2)
			before := f.state()
			raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": tc.data(f)})
			f.pending(string(raw))
			f.apply(tc.want)
			state := f.state()
			if len(state.Graph.Facts) != 2 || state.Revision != before.Revision || state.DecisionRevision != before.DecisionRevision || state.Steps[0].Status != "running" {
				t.Fatal("rejected conclusion leaked a fact or terminal state")
			}
		})
	}
}

func TestVersionTwoCannotUseBareCompatibilityConclusion(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		f := newExecutionProtocolFixture(t)
		live := true
		f.register("explore", &live, 2)
		f.request("POST", f.base()+"/intents/"+f.intent+"/conclude", map[string]string{"worker": f.lease, "description": "bare conclusion"}, fenced, http.StatusConflict, nil)
		if len(f.state().Graph.Facts) != 2 {
			t.Fatal("compatibility endpoint bypassed the registered evidence contract")
		}
	}
}

func TestEvidenceCompletionCannotReuseOtherStepOrCorrectedObservation(t *testing.T) {
	for _, otherStep := range []bool{false, true} {
		name := "corrected observation"
		if otherStep {
			name = "another step"
		}
		t.Run(name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.register("explore", &live, 2)
			original := f.action("fact", "first-observation", evidenceFixtureFact(f.run))
			if otherStep {
				f.run, f.lease = "second-run", "planner@second-run"
				f.register("explore", &live, 2)
			} else {
				corrective := evidenceFixtureFact(f.run)
				corrective["description"] = "The observed response came from a stale fixture"
				source := f.action("fact", "correction-observation", corrective)
				f.action("fact_relation", "correction", map[string]any{"kind": "refutes", "source": source.ID, "target": original.ID, "reason": "Response was not from the selected target"})
			}
			before := f.state()
			raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": original.ID}})
			f.pending(string(raw))
			f.apply(http.StatusConflict)
			after := f.state()
			if len(after.Graph.Facts) != len(before.Graph.Facts) || after.Revision != before.Revision {
				t.Fatal("invalid result anchor mutated the board")
			}
		})
	}
}
