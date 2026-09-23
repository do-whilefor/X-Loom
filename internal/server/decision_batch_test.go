package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"xloom/internal/board"
)

func newDecisionBatchFixture(t *testing.T) *executionProtocolFixture {
	f := newExecutionProtocolFixture(t)
	live := true
	f.registerWithFields("reason", &live, 2, map[string]any{"decision": map[string]any{"version": 2, "state_version": board.DecisionStateVersion(f.state())}}, http.StatusCreated)
	return f
}
func batchAction(op, ref, payload string) board.DecisionAction {
	return board.DecisionAction{Op: op, Ref: ref, Payload: json.RawMessage(payload)}
}
func (f *executionProtocolFixture) batch(actions ...board.DecisionAction) board.DecisionBatch {
	return board.DecisionBatch{ExpectedVersion: board.DecisionStateVersion(f.state()), Actions: actions}
}
func (f *executionProtocolFixture) decision(op string, batch board.DecisionBatch, want int) board.DecisionReceipt {
	var receipt board.DecisionReceipt
	f.request("POST", f.base()+"/state/decisions/"+op, batch, true, want, &receipt)
	return receipt
}
func (f *executionProtocolFixture) decisionReceipt(want int) board.DecisionReceipt {
	var receipt board.DecisionReceipt
	f.request("GET", f.base()+"/state/decisions/receipt", nil, true, want, &receipt)
	return receipt
}

func TestDecisionPreviewIsIsolatedAndCommitResolvesReferencesOnce(t *testing.T) {
	f := newDecisionBatchFixture(t)
	before := f.state()
	batch := f.batch(
		batchAction("goal", "inspect", `{"action":"add","condition":"Inspect fixture"}`),
		batchAction("step", "check", `{"action":"add","goal_id":"$inspect","from":["origin"],"description":"Check $inspect literally in fixture"}`),
	)
	preview := f.decision("preview", batch, http.StatusOK)
	if preview.Committed || len(preview.Results) != 2 || preview.IDs["inspect"] == "" || preview.IDs["check"] == "" {
		t.Fatalf("invalid preview: %+v", preview)
	}
	if !reflect.DeepEqual(before, f.state()) || f.decisionReceipt(http.StatusOK).Committed {
		t.Fatal("preview published its draft or receipt")
	}
	receipt := f.decision("commit", batch, http.StatusOK)
	if !receipt.Committed || receipt.Completed || len(receipt.Results) != 2 {
		t.Fatalf("invalid receipt: %+v", receipt)
	}
	state := f.state()
	if len(state.Steps) != 1 || state.Steps[0].GoalID != receipt.IDs["inspect"] || state.Steps[0].Description != "Check $inspect literally in fixture" {
		t.Fatalf("references or literal text changed incorrectly: %+v", state.Steps)
	}
	batch.ExpectedVersion = "old response was lost"
	if replay := f.decision("commit", batch, http.StatusOK); !reflect.DeepEqual(receipt, replay) {
		t.Fatal("lost response retry returned a different receipt")
	}
	if read := f.decisionReceipt(http.StatusOK); !reflect.DeepEqual(receipt, read) {
		t.Fatal("receipt query disagreed with commit")
	}
	if !reflect.DeepEqual(state, f.state()) {
		t.Fatal("receipt replay duplicated state")
	}
	var executions []board.Execution
	f.request("GET", "/executions?namespace=protocol-test", nil, false, http.StatusOK, &executions)
	if len(executions) != 1 || executions[0].Status != "succeeded" {
		t.Fatal("batch commit did not atomically acknowledge execution")
	}
	f.decision("commit", f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"different plan"}`)), http.StatusConflict)
}

func TestDecisionBatchRejectsIllegalHalfWithoutPublishingChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action board.DecisionAction
	}{
		{"invalid_source", batchAction("step", "", `{"action":"add","from":["missing"],"description":"invalid source"}`)},
		{"unknown_alias", batchAction("step", "", `{"action":"add","goal_id":"$missing","from":["origin"],"description":"invalid alias"}`)},
		{"forbidden_evidence", batchAction("fact", "", `{"description":"invented"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionBatchFixture(t)
			before := f.state()
			batch := f.batch(batchAction("goal", "g", `{"action":"add","condition":"Temporary goal"}`), tc.action)
			want := http.StatusUnprocessableEntity
			if tc.name == "invalid_source" {
				want = http.StatusNotFound
			}
			f.decision("preview", batch, want)
			f.decision("commit", batch, want)
			if !reflect.DeepEqual(before, f.state()) || f.decisionReceipt(http.StatusOK).Committed {
				t.Fatal("invalid batch left a half-plan")
			}
		})
	}
}

