package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func executionQueryStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "queries.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func executionQueryTx(t *testing.T, store *Store, fn func(*Tx)) {
	t.Helper()
	if err := store.Do(context.Background(), func(tx *Tx) error { fn(tx); return nil }); err != nil {
		t.Fatal(err)
	}
}

func putQueryExecution(t *testing.T, tx *Tx, e Execution, generation int64, version string) {
	t.Helper()
	if e.ProjectID == "" {
		e.ProjectID = "p"
	}
	if e.Namespace == "" {
		e.Namespace = "ns"
	}
	if e.Kind == "" {
		e.Kind = "reason"
	}
	if e.RetryKey == "" {
		e.RetryKey = "reason:key"
	}
	if e.CreatedAt == "" {
		e.CreatedAt = "2026-09-23T00:00:00Z"
	}
	if e.Job == nil {
		e.Job = json.RawMessage(`{"graph":{}}`)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO projects(id,title,status,created_at) VALUES(?,'fixture','active',?)`, e.ProjectID, tx.Now); err != nil {
		t.Fatal(err)
	}
	_, err := tx.Exec(`INSERT INTO xloom_executions(`+executionColumns+`,`+executionMetadataColumns+`,metadata_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, e.ProjectID, e.ID, e.Namespace, "planner", e.Kind, e.Intent, "planner@"+e.ID, []byte(e.Job), e.RetryKey, e.Status, []byte(e.Result), e.Resumes, e.CreatedAt, e.CreatedAt, generation, 10, 9, version, version != "", 3, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutionPendingKeysetExcludesHistoryAndKeepsCleanup(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "completed", Status: "succeeded", Job: json.RawMessage(`{"payload":"` + strings.Repeat("x", 2<<20) + `"}`)}, 0, "old")
		putQueryExecution(t, tx, Execution{ID: "foreign", Namespace: "elsewhere", Status: "running"}, 0, "")
		statuses := []string{"prepared", "running", "retryable", "result_pending"}
		for n := 0; n < 105; n++ {
			putQueryExecution(t, tx, Execution{ID: fmt.Sprintf("run-%03d", n), Status: statuses[n%len(statuses)], CreatedAt: fmt.Sprintf("%03d", 105-n)}, int64(n%3), "")
		}
		if _, err := tx.Exec("UPDATE projects SET status='completed' WHERE id='p'"); err != nil {
			t.Fatal(err)
		}
		page, err := tx.PendingExecutions("ns", 0, 100)
		if err != nil || len(page.Items) != 100 || page.NextCursor == 0 {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		for n, e := range page.Items {
			if e.ID != fmt.Sprintf("run-%03d", n) || !e.Pending() {
				t.Fatalf("keyset order lost: %+v", e)
			}
		}
		// A transition between pages must not shift an offset and skip a run.
		if _, err := tx.Exec("UPDATE xloom_executions SET status='cancelled' WHERE id='run-000'"); err != nil {
			t.Fatal(err)
		}
		next, err := tx.PendingExecutions("ns", page.NextCursor, 100)
		if err != nil || len(next.Items) != 5 || next.Items[0].ID != "run-100" || next.NextCursor != 0 {
			t.Fatalf("next=%+v err=%v", next, err)
		}
		if _, err := tx.PendingExecutions("ns", 0, 101); err == nil {
			t.Fatal("oversized page accepted")
		}
	})
}

func TestExecuteCheckOmitsDecisionBoundary(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "decided", Status: "succeeded"}, 0, "")
		for _, kind := range []string{"bootstrap", "explore", "reason"} {
			check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "ns", Kind: kind, Intent: "step", RetryKey: kind + ":step"})
			if err != nil || (check.LatestDecision != nil) != (kind == "reason") {
				t.Fatalf("%s decision boundary=%+v, error=%v", kind, check.LatestDecision, err)
			}
		}
	})
}

