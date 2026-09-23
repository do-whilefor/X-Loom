package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestLegacyResultPreservesWorkerNormalizationAndIdentityErrors(t *testing.T) {
	for _, tc := range []struct {
		name, kind, output string
		status             int
	}{
		{"plan", "reason", `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"Observe the fixture"}]}}`, http.StatusOK},
		{"complete", "reason", `{"accepted":true,"data":{"complete":{"from":["origin"],"description":"Fixture complete"}}}`, http.StatusOK},
		{"explore", "explore", `{"accepted":true,"data":{"description":"Observed the fixture"}}`, http.StatusForbidden},
		{"bootstrap", "bootstrap", `{"accepted":true,"data":{"fact":{"description":"Observed the fixture"},"complete":{"description":"Fixture complete"}}}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, store := newSnapshotHTTPFixture(t)
			f.register(tc.kind, nil, 0)
			// Reproduce an old imported execution whose backend/lease retained
			// leading whitespace. HTTP's worker field historically trims it.
			previous := f.lease
			f.lease = " " + previous
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				if _, err := tx.Exec("UPDATE xloom_executions SET lease=?,backend=? WHERE project_id=? AND id=?", f.lease, " planner", f.project, f.run); err != nil {
					return err
				}
				if tc.kind == "reason" {
					_, err := tx.Exec("UPDATE projects SET reason_worker=? WHERE id=?", f.lease, f.project)
					return err
				}
				_, err := tx.Exec("UPDATE intents SET worker=? WHERE project_id=? AND id=?", f.lease, f.project, f.intent)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			f.pending(tc.output)
			before := f.state()
			response := f.apply(tc.status)
			after := f.state()
			if tc.status == http.StatusOK {
				if len(after.Graph.Intents) != 1 || after.Graph.Intents[0].Creator != previous {
					t.Fatalf("legacy creator normalization changed: %+v", after.Graph.Intents)
				}
			} else if !reflect.DeepEqual(before, after) || !strings.Contains(response, "matching Execute identity") {
				t.Fatalf("legacy identity error changed or mutated state: %s", response)
			}
		})
	}
}

func TestIntentIdentityErrorPrecedesMalformedConclusionDescription(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("explore", nil, 0)
	response := f.request("POST", f.base()+"/intents/"+f.intent+"/conclude", map[string]any{"worker": "different-executor", "description": nil}, true, http.StatusForbidden, nil)
	if !strings.Contains(response, "matching Execute identity") {
		t.Fatalf("HTTP error precedence changed: %s", response)
	}
}

func TestLegacyFinalPlanAcceptsValidSiblingsOfInvalidDirections(t *testing.T) {
	for name, invalid := range map[string]string{
		"missing source":    `{"from":["missing"],"description":"Invalid"}`,
		"duplicate source":  `{"from":["origin","origin"],"description":"Invalid"}`,
		"null source":       `{"from":["origin",null],"description":"Invalid"}`,
		"wrong source type": `{"from":["origin",42],"description":"Invalid"}`,
		"wrong description": `{"from":["origin"],"description":42}`,
		"empty description": `{"from":["origin"],"description":"  "}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			f.register("reason", nil, 0)
			f.pending(`{"accepted":true,"data":{"intents":[` + invalid + `,{"from":["origin"],"description":"  Valid sibling  "}]}}`)
			f.apply(http.StatusOK)
			f.apply(http.StatusOK)
			state := f.state()
			if len(state.Steps) != 1 || state.Steps[0].ID != "i001" || state.Steps[0].Description != "Valid sibling" || state.Revision != 1 || len(legacyEvents(f)) != 1 {
				t.Fatalf("partial acceptance or replay changed: %+v", state)
			}
		})
	}
}

