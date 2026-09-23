package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

type staleDecisionRunner struct {
	batchProtocolRunner
	first func(context.Context, worker.Job) (worker.Result, error)
	seen  []worker.Job
}

func (r *staleDecisionRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.mu.Lock()
	r.seen = append(r.seen, job)
	first := len(r.seen) == 1
	r.mu.Unlock()
	if first {
		return r.first(ctx, job)
	}
	return r.batchProtocolRunner.Run(ctx, backend, job)
}

func staleDecisionFailure(t *testing.T, scheduler *Scheduler) board.Execution {
	t.Helper()
	runs := retryExecutions(t, scheduler)
	if len(runs) != 1 {
		t.Fatalf("stale run was retried before a fresh scheduling pass: %+v", runs)
	}
	var result worker.Result
	if err := json.Unmarshal(runs[0].Result, &result); err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != "failed" || result.FailureKind != "state_changed" || result.Retryable {
		t.Fatalf("stale run did not finish as a terminal state_changed failure: %+v %+v", runs[0], result)
	}
	return runs[0]
}

func assertFreshDecisionReplacement(t *testing.T, scheduler *Scheduler, runner *staleDecisionRunner, original board.Execution) {
	t.Helper()
	// Once the first no-op commit consumes the new input, further ticks must
	// neither restart the stale snapshot nor schedule the replacement twice.
	retryTicks(t, scheduler, 6)
	runs := retryExecutions(t, scheduler)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.seen) != 2 || len(runs) != 2 {
		t.Fatalf("changed input did not receive exactly one replacement: jobs=%d runs=%d", len(runner.seen), len(runs))
	}
	old, next := runner.seen[0], runner.seen[1]
	if next.RunID == old.RunID || next.PreviousRunID != "" || next.Decision.StateVersion == old.Decision.StateVersion || next.InputSnapshot.ID == old.InputSnapshot.ID || next.InputSnapshot.Revision <= old.InputSnapshot.Revision || next.InputSnapshot.HintCount != 1 {
		t.Fatalf("replacement reused stale input: old=%+v next=%+v", old.InputSnapshot, next.InputSnapshot)
	}
	if runs[0].Status != "failed" || runs[1].Status != "succeeded" || runs[0].RetryKey == runs[1].RetryKey || string(runs[0].Job) != string(original.Job) || string(runs[0].Result) != string(original.Result) {
		t.Fatalf("replacement rewrote or retried the obsolete attempt: %+v", runs)
	}
}

func TestDecisionHeartbeatCancelsStalledRunWithoutSchedulerTick(t *testing.T) {
	fixture, _, _, graph := automaticRetryFixture(t, 0, "")
	started := make(chan worker.Job, 1)
	stopped := make(chan error, 1)
	runner := &staleDecisionRunner{first: func(ctx context.Context, job worker.Job) (worker.Result, error) {
		started <- job
		// Represent a model request which cannot return until it is cancelled.
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return worker.Result{}, ctx.Err()
	}}
	scheduler := New(fixture.Config, runner)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer func() { cancel(); scheduler.wg.Wait() }()
	if err := scheduler.Step(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case job := <-started:
		if job.Kind != "reason" || scheduler.Config.Runtime.MaxWorkers != 1 || len(scheduler.running) != 1 {
			t.Fatal("fixture did not occupy the only Worker with Decide")
		}
	case <-ctx.Done():
		t.Fatal("Decide did not start")
	}
	if err := scheduler.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Concurrent Execute published a new observation", "creator": "fixture"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Do not call Step: cancellation must still happen when all slots are full
	// and the scheduler cannot load another scheduling page.
	select {
	case cause := <-stopped:
		if !decisionStateChanged(cause) {
			t.Fatalf("stalled request lost its stale-input cancellation cause: %v", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("changed input did not cancel the stalled Decide within five heartbeat intervals")
	}
	scheduler.wg.Wait()
	original := staleDecisionFailure(t, scheduler)
	assertFreshDecisionReplacement(t, scheduler, runner, original)
}

func TestDecisionRetryChecksVersionBeforeStartingAnotherRunner(t *testing.T) {
	fixture, _, _, graph := automaticRetryFixture(t, 0, "")
	// Keep the periodic heartbeat out of this test: the same-run recovery
	// preflight must discover the change itself before restarting the Worker.
	fixture.Config.Runtime.Interval = 10
	runner := &staleDecisionRunner{first: func(ctx context.Context, _ worker.Job) (worker.Result, error) {
		if err := fixture.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Input changed before transport recovery", "creator": "fixture"}, nil, nil); err != nil {
			return worker.Result{}, err
		}
		return worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: "synthetic connection loss"}, nil
	}}
	scheduler := New(fixture.Config, runner)
	retryTicks(t, scheduler, 1)
	original := staleDecisionFailure(t, scheduler)
	runner.mu.Lock()
	calls := len(runner.seen)
	runner.mu.Unlock()
	if calls != 1 {
		t.Fatalf("same-run recovery started %d Workers against stale input", calls)
	}
	assertFreshDecisionReplacement(t, scheduler, runner, original)
}

