package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

// The first planner observes a real concurrent business update, then reports
// the same terminal conflict as the Worker. All reads and CAS checks cross the
// production dispatcher bridge and HTTP server; no model process is involved.
type conflictDecisionRunner struct {
	batchProtocolRunner
	client     *Client
	conflictOp string
	seen       []worker.Job
}

func (r *conflictDecisionRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.seen = append(r.seen, job)
	if job.Kind != "reason" || job.InputSnapshot == nil || job.Decision == nil {
		return worker.Result{}, errors.New("conflict fixture requires a registered decision snapshot")
	}
	first := len(r.seen) == 1
	if first {
		if err := r.client.Do(ctx, "POST", projectPath(job.Graph.Project.ID)+"/hints", map[string]string{
			"content": "A new observation requires reviewing the plan", "creator": "concurrent-fixture",
		}, nil, nil); err != nil {
			return worker.Result{}, err
		}
	}
	for _, op := range []string{"read_snapshot", "read_graph"} {
		result, err := r.graph(ctx, job, worker.GraphRequest{Op: op, Section: "hints", ExpectedVersion: job.Decision.StateVersion})
		if err != nil {
			return worker.Result{}, err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return worker.Result{}, err
		}
		var page struct {
			StateVersion string       `json:"state_version"`
			Items        []board.Hint `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return worker.Result{}, err
		}
		wantHints := 1
		if first {
			wantHints = 0
		}
		if page.StateVersion != job.Decision.StateVersion || len(page.Items) != wantHints {
			return worker.Result{}, fmt.Errorf("%s substituted another input: version=%s hints=%d, want version=%s hints=%d", op, page.StateVersion, len(page.Items), job.Decision.StateVersion, wantHints)
		}
	}
	if first {
		_, err := r.graph(ctx, job, worker.GraphRequest{Op: r.conflictOp, Batch: &board.DecisionBatch{
			ExpectedVersion: job.Decision.StateVersion, Actions: []board.DecisionAction{},
		}})
		var protocol *ProtocolError
		if !errors.As(err, &protocol) || protocol.Status != http.StatusConflict || !strings.Contains(err.Error(), "state_changed:") {
			return worker.Result{}, fmt.Errorf("outdated decision did not receive a definitive conflict: %v", err)
		}
		return worker.Result{Status: "failed", FailureKind: "state_changed", Error: err.Error()}, nil
	}
	return r.batchProtocolRunner.Run(ctx, backend, job)
}

func TestDecisionConflictSchedulesChangedInputWithoutRetryingOldSnapshot(t *testing.T) {
	for _, op := range []string{"decision_preview", "decision_commit"} {
		t.Run(op, func(t *testing.T) {
			fixture, _, _, _ := automaticRetryFixture(t, 0, "")
			runner := &conflictDecisionRunner{client: fixture.Client, conflictOp: op}
			scheduler := New(fixture.Config, runner)
			retryTicks(t, scheduler, 1)
			original := retryExecutions(t, scheduler)[0]
			var failure worker.Result
			if err := json.Unmarshal(original.Result, &failure); err != nil {
				t.Fatal(err)
			}
			if original.Status != "failed" || failure.FailureKind != "state_changed" || failure.Retryable {
				t.Fatalf("conflict was not a terminal superseded attempt: %+v %+v", original, failure)
			}
			// Recovery must use current server input, including across a restart.
			scheduler = New(scheduler.Config, runner)
			retryTicks(t, scheduler, 6)
			runs := retryExecutions(t, scheduler)
			if len(runner.seen) != 2 || len(runs) != 2 {
				t.Fatalf("changed input was lost or scheduled repeatedly: jobs=%d runs=%d", len(runner.seen), len(runs))
			}
			old, next := runner.seen[0], runner.seen[1]
			if next.RunID == old.RunID || next.PreviousRunID != "" || next.InputSnapshot.ID == old.InputSnapshot.ID || next.InputSnapshot.Revision <= old.InputSnapshot.Revision || next.Decision.StateVersion == old.Decision.StateVersion || next.InputSnapshot.HintCount != 1 {
				t.Fatalf("new decision did not bind the updated immutable input: old=%+v next=%+v", old.InputSnapshot, next.InputSnapshot)
			}
			if runs[0].Status != "failed" || runs[1].Status != "succeeded" || runs[0].RetryKey == runs[1].RetryKey || string(runs[0].Job) != string(original.Job) || string(runs[0].Result) != string(original.Result) {
				t.Fatalf("replacement reused or rewrote the conflicted attempt: %+v", runs)
			}
		})
	}
}

func TestDecisionConflictWithoutChangedInputDoesNotRenewRetryAllowance(t *testing.T) {
	scheduler, runner, _, graph := automaticRetryFixture(t, 99, "state_changed")
	retryTicks(t, scheduler, 1)
	original := retryExecutions(t, scheduler)[0]
	for range 2 {
		scheduler = New(scheduler.Config, runner)
		retryTicks(t, scheduler, 4)
	}
	if len(runner.seen) != 1 || len(retryExecutions(t, scheduler)) != 1 {
		t.Fatal("unchanged input repeatedly reauthorized the conflicted decision")
	}
	err := scheduler.Client.Do(context.Background(), "POST", projectPath(graph.Project.ID)+"/executions/"+original.ID+"/retry", map[string]bool{"automatic": true}, nil, nil)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Status != http.StatusConflict {
		t.Fatalf("unchanged conflict received an automatic retry grant: %v", err)
	}
}