func TestScheduleExecutionChecksMatchIndividualAdmission(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		intents := []Intent{{ID: "new"}, {ID: "pending"}, {ID: "failed"}, {ID: "granted"}, {ID: "foreign"}, {ID: "old-round"}, {ID: "boot", Description: "bootstrap", Creator: "dispatcher.bootstrap", From: []string{"origin"}}}
		for _, e := range []Execution{
			{ID: "p", Kind: "explore", Intent: "pending", RetryKey: "another-key", Status: "result_pending"},
			{ID: "f", Kind: "explore", Intent: "failed", RetryKey: "explore:failed", Status: "failed"},
			{ID: "g0", Kind: "explore", Intent: "granted", RetryKey: "explore:granted", Status: "failed"},
			{ID: "g1", Kind: "explore", Intent: "granted", RetryKey: "other-key", Status: "retry_requested"},
			{ID: "g2", Kind: "explore", Intent: "granted", RetryKey: "third-key", Status: "retry_requested"},
			{ID: "other", Namespace: "elsewhere", Kind: "explore", Intent: "foreign", RetryKey: "explore:foreign", Status: "running"},
			{ID: "old", Kind: "explore", Intent: "old-round", RetryKey: "explore:old-round", Status: "running"},
			{ID: "b", Kind: "bootstrap", Intent: "boot", RetryKey: "bootstrap:boot", Status: "failed"},
		} {
			putQueryExecution(t, tx, e, 0, "")
		}
		checks, err := tx.ScheduleExecutionChecks("p", "ns", intents)
		if err != nil || len(checks) != len(intents) {
			t.Fatalf("checks=%+v err=%v", checks, err)
		}
		for _, i := range intents {
			kind := "explore"
			if i.ID == "boot" {
				kind = "bootstrap"
			}
			key := kind + ":" + i.ID
			want, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "ns", Generation: 1, Kind: kind, Intent: i.ID, RetryKey: key})
			got := checks[key]
			if err != nil || got.Pending != want.Pending || got.Blocked != want.Blocked || got.PreviousRunID != want.PreviousRunID {
				t.Fatalf("%s batch=%+v individual=%+v err=%v", key, got, want, err)
			}
		}
		if checks["explore:granted"].PreviousRunID != "g2" {
			t.Fatal("lost retry-grant tie ordering")
		}
		checks, err = tx.ScheduleExecutionChecks("p", "ns", nil)
		if err != nil || len(checks) != 0 {
			t.Fatalf("empty page=%+v err=%v", checks, err)
		}
	})
}

