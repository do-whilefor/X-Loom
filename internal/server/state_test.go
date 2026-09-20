package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	b "xloom/internal/board"
)

func stateFixture(t *testing.T) http.Handler {
	t.Helper()
	_, h := fixture(t)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Observe scoped result","creator":"human"}`, 201)
	call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"worker@run-a"}`, 200)
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"planner@decision","trigger":"initial"}`, 200)
	return h
}
func stateHeaders(reason bool) []string {
	if reason {
		return []string{"X-Xloom-Run", "planner@decision", "X-Xloom-Lease", "reason"}
	}
	return []string{"X-Xloom-Run", "worker@run-a", "X-Xloom-Lease", "explore", "X-Xloom-Intent", "i001"}
}
func stateActionCall(t *testing.T, h http.Handler, reason bool, op, key, payload string, code int) b.StateActionResult {
	t.Helper()
	body, _ := json.Marshal(b.StateAction{Op: op, IdempotencyKey: key, Payload: json.RawMessage(payload)})
	result := call(t, h, "POST", "/projects/proj_001/state/actions", string(body), code, stateHeaders(reason)...)
	raw, _ := json.Marshal(result)
	var out b.StateActionResult
	json.Unmarshal(raw, &out)
	return out
}
func readState(t *testing.T, h http.Handler) b.State {
	t.Helper()
	result := call(t, h, "GET", "/projects/proj_001/state", "", 200)
	raw, _ := json.Marshal(result)
	var state b.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

const observedFact = `{"description":"HTTP 200 for this request","scope":"GET / on the assigned host","observed_at":"2026-01-01T00:00:00Z","evidence":[{"run_id":"run-a","path":"/workspace/.xloom/runs/run-a/output-001.txt","excerpt":"HTTP/1.1 200 OK","start_line":1,"end_line":1}]}`

func TestStateMapsLegacyGraphWithoutChangingItsJSON(t *testing.T) {
	h := stateFixture(t)
	s := readState(t, h)
	if s.Revision != 0 || len(s.Goals) != 1 || s.Goals[0].Condition != "Done" || len(s.Steps) != 1 || s.Steps[0].ID != "i001" || s.Steps[0].Status != "running" || len(s.FactRecords) != 2 || !s.FactRecords[0].Legacy {
		t.Fatalf("legacy mapping: %+v", s)
	}
	legacy := call(t, h, "GET", "/projects/proj_001", "", 200)
	if len(legacy) != 4 || legacy["fact_records"] != nil || legacy["goals"] != nil {
		t.Fatal("legacy graph representation changed")
	}
}

func TestIntermediateFactIdempotencyDoesNotCompleteStep(t *testing.T) {
	h := stateFixture(t)
	first := stateActionCall(t, h, false, "fact", "observation", observedFact, 200)
	second := stateActionCall(t, h, false, "fact", "observation", observedFact, 200)
	if first.ID != second.ID || first.Revision != second.Revision {
		t.Fatal("idempotent action changed result")
	}
	s := readState(t, h)
	if s.Revision != 1 || s.DecisionRevision != 1 || len(s.Graph.Facts) != 3 || s.Graph.Intents[0].To != nil || s.Graph.Project.Status != "active" || s.Steps[0].Status != "running" {
		t.Fatalf("intermediate fact changed step/project: %+v", s)
	}
	stateActionCall(t, h, false, "fact", "observation", strings.ReplaceAll(observedFact, "HTTP 200", "HTTP 404"), 409)
	stateActionCall(t, h, true, "fact", "observation-2", observedFact, 403)
	stateActionCall(t, h, false, "goal", "unauthorized", `{"action":"add","condition":"replace user goal"}`, 403)
	call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"fact","idempotency_key":"unfenced","payload":`+observedFact+`}`, 403)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Unauthorized plan","creator":"worker@run-a"}`, 403, stateHeaders(false)...)
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"Unauthorized completion","worker":"worker@run-a"}`, 403, stateHeaders(false)...)
}

func TestFactEvidenceAndCorrectionPreserveHistory(t *testing.T) {
	h := stateFixture(t)
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(observedFact, `"description":`, `"hypothesis":true,"description":`, 1),
		"foreign run":   strings.Replace(observedFact, "run-a", "other-run", 1),
		"no excerpt":    strings.Replace(observedFact, `"excerpt":"HTTP/1.1 200 OK"`, `"excerpt":""`, 1),
		"future":        strings.Replace(observedFact, "2026-01-01", "2999-01-01", 1),
	} {
		want := 422
		if name == "foreign run" {
			want = 403
		}
		stateActionCall(t, h, false, "fact", name, raw, want)
	}
	stateActionCall(t, h, false, "fact", "one", observedFact, 200)
	stateActionCall(t, h, false, "fact", "two", strings.Replace(observedFact, "HTTP 200", "HTTP 404 on subsequent request", 1), 200)
	stateActionCall(t, h, false, "fact_relation", "relation", `{"kind":"supersedes","source":"f002","target":"f001","reason":"A later request changed the observation"}`, 200)
	s := readState(t, h)
	if len(s.Graph.Facts) != 4 || s.FactRecords[2].Status != "superseded" || s.FactRecords[3].Status != "valid" || s.Graph.Facts[2].Description != "HTTP 200 for this request" {
		t.Fatal("correction destroyed history or failed to invalidate the old view")
	}
	stateActionCall(t, h, true, "step", "invalid-source", `{"action":"add","from":["f001"],"description":"Rely on superseded observation"}`, 409)
	stateActionCall(t, h, true, "fact_relation", "root-input", `{"kind":"refutes","source":"f002","target":"origin","reason":"change user constraints"}`, 403)
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"stale evidence","worker":"planner@decision"}`, 409, stateHeaders(true)...)
	r := httptest.NewRequest("GET", "/projects/proj_001/state/events?after=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var events []b.StateEvent
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil || len(events) != 2 || events[1].Op != "fact_relation" || events[1].RunID != "worker@run-a" {
		t.Fatalf("history=%s %v", w.Body.String(), err)
	}
}

