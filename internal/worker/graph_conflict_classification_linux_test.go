//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func TestGraphStateConflictRequiresDefinitiveMarker(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		want    bool
	}{
		{name: "no_error"},
		{name: "local_conflict", message: "state_changed: input is no longer current", want: true},
		{name: "http_conflict", message: `board HTTP 409: {"detail":"state_changed: input is no longer current"}`, want: true},
		{name: "validation_status", message: `board HTTP 422: {"detail":"state_changed: invalid user input"}`},
		{name: "missing_status", message: `board HTTP 404: {"detail":"state_changed: missing user input"}`},
		{name: "validation_alias", message: `board HTTP 422: {"detail":"unknown or forward decision reference $state_changed"}`},
		{name: "conflict_alias", message: `board HTTP 409: {"detail":"unknown or forward decision reference $state_changed"}`},
		{name: "nested_detail", message: `board HTTP 409: {"detail":{"action":1,"error":"state_changed: invalid user input"}}`},
		{name: "unrelated_field", message: `board HTTP 409: {"detail":"invalid action","error":"state_changed: invalid user input"}`},
		{name: "transport_context", message: "connection closed after submitting $state_changed; commit outcome unknown"},
		{name: "malformed_response", message: `board HTTP 409: {"detail":"state_changed: invalid JSON"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.message != "" {
				err = errors.New(test.message)
			}
			if got := graphStateConflict(err); got != test.want {
				t.Fatalf("graphStateConflict(%v) = %v, want %v", err, got, test.want)
			}
		})
	}
}

func TestDecisionValidationMentioningStateChangedAllowsDraftRepair(t *testing.T) {
	for _, operation := range []string{"preview", "commit"} {
		t.Run(operation, func(t *testing.T) {
			job, runDir := draftRunJob(t), t.TempDir()
			calls, rejected, commits, receipts := 0, 0, 0, 0
			const validation = `board HTTP 422: {"detail":"unknown or forward decision reference $state_changed"}`
			bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
				if request.Op == "decision_receipt" {
					receipts++
					return board.DecisionReceipt{}, nil
				}
				if request.Batch == nil || request.Batch.ExpectedVersion != job.Decision.StateVersion || len(request.Batch.Actions) != 1 {
					t.Fatalf("validation repair lost the draft binding: %+v", request)
				}
				if rejected == 0 {
					if request.Op != "decision_"+operation || !strings.Contains(string(request.Batch.Actions[0].Payload), "$state_changed") {
						t.Fatalf("fixture did not reject its invalid alias: %+v", request)
					}
					rejected++
					return nil, errors.New(validation)
				}
				if request.Op != "decision_commit" || request.Batch.Actions[0].Ref != "corrected" || strings.Contains(string(request.Batch.Actions[0].Payload), "$state_changed") {
					t.Fatalf("repair published the invalid draft: %+v", request)
				}
				commits++
				return board.DecisionReceipt{Committed: true, StateVersion: job.Decision.StateVersion}, nil
			}}
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				var actions []agent.Message
				switch calls {
				case 1:
					actions = []agent.Message{
						draftModelCall("invalid", "graph_action", `{"op":"step","idempotency_key":"invalid","payload":{"action":"add","goal_id":"$state_changed","from":["origin"],"description":"Inspect the boundary"}}`),
						draftModelCall("validate", "graph_action", `{"op":"`+operation+`","idempotency_key":"validate","payload":{}}`),
					}
				case 2:
					last := history[len(history)-1]
					var errorText string
					if len(last.Content) != 2 || !last.Content[1].IsError || json.Unmarshal(last.Content[1].Content, &errorText) != nil || errorText != validation {
						t.Fatalf("model did not receive the repairable validation error: %+v", last)
					}
					actions = []agent.Message{
						draftModelCall("reset", "graph_action", `{"op":"reset","idempotency_key":"reset","payload":{}}`),
						draftModelCall("corrected", "graph_action", `{"op":"step","idempotency_key":"corrected","payload":{"action":"add","goal_id":"goal","from":["origin"],"description":"Inspect the boundary"}}`),
						draftModelCall("publish", "graph_action", `{"op":"commit","idempotency_key":"publish","payload":{}}`),
					}
				default:
					t.Fatal("validation repair required an unexpected model request")
					return agent.Message{}, nil
				}
				message := actions[0]
				for _, action := range actions[1:] {
					message.Content = append(message.Content, action.Content...)
				}
				return message, nil
			})
			result, err := Run(context.Background(), job, Options{Provider: provider, RunDir: runDir, Output: bridge})
			wantReceipts := 1
			if operation == "commit" {
				wantReceipts++ // Reconcile the failed commit before resetting its draft.
			}
			if err != nil || result.Status != "success" || result.Text != committedDecisionText || calls != 2 || rejected != 1 || commits != 1 || receipts != wantReceipts {
				t.Fatalf("validation was treated as a terminal conflict: %+v err=%v calls=%d rejected=%d commits=%d receipts=%d", result, err, calls, rejected, commits, receipts)
			}
			if result.Metrics == nil || result.Metrics.StateChanged != 0 || !result.Metrics.Committed || outcomeSession(t, runDir).DecisionConflict != "" {
				t.Fatal("validation error was persisted or measured as a version conflict")
			}
		})
	}
}

func TestUnknownCommitMentioningStateChangedRecoversReceipt(t *testing.T) {
	job, runDir := draftRunJob(t), t.TempDir()
	calls, commits, receipts := 0, 0, 0
	finalVersion := strings.Repeat("e", 64)
	const unknown = "connection closed after submitting $state_changed; commit outcome unknown"
	bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		switch request.Op {
		case "decision_receipt":
			receipts++
			return board.DecisionReceipt{Committed: commits == 1, StateVersion: finalVersion}, nil
		case "decision_commit":
			commits++
			if commits != 1 || request.Batch == nil || len(request.Batch.Actions) != 1 || request.Batch.ExpectedVersion != job.Decision.StateVersion {
				t.Fatalf("unknown commit was lost or republished: %+v commits=%d", request, commits)
			}
			return nil, errors.New(unknown)
		default:
			t.Fatalf("unexpected operation after unknown commit: %s", request.Op)
			return nil, nil
		}
	}}
	provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		switch calls {
		case 1:
			message := draftModelCall("stage", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":["origin"],"description":"Inspect once"}}`)
			publish := draftModelCall("publish", "graph_action", `{"op":"commit","idempotency_key":"publish","payload":{}}`)
			message.Content = append(message.Content, publish.Content...)
			return message, nil
		case 2:
			last := history[len(history)-1]
			var errorText string
			if len(last.Content) != 2 || !last.Content[1].IsError || json.Unmarshal(last.Content[1].Content, &errorText) != nil || errorText != unknown {
				t.Fatalf("unknown commit response was not retained: %+v", last)
			}
			// Reset must reconcile the uncertain commit before discarding anything.
			return draftModelCall("reset", "graph_action", `{"op":"reset","idempotency_key":"reset","payload":{}}`), nil
		default:
			t.Fatal("receipt recovery discarded the commit and resumed planning")
			return agent.Message{}, nil
		}
	})
	result, err := Run(context.Background(), job, Options{Provider: provider, RunDir: runDir, Output: bridge})
	if err != nil || result.Status != "success" || result.Text != committedDecisionText || result.StateVersion != finalVersion || calls != 2 || commits != 1 || receipts != 2 {
		t.Fatalf("unknown commit was misclassified or repeated: %+v err=%v calls=%d commits=%d receipts=%d", result, err, calls, commits, receipts)
	}
	if result.Metrics == nil || result.Metrics.StateChanged != 0 || !result.Metrics.Committed || outcomeSession(t, runDir).DecisionConflict != "" {
		t.Fatal("unknown commit was persisted or measured as a rejected transaction")
	}
}
