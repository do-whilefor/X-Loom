package board

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEmptyDecisionRequiresExecutableWork(t *testing.T) {
	for _, status := range []string{"initial", "completed", "abandoned", "failed", "invalid_open", "invalid_running", "open", "running"} {
		t.Run(status, func(t *testing.T) {
			f := newPlanFixture(t)
			if status != "initial" {
				from := "origin"
				if strings.HasPrefix(status, "invalid_") {
					from = "f001"
				}
				id := f.action("step", "existing", stepInput("Existing investigation", []string{from}, "goal", 0)).ID
				if status == "abandoned" {
					f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "Direction ruled out"})
				}
				if strings.HasPrefix(status, "invalid_") {
					f.action("fact_relation", "correction", map[string]any{"kind": "refutes", "source": "f002", "target": "f001", "reason": "Corrected observation"})
				}
				f.tx(func(tx *Tx) error {
					if status == "failed" {
						putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "failed-work", Kind: "explore", Intent: id, Status: "failed", Result: json.RawMessage(`{"error":"fixture failure"}`)}, 0, "")
					}
					g, err := tx.Load("proj_001")
					if err != nil {
						return err
					}
					if status == "running" || status == "invalid_running" {
						g.Intents[0].Worker, g.Intents[0].Heartbeat = Ptr("executor@work"), Ptr(tx.Now)
					}
					if status == "completed" {
						g.Intents[0].To, g.Intents[0].ConcludedAt = Ptr("f001"), Ptr(tx.Now)
					}
					return tx.Save(g)
				})
			}
			f.tx(func(tx *Tx) error {
				state, err := tx.State("proj_001")
				if err != nil {
					return err
				}
				before, _ := json.Marshal(state)
				version := DecisionStateVersion(state)
				job, _ := json.Marshal(map[string]any{"kind": "reason", "run_id": "first", "graph": state.Graph, "state": state,
					"decision": map[string]any{"version": 2, "state_version": version}, "budget": map[string]int{"max_intents": 3}})
				if err := tx.RegisterExecution(Execution{ProjectID: "proj_001", ID: "first", Namespace: "fixture", Backend: "planner", Kind: "reason", Lease: f.fence.Run, Job: job, RetryKey: "reason:idle"}); err != nil {
					return err
				}
				original, err := tx.batchExecution("proj_001", f.fence)
				if err != nil {
					return err
				}
				var writes int
				if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM xloom_state_actions)+(SELECT COUNT(*) FROM xloom_state_events)").Scan(&writes); err != nil {
					return err
				}
				batch := DecisionBatch{ExpectedVersion: version}
				if status == "initial" {
					// Model reset after a rejected plan must not convert a failure
					// into an empty acknowledgement that leaves no future work.
					invalid := DecisionBatch{ExpectedVersion: version, Actions: []DecisionAction{
						{Op: "goal", Ref: "temporary", Payload: json.RawMessage(`{"action":"add","condition":"Temporary direction"}`)},
						{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["missing"],"description":"Invalid input"}`)},
					}}
					if _, err := tx.CommitDecision("proj_001", f.fence, invalid); err == nil {
						t.Fatal("invalid plan was accepted")
					}
				}
				allowed := status == "open" || status == "running"
				for _, commit := range []bool{false, true} {
					receipt, err := tx.decisionBatch("proj_001", f.fence, batch, commit)
					if allowed {
						if err != nil || receipt.Committed != commit || receipt.ChangedActions != 0 {
							t.Fatalf("valid pending work rejected: commit=%v receipt=%+v err=%v", commit, receipt, err)
						}
						continue
					}
					var api *APIError
					if !errors.As(err, &api) || api.Status != 422 || !strings.Contains(err.Error(), "empty decision would leave the project idle") || receipt.Committed {
						t.Fatalf("idle empty batch was accepted: commit=%v receipt=%+v err=%v", commit, receipt, err)
					}
					current, err := tx.State("proj_001")
					if err != nil {
						return err
					}
					after, _ := json.Marshal(current)
					e, err := tx.batchExecution("proj_001", f.fence)
					if err != nil {
						return err
					}
					saved, err := tx.DecisionReceipt("proj_001", f.fence)
					if err != nil {
						return err
					}
					var afterWrites int
					if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM xloom_state_actions)+(SELECT COUNT(*) FROM xloom_state_events)").Scan(&afterWrites); err != nil {
						return err
					}
					if string(after) != string(before) || e.Status != original.Status || saved.Committed || afterWrites != writes {
						t.Fatal("rejected empty batch changed state, revision, lease, execution or receipt")
					}
				}
				if allowed {
					// Receipt replay remains valid after the outstanding work ends.
					g, err := tx.Load("proj_001")
					if err != nil {
						return err
					}
					g.Intents[0].To, g.Intents[0].ConcludedAt = Ptr("f001"), Ptr(tx.Now)
					if err := tx.Save(g); err != nil {
						return err
					}
				} else {
					batch.Actions = []DecisionAction{{Op: "step", Ref: "corrected", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Inspect a new viable direction"}`)}}
				}
				result, err := tx.CommitDecision("proj_001", f.fence, batch)
				if err != nil || !result.Committed || (!allowed && result.ChangedActions != 1) {
					t.Fatalf("same run could not commit corrected plan or replay its receipt: %+v %v", result, err)
				}
				return nil
			})
		})
	}
}