func TestExecutionCheckPreservesGrantsPendingAndAttemptAllowance(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		q := ExecutionCheckQuery{ProjectID: "p", Namespace: "ns", Generation: 2, Kind: "reason", RetryKey: "reason:key", StateVersion: "same"}
		failure := json.RawMessage(`{"status":"failed","failure_kind":"request_timeout"}`)
		putQueryExecution(t, tx, Execution{ID: "failed", Status: "failed", Result: failure}, 2, "same")
		putQueryExecution(t, tx, Execution{ID: "old-generation", Status: "failed", Result: failure}, 1, "old")
		putQueryExecution(t, tx, Execution{ID: "foreign", Namespace: "other", Status: "failed", Result: failure}, 2, "same")
		check, err := tx.CheckExecutions(q)
		if err != nil || !check.Blocked || !check.Repeated || check.Attempts != 1 || check.AutomaticRetryID != "failed" {
			t.Fatalf("initial check=%+v err=%v", check, err)
		}
		if _, err := tx.Exec("UPDATE xloom_executions SET status='running' WHERE id='old-generation'"); err != nil {
			t.Fatal(err)
		}
		check, err = tx.CheckExecutions(q)
		if err != nil || !check.Pending || check.AutomaticRetryID != "" {
			t.Fatalf("stale pending work authorized an automatic successor: %+v err=%v", check, err)
		}
		if _, err := tx.Exec("UPDATE xloom_executions SET status='failed' WHERE id='old-generation'"); err != nil {
			t.Fatal(err)
		}
		putQueryExecution(t, tx, Execution{ID: "grant", Status: "retry_requested", RetryKey: "different-input"}, 2, "")
		check, err = tx.CheckExecutions(q)
		if err != nil || check.Blocked || check.Pending || check.PreviousRunID != "grant" || check.AutomaticRetryID != "" {
			t.Fatalf("cross-key grant lost: %+v err=%v", check, err)
		}
		putQueryExecution(t, tx, Execution{ID: "pending", Status: "running", RetryKey: "another-input"}, 2, "")
		check, err = tx.CheckExecutions(q)
		if err != nil || !check.Pending || !check.Blocked || check.AutomaticRetryID != "" {
			t.Fatalf("cross-key pending lost: %+v err=%v", check, err)
		}
		if _, err = tx.Exec("UPDATE xloom_executions SET status='retried' WHERE id IN ('grant','pending')"); err != nil {
			t.Fatal(err)
		}
		putQueryExecution(t, tx, Execution{ID: "human-successor", Status: "retried"}, 2, "same")
		check, err = tx.CheckExecutions(q)
		if err != nil || check.Attempts != 2 || check.AutomaticRetryID != "" {
			t.Fatalf("all-status allowance lost: %+v err=%v", check, err)
		}
		putQueryExecution(t, tx, Execution{ID: "step-a", Kind: "explore", Intent: "a", Status: "running", RetryKey: "explore:a"}, 2, "")
		q.Kind, q.Intent, q.RetryKey = "explore", "b", "explore:b"
		check, err = tx.CheckExecutions(q)
		if err != nil || check.Pending || check.Blocked {
			t.Fatalf("unrelated step blocked: %+v err=%v", check, err)
		}
		q.Intent = "a"
		check, err = tx.CheckExecutions(q)
		if err != nil || !check.Pending || !check.Blocked {
			t.Fatalf("same step different key did not block: %+v err=%v", check, err)
		}
	})
}

func TestExecutionCheckLatestBaselineAndRepeatedUseMetadata(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "initial", Status: "succeeded"}, 4, "initial-state")
		putQueryExecution(t, tx, Execution{ID: "newest-legacy", Status: "succeeded"}, 4, "")
		putQueryExecution(t, tx, Execution{ID: "later-insertion-earlier-time", Status: "succeeded", CreatedAt: "2000"}, 4, "earlier-state")
		putQueryExecution(t, tx, Execution{ID: "other-generation", Status: "succeeded", CreatedAt: "9999"}, 5, "other-state")
		putQueryExecution(t, tx, Execution{ID: "failed-nonlatest", Status: "cancelled", RetryKey: "different"}, 3, "repeated-state")
		// After metadata capture, compact reads must not depend on any Job or
		// historical Result being available or decodable.
		if _, err := tx.Exec("UPDATE xloom_executions SET job='unavailable',result='unavailable'"); err != nil {
			t.Fatal(err)
		}
		check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "ns", Generation: 4, Kind: "reason", RetryKey: "new-key", StateVersion: "repeated-state"})
		if err != nil || !check.Repeated || check.LatestDecision == nil || check.LatestDecision.ID != "newest-legacy" || check.LatestDecision.HasState || check.LatestDecision.FactCount != 3 {
			t.Fatalf("metadata check=%+v err=%v", check, err)
		}
		identity, err := tx.GetExecutionSummary("ns", "p", "newest-legacy")
		if err != nil || identity.ID != "newest-legacy" {
			t.Fatalf("identity=%+v err=%v", identity, err)
		}
		if _, err = tx.GetExecutionSummary("other", "p", "newest-legacy"); err == nil {
			t.Fatal("identity crossed namespace")
		}
	})
}

