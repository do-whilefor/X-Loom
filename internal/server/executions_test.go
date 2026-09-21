package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	b "xloom/internal/board"
)

func executionHeaders(e b.Execution) []string {
	return []string{"X-Xloom-Run", e.Lease, "X-Xloom-Lease", e.Kind, "X-Xloom-Intent", e.Intent}
}
func makeExecution(t *testing.T, h http.Handler, kind, id, previous string) b.Execution {
	t.Helper()
	lease := "backend@" + id
	iid := ""
	if kind == "reason" {
		call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"`+lease+`","trigger":"test"}`, 200)
	} else {
		iid = "i001"
		call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"`+lease+`"}`, 200)
	}
	d := call(t, h, "GET", "/projects/proj_001", "", 200)
	graph, _ := json.Marshal(d)
	var g b.Graph
	json.Unmarshal(graph, &g)
	job := map[string]any{"run_id": id, "kind": kind, "graph": g, "workspace": "/workspace", "budget": map[string]int{"max_intents": 3}, "previous_run_id": previous}
	if kind != "reason" {
		job["intent"] = g.Intents[0]
	}
	raw, _ := json.Marshal(job)
	key := kind + ":" + iid
	if kind == "reason" {
		key += "evidence-boundary"
	}
	return b.Execution{ProjectID: "proj_001", ID: id, Namespace: "test", Backend: "backend", Kind: kind, Intent: iid, Lease: lease, Job: raw, RetryKey: key}
}
func registerExecutionCall(t *testing.T, h http.Handler, e b.Execution, code int) b.Execution {
	t.Helper()
	raw, _ := json.Marshal(e)
	response := call(t, h, "POST", "/projects/proj_001/executions", string(raw), code, executionHeaders(e)...)
	raw, _ = json.Marshal(response)
	var saved b.Execution
	json.Unmarshal(raw, &saved)
	return saved
}
func executionOp(t *testing.T, h http.Handler, e b.Execution, op, body string, code int) map[string]json.RawMessage {
	t.Helper()
	return call(t, h, "POST", "/projects/proj_001/executions/"+e.ID+"/"+op, body, code, executionHeaders(e)...)
}
func pendingResult(t *testing.T, h http.Handler, e b.Execution, text string, conclude bool) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"status": "result_pending", "result": map[string]any{"status": "success", "text": text, "conclude": conclude}})
	executionOp(t, h, e, "status", string(raw), 200)
}
func executionFixture(t *testing.T, kind string) (http.Handler, b.Execution) {
	t.Helper()
	_, h := fixture(t)
	create(t, h)
	if kind != "reason" {
		call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Execute one step","creator":"human"}`, 201)
	}
	e := makeExecution(t, h, kind, "first", "")
	registerExecutionCall(t, h, e, 201)
	return h, e
}

func TestExecutionRegistrationIsImmutableAndScoped(t *testing.T) {
	h, e := executionFixture(t, "explore")
	if saved := registerExecutionCall(t, h, e, 201); saved.Status != "prepared" || saved.Resumes != 0 {
		t.Fatalf("registration: %+v", saved)
	}
	changed := e
	changed.Namespace = "other"
	registerExecutionCall(t, h, changed, 409)
	changed = e
	changed.Job = json.RawMessage(`{"run_id":"first","kind":"explore","workspace":"/workspace","graph":{"project":{"id":"another"}},"intent":{"id":"i001"}}`)
	registerExecutionCall(t, h, changed, 422)
	changed = e
	changed.RetryKey = "different-key-to-bypass-failure"
	registerExecutionCall(t, h, changed, 422)
	raw, _ := json.Marshal(e)
	call(t, h, "POST", "/projects/proj_001/executions", string(raw), 403)
}

func TestPendingExecutionResultCannotBeOverwrittenOrReplayed(t *testing.T) {
	h, e := executionFixture(t, "explore")
	pendingResult(t, h, e, `{"description":"confirmed once"}`, false)
	executionOp(t, h, e, "status", `{"status":"running"}`, 409)
	executionOp(t, h, e, "status", `{"status":"result_pending","result":{"status":"success","text":"different"}}`, 409)
	executionOp(t, h, e, "apply", `{}`, 200)
	executionOp(t, h, e, "apply", `{}`, 200)
	s := readState(t, h)
	if len(s.Graph.Facts) != 3 || len(s.Graph.Intents) != 1 || b.Value(s.Graph.Intents[0].To) != "f001" {
		t.Fatal("lost apply response duplicated a business mutation")
	}
	executionOp(t, h, e, "status", `{"status":"running"}`, 409)
}

