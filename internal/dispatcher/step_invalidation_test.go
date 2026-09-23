package dispatcher

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

type invalidatingRetryRunner struct {
	client *Client
	calls  int
}

func (r *invalidatingRetryRunner) Run(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	r.calls++
	if r.calls > 1 {
		return worker.Result{Status: "failed", Error: "invalid source reached a second process"}, nil
	}
	// Another Decide corrects the premise while the first process is active.
	// The transient result then attempts the normal retry path.
	lease := Lease{Run: "planner@correction", Kind: "reason"}
	err := r.client.Do(ctx, "POST", projectPath(job.Graph.Project.ID)+"/state/actions", map[string]any{
		"op": "fact_relation", "idempotency_key": "correct-before-retry",
		"payload": map[string]string{"kind": "refutes", "source": "f002", "target": "f001", "reason": "A separate check disproved the task's premise"},
	}, nil, &lease)
	if err != nil {
		return worker.Result{}, err
	}
	return worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: "synthetic interrupted connection"}, nil
}

func (*invalidatingRetryRunner) Cleanup(context.Context, string, string) error { return nil }
func (*invalidatingRetryRunner) Projects(context.Context) ([]string, error)    { return nil, nil }

func TestRetryRechecksCorrectedStepBeforeStartingAnotherProcess(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "retry-correction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var state board.State
	err = store.Do(ctx, func(tx *board.Tx) error {
		graph := board.Graph{
			Project: board.Project{ID: "retry-fixture", Status: "active", Title: "Retry fixture", CreatedAt: tx.Now, Reason: &board.Reason{Worker: "planner@correction", StartedAt: tx.Now, Heartbeat: tx.Now}},
			Facts:   []board.Fact{{ID: "origin", Description: "Synthetic test scope"}, {ID: "goal", Description: "Verify a bounded observation"}, {ID: "f001", Description: "Initial premise"}, {ID: "f002", Description: "Independent corrective observation"}},
			Intents: []board.Intent{{ID: "i001", From: []string{"f001"}, Description: "Verify the initial premise", Creator: "fixture", Worker: board.Ptr("fixture@execute-retry"), Heartbeat: board.Ptr(tx.Now), CreatedAt: tx.Now}},
		}
		if err := tx.Save(graph); err != nil {
			return err
		}
		var err error
		state, err = tx.State(graph.Project.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.New(store))
	defer httpServer.Close()
	runner := &invalidatingRetryRunner{}
	scheduler := New(config.Config{Server: httpServer.URL, Runtime: config.Runtime{MaxWorkers: 1}}, runner)
	runner.client = scheduler.Client
	job := worker.Job{RunID: "execute-retry", Kind: "explore", Workspace: "/workspace", Graph: state.Graph, State: &state, Intent: &state.Graph.Intents[0], ResultContractVersion: 1, GraphRPC: true}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	registered := board.Execution{ProjectID: state.Graph.Project.ID, ID: job.RunID, Namespace: "xloom", Backend: "fixture", Kind: job.Kind, Intent: job.Intent.ID, Lease: "fixture@" + job.RunID, Job: raw, RetryKey: "explore:" + job.Intent.ID}
	lease := Lease{Run: registered.Lease, Kind: registered.Kind, Intent: registered.Intent}
	if err := scheduler.Client.Do(ctx, "POST", projectPath(registered.ProjectID)+"/executions", registered, &registered, &lease); err != nil {
		t.Fatal(err)
	}
	active := &task{Job: job, Worker: config.Worker{Name: "fixture", Type: "mock"}, Lease: lease, Execution: registered}
	outcome, runErr := scheduler.runRegistered(ctx, active, func() {})
	if runner.calls != 1 || outcome != "cancelled" || runErr == nil || !strings.Contains(runErr.Error(), "not effective evidence") {
		t.Fatalf("invalidated retry started another process or lost its cause: calls=%d outcome=%s err=%v", runner.calls, outcome, runErr)
	}
	var executions []board.Execution
	if err := scheduler.Client.Do(ctx, "GET", "/executions?namespace=xloom", nil, &executions, nil); err != nil {
		t.Fatal(err)
	}
	if len(executions) != 1 || executions[0].Status != "cancelled" {
		t.Fatalf("retry cancellation was not durable: %+v", executions)
	}
	var result worker.Result
	if err := json.Unmarshal(executions[0].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.FailureKind != "invalidated_before_start" {
		t.Fatalf("cancellation lost its diagnostic: %+v", result)
	}
	var after board.State
	if err := scheduler.Client.Do(ctx, "GET", projectPath(registered.ProjectID)+"/state", nil, &after, nil); err != nil {
		t.Fatal(err)
	}
	if len(after.FactRecords) != len(state.FactRecords) || after.Steps[0].Result != nil || after.Steps[0].Status != "failed" {
		t.Fatal("blocked retry manufactured an observation or completed its Step")
	}
}
