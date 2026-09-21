package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	b "xloom/internal/board"
)

func TestTerminateRetainsEvidenceAndRevokesExecution(t *testing.T) {
	for _, source := range []string{"active", "stopped", "continued"} {
		t.Run(source, func(t *testing.T) {
			store, h := fixture(t)
			now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
			store.Now = func() time.Time { return now }
			call(t, h, "POST", "/projects", `{"title":"未完成项目","origin":"原始输入","goal":"待验证目标","scenario":"audit"}`, 201)
			call(t, h, "POST", "/projects/proj_001/hints", `{"content":"保留提示","creator":"user"}`, 201)
			call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"正在验证","creator":"human"}`, 201)
			old := makeExecution(t, h, "explore", "oldrun", "")
			registerExecutionCall(t, h, old, 201)
			fact := strings.ReplaceAll(observedFact, "run-a", "oldrun")
			call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"fact","idempotency_key":"observation","payload":`+fact+`}`, 200, executionHeaders(old)...)
			call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"finding","idempotency_key":"finding","payload":{"claim":"保留发现","scope":"test","status":"candidate","sources":["f001"],"evidence":[]}}`, 200, executionHeaders(old)...)
			pendingResult(t, h, old, `{"description":"尚未写回的输出"}`, false)
			if source != "active" {
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
			}
			if source == "continued" {
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
			}
			before := readState(t, h)
			var prior []b.Execution
			getUIJSON(t, h, "/executions?namespace=test", &prior)
			now = now.Add(time.Second)
			response := call(t, h, "POST", "/projects/proj_001/terminate", `{"expected_generation":0}`, 200)
			var project b.Project
			if err := json.Unmarshal(response["project"], &project); err != nil {
				t.Fatal(err)
			}
			if project.Status != "terminated" || project.TerminatedAt != now.Format(time.RFC3339) || project.CreatedAt != before.Graph.Project.CreatedAt || project.Reason != nil || project.Generation != 0 || project.Scenario != "audit" {
				t.Fatalf("termination metadata: %+v", project)
			}
			after := readState(t, h)
			if len(after.Graph.Facts) != 3 || len(after.Graph.Intents) != 1 || after.Graph.Intents[0].Worker != nil || after.Graph.Intents[0].To != nil || len(after.Findings) != 1 || after.Findings[0].Claim != "保留发现" || len(after.Graph.Hints) != 1 || after.Graph.Hints[0] != before.Graph.Hints[0] || after.Goals[0].Status != "open" {
				t.Fatalf("termination damaged evidence or completed Goal: %+v", after)
			}
			var executions []b.Execution
			getUIJSON(t, h, "/executions?namespace=test", &executions)
			if len(executions) != 1 || executions[0].Status != "cancelled" || string(executions[0].Result) != string(prior[0].Result) {
				t.Fatalf("pending result lost or execution survived: %+v", executions)
			}
			if err := store.Do(context.Background(), func(tx *b.Tx) error {
				var count int
				if err := tx.QueryRow("SELECT COUNT(*) FROM xloom_paused_executions WHERE project_id='proj_001'").Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Fatal("pause recovery survived termination")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Termination blocks every automatic continuation and delayed write.
			call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 409)
			call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 409)
			call(t, h, "POST", "/projects/proj_001/reopen", `{"description":"resume","creator":"user"}`, 403)
			call(t, h, "POST", "/projects/proj_001/hints", `{"content":"late","creator":"user"}`, 403)
			call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"fact","idempotency_key":"late","payload":`+fact+`}`, 403, executionHeaders(old)...)
			call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"late completion","worker":"`+old.Lease+`"}`, 403, executionHeaders(old)...)
			executionOp(t, h, old, "apply", `{}`, 409)
			executionOp(t, h, old, "resume", `{}`, 409)
			executionOp(t, h, old, "status", `{"status":"cancelled"}`, 409)
			call(t, h, "POST", "/projects/proj_001/executions/oldrun/retry", `{}`, 403)
			// Repeat requests are read-only and keep the original terminal time.
			now = now.Add(time.Minute)
			call(t, h, "POST", "/projects/proj_001/terminate", `{"expected_generation":0}`, 200)
			repeated := readState(t, h)
			if repeated.Graph.Project.TerminatedAt != project.TerminatedAt || repeated.Revision != after.Revision {
				t.Fatal("duplicate termination rewrote time or history")
			}
			call(t, h, "PUT", "/projects/proj_001/title", `{"title":"已终止但可查阅"}`, 200)
			call(t, h, "POST", "/projects/proj_001/restart", `{"expected_generation":0}`, 200)
			restarted := readState(t, h)
			if restarted.Graph.Project.Status != "active" || restarted.Graph.Project.TerminatedAt != "" || restarted.Graph.Project.Generation != 1 || restarted.Graph.Project.Title != "已终止但可查阅" || len(restarted.Findings) != 0 || len(restarted.Graph.Hints) != 1 {
				t.Fatalf("explicit restart failed: %+v", restarted)
			}
			call(t, h, "POST", "/projects/proj_001/terminate", `{"expected_generation":0}`, 409)
		})
	}
}

func TestTerminateRejectsCompletedAndWorkerRequests(t *testing.T) {
	h, e := executionFixture(t, "reason")
	call(t, h, "POST", "/projects/proj_001/terminate", `{}`, 403, executionHeaders(e)...)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"verified","creator":"human","worker":"human"}`, 201)
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"human","description":"verified evidence"}`, 200)
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"achieved","worker":"human"}`, 200)
	call(t, h, "POST", "/projects/proj_001/terminate", `{}`, 409)
	if state := readState(t, h); state.Graph.Project.Status != "completed" || state.Graph.Project.TerminatedAt != "" || state.Goals[0].Status != "achieved" {
		t.Fatal("termination changed a completed project")
	}
	for _, body := range []string{`{"expected_generation":-1}`, `{"expected_generation":"0"}`, `{"unknown":0}`} {
		call(t, h, "POST", "/projects/proj_001/terminate", body, 422)
	}
	call(t, h, "POST", "/projects/missing/terminate", `{}`, 404)
}

func TestTerminatedProjectCanBeDeleted(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/terminate", `{}`, 200)
	call(t, h, "DELETE", "/projects/proj_001", "", 204)
	call(t, h, "GET", "/projects/proj_001", "", 404)
}