func TestBootstrapAtomicApplyCompletesAfterConcludingItsLease(t *testing.T) {
	h, e := executionFixture(t, "bootstrap")
	pendingResult(t, h, e, `{"fact":{"description":"verified"},"complete":{"description":"goal proven"}}`, false)
	executionOp(t, h, e, "apply", `{}`, 200)
	executionOp(t, h, e, "apply", `{}`, 200)
	s := readState(t, h)
	if s.Graph.Project.Status != "completed" || len(s.Graph.Facts) != 3 || len(s.Graph.Intents) != 2 {
		t.Fatalf("bootstrap apply: %+v", s.Graph)
	}
}

func TestExecutionResumeIsBoundedAndCannotStealOrReviveRevokedRun(t *testing.T) {
	for _, scenario := range []string{"allowance", "other owner", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			h, e := executionFixture(t, "explore")
			call(t, h, "POST", "/projects/proj_001/intents/i001/release", `{"worker":"`+e.Lease+`"}`, 200)
			switch scenario {
			case "allowance":
				executionOp(t, h, e, "resume", `{}`, 200)
				r := executionOp(t, h, e, "resume", `{}`, 200)
				if string(r["resumes"]) != "2" {
					t.Fatal("recovery allowance not durable")
				}
				executionOp(t, h, e, "resume", `{}`, 409)
			case "other owner":
				call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"other"}`, 200)
				executionOp(t, h, e, "resume", `{}`, 409)
			case "stopped":
				// Reclaim before stopping, so the original identity is revoked.
				call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"`+e.Lease+`"}`, 200)
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
				executionOp(t, h, e, "resume", `{}`, 409)
			}
		})
	}
}

func TestExplicitRetryIsConsumedOnceAndLinkedToPreviousAttempt(t *testing.T) {
	h, old := executionFixture(t, "explore")
	executionOp(t, h, old, "status", `{"status":"failed","result":{"status":"failed","error":"terminal failure"}}`, 200)
	call(t, h, "POST", "/projects/proj_001/intents/i001/release", `{"worker":"`+old.Lease+`"}`, 200)
	second := makeExecution(t, h, "explore", "second", "")
	registerExecutionCall(t, h, second, 409)
	call(t, h, "POST", "/projects/proj_001/executions/first/retry", `{}`, 200)
	registerExecutionCall(t, h, second, 409) // A grant must be explicitly linked.
	var job map[string]any
	json.Unmarshal(second.Job, &job)
	job["previous_run_id"] = "first"
	second.Job, _ = json.Marshal(job)
	registerExecutionCall(t, h, second, 201)
	registerExecutionCall(t, h, second, 201) // A lost registration reply is safe.
	call(t, h, "POST", "/projects/proj_001/intents/i001/release", `{"worker":"`+second.Lease+`"}`, 200)
	third := makeExecution(t, h, "explore", "third", "first")
	registerExecutionCall(t, h, third, 409)
	call(t, h, "POST", "/projects/proj_001/executions/first/retry", `{}`, 409)
	executionOp(t, h, second, "retry", `{}`, 403)
}

func TestReasonApplyRejectsAllInvalidDirectionsAndRequiresOwnDecisionReceipt(t *testing.T) {
	for _, mode := range []string{"invalid", "mixed", "decided without actions", "decided with actions"} {
		t.Run(mode, func(t *testing.T) {
			h, e := executionFixture(t, "reason")
			text := `{"intents":[{"from":["missing"],"description":"unsupported"}]}`
			want, count := 422, 0
			if mode == "mixed" {
				text = `{"intents":[{"from":["origin","origin"],"description":"bad duplicate"},{"from":["origin"],"description":"valid"}]}`
				want, count = 200, 1
			}
			if mode == "decided without actions" || mode == "decided with actions" {
				text, want = `{"accepted":true,"data":{"decided":true}}`, 409
				if mode == "decided with actions" {
					body := `{"op":"step","idempotency_key":"plan","payload":{"action":"add","from":["origin"],"description":"the graph tool already created this step"}}`
					call(t, h, "POST", "/projects/proj_001/state/actions", body, 200, executionHeaders(e)...)
					want, count = 200, 1
				}
			}
			pendingResult(t, h, e, text, false)
			executionOp(t, h, e, "apply", `{}`, want)
			if s := readState(t, h); len(s.Graph.Intents) != count {
				t.Fatal("invalid plan leaked or valid plan missing", s.Graph.Intents)
			}
		})
	}
}