func TestDecisionBatchRechecksStateAfterPreview(t *testing.T) {
	f := newDecisionBatchFixture(t)
	batch := f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"Check original input"}`))
	f.decision("preview", batch, http.StatusOK)
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "New constraint requires reconsideration"}, false, http.StatusCreated, nil)
	f.decision("commit", batch, http.StatusConflict)
	if len(f.state().Steps) != 0 || f.decisionReceipt(http.StatusOK).Committed {
		t.Fatal("stale plan became dispatchable")
	}
	// The planner has explicitly reread and reconsidered this fixture constraint.
	batch.ExpectedVersion = board.DecisionStateVersion(f.state())
	batch.Actions[0].Payload = json.RawMessage(`{"action":"add","from":["origin"],"description":"Check input under the new user constraint"}`)
	if !f.decision("commit", batch, http.StatusOK).Committed {
		t.Fatal("refreshed plan could not progress")
	}
}

func TestDecisionBatchRequiresRegisteredVersionAndFencesDirectWrites(t *testing.T) {
	f := newDecisionBatchFixture(t)
	f.request("POST", f.base()+"/state/actions", map[string]any{"op": "step", "idempotency_key": "bypass", "payload": map[string]any{"action": "add", "from": []string{"origin"}, "description": "bypass"}, "expected_version": board.DecisionStateVersion(f.state())}, true, http.StatusForbidden, nil)
	for _, version := range []int{0, 1} {
		legacy := newExecutionProtocolFixture(t)
		legacy.register("reason", nil, version)
		legacy.decision("commit", legacy.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"Old protocol"}`)), http.StatusForbidden)
		legacy.action("step", "legacy-action", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Old protocol still works"})
	}
}