func TestDecisionTerminalFailureSurvivesConcurrentStaleCancellation(t *testing.T) {
	fixture, _, _, graph := automaticRetryFixture(t, 0, "")
	fixture.Config.Runtime.Interval = 30
	runner := &staleDecisionRunner{first: func(ctx context.Context, _ worker.Job) (worker.Result, error) {
		if err := fixture.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Input changed before recovery", "creator": "fixture"}, nil, nil); err != nil {
			return worker.Result{}, err
		}
		return worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: "synthetic connection loss"}, nil
	}}
	scheduler := New(fixture.Config, runner)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	backend := scheduler.Config.Workers[0]
	run := &task{Job: worker.Job{RunID: "failure-cancel-race", Kind: "reason", Graph: graph, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", EnvironmentID: scheduler.environmentID(backend), Budget: scheduler.Config.Task("reason")}, Worker: backend, Lease: Lease{Run: backend.Name + "@failure-cancel-race", Kind: "reason"}}
	if err := scheduler.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/reason/claim", map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	run.Job.DecisionTrigger = "initial"
	if err := scheduler.register(ctx, run); err != nil {
		t.Fatal(err)
	}
	lateConflict := &ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"state_changed: late heartbeat signal"}`}
	var failures []worker.Result
	scheduler.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
		var update struct {
			Status string        `json:"status"`
			Result worker.Result `json:"result"`
		}
		if request.Method == "POST" && request.URL.Path == executionPath(run)+"/status" {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			err = json.NewDecoder(body).Decode(&update)
			_ = body.Close()
			if err != nil {
				return nil, err
			}
		}
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil && response.StatusCode == http.StatusOK && update.Status == "failed" {
			// The real terminal write revokes the lease. Deliver an already
			// in-flight heartbeat conflict before its acknowledgement returns.
			failures = append(failures, update.Result)
			cancel(lateConflict)
		}
		return response, err
	})}
	outcome, err := scheduler.runTask(ctx, run)
	if outcome != "failed" || err == nil || context.Cause(ctx) != lateConflict || len(failures) != 1 {
		t.Fatalf("settled stale failure changed after late cancellation: outcome=%q err=%v cause=%v writes=%d", outcome, err, context.Cause(ctx), len(failures))
	}
	terminal := staleDecisionFailure(t, scheduler)
	var result worker.Result
	if err := json.Unmarshal(terminal.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != failures[0].Error || err.Error() != result.Error || result.Error == lateConflict.Error() {
		t.Fatalf("late heartbeat replaced the original preflight failure: original=%+v stored=%+v err=%v", failures[0], result, err)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("terminal cancellation race started another stale Worker: calls=%d", len(runner.seen))
	}
}

func TestDecisionCommittedReceiptWinsStaleCancellation(t *testing.T) {
	fixture, _, _, graph := automaticRetryFixture(t, 0, "")
	runner := &batchProtocolRunner{directions: 1}
	scheduler := New(fixture.Config, runner)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	runner.afterCommit = func() {
		cancel(&ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"state_changed: decision input is no longer current"}`})
	}
	backend := scheduler.Config.Workers[0]
	run := &task{Job: worker.Job{RunID: "commit-cancel-race", Kind: "reason", Graph: graph, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", EnvironmentID: scheduler.environmentID(backend), Budget: scheduler.Config.Task("reason")}, Worker: backend, Lease: Lease{Run: backend.Name + "@commit-cancel-race", Kind: "reason"}}
	if err := scheduler.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/reason/claim", map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	run.Job.DecisionTrigger = "initial"
	if err := scheduler.register(ctx, run); err != nil {
		t.Fatal(err)
	}
	outcome, err := scheduler.runTask(ctx, run)
	if outcome != "success" || err != nil || !decisionStateChanged(context.Cause(ctx)) {
		t.Fatalf("late stale-input signal overrode a durable commit: outcome=%q err=%v cause=%v", outcome, err, context.Cause(ctx))
	}
	runs := retryExecutions(t, scheduler)
	if len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatalf("committed execution was rewritten after cancellation: %+v", runs)
	}
	var state board.State
	if err := scheduler.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if len(state.Steps) != 1 || state.Graph.Project.Reason != nil {
		t.Fatalf("committed actions or lease release were lost: %+v", state)
	}
}

