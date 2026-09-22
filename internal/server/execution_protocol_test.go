package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
)

// Exercise the HTTP application boundary with real SQLite transactions. No
// Worker or model is involved: these are the outputs a faulty Worker could send.
type executionProtocolFixture struct {
	t       *testing.T
	handler http.Handler
	project string
	run     string
	lease   string
	kind    string
	intent  string
}

func newExecutionProtocolFixture(t *testing.T) *executionProtocolFixture {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "protocol.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// A fixed clock keeps these tests independent of heartbeat timing.
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	f := &executionProtocolFixture{t: t, handler: New(store), run: "protocol-run", lease: "planner@protocol-run"}
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Protocol regression", "origin": "Synthetic local input", "goal": "Verify a synthetic result", "bootstrap_enabled": false}, false, http.StatusCreated, &graph)
	f.project = graph.Project.ID
	return f
}

func (f *executionProtocolFixture) base() string { return "/projects/" + f.project }

func (f *executionProtocolFixture) request(method, path string, body any, fenced bool, want int, out any) string {
	f.t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if fenced {
		r.Header.Set("X-Xloom-Run", f.lease)
		r.Header.Set("X-Xloom-Lease", f.kind)
		r.Header.Set("X-Xloom-Intent", f.intent)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	if out != nil {
		if err = json.Unmarshal(w.Body.Bytes(), out); err != nil {
			f.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return w.Body.String()
}

func (f *executionProtocolFixture) state() board.State {
	f.t.Helper()
	var state board.State
	f.request("GET", f.base()+"/state", nil, false, http.StatusOK, &state)
	return state
}

func (f *executionProtocolFixture) newIntent() board.Intent {
	f.t.Helper()
	var intent board.Intent
	f.request("POST", f.base()+"/intents", map[string]any{"from": []string{"origin"}, "description": "Observe the synthetic fixture", "creator": "fixture"}, false, http.StatusCreated, &intent)
	return intent
}

// nil preserves the old job format that omitted graph_rpc entirely.
func (f *executionProtocolFixture) register(kind string, graphRPC *bool, version int) {
	f.t.Helper()
	f.registerWithFields(kind, graphRPC, version, nil, http.StatusCreated)
}

func (f *executionProtocolFixture) registerWithFields(kind string, graphRPC *bool, version int, fields map[string]any, want int) {
	f.t.Helper()
	f.kind = kind
	var intent *board.Intent
	if kind == "reason" {
		f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": f.lease, "trigger": "initial"}, false, http.StatusOK, nil)
	} else {
		created := f.newIntent()
		f.intent = created.ID
		f.request("POST", f.base()+"/intents/"+created.ID+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusOK, &created)
		intent = &created
	}
	state := f.state()
	job := map[string]any{
		"run_id": f.run, "kind": kind, "workspace": "/workspace",
		"graph": state.Graph, "state": state, "intent": intent,
		"budget": map[string]int{"max_intents": 3, "conclude_timeout": 60},
	}
	if graphRPC != nil {
		job["graph_rpc"] = *graphRPC
	}
	if version != 0 {
		job["result_contract_version"] = version
	}
	if kind == "reason" {
		job["decision"] = map[string]any{"version": 1, "state_version": board.DecisionStateVersion(state)}
	}
	for key, value := range fields {
		job[key] = value
	}
	raw, err := json.Marshal(job)
	if err != nil {
		f.t.Fatal(err)
	}
	key := "reason:protocol-fixture"
	if kind != "reason" {
		key = kind + ":" + f.intent
	}
	e := board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: kind, Intent: f.intent, Lease: f.lease, Job: raw, RetryKey: key}
	f.request("POST", f.base()+"/executions", e, true, want, nil)
}

func (f *executionProtocolFixture) action(op, key string, payload any) board.StateActionResult {
	f.t.Helper()
	var receipt board.StateActionResult
	f.request("POST", f.base()+"/state/actions", map[string]any{
		"op": op, "idempotency_key": key, "payload": payload,
		"expected_version": board.DecisionStateVersion(f.state()),
	}, true, http.StatusOK, &receipt)
	return receipt
}

func (f *executionProtocolFixture) pending(text string) {
	f.t.Helper()
	result := map[string]any{"status": "success", "text": text, "state_version": board.DecisionStateVersion(f.state())}
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "result_pending", "result": result}, true, http.StatusOK, nil)
}

func (f *executionProtocolFixture) apply(want int) string {
	f.t.Helper()
	// These untrusted request fields must never override the persisted Job.
	return f.request("POST", f.base()+"/executions/"+f.run+"/apply", map[string]any{"graph_rpc": false, "result_contract_version": 0}, true, want, nil)
}