func TestFindingDeduplicatesAndTracksEvidenceValidity(t *testing.T) {
	h := stateFixture(t)
	stateActionCall(t, h, false, "fact", "one", observedFact, 200)
	stateActionCall(t, h, false, "finding", "candidate", `{"claim":"Observed host responds","scope":"assigned host","status":"candidate"}`, 200)
	stateActionCall(t, h, false, "finding", "verified", `{"claim":"Observed  host responds","scope":"assigned host","status":"verified","sources":["f001"]}`, 200)
	stateActionCall(t, h, false, "finding", "repeat", `{"claim":"Observed host responds","scope":"assigned host","status":"verified","sources":["f001"]}`, 200)
	s := readState(t, h)
	if len(s.Findings) != 1 || len(s.Findings[0].Sources) != 1 || s.Findings[0].Status != "verified" || !s.Findings[0].SupportValid || s.Graph.Project.Status != "active" {
		t.Fatalf("finding state: %+v", s.Findings)
	}
	stateActionCall(t, h, false, "finding", "invalid-verification", `{"claim":"A different hypothesis","scope":"assigned host","status":"verified"}`, 422)
	stateActionCall(t, h, false, "fact", "two", strings.Replace(observedFact, "HTTP 200", "Connection refused later", 1), 200)
	stateActionCall(t, h, false, "fact_relation", "refute", `{"kind":"refutes","source":"f002","target":"f001","reason":"New observation conflicts in the stated scope"}`, 200)
	if readState(t, h).Findings[0].SupportValid {
		t.Fatal("finding still claims effective support after its source was invalidated")
	}
}

func TestGoalEvidenceAndStepAbandonmentCannotManufactureCompletion(t *testing.T) {
	h := stateFixture(t)
	stateActionCall(t, h, true, "goal", "root", `{"action":"withdraw","id":"goal","reason":"make it easy"}`, 403)
	goal := stateActionCall(t, h, true, "goal", "child", `{"action":"add","parent_id":"goal","condition":"Verify response"}`, 200)
	stateActionCall(t, h, true, "goal", "unsupported", `{"action":"achieve","id":"`+goal.ID+`","reason":"No proof","sources":["origin"]}`, 409)
	stateActionCall(t, h, false, "fact", "one", observedFact, 200)
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"Child goal remains open","worker":"planner@decision"}`, 409, stateHeaders(true)...)
	stateActionCall(t, h, true, "step", "running-input-change", `{"action":"priority","id":"i001","priority":4,"reason":"Change during execution"}`, 409)
	stateActionCall(t, h, true, "step", "abandon", `{"action":"abandon","id":"i001","reason":"Direction superseded"}`, 200)
	s := readState(t, h)
	if s.Steps[0].Status != "abandoned" || s.Graph.Intents[0].To != nil || s.Graph.Project.Status != "active" {
		t.Fatal("abandonment fabricated a successful result")
	}
	stateActionCall(t, h, false, "fact", "late", observedFact, 409)
	call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"other-run"}`, 409)
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"worker@run-a","description":"late"}`, 409, stateHeaders(false)...)
	stateActionCall(t, h, true, "goal", "achieve", `{"action":"achieve","id":"`+goal.ID+`","reason":"Observed response proves condition","sources":["f001"]}`, 200)
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"Evidence proves the root goal","worker":"planner@decision"}`, 200, stateHeaders(true)...)
}

func TestStateConcurrentDuplicateAndStoppedRun(t *testing.T) {
	h := stateFixture(t)
	body := `{"op":"fact","idempotency_key":"concurrent","payload":` + observedFact + `}`
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/projects/proj_001/state/actions", strings.NewReader(body))
			headers := stateHeaders(false)
			for n := 0; n < len(headers); n += 2 {
				r.Header.Set(headers[n], headers[n+1])
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("duplicate action: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	s := readState(t, h)
	if s.Revision != 1 || len(s.Graph.Facts) != 3 {
		t.Fatal("concurrent duplicate facts were committed")
	}
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	stateActionCall(t, h, false, "fact", "stopped", observedFact, 403)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"planner@decision","trigger":"recovery"}`, 409, stateHeaders(true)...)
	call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"worker@run-a"}`, 409, stateHeaders(false)...)
}
