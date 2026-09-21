package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	b "xloom/internal/board"
)

func TestRestartClearsRoundAndRetainsHumanInputs(t *testing.T) {
	for _, status := range []string{"active", "stopped", "completed"} {
		t.Run(status, func(t *testing.T) {
			store, h := fixture(t)
			now := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
			store.Now = func() time.Time { return now }
			call(t, h, "POST", "/projects", `{"title":"保留标题","origin":"原始输入","goal":"原始目标","scenario":"audit","bootstrap_enabled":true}`, 201)
			call(t, h, "POST", "/projects/proj_001/hints", `{"creator":"user","content":"保留提示"}`, 201)
			call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"旧任务","creator":"human"}`, 201)
			old := makeExecution(t, h, "explore", "oldrun", "")
			registerExecutionCall(t, h, old, 201)
			call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"planner@decision","trigger":"initial"}`, 200)
			fact := strings.ReplaceAll(observedFact, "run-a", "oldrun")
			call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"fact","idempotency_key":"old-fact","payload":`+fact+`}`, 200, executionHeaders(old)...)
			call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"finding","idempotency_key":"old-finding","payload":{"claim":"旧发现","scope":"test","status":"candidate","sources":["f001"],"evidence":[]}}`, 200, executionHeaders(old)...)
			if status == "stopped" {
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
			} else if status == "completed" {
				call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"旧完成","worker":"planner@decision"}`, 200, stateHeaders(true)...)
			}
			before := readState(t, h)
			now = now.Add(10 * time.Second)
			response := call(t, h, "POST", "/projects/proj_001/restart", `{"expected_generation":0}`, 200)
			var restarted b.Project
			if err := json.Unmarshal(response["project"], &restarted); err != nil {
				t.Fatal(err)
			}
			if restarted.Generation != 1 || restarted.RestartedAt != now.Format(time.RFC3339) || restarted.CreatedAt != before.Graph.Project.CreatedAt || restarted.Status != "active" || restarted.Reason != nil || restarted.Title != "保留标题" || restarted.Scenario != "audit" || !restarted.Bootstrap {
				t.Fatalf("restart metadata: %+v", restarted)
			}
			after := readState(t, h)
			if len(after.Graph.Facts) != 2 || len(after.Graph.Intents) != 0 || len(after.Graph.Hints) != 1 || after.Graph.Hints[0] != before.Graph.Hints[0] || len(after.Findings) != 0 || len(after.Steps) != 0 || len(after.FactRelations) != 0 || len(after.Goals) != 1 || after.Goals[0].Status != "open" || after.Revision != 0 || after.DecisionRevision != 0 {
				t.Fatalf("old round survived: %+v", after)
			}
			if after.Graph.Facts[0].Description != "原始输入" || after.Graph.Facts[1].Description != "原始目标" {
				t.Fatal("original inputs changed")
			}
			if err := store.Do(context.Background(), func(tx *b.Tx) error {
				for _, table := range []string{"xloom_state", "xloom_state_actions", "xloom_state_events", "xloom_executions", "xloom_paused_executions", "intents", "intent_sources"} {
					var count int
					if err := tx.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE project_id=?", "proj_001").Scan(&count); err != nil {
						return err
					}
					if count != 0 {
						t.Errorf("%s retained %d records", table, count)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Old managed workers cannot write back, recover, or claim a new lease
			// even though their execution records have intentionally been erased.
			executionOp(t, h, old, "apply", `{}`, 404)
			executionOp(t, h, old, "resume", `{}`, 404)
			call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"fact","idempotency_key":"old-fact","payload":`+fact+`}`, 409, executionHeaders(old)...)
			call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"`+old.Lease+`","trigger":"late"}`, 409)
			call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"planner@decision","trigger":"late"}`, 409)
			call(t, h, "POST", "/projects/proj_001/restart", `{"expected_generation":0}`, 409)
			// IDs remain monotonic: a stale path cannot point at a new step.
			created := call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"新任务","creator":"human"}`, 201)
			if string(created["id"]) == `"i001"` {
				t.Fatal("restart recycled step ID")
			}
		})
	}
}

func TestRestartRejectsOldSnapshotAndAcceptsNewRound(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/restart", `{}`, 200)
	current := makeExecution(t, h, "reason", "newrun", "")
	stale := current
	var job map[string]json.RawMessage
	if err := json.Unmarshal(current.Job, &job); err != nil {
		t.Fatal(err)
	}
	var graph b.Graph
	if err := json.Unmarshal(job["graph"], &graph); err != nil {
		t.Fatal(err)
	}
	graph.Project.Generation = 0
	job["graph"], _ = json.Marshal(graph)
	stale.Job, _ = json.Marshal(job)
	registerExecutionCall(t, h, stale, 409)
	registerExecutionCall(t, h, current, 201)
	var views []b.ExecutionView
	raw := getUIJSON(t, h, "/projects/proj_001/executions", &views)
	if len(views) != 1 || views[0].Generation != 1 || strings.Contains(raw, `"job"`) || strings.Contains(raw, `"graph"`) {
		t.Fatalf("UI execution projection lost generation or leaked job: %s", raw)
	}
	call(t, h, "POST", "/projects/proj_001/restart", `{}`, 403, executionHeaders(current)...)
	for _, body := range []string{`{"expected_generation":-1}`, `{"expected_generation":1.2}`, `{"expected_generation":"1"}`, `{"unknown":1}`} {
		call(t, h, "POST", "/projects/proj_001/restart", body, 422)
	}
	call(t, h, "POST", "/projects/missing/restart", `{}`, 404)
}

func TestConcurrentRestartUsesOneGeneration(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	results := make(chan int, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request := httptest.NewRequest("POST", "/projects/proj_001/restart", strings.NewReader(`{"expected_generation":0}`))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			results <- response.Code
		}()
	}
	workers.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 || readState(t, h).Graph.Project.Generation != 1 {
		t.Fatalf("concurrent restart did not fence the second request: %v", counts)
	}
}