func TestStoppedPendingExecutionCannotCommitEvenAfterProjectReactivation(t *testing.T) {
	h, e := executionFixture(t, "explore")
	pendingResult(t, h, e, `{"description":"late"}`, false)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
	executionOp(t, h, e, "apply", `{}`, 409)
	executionOp(t, h, e, "status", `{"status":"cancelled","result":{"status":"failed","error":"hard cancelled"}}`, 409)
	s := readState(t, h)
	if len(s.Graph.Facts) != 2 || s.Steps[0].Status != "open" {
		t.Fatal("revoked pending result became a fact")
	}
	var executions []b.Execution
	getUIJSON(t, h, "/executions?namespace=test", &executions)
	if len(executions) != 1 || executions[0].Status != "retry_requested" || string(executions[0].Result) != `{"conclude":false,"status":"success","text":"{\"description\":\"late\"}"}` {
		t.Fatalf("continue lost the immutable pending result or retry grant: %+v", executions)
	}
}

func TestExecutionExpiredLeaseMustBeReclaimedBeforeApply(t *testing.T) {
	store, h := fixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"one","creator":"human"}`, 201)
	e := makeExecution(t, h, "explore", "first", "")
	registerExecutionCall(t, h, e, 201)
	pendingResult(t, h, e, `{"description":"durable observation"}`, false)
	now = now.Add(time.Minute)
	executionOp(t, h, e, "apply", `{}`, 409)
	executionOp(t, h, e, "resume", `{}`, 200)
	executionOp(t, h, e, "apply", `{}`, 200)
}

func TestStopRevokesRegisteredRunEvenWhenLeaseAlreadyExpired(t *testing.T) {
	store, h := fixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"one","creator":"human"}`, 201)
	e := makeExecution(t, h, "explore", "first", "")
	registerExecutionCall(t, h, e, 201)
	now = now.Add(time.Minute)
	// Expire runs at the beginning of this request, before stop revokes them.
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
	executionOp(t, h, e, "resume", `{}`, 409)
}

