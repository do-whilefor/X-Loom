package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
)

// A slow planner must not spend the remainder of its fixed budget asking the
// model to refresh and restage a transaction already rejected by the server.
func TestDecisionConflictEndsAttemptAtSettledBoundary(t *testing.T) {
	for _, operation := range []string{"preview", "commit"} {
		t.Run(operation, func(t *testing.T) {
			job, runDir := draftRunJob(t), t.TempDir()
			job.Budget.Timeout = 300
			start := time.Now()
			now := start
			calls, writes, receipts := 0, 0, 0
			const conflict = "state_changed: new execution evidence changed this decision input"
			bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
				switch request.Op {
				case "decision_receipt":
					receipts++
					return board.DecisionReceipt{}, nil
				case "decision_" + operation:
					writes++
					if request.Batch.ExpectedVersion != job.Decision.StateVersion || len(request.Batch.Actions) != 1 {
						t.Fatalf("draft lost its original input: %+v", request.Batch)
					}
					return nil, errors.New(conflict)
				default:
					t.Fatalf("operation continued after definitive conflict: %s", request.Op)
					return nil, nil
				}
			}}
			provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				calls++
				if calls > 1 {
					t.Fatal("conflict bought another model request")
				}
				// Model wall time is represented by the session clock, without a
				// minutes-long sleep or resetting the actual parent deadline.
				now = start.Add(284 * time.Second)
				message := draftModelCall("stage", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":["origin"],"description":"Check input"}}`)
				for _, next := range []agent.Message{
					draftModelCall("publish", "graph_action", `{"op":"`+operation+`","idempotency_key":"publish","payload":{}}`),
					draftModelCall("refresh", "read_graph", `{"section":"overview"}`),
					draftModelCall("restage", "graph_action", `{"op":"reset","idempotency_key":"reset","payload":{}}`),
				} {
					message.Content = append(message.Content, next.Content...)
				}
				return message, nil
			})
			opts := Options{Provider: provider, RunDir: runDir, Output: bridge, Now: func() time.Time { return now }}
			result, err := Run(context.Background(), job, opts)
			if err != nil || result.Status != "failed" || result.FailureKind != "state_changed" || result.Retryable || result.Error != conflict || calls != 1 || writes != 1 {
				t.Fatalf("conflict was not an immediate terminal input change: %+v err=%v calls=%d writes=%d", result, err, calls, writes)
			}
			if result.Metrics == nil || result.Metrics.StateChanged != 1 || result.Metrics.Committed {
				t.Fatalf("conflict metrics lost the rejected transaction: %+v", result.Metrics)
			}
			saved := outcomeSession(t, runDir)
			deadline := start.Add(300 * time.Second)
			if saved.DecisionConflict != conflict || !saved.ExecutionDeadline.Equal(deadline) || !saved.ReasonDeadline.Equal(deadline) || saved.RepairCount != 0 {
				t.Fatal("conflict changed the original budget or entered output repair")
			}
			last := saved.History[len(saved.History)-1]
			if last.Role != "user" || len(last.Content) != 4 {
				t.Fatalf("tool group was not settled: %+v", last)
			}
			for _, block := range last.Content[1:] {
				if block.Type != "tool_result" || !block.IsError || !strings.Contains(string(block.Content), "state_changed") {
					t.Fatalf("remaining call escaped conflict fencing: %+v", block)
				}
			}
			// Re-delivery cannot turn a terminal conflict into a same-run retry.
			if replay, err := Run(context.Background(), job, opts); err != nil || replay.FailureKind != "state_changed" || calls != 1 || receipts != 1 {
				t.Fatalf("terminal conflict replay performed work: %+v err=%v calls=%d receipts=%d", replay, err, calls, receipts)
			}
			// Simulate interruption after durable tool results but before saving
			// the terminal result: retain conflict and first reconcile receipt.
			saved.Result = nil
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(runDir, "session.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if resumed, err := Run(context.Background(), job, opts); err != nil || resumed.FailureKind != "state_changed" || calls != 1 || writes != 1 || receipts != 2 {
				t.Fatalf("recovery reopened rejected decision: %+v err=%v calls=%d writes=%d receipts=%d", resumed, err, calls, writes, receipts)
			}
		})
	}
}
