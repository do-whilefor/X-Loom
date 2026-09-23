package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestExecutionQueryRoutesReturnCompactIdentityAndExactDetail(t *testing.T) {
	f := newDecisionBatchFixture(t)
	base := f.base() + "/executions/" + f.run
	var full board.Execution
	f.request("GET", base+"?namespace=protocol-test", nil, false, http.StatusOK, &full)
	var saved struct {
		State *board.State `json:"state"`
		Graph board.Graph  `json:"graph"`
	}
	if err := json.Unmarshal(full.Job, &saved); err != nil || saved.State == nil {
		t.Fatalf("detail lost immutable input: %v", err)
	}
	var identity board.ExecutionSummary
	raw := f.request("GET", base+"/identity?namespace=protocol-test", nil, false, http.StatusOK, &identity)
	if strings.Contains(raw, `"job"`) || strings.Contains(raw, `"result"`) || identity.ID != full.ID || identity.Lease != full.Lease || identity.StateVersion != board.DecisionStateVersion(*saved.State) || !identity.HasState || identity.FactCount != len(saved.Graph.Facts) {
		t.Fatalf("invalid compact identity: %s", raw)
	}
	var page board.ExecutionPage
	f.request("GET", "/executions/pending?namespace=protocol-test&limit=1", nil, false, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Items[0].ID != full.ID || page.NextCursor != 0 {
		t.Fatalf("pending page=%+v", page)
	}
	var check board.ExecutionCheck
	f.request("GET", f.base()+"/executions/check?namespace=protocol-test&kind=reason&generation=0&retry_key=unrelated&state_version="+identity.StateVersion, nil, false, http.StatusOK, &check)
	if !check.Pending || !check.Blocked || !check.Repeated {
		t.Fatalf("check=%+v", check)
	}
	for _, suffix := range []string{"", "/identity"} {
		f.request("GET", base+suffix+"?namespace=another", nil, false, http.StatusNotFound, nil)
		f.request("GET", base+suffix, nil, false, http.StatusUnprocessableEntity, nil)
	}
	for _, query := range []string{"namespace=protocol-test&limit=101", "namespace=protocol-test&after=-1", "namespace=protocol-test&limit=0", "namespace=protocol-test&after=NaN", "limit=1"} {
		f.request("GET", "/executions/pending?"+query, nil, false, http.StatusUnprocessableEntity, nil)
	}
	for _, query := range []string{"namespace=protocol-test&kind=reason&generation=-1", "namespace=protocol-test&kind=unknown", "namespace=protocol-test&kind=explore", "namespace=protocol-test&kind=reason&intent=some-step"} {
		f.request("GET", f.base()+"/executions/check?"+query, nil, false, http.StatusUnprocessableEntity, nil)
	}
}

func TestAutomaticRetryRouteDoesNotRestoreHistoricalJob(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &executionProtocolFixture{t: t, handler: New(store), run: "original", lease: "planner@original"}
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Retry metadata", "origin": "Synthetic fixture", "goal": "Check grant", "bootstrap_enabled": false}, false, http.StatusCreated, &graph)
	f.project = graph.Project.ID
	f.register("reason", nil, 0)
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "failed", "result": map[string]string{"status": "failed", "failure_kind": "transport"}}, true, http.StatusOK, nil)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec("UPDATE xloom_executions SET job='unavailable' WHERE project_id=? AND id=?", f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := f.base() + "/executions/" + f.run + "/retry"
	f.request("POST", path, map[string]any{"automatic": "yes"}, false, http.StatusUnprocessableEntity, nil)
	f.request("POST", path, map[string]bool{"automatic": true}, true, http.StatusForbidden, nil)
	for range 2 {
		var receipt map[string]string
		f.request("POST", path, map[string]bool{"automatic": true}, false, http.StatusOK, &receipt)
		if receipt["previous_run_id"] != f.run {
			t.Fatalf("wrong retry parent: %+v", receipt)
		}
	}
	var identity board.ExecutionSummary
	f.request("GET", f.base()+"/executions/"+f.run+"/identity?namespace=protocol-test", nil, false, http.StatusOK, &identity)
	if identity.Status != "retry_requested" {
		t.Fatalf("grant missing from metadata: %+v", identity)
	}
}