func TestExecutionCheckAutomaticClassificationMatchesAuthority(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		cases := []Execution{
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"recovery_exhausted"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"budget_exhausted"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"request_timeout"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"transient_infrastructure"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"transport"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"rate_limit"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"unavailable"}`)},
			{Status: "cancelled", Result: json.RawMessage(`{"status":"failed","failure_kind":"transport"}`)},
			{Status: "rejected", Result: json.RawMessage(`{"status":"failed","failure_kind":"transport"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"success","failure_kind":"transport"}`)},
			{Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"configuration"}`)},
			{Status: "failed", Result: json.RawMessage(`malformed historical result`)},
		}
		for n, e := range cases {
			e.ID, e.ProjectID, e.Kind = fmt.Sprintf("case-%d", n), fmt.Sprintf("project-%d", n), "reason"
			putQueryExecution(t, tx, e, 0, "")
			check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: e.ProjectID, Namespace: "ns", Kind: "reason", RetryKey: "reason:key"})
			if err != nil || (check.AutomaticRetryID != "") != AutomaticDecisionRetryEligible(e) {
				t.Fatalf("case %d: classification=%+v err=%v", n, check, err)
			}
		}
	})
}

func TestAutomaticDecisionRetryUsesMetadataAndScopesAttempts(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		failure := json.RawMessage(`{"status":"failed","failure_kind":"transport","text":"retained evidence"}`)
		key := "explore:i1"
		putQueryExecution(t, tx, Execution{ID: "failed", RetryKey: key, Status: "failed", Result: failure}, 0, "same")
		putQueryExecution(t, tx, Execution{ID: "step", Kind: "explore", Intent: "i1", RetryKey: key, Status: "failed"}, 0, "")
		putQueryExecution(t, tx, Execution{ID: "foreign", Namespace: "other", RetryKey: key, Status: "failed", Result: failure}, 0, "same")
		// Restoring a previous Job is unnecessary when authorizing a new run.
		if _, err := tx.Exec("UPDATE xloom_executions SET job='unavailable' WHERE id='failed'"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE projects SET reason_worker='planner@failed',reason_trigger='test' WHERE id='p'"); err != nil {
			t.Fatal(err)
		}
		check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "ns", Kind: "reason", RetryKey: key})
		if err != nil || check.Attempts != 1 || check.AutomaticRetryID != "failed" {
			t.Fatalf("unrelated task consumed retry allowance: %+v err=%v", check, err)
		}
		for range 2 {
			if err := tx.RequestAutomaticDecisionRetry(Execution{ProjectID: "p", ID: "failed"}); err != nil {
				t.Fatalf("metadata retry grant: %v", err)
			}
		}
		current, err := tx.Execution("p", "failed")
		if err != nil || current.Status != "retry_requested" || string(current.Job) != "unavailable" || string(current.Result) != string(failure) {
			t.Fatalf("grant changed original input/result: %+v err=%v", current, err)
		}
		revoked, err := tx.RunRevoked("p", "planner@failed")
		if err != nil || !revoked {
			t.Fatalf("grant did not revoke original lease: %v", err)
		}
		var reason *string
		if err := tx.QueryRow("SELECT reason_worker FROM projects WHERE id='p'").Scan(&reason); err != nil || reason != nil {
			t.Fatalf("grant did not release original lease: %v", err)
		}
		putQueryExecution(t, tx, Execution{ID: "successor", RetryKey: key, Status: "failed", Result: failure}, 0, "same")
		if err := tx.RequestAutomaticDecisionRetry(Execution{ProjectID: "p", ID: "failed"}); err == nil {
			t.Fatal("registered successor replenished automatic retry allowance")
		}
	})
}