func TestLegacyFinalPlanSQLFailureRollsBackAllDirectionsAndReceipt(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	f.register("reason", nil, 0)
	f.pending(`{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"First insertion"},{"from":["origin"],"description":"Fail second insertion"}]}}`)
	before := f.state()
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_second_direction BEFORE INSERT ON intents WHEN NEW.description='Fail second insertion' BEGIN SELECT RAISE(ABORT,'injected SQL failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.apply(http.StatusInternalServerError)
	if after := f.state(); !reflect.DeepEqual(before, after) || len(legacyEvents(f)) != 0 {
		t.Fatalf("SQL failure retained partial state or events: %+v", after)
	}
	var stored board.Execution
	f.request("GET", f.base()+"/executions/"+f.run+"?namespace=protocol-test", nil, false, http.StatusOK, &stored)
	if stored.Status != "result_pending" {
		t.Fatalf("SQL failure committed receipt: %s", stored.Status)
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec(`DROP TRIGGER reject_second_direction`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.apply(http.StatusOK)
	state := f.state()
	if len(state.Steps) != 2 || state.Steps[0].ID != "i001" || state.Steps[1].ID != "i002" || state.Revision != 2 {
		t.Fatalf("SQL rollback lost counter or transaction replay: %+v", state)
	}
}

func legacyEvents(f *executionProtocolFixture) []board.StateEvent {
	f.t.Helper()
	var events []board.StateEvent
	f.request("GET", f.base()+"/state/events", nil, false, http.StatusOK, &events)
	return events
}

func TestLegacyHTTPMutationsPublishOrderedBusinessEvents(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	intent := f.newIntent()
	initial := f.state()
	if initial.Revision != 1 || initial.DecisionRevision != 0 {
		t.Fatalf("creating a Step did not emit its event, or retriggered Decide: %+v", initial)
	}
	for _, op := range []string{"heartbeat", "heartbeat", "release", "release", "heartbeat"} {
		f.request("POST", f.base()+"/intents/"+intent.ID+"/"+op, map[string]string{"worker": "legacy-executor"}, false, http.StatusOK, nil)
	}
	f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": "legacy-planner", "trigger": "initial"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/reason/heartbeat", map[string]string{"worker": "legacy-planner"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/reason/release", map[string]string{"worker": "legacy-planner"}, false, http.StatusOK, nil)
	if state := f.state(); state.Revision != initial.Revision || state.DecisionRevision != initial.DecisionRevision {
		t.Fatal("claims, heartbeats or releases emitted domain changes")
	}
	var conclusion board.Conclusion
	f.request("POST", f.base()+"/intents/"+intent.ID+"/conclude", map[string]string{"worker": "legacy-executor", "description": "Observed an authenticated fixture response"}, false, http.StatusOK, &conclusion)
	f.request("POST", f.base()+"/complete", map[string]any{"from": []string{conclusion.Fact.ID}, "description": "Required fixture checked", "worker": "legacy-planner"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/reopen", map[string]string{"description": "The fixture changed", "creator": "user"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Check the changed response", "creator": "user"}, false, http.StatusCreated, nil)
	state, events := f.state(), legacyEvents(f)
	ops := []string{"step", "step_completed", "complete", "reopen", "hint"}
	if len(events) != len(ops) || state.Revision != 5 || state.DecisionRevision != 3 {
		t.Fatalf("legacy changes were not represented exactly once: revision=%d decision=%d events=%+v", state.Revision, state.DecisionRevision, events)
	}
	for n, event := range events {
		if event.Op != ops[n] || event.Revision != int64(n+1) || event.ID == "" || !json.Valid(event.Payload) || !json.Valid(event.Result) {
			t.Fatalf("invalid event %d: %+v", n, event)
		}
	}
	var recorded board.Conclusion
	if err := json.Unmarshal(events[1].Result, &recorded); err != nil || recorded.Fact.ID != conclusion.Fact.ID || recorded.Intent.ID != intent.ID || events[1].RunID != "legacy-executor" {
		t.Fatalf("completion event lost its observation, Step or run: %+v", events[1])
	}
}

func TestLegacyEventDoesNotEnableExtendedCompletionRules(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Legacy input", "creator": "user"}, false, http.StatusCreated, nil)
	// Pure compatibility callers historically may complete using original input.
	// Event counters alone must not opt them into the extended fact contract.
	f.request("POST", f.base()+"/complete", map[string]any{"from": []string{"origin"}, "description": "Legacy completion", "worker": "legacy"}, false, http.StatusOK, nil)
	if f.state().Graph.Project.Status != "completed" || len(legacyEvents(f)) != 2 {
		t.Fatal("event adaptation changed the legacy completion contract")
	}
}

func TestLegacyFinalResultReplayAndModernActionsDoNotDuplicateEvents(t *testing.T) {
	t.Run("persisted final result", func(t *testing.T) {
		f := newExecutionProtocolFixture(t)
		f.register("explore", nil, 0)
		before := f.state()
		f.pending(`{"accepted":true,"data":{"description":"Legacy verified observation"}}`)
		f.apply(http.StatusOK)
		first := f.state()
		f.apply(http.StatusOK)
		after := f.state()
		if first.Revision != before.Revision+1 || first.DecisionRevision != before.DecisionRevision+1 || after.Revision != first.Revision || len(legacyEvents(f)) != int(after.Revision) {
			t.Fatal("result replay duplicated or omitted the legacy completion event")
		}
	})
	t.Run("modern action and duplicate compatibility plan", func(t *testing.T) {
		f := newExecutionProtocolFixture(t)
		f.register("reason", nil, 0)
		input := map[string]any{"action": "add", "from": []string{"origin"}, "description": "Observe one fixture"}
		f.action("step", "one-step", input)
		f.action("step", "one-step", input)
		f.request("POST", f.base()+"/intents", map[string]any{"from": []string{"origin"}, "description": "Observe one fixture", "creator": f.lease}, true, http.StatusOK, nil)
		if state := f.state(); state.Revision != 1 || state.DecisionRevision != 0 || len(legacyEvents(f)) != 1 {
			t.Fatal("modern command or duplicate compatibility plan emitted extra events")
		}
	})
}
