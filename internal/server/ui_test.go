package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	b "xloom/internal/board"
)

func getUIJSON(t *testing.T, h http.Handler, path string, out any) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("GET %s: got %d: %s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
	return w.Body.String()
}

func TestCreateProjectScenarioValidationAndSummary(t *testing.T) {
	_, h := fixture(t)
	for _, value := range []string{`"reason"`, `""`, `null`, `false`, `1`, `[]`, `{}`} {
		call(t, h, "POST", "/projects", `{"title":"test","origin":"known","goal":"done","scenario":`+value+`}`, 422)
	}
	for _, scenario := range []string{"ctf", "pentest", "audit"} {
		response := call(t, h, "POST", "/projects", `{"title":"test","origin":"known","goal":"done","scenario":"`+scenario+`"}`, 201)
		var project b.Project
		if err := json.Unmarshal(response["project"], &project); err != nil {
			t.Fatal(err)
		}
		if project.Scenario != scenario {
			t.Fatalf("scenario not returned: %+v", project)
		}
		var graph b.Graph
		getUIJSON(t, h, "/projects/"+project.ID, &graph)
		if graph.Project.Scenario != scenario {
			t.Fatal("scenario disappeared on project read")
		}
	}
	create(t, h)
	var projects []b.Summary
	getUIJSON(t, h, "/projects", &projects)
	if len(projects) != 4 || projects[0].ID != "proj_001" || projects[0].Scenario != "ctf" || projects[1].Scenario != "pentest" || projects[2].Scenario != "audit" || projects[3].Scenario != "" {
		t.Fatalf("unexpected summaries: %+v", projects)
	}
	var legacy map[string]any
	getUIJSON(t, h, "/projects/proj_004", &legacy)
	if _, exists := legacy["project"].(map[string]any)["scenario"]; exists {
		t.Fatal("legacy graph gained a scenario field")
	}
	call(t, h, "PUT", "/projects/proj_001/title", `{"title":"renamed"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	var graph b.Graph
	getUIJSON(t, h, "/projects/proj_001", &graph)
	if graph.Project.Scenario != "ctf" {
		t.Fatal("project mutations lost scenario")
	}
}

func TestProjectExecutionProjectionIsScopedAndRecoveryAPIUnchanged(t *testing.T) {
	s, h := fixture(t)
	create(t, h)
	create(t, h)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Investigate","creator":"human"}`, 201)
	e := makeExecution(t, h, "explore", "first", "")
	e = registerExecutionCall(t, h, e, 201)
	if err := s.Do(context.Background(), func(tx *b.Tx) error {
		// Same execution ID in another project must never cross the URL scope.
		_, err := tx.Exec(`INSERT INTO xloom_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,result,resumes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "proj_002", "first", "private-namespace", "other-backend", "reason", "", "private-lease", []byte(`{"env":"private-environment"}`), "private-retry-key", "failed", []byte(`{"text":"other-project-result"}`), 2, tx.Now, tx.Now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	executionOp(t, h, e, "status", `{"status":"running","result":{"text":"model answer","error":"diagnostic","env":{"secret":"private-result-env"},"job":"private-result-job"}}`, 200)
	var views []b.ExecutionView
	raw := getUIJSON(t, h, "/projects/proj_001/executions", &views)
	if len(views) != 1 || views[0].ProjectID != "proj_001" || views[0].ID != e.ID || views[0].Kind != e.Kind || views[0].Intent != e.Intent || views[0].Result == nil || views[0].Result.Text != "model answer" || views[0].Result.Error != "diagnostic" {
		t.Fatalf("unexpected execution view: %+v", views)
	}
	for _, absent := range []string{`"job"`, `"lease"`, `"retry_key"`, `"namespace"`, `"env"`, "private-", "other-project-result", "/workspace"} {
		if strings.Contains(raw, absent) {
			t.Fatalf("execution view leaked %s", absent)
		}
	}
	getUIJSON(t, h, "/projects/proj_002/executions", &views)
	if len(views) != 1 || views[0].ProjectID != "proj_002" || views[0].Result.Text != "other-project-result" {
		t.Fatal("second project view not scoped")
	}
	if raw := getUIJSON(t, h, "/projects/proj_003/executions", &views); strings.TrimSpace(raw) != "[]" {
		t.Fatalf("empty project should have an empty array: %s", raw)
	}
	call(t, h, "GET", "/projects/does-not-exist/executions", "", 404)
	// The dispatcher still gets the complete immutable recovery record.
	var recovery []b.Execution
	getUIJSON(t, h, "/executions?namespace=test", &recovery)
	if len(recovery) != 1 || recovery[0].Lease != e.Lease || recovery[0].RetryKey != e.RetryKey || string(recovery[0].Job) != string(e.Job) {
		t.Fatal("read-only UI endpoint altered dispatcher recovery data")
	}
	executionOp(t, h, e, "resume", `{}`, 200)
	getUIJSON(t, h, "/projects/proj_001/executions", &views)
	if views[0].Resumes != 1 {
		t.Fatal("existing resume API no longer updates execution")
	}
}

func TestUIOverviewCountsLiveLeasesWithoutInventedLimits(t *testing.T) {
	s, h := fixture(t)
	now := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	create(t, h)
	create(t, h)
	if err := s.Do(context.Background(), func(tx *b.Tx) error {
		for _, project := range []string{"proj_001", "proj_002"} {
			g, err := tx.Load(project)
			if err != nil {
				return err
			}
			g.Project.Reason = &b.Reason{Worker: "shared-worker", Trigger: "test", StartedAt: tx.Now, Heartbeat: tx.Now}
			g.Intents = []b.Intent{
				{ID: "i001", Description: "same lease", Creator: "human", Worker: b.Ptr("shared-worker"), Heartbeat: b.Ptr(tx.Now), CreatedAt: tx.Now},
				{ID: "i002", Description: "ended", Creator: "human", Worker: b.Ptr("ended"), Heartbeat: b.Ptr(tx.Now), CreatedAt: tx.Now, ConcludedAt: b.Ptr(tx.Now)},
				{ID: "i003", Description: "expired", Creator: "human", Worker: b.Ptr("expired"), Heartbeat: b.Ptr("2026-01-01T00:00:00Z"), CreatedAt: tx.Now},
				{ID: "i004", Description: "revoked", Creator: "human", Worker: b.Ptr("revoked"), Heartbeat: b.Ptr(tx.Now), CreatedAt: tx.Now},
			}
			if project == "proj_002" {
				g.Project.Status = "stopped"
			}
			if err := tx.Save(g); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO xloom_revoked_runs(project_id,worker) VALUES(?,?)", project, "revoked"); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO xloom_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,resumes,created_at,updated_at) VALUES('proj_001','historical','test','b','explore','i001','unowned','{}','test','running',0,?,?)`, tx.Now, tx.Now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var overview b.UIOverview
	raw := getUIJSON(t, h, "/ui/overview", &overview)
	if overview.ActiveWorkers != 1 || overview.ExecutionCounts["running"] != 1 || overview.ObservedAt != now.Format(time.RFC3339) {
		t.Fatalf("wrong overview: %+v", overview)
	}
	if strings.Contains(raw, "limit") || strings.Contains(raw, "job") {
		t.Fatalf("overview invented configuration or leaked inputs: %s", raw)
	}
	now = now.Add(time.Minute)
	getUIJSON(t, h, "/ui/overview", &overview)
	if overview.ActiveWorkers != 0 || overview.ExecutionCounts["running"] != 1 {
		t.Fatal("live lease count must expire independently of persisted execution statuses")
	}
}