func TestExecutionRegistrationValidatesMetadataBeforeConsumingRetry(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "original", Status: "retry_requested", Job: json.RawMessage(`unavailable`)}, 0, "")
		if _, err := tx.Exec("UPDATE projects SET reason_worker='planner@successor',reason_trigger='retry' WHERE id='p'"); err != nil {
			t.Fatal(err)
		}
		e := Execution{ProjectID: "p", ID: "successor", Namespace: "ns", Backend: "planner", Kind: "reason", Lease: "planner@successor", RetryKey: "reason:key",
			Job: json.RawMessage(`{"graph":{"project":{"id":"p"}},"previous_run_id":"original","decision_revision":"invalid"}`)}
		if err := tx.RegisterExecution(e); err == nil {
			t.Fatal("invalid input metadata was accepted")
		}
		grant, err := tx.GetExecutionSummary("ns", "p", "original")
		if err != nil || grant.Status != "retry_requested" {
			t.Fatalf("invalid successor consumed its grant: %+v err=%v", grant, err)
		}
		e.Job = json.RawMessage(`{"graph":{"project":{"id":"p"}},"previous_run_id":"original"}`)
		if err := tx.RegisterExecution(e); err != nil {
			t.Fatalf("valid successor could not consume compact grant identity: %v", err)
		}
		grant, err = tx.GetExecutionSummary("ns", "p", "original")
		if err != nil || grant.Status != "retried" {
			t.Fatalf("successful successor did not consume grant: %+v err=%v", grant, err)
		}
	})
}

func TestExecutionMetadataMigrationBackfillsOnceWithoutChangingJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema + executionSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO projects(id,title,status,created_at) VALUES('p','fixture','active','now')"); err != nil {
		t.Fatal(err)
	}
	state := State{Revision: 31, DecisionRevision: 19, Graph: Graph{Project: Project{ID: "p", Generation: 7}, Facts: []Fact{{ID: "origin"}}, Hints: []Hint{{ID: "h1"}}, Intents: []Intent{{ID: "open"}, {ID: "done", To: Ptr("f1")}}}}
	job, _ := json.Marshal(map[string]any{"graph": state.Graph, "state": state, "decision_revision": 17})
	_, err = db.Exec(`INSERT INTO xloom_executions(`+executionColumns+`) VALUES('p','old','ns','planner','reason','','planner@old',?,'reason:old','succeeded',NULL,0,'now','now')`, job)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	executionQueryTx(t, s, func(tx *Tx) {
		e, err := tx.GetExecutionSummary("ns", "p", "old")
		if err != nil || e.Generation != 7 || e.InputRevision != 31 || e.DecisionRevision != 17 || e.StateVersion != DecisionStateVersion(state) || !e.HasState || e.FactCount != 1 || e.HintCount != 1 || e.OpenCount != 1 {
			t.Fatalf("backfill=%+v err=%v", e, err)
		}
		full, err := tx.Execution("p", "old")
		if err != nil || string(full.Job) != string(job) {
			t.Fatalf("migration rewrote immutable job: %v", err)
		}
		if _, err = tx.Exec("UPDATE xloom_executions SET job='unavailable' WHERE id='old'"); err != nil {
			t.Fatal(err)
		}
	})
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen repeated the backfill: %v", err)
	}
	defer s.Close()
	executionQueryTx(t, s, func(tx *Tx) {
		check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "ns", Generation: 7, Kind: "reason", StateVersion: DecisionStateVersion(state)})
		if err != nil || !check.Repeated || check.LatestDecision == nil || check.LatestDecision.InputRevision != 31 {
			t.Fatalf("reopened metadata=%+v err=%v", check, err)
		}
	})
}

func TestExecutionMetadataMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schema + executionSchema + `INSERT INTO projects(id,title,created_at) VALUES('p','test','now'); INSERT INTO xloom_executions(` + executionColumns + `) VALUES('p','bad','ns','planner','reason','','planner@bad','invalid','reason:bad','failed',NULL,0,'now','now');`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("invalid legacy input silently received valid metadata")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('xloom_executions') WHERE name='metadata_version'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration left partial schema: count=%d err=%v", count, err)
	}
}