func TestGraphAndFinalDirectionsShareRegisteredDecisionLimit(t *testing.T) {
	h, e := executionFixture(t, "reason")
	for _, key := range []string{"one", "two", "three"} {
		body := `{"op":"step","idempotency_key":"` + key + `","payload":{"action":"add","from":["origin"],"description":"independent direction"}}`
		call(t, h, "POST", "/projects/proj_001/state/actions", body, 200, executionHeaders(e)...)
	}
	call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"step","idempotency_key":"abandon","payload":{"action":"abandon","id":"i001","reason":"Does not buy another planning slot"}}`, 200, executionHeaders(e)...)
	call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"step","idempotency_key":"four","payload":{"action":"add","from":["origin"],"description":"exceeds total direction budget"}}`, 409, executionHeaders(e)...)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"cannot bypass through legacy API","creator":"backend@first"}`, 409, executionHeaders(e)...)
	pendingResult(t, h, e, `{"accepted":true,"data":{"decided":true}}`, false)
	executionOp(t, h, e, "apply", `{}`, 200)
	s := readState(t, h)
	if len(s.Graph.Intents) != 3 || s.DecisionRevision != 0 {
		t.Fatal("planning quota changed or planning self-triggered a new decision")
	}
}

func TestExecuteFailureBecomesVisibleStateAndOneDecisionTrigger(t *testing.T) {
	h, e := executionFixture(t, "explore")
	executionOp(t, h, e, "status", `{"status":"failed","result":{"status":"failed","failure_kind":"invalid_output","error":"repair allowance exhausted"}}`, 200)
	s := readState(t, h)
	if len(s.Graph.Facts) != 2 || len(s.Steps) != 1 || s.Steps[0].Status != "failed" || s.Steps[0].Reason != "invalid_output: repair allowance exhausted" || s.DecisionRevision != 1 {
		t.Fatalf("failure did not reach shared state without a fake fact: %+v", s)
	}
	// A still-present lease cannot authorize late tool writes after terminal
	// registration; the old run is fenced before the dispatcher releases it.
	call(t, h, "POST", "/projects/proj_001/state/actions", `{"op":"fact","idempotency_key":"late-after-failure","payload":`+observedFact+`}`, 409, executionHeaders(e)...)
	// Rejected duplicate status writes never manufacture additional triggers.
	executionOp(t, h, e, "status", `{"status":"failed","result":{"status":"failed","failure_kind":"invalid_output","error":"repair allowance exhausted"}}`, 409)
	if readState(t, h).DecisionRevision != 1 {
		t.Fatal("duplicate failure retriggered a decision")
	}
	call(t, h, "POST", "/projects/proj_001/intents/i001/release", `{"worker":"`+e.Lease+`"}`, 200)
	call(t, h, "POST", "/projects/proj_001/executions/first/retry", `{}`, 200)
	second := makeExecution(t, h, "explore", "second", "first")
	registerExecutionCall(t, h, second, 201)
	if s := readState(t, h); s.Steps[0].Status != "running" || s.DecisionRevision != 1 {
		t.Fatal("new registered attempt still displays the prior failure")
	}
}

func TestContinueAllowsExplicitNewAttemptButNeverOldResume(t *testing.T) {
	h, e := executionFixture(t, "explore")
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	executionOp(t, h, e, "status", `{"status":"cancelled","result":{"status":"failed","error":"project stopped"}}`, 200)
	call(t, h, "POST", "/projects/proj_001/executions/first/retry", `{}`, 403)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
	// Continue already granted exactly one retry for this paused execution.
	call(t, h, "POST", "/projects/proj_001/executions/first/retry", `{}`, 409)
	executionOp(t, h, e, "resume", `{}`, 409)
	next := makeExecution(t, h, "explore", "explicit-new-run", "first")
	registerExecutionCall(t, h, next, 201)
}

func TestAbandonAcknowledgementDoesNotRetriggerItsOwnDecision(t *testing.T) {
	h, e := executionFixture(t, "explore")
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"planner@decision","trigger":"change plan"}`, 200)
	stateActionCall(t, h, true, "step", "abandon", `{"action":"abandon","id":"i001","reason":"This direction is no longer needed"}`, 200)
	executionOp(t, h, e, "status", `{"status":"cancelled","result":{"status":"failed","failure_kind":"hard_cancelled","error":"lease revoked by planner"}}`, 200)
	s := readState(t, h)
	if s.Steps[0].Status != "abandoned" || s.DecisionRevision != 0 || s.Revision != 2 {
		t.Fatal("acknowledging the decision's own abandonment created a new decision trigger", s)
	}
}

func TestReactivatedClaimsPreserveLegacyNamesButRejectRegisteredRuns(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		for _, registered := range []bool{false, true} {
			name := kind + "/legacy"
			if registered {
				name = kind + "/registered"
			}
			t.Run(name, func(t *testing.T) {
				_, h := fixture(t)
				create(t, h)
				if kind == "explore" {
					call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"one","creator":"human"}`, 201)
				}
				e := makeExecution(t, h, kind, "original", "")
				if registered {
					registerExecutionCall(t, h, e, 201)
				}
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
				path := "/projects/proj_001/intents/i001/heartbeat"
				body := `{"worker":"` + e.Lease + `"}`
				if kind == "reason" {
					path = "/projects/proj_001/reason/claim"
					body = `{"worker":"` + e.Lease + `","trigger":"reactivation"}`
				}
				// Explicit execution isolation remains strict even for a name that
				// was never registered, matching the earlier fenced-write contract.
				call(t, h, "POST", path, body, 409, executionHeaders(e)...)
				want := 200
				if registered {
					want = 409
				}
				call(t, h, "POST", path, body, want)
				if !registered && kind == "reason" {
					call(t, h, "POST", "/projects/proj_001/reason/heartbeat", `{"worker":"`+e.Lease+`"}`, 200)
				}
			})
		}
	}
}