func TestLiveDecisionCannotSubmitCompatibilitySteps(t *testing.T) {
	outputs := map[string]string{
		"plural":   `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"Observe the synthetic fixture"}]}}`,
		"singular": `{"accepted":true,"data":{"intent":{"from":["origin"],"description":"Observe the synthetic fixture"}}}`,
	}
	for versionName, version := range map[string]int{"legacy_version": 0, "current_version": 1} {
		for name, output := range outputs {
			for _, alreadyCreated := range []bool{false, true} {
				label := "without_graph_action"
				if alreadyCreated {
					label = "after_graph_action"
				}
				t.Run(versionName+"/"+name+"/"+label, func(t *testing.T) {
					f := newExecutionProtocolFixture(t)
					live := true
					f.register("reason", &live, version)
					if alreadyCreated {
						f.action("step", "one-direction", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Observe the synthetic fixture"})
					}
					before := f.state()
					f.pending(output)
					f.apply(http.StatusUnprocessableEntity)
					f.apply(http.StatusUnprocessableEntity)
					after := f.state()
					if len(after.Steps) != len(before.Steps) || after.Revision != before.Revision || after.Graph.Project.Status != "active" {
						t.Fatal("rejected final plan changed the project or created a duplicate step")
					}
					var entries []board.Execution
					f.request("GET", "/executions?namespace=protocol-test", nil, false, http.StatusOK, &entries)
					if len(entries) != 1 || entries[0].Status != "result_pending" {
						t.Fatalf("rejected apply changed execution receipt: %+v", entries)
					}
				})
			}
		}
	}
}

func TestCompatibilityDecisionCanStillSubmitSteps(t *testing.T) {
	compat := false
	for name, flag := range map[string]*bool{"omitted": nil, "false": &compat} {
		t.Run(name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			f.register("reason", flag, 0)
			f.pending(`{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"Observe the synthetic fixture"}]}}`)
			f.apply(http.StatusOK)
			f.apply(http.StatusOK)
			if steps := f.state().Steps; len(steps) != 1 || steps[0].Description != "Observe the synthetic fixture" {
				t.Fatalf("compatibility plan was lost or applied twice: %+v", steps)
			}
		})
	}
}