func TestUncommittedDecisionCannotCrossStopOrRestart(t *testing.T) {
	for _, action := range []string{"stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			f := newDecisionBatchFixture(t)
			batch := f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"Draft before management action"}`))
			f.decision("preview", batch, http.StatusOK)
			if action == "stop" {
				f.request("PUT", f.base()+"/status", map[string]string{"status": "stopped"}, false, http.StatusOK, nil)
				f.request("PUT", f.base()+"/status", map[string]string{"status": "active"}, false, http.StatusOK, nil)
			} else {
				f.request("POST", f.base()+"/restart", map[string]any{}, false, http.StatusOK, nil)
			}
			want := http.StatusConflict
			if action == "restart" {
				want = http.StatusForbidden
			}
			f.decision("commit", batch, want)
			if len(f.state().Steps) != 0 {
				t.Fatal("old draft crossed its revoked execution boundary")
			}
		})
	}
}

func TestDecisionCanWithdrawAuxiliaryPlanAndCompleteAtomically(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	observed := f.newIntent()
	var result board.Conclusion
	f.request("POST", f.base()+"/intents/"+observed.ID+"/conclude", map[string]string{"worker": "fixture", "description": "Requested fixture rejects unauthenticated access"}, false, http.StatusOK, &result)
	f.kind = "reason"
	f.lease = "setup-planner"
	f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": f.lease, "trigger": "initial"}, false, http.StatusOK, nil)
	goal := f.action("goal", "auxiliary-goal", map[string]any{"action": "add", "condition": "Optional supporting investigation"})
	step := f.action("step", "auxiliary-step", map[string]any{"action": "add", "goal_id": goal.ID, "from": []string{"origin"}, "description": "Optional additional check"})
	f.request("POST", f.base()+"/reason/release", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	f.request("POST", f.base()+"/intents/"+step.ID+"/heartbeat", map[string]string{"worker": "existing-executor"}, false, http.StatusOK, nil)
	f.lease = "planner@" + f.run
	live := true
	f.registerWithFields("reason", &live, 2, map[string]any{"decision": map[string]any{"version": 2, "state_version": board.DecisionStateVersion(f.state())}}, http.StatusCreated)
	marshal := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	complete := batchAction("complete", "", marshal(map[string]any{"from": []string{result.Fact.ID}, "description": "User condition is supported by this observation"}))
	f.decision("commit", f.batch(complete), http.StatusConflict)
	abandon := batchAction("step", "", marshal(map[string]any{"action": "abandon", "id": step.ID, "reason": "Existing witness meets the root condition; this auxiliary check is unnecessary"}))
	withdraw := batchAction("goal", "", marshal(map[string]any{"action": "withdraw", "id": goal.ID, "reason": "Existing witness meets the root condition"}))
	batch := f.batch(abandon, withdraw, complete)
	before := f.state()
	// Even cancellation and its lease revocation must roll back when a later
	// replacement in the same proposed plan is invalid.
	f.decision("commit", f.batch(abandon, batchAction("step", "", `{"action":"add","from":["missing"],"description":"Invalid replacement"}`)), http.StatusNotFound)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("invalid replacement left the original task abandoned")
	}
	preview := f.decision("preview", batch, http.StatusOK)
	if preview.Completed || preview.Committed || preview.ValidationScope != "protocol_only" || preview.CompletionReview == nil || !reflect.DeepEqual(before, f.state()) {
		t.Fatal("completion preview changed formal work")
	}
	// A preview's proposed cancellation must not revoke the executing lease.
	f.request("POST", f.base()+"/intents/"+step.ID+"/heartbeat", map[string]string{"worker": "existing-executor"}, false, http.StatusOK, nil)
	committed := f.decision("commit", batch, http.StatusOK)
	if !committed.Completed || !committed.Committed || committed.ValidationScope != "" || committed.CompletionReview != nil {
		t.Fatal("root completion and withdrawal were not committed together")
	}
	state := f.state()
	if state.Graph.Project.Status != "completed" || state.Goals[0].Condition != before.Goals[0].Condition || len(state.Steps) != 3 {
		t.Fatalf("completion changed root conditions or lost work: %+v", state)
	}
	if !reflect.DeepEqual(committed, f.decisionReceipt(http.StatusOK)) {
		t.Fatal("completed and revoked planner cannot read its durable receipt")
	}
	f.decision("commit", batch, http.StatusOK)
	f.request("POST", f.base()+"/intents/"+step.ID+"/conclude", map[string]string{"worker": "existing-executor", "description": "Late result"}, false, http.StatusForbidden, nil)
}

func TestDecisionCannotWithdrawRootOrCompleteWithoutObservedSupport(t *testing.T) {
	f := newDecisionBatchFixture(t)
	f.decision("commit", f.batch(batchAction("goal", "", `{"action":"withdraw","id":"goal","reason":"Avoid untested scope"}`)), http.StatusForbidden)
	f.decision("commit", f.batch(batchAction("complete", "", `{"from":["origin"],"description":"No checks remain"}`)), http.StatusConflict)
	if f.state().Graph.Project.Status != "active" {
		t.Fatal("queue empty or withdrawn user requirements counted as completion")
	}
}

func TestDecisionBatchNoopEndsExecutionWithoutChangingSharedState(t *testing.T) {
	f := newDecisionBatchFixture(t)
	before := f.state()
	receipt := f.decision("commit", f.batch([]board.DecisionAction{}...), http.StatusOK)
	after := f.state()
	if !receipt.Committed || receipt.Completed || receipt.StateVersion != board.DecisionStateVersion(before) || after.Revision != before.Revision || after.DecisionRevision != before.DecisionRevision || after.Graph.Project.Reason != nil {
		t.Fatal("empty commit changed shared content or retained its lease")
	}
}

func TestDecisionBatchRespectsDirectionLimitAtomically(t *testing.T) {
	f := newDecisionBatchFixture(t)
	actions := []board.DecisionAction{}
	for _, name := range []string{"first", "second", "third", "fourth"} {
		raw, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": name})
		actions = append(actions, batchAction("step", "", string(raw)))
	}
	f.decision("commit", f.batch(actions...), http.StatusConflict)
	if len(f.state().Steps) != 0 {
		t.Fatal("over-limit batch published its earlier directions")
	}
}

func TestConcurrentDecisionCommitRetriesShareOneReceipt(t *testing.T) {
	f := newDecisionBatchFixture(t)
	batch := f.batch(batchAction("step", "check", `{"action":"add","from":["origin"],"description":"Exactly one direction"}`))
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	for n := 0; n < 2; n++ {
		go func() {
			r := httptest.NewRequest("POST", f.base()+"/state/decisions/commit", bytes.NewReader(raw))
			r.Header.Set("X-Xloom-Run", f.lease)
			r.Header.Set("X-Xloom-Lease", f.kind)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			responses <- w
		}()
	}
	var first board.DecisionReceipt
	for n := 0; n < 2; n++ {
		response := <-responses
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent retry: %d %s", response.Code, response.Body.String())
		}
		var receipt board.DecisionReceipt
		if err = json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = receipt
		} else if !reflect.DeepEqual(first, receipt) {
			t.Fatal("concurrent retries disagreed on receipt")
		}
	}
	if len(f.state().Steps) != 1 {
		t.Fatal("concurrent commit created duplicate work")
	}
}