func TestDecisionHeartbeatCancelsBlockedHealthBeforeRunnerStarts(t *testing.T) {
	scheduler, runner, _, graph := automaticRetryFixture(t, 0, "")
	scheduler.Config.Runtime.HealthMode = "startup_and_task"
	started := make(chan struct{})
	stopped := make(chan error, 1)
	scheduler.CheckHealth = func(ctx context.Context, _ config.Worker) error {
		close(started)
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer func() { cancel(); scheduler.wg.Wait() }()
	if err := scheduler.Step(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("task health check did not start")
	}
	if err := scheduler.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Input changed during readiness probe", "creator": "fixture"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-stopped:
		if !decisionStateChanged(cause) {
			t.Fatalf("readiness probe lost invalidation cause: %v", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale input did not interrupt the readiness probe")
	}
	scheduler.wg.Wait()
	staleDecisionFailure(t, scheduler)
	if len(runner.seen) != 0 {
		t.Fatal("stale Decide reached Runner after its readiness probe was cancelled")
	}
}

func TestDecisionLeaseVersionCheckPreservesLegacyHeartbeat(t *testing.T) {
	scheduler, _, _, graph := automaticRetryFixture(t, 0, "")
	ctx := context.Background()
	run := &task{Job: worker.Job{Kind: "reason", Graph: graph}, Lease: Lease{Run: "legacy@heartbeat", Kind: "reason"}}
	base := projectPath(graph.Project.ID)
	if err := scheduler.Client.Do(ctx, "POST", base+"/reason/claim", map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var state board.State
	if err := scheduler.Client.Do(ctx, "GET", base+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	version := board.DecisionStateVersion(state)
	if err := scheduler.Client.Do(ctx, "POST", base+"/hints", map[string]string{"content": "Legacy planners can publish incrementally", "creator": "fixture"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []*board.DecisionContext{nil, {Version: 1, StateVersion: version}} {
		run.Job.Decision = decision
		if err := scheduler.renewLease(ctx, run); err != nil {
			t.Fatalf("legacy heartbeat unexpectedly checked the initial input version: decision=%+v err=%v", decision, err)
		}
	}
	run.Job.Decision = &board.DecisionContext{Version: 2, StateVersion: version}
	if err := scheduler.renewLease(ctx, run); !decisionStateChanged(err) {
		t.Fatalf("bound version 2 heartbeat did not reject changed input: %v", err)
	}
}

func TestDecisionStateChangedRequiresStructuredConflict(t *testing.T) {
	conflict := &ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"state_changed: decision input is no longer current"}`}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"structured", conflict, true},
		{"wrapped", fmt.Errorf("heartbeat: %w", conflict), true},
		{"nil", nil, false},
		{"ordinary_text", errors.New("state_changed: model output"), false},
		{"wrong_status", &ProtocolError{Status: http.StatusForbidden, Detail: conflict.Detail}, false},
		{"unstructured", &ProtocolError{Status: http.StatusConflict, Detail: "state_changed: unstructured response"}, false},
		{"quoted_in_detail", &ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"tool output contains state_changed: but lease was revoked"}`}, false},
		{"other_conflict", &ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"lease is no longer current"}`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionStateChanged(tc.err); got != tc.want {
				t.Fatalf("decisionStateChanged(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
