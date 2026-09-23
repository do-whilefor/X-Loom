package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

type automaticRetryRunner struct {
	batchProtocolRunner
	failures int
	failure  string
	seen     []worker.Job
}

func (r *automaticRetryRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.seen = append(r.seen, job)
	if len(r.seen) <= r.failures {
		return worker.Result{Status: "failed", FailureKind: r.failure, Error: "synthetic terminal failure"}, nil
	}
	return r.batchProtocolRunner.Run(ctx, backend, job)
}

func automaticRetryFixture(t *testing.T, failures int, failure string) (*Scheduler, *automaticRetryRunner, *board.Store, board.Graph) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	api := httptest.NewServer(server.New(store))
	t.Cleanup(api.Close)
	runner := &automaticRetryRunner{batchProtocolRunner: batchProtocolRunner{directions: 1}, failures: failures, failure: failure}
	cfg := config.Config{Server: api.URL,
		Runtime: config.Runtime{Interval: 1, MaxWorkers: 1, MaxProjects: 1, MaxProjectWorkers: 1, HealthMode: "disabled"},
		Tasks:   config.Tasks{Reason: config.Task{Timeout: 60, MaxIntents: 3}, Explore: config.Task{ConcludeTimeout: 60}},
		Workers: []config.Worker{{Name: "retry-fixture", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 1}},
	}
	s := New(cfg, runner)
	var graph board.Graph
	if err := s.Client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Retry fixture", "origin": "Synthetic input", "goal": "Check fixture"}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	return s, runner, store, graph
}

func retryTicks(t *testing.T, s *Scheduler, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range count {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
		s.wg.Wait()
	}
}

func retryExecutions(t *testing.T, s *Scheduler) []board.Execution {
	t.Helper()
	var executions []board.Execution
	if err := s.Client.Do(context.Background(), "GET", "/executions?namespace=xloom", nil, &executions, nil); err != nil {
		t.Fatal(err)
	}
	return executions
}

func TestFailedColdStartDecideAutomaticallyContinuesOnce(t *testing.T) {
	s, runner, _, graph := automaticRetryFixture(t, 1, "recovery_exhausted")
	retryTicks(t, s, 1)
	original := retryExecutions(t, s)[0]
	retryTicks(t, s, 1)
	if len(runner.seen) != 2 || runner.seen[0].RunID == runner.seen[1].RunID || runner.seen[1].PreviousRunID != runner.seen[0].RunID {
		t.Fatalf("expected a distinct successor: %+v", runner.seen)
	}
	runs := retryExecutions(t, s)
	if len(runs) != 2 || runs[0].Status != "retried" || runs[1].Status != "succeeded" || string(runs[0].Job) != string(original.Job) || string(runs[0].Result) != string(original.Result) {
		t.Fatalf("successor changed original identity/result or did not commit: %+v", runs)
	}
	var state board.State
	if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if len(state.Steps) != 1 {
		t.Fatalf("successor did not publish the cold-start plan: %+v", state.Steps)
	}
	oldLease := Lease{Run: runs[0].Lease, Kind: "reason"}
	err := s.Client.Do(context.Background(), "POST", projectPath(graph.Project.ID)+"/reason/heartbeat", map[string]string{"worker": runs[0].Lease}, nil, &oldLease)
	if err == nil {
		t.Fatal("old Decide lease became valid again")
	}
}

func TestAutomaticDecisionRetryBoundSurvivesDispatcherRestart(t *testing.T) {
	s, runner, _, graph := automaticRetryFixture(t, 99, "budget_exhausted")
	retryTicks(t, s, 1)
	s = New(s.Config, runner) // Recovery before authorization.
	retryTicks(t, s, 1)
	s = New(s.Config, runner) // Recovery after the successor also failed.
	retryTicks(t, s, 5)
	if len(runner.seen) != 2 {
		t.Fatalf("automatic retry allowance was replenished: %d", len(runner.seen))
	}
	runs := retryExecutions(t, s)
	path := projectPath(graph.Project.ID) + "/executions/" + runs[1].ID + "/retry"
	err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Status != 409 {
		t.Fatalf("third automatic attempt was not rejected: %v", err)
	}
	if err := s.Client.Do(context.Background(), "POST", path, map[string]any{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	retryTicks(t, s, 3)
	if len(runner.seen) != 3 {
		t.Fatalf("manual retry was blocked or renewed automatic budget: %d", len(runner.seen))
	}
}

func TestAutomaticDecisionRetryGrantIsDurableAndIdempotent(t *testing.T) {
	s, runner, _, graph := automaticRetryFixture(t, 1, "transport")
	retryTicks(t, s, 1)
	original := retryExecutions(t, s)[0]
	path := projectPath(graph.Project.ID) + "/executions/" + original.ID + "/retry"
	for range 2 {
		if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	s = New(s.Config, runner)
	retryTicks(t, s, 1)
	if len(runner.seen) != 2 || runner.seen[1].PreviousRunID != original.ID {
		t.Fatal("restart lost or duplicated the one-use retry grant")
	}
}

func TestAutomaticDecisionRetryGrantCannotCrossProjectRestart(t *testing.T) {
	s, runner, store, graph := automaticRetryFixture(t, 99, "transport")
	retryTicks(t, s, 1)
	original := retryExecutions(t, s)[0]
	path := projectPath(graph.Project.ID) + "/executions/" + original.ID + "/retry"
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error { _, err := tx.RestartProject(graph.Project.ID, nil); return err }); err != nil {
		t.Fatal(err)
	}
	s = New(s.Config, runner)
	retryTicks(t, s, 5)
	if len(runner.seen) != 3 || runner.seen[1].Graph.Project.Generation != 1 || runner.seen[1].PreviousRunID != "" || runner.seen[2].PreviousRunID != runner.seen[1].RunID {
		t.Fatalf("old grant crossed project restart: %+v", runner.seen)
	}
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err == nil {
		t.Fatal("removed old grant was recreated")
	}
}

func TestAutomaticDecisionRetryCannotOverrideAnotherPlanner(t *testing.T) {
	s, _, store, graph := automaticRetryFixture(t, 99, "transport")
	retryTicks(t, s, 1)
	original := retryExecutions(t, s)[0]
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		g.Project.Reason = &board.Reason{Worker: "other@planner", StartedAt: tx.Now, Heartbeat: tx.Now}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	path := projectPath(graph.Project.ID) + "/executions/" + original.ID + "/retry"
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err == nil {
		t.Fatal("automatic retry overrode a competing planner")
	}
	var after board.Graph
	if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID), nil, &after, nil); err != nil {
		t.Fatal(err)
	}
	if after.Project.Reason == nil || after.Project.Reason.Worker != "other@planner" || retryExecutions(t, s)[0].Status != "failed" {
		t.Fatal("rejected grant partially changed state")
	}
}