func TestLiveDecisionRetainsNonCreatingResults(t *testing.T) {
	for name, output := range map[string]string{
		"decided":  `{"accepted":true,"data":{"decided":true}}`,
		"noop":     `{"accepted":true,"data":{}}`,
		"rejected": `{"accepted":false,"reason":"Cannot establish a useful next action"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.register("reason", &live, 1)
			f.action("step", "one-direction", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Observe the synthetic fixture"})
			f.pending(output)
			body := f.apply(http.StatusOK)
			f.apply(http.StatusOK)
			if name == "rejected" && !strings.Contains(body, `"rejected"`) {
				t.Fatalf("declined decision was not retained: %s", body)
			}
			if state := f.state(); len(state.Steps) != 1 || state.Graph.Project.Status != "active" {
				t.Fatal("final non-creating result changed existing work")
			}
		})
	}
}

func TestLiveDecisionCanAchieveGoalThenComplete(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	observed := f.newIntent()
	var conclusion board.Conclusion
	f.request("POST", f.base()+"/intents/"+observed.ID+"/conclude", map[string]string{"worker": "fixture", "description": "Synthetic fixture was verified"}, false, http.StatusOK, &conclusion)
	live := true
	f.register("reason", &live, 1)
	goal := f.action("goal", "child-goal", map[string]any{"action": "add", "parent_id": "goal", "condition": "Verify the synthetic fixture"})
	f.action("goal", "achieve-child", map[string]any{"action": "achieve", "id": goal.ID, "sources": []string{conclusion.Fact.ID}, "reason": "Confirmed by the fixture observation"})
	text, err := json.Marshal(map[string]any{"accepted": true, "data": map[string]any{"complete": map[string]any{"from": []string{conclusion.Fact.ID}, "description": "Synthetic verification complete"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.pending(string(text))
	f.apply(http.StatusOK)
	f.apply(http.StatusOK)
	state := f.state()
	if state.Graph.Project.Status != "completed" || len(state.Graph.Intents) != 2 {
		t.Fatalf("goal actions blocked completion or replay duplicated it: %+v", state.Graph)
	}
}

func TestNonCompletedWorkerOutcomeCannotBeAppliedAsSuccess(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		for _, outcome := range []string{"continue", "incomplete"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				live := true
				f.register(kind, &live, 1)
				f.pending(`{"accepted":true,"outcome":"` + outcome + `","reason":"Required fixture reads remain"}`)
				f.apply(http.StatusUnprocessableEntity)
				state := f.state()
				if len(state.Graph.Facts) != 2 || len(state.Steps) != 1 || state.Steps[0].Result != nil || state.Steps[0].Status != "running" || state.Graph.Project.Status != "active" {
					t.Fatal("non-completed outcome produced a fact or completed the task")
				}
			})
		}
	}
}

func TestIncompleteExecutionFailureNeverCreatesFact(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "worker_failed"
		if pending {
			name = "rejected_pending_result"
		}
		t.Run(name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.register("explore", &live, 1)
			output := `{"accepted":true,"outcome":"incomplete","reason":"Required fixture reads remain"}`
			if pending {
				f.pending(output)
				f.apply(http.StatusUnprocessableEntity)
			}
			failure := map[string]any{"status": "failed", "failure_kind": "incomplete", "error": "Required fixture reads remain", "text": output}
			f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "failed", "result": failure}, true, http.StatusOK, nil)
			f.apply(http.StatusConflict)
			state := f.state()
			if len(state.Graph.Facts) != 2 || len(state.Steps) != 1 || state.Steps[0].Status != "failed" || state.Steps[0].Result != nil || state.Graph.Project.Status != "active" {
				t.Fatal("incomplete failure created a fact or ended the project")
			}
			if !strings.Contains(state.Steps[0].Reason, "Required fixture reads remain") {
				t.Fatalf("incomplete diagnostic was lost: %q", state.Steps[0].Reason)
			}
		})
	}
}

func TestExecutionRegistrationRejectsInvalidProtocolFields(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		value       any
	}{
		{"null_graph_mode", "graph_rpc", nil},
		{"string_graph_mode", "graph_rpc", "true"},
		{"numeric_graph_mode", "graph_rpc", 1},
		{"null_version", "result_contract_version", nil},
		{"string_version", "result_contract_version", "1"},
		{"boolean_version", "result_contract_version", true},
		{"negative_version", "result_contract_version", -1},
		{"fractional_version", "result_contract_version", 1.5},
		{"unsupported_version", "result_contract_version", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.registerWithFields("reason", &live, 1, map[string]any{tt.field: tt.value}, http.StatusUnprocessableEntity)
			var executions []board.Execution
			f.request("GET", "/executions?namespace=protocol-test", nil, false, http.StatusOK, &executions)
			if len(executions) != 0 {
				t.Fatal("invalid protocol was saved as a resumable execution")
			}
		})
	}
}

func TestLegacyExecutionCannotIgnoreExplicitNonCompletion(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		for _, outcome := range []string{"continue", "incomplete"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				f.register(kind, nil, 0)
				data := map[string]any{"description": "Only 19 of 30 chunks verified"}
				if kind == "bootstrap" {
					data = map[string]any{"fact": data, "complete": map[string]string{"description": "Process exited"}}
				}
				raw, err := json.Marshal(map[string]any{"accepted": true, "outcome": outcome, "data": data})
				if err != nil {
					t.Fatal(err)
				}
				before := f.state()
				f.pending(string(raw))
				f.apply(http.StatusUnprocessableEntity)
				after := f.state()
				if after.Revision != before.Revision || len(after.Graph.Facts) != len(before.Graph.Facts) || after.Steps[0].Status != "running" || after.Graph.Project.Status != "active" {
					t.Fatal("legacy parser ignored explicit non-completion and ended the task")
				}
			})
		}
	}
}

func TestRegisteredExecutionContractCannotBeDowngraded(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		for _, live := range []bool{false, true} {
			name := kind + "/compatibility_graph"
			if live {
				name = kind + "/live_graph"
			}
			t.Run(name, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				f.register(kind, &live, 1)
				output := `{"accepted":true,"data":{"description":"19 of 30 chunks checked"}}`
				if kind == "bootstrap" {
					output = `{"accepted":true,"data":{"fact":{"description":"19 of 30 chunks checked"},"complete":{"description":"Worker exited"}}}`
				}
				before := f.state()
				f.pending(output)
				// apply() deliberately asks to use the old result protocol. Only
				// the registered job is authoritative, regardless of graph mode.
				f.apply(http.StatusUnprocessableEntity)
				after := f.state()
				if after.Revision != before.Revision || len(after.Graph.Facts) != len(before.Graph.Facts) || after.Steps[0].Status != "running" || after.Graph.Project.Status != "active" {
					t.Fatal("unversioned partial output was applied as completion")
				}
			})
		}
	}
}

func TestCompletedExecutionAppliesOnceAndKeepsProjectBoundary(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		for _, conclude := range []bool{false, true} {
			name := kind + "/execution"
			if conclude {
				name = kind + "/conclusion"
			}
			t.Run(name, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				live := true
				f.register(kind, &live, 1)
				output := `{"accepted":true,"outcome":"completed","data":{"description":"All 30 chunks verified"}}`
				if kind == "bootstrap" {
					output = `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"All 30 chunks verified"},"complete":{"description":"All required chunks match the fixture"}}}`
				}
				result := map[string]any{"status": "success", "text": output, "conclude": conclude}
				f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "result_pending", "result": result}, true, http.StatusOK, nil)
				f.apply(http.StatusOK)
				first := f.state()
				f.apply(http.StatusOK)
				after := f.state()
				if len(after.Graph.Facts) != 3 || after.Steps[0].Status != "completed" || after.Revision != first.Revision || len(after.Graph.Intents) != len(first.Graph.Intents) {
					t.Fatalf("completed result was lost or applied twice: %+v", after)
				}
				wantStatus := "active"
				if kind == "bootstrap" && !conclude {
					wantStatus = "completed"
				}
				if after.Graph.Project.Status != wantStatus {
					t.Fatalf("project status=%q, want %q", after.Graph.Project.Status, wantStatus)
				}
			})
		}
	}
}