func TestAutomaticDecisionRetryNeverReplaysRefusalOrCancellation(t *testing.T) {
	for _, status := range []string{"cancelled", "rejected"} {
		t.Run(status, func(t *testing.T) {
			s, runner, store, graph := automaticRetryFixture(t, 99, "transport")
			retryTicks(t, s, 1)
			original := retryExecutions(t, s)[0]
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				_, err := tx.Exec("UPDATE xloom_executions SET status=? WHERE project_id=? AND id=?", status, graph.Project.ID, original.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			retryTicks(t, s, 3)
			path := projectPath(graph.Project.ID) + "/executions/" + original.ID + "/retry"
			if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err == nil || len(runner.seen) != 1 {
				t.Fatalf("%s attempt was automatically retried", status)
			}
		})
	}
}

func TestAutomaticDecisionRetryExcludesNonRecoverableFailures(t *testing.T) {
	for _, failure := range []string{"configuration", "invalid_output", "result_contract", "session_invalid", "hard_cancelled", "project_paused", "project_terminated", "execution", "provider_http"} {
		t.Run(failure, func(t *testing.T) {
			s, runner, _, _ := automaticRetryFixture(t, 99, failure)
			retryTicks(t, s, 3)
			if len(runner.seen) != 1 {
				t.Fatalf("ineligible failure retried: %d", len(runner.seen))
			}
		})
	}
}

func TestAutomaticDecisionRetryValidatesManagementAndProjectFence(t *testing.T) {
	s, runner, store, graph := automaticRetryFixture(t, 99, "recovery_exhausted")
	retryTicks(t, s, 1)
	original := retryExecutions(t, s)[0]
	path := projectPath(graph.Project.ID) + "/executions/" + original.ID + "/retry"
	for _, value := range []any{nil, "true", 1} {
		err := s.Client.Do(context.Background(), "POST", path, map[string]any{"automatic": value}, nil, nil)
		var protocol *ProtocolError
		if !errors.As(err, &protocol) || protocol.Status != 422 {
			t.Fatalf("invalid automatic field accepted: %v", err)
		}
	}
	lease := Lease{Run: original.Lease, Kind: "reason"}
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, &lease); err == nil {
		t.Fatal("Worker authorized a retry")
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		if err = tx.SetStatus(&g, "stopped"); err != nil {
			return err
		}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	retryTicks(t, s, 2)
	if len(runner.seen) != 1 {
		t.Fatal("stopped project ran an automatic successor")
	}
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err == nil {
		t.Fatal("stopped project authorized successor")
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error { _, err := tx.RestartProject(graph.Project.ID, nil); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err == nil {
		t.Fatal("old generation retry survived restart")
	}
	retryTicks(t, s, 5)
	if len(runner.seen) != 3 || runner.seen[1].Graph.Project.Generation != 1 || runner.seen[1].PreviousRunID != "" || runner.seen[2].PreviousRunID != runner.seen[1].RunID {
		t.Fatalf("new round did not receive an independent bounded allowance: %+v", runner.seen)
	}
}

func TestDispatcherRecoveryExhaustionReleasesLeaseBeforeSuccessor(t *testing.T) {
	s, runner, store, graph := automaticRetryFixture(t, 99, "transport")
	retryTicks(t, s, 1)
	original := retryExecutions(t, s)[0]
	// Reconstruct a process interrupted after consuming both resume attempts.
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		if _, err := tx.Exec("UPDATE xloom_executions SET status='running',result=NULL,resumes=2 WHERE project_id=? AND id=?", original.ProjectID, original.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM xloom_revoked_runs WHERE project_id=? AND worker=?", original.ProjectID, original.Lease); err != nil {
			return err
		}
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		g.Project.Reason = &board.Reason{Worker: original.Lease, StartedAt: tx.Now, Heartbeat: tx.Now}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	s = New(s.Config, runner)
	retryTicks(t, s, 1)
	failed := retryExecutions(t, s)[0]
	var result worker.Result
	if err := json.Unmarshal(failed.Result, &result); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || result.FailureKind != "recovery_exhausted" {
		t.Fatalf("recovery exhaustion lost classification: %+v %+v", failed, result)
	}
	retryTicks(t, s, 3)
	if len(runner.seen) != 2 || runner.seen[1].PreviousRunID != original.ID {
		t.Fatal("revoked old lease blocked successor after dispatcher recovery exhaustion")
	}
}
