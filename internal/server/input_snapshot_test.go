package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/worker"
)

func newSnapshotHTTPFixture(t *testing.T) (*executionProtocolFixture, *board.Store) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	f := &executionProtocolFixture{t: t, handler: New(store), run: "snapshot-run", lease: "planner@snapshot-run"}
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Frozen input", "origin": "Synthetic input", "goal": "Inspect the fixture", "bootstrap_enabled": false}, false, http.StatusCreated, &graph)
	f.project = graph.Project.ID
	return f, store
}

// The caller claims work, then submits identity/configuration only. This helper
// intentionally never sends State, Facts, Hints or a client-built input view.
func snapshotTemplate(f *executionProtocolFixture, kind string) board.Execution {
	f.t.Helper()
	f.kind, f.intent = kind, ""
	var intent *board.Intent
	if kind == "reason" {
		f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": f.lease, "trigger": "initial"}, false, http.StatusOK, nil)
	} else {
		i := f.newIntent()
		f.intent = i.ID
		f.request("POST", f.base()+"/intents/"+i.ID+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusOK, &i)
		intent = &i
	}
	project := f.state().Graph.Project
	job, err := json.Marshal(map[string]any{
		"run_id": f.run, "kind": kind, "workspace": "/workspace", "graph_rpc": true, "result_contract_version": 2,
		"graph": board.Graph{Project: project}, "intent": intent, "decision_trigger": "initial",
		"budget": map[string]int{"max_intents": 3, "conclude_timeout": 60},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: kind, Intent: f.intent, Lease: f.lease, Job: job}
}

func prepareSnapshot(t *testing.T, f *executionProtocolFixture, template board.Execution) (board.Execution, worker.Job) {
	t.Helper()
	raw, err := json.Marshal(template)
	if err != nil || len(raw) > 8192 {
		t.Fatalf("preparation request is not a small template: %d bytes, %v", len(raw), err)
	}
	var saved board.Execution
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusCreated, &saved)
	var job worker.Job
	if err := json.Unmarshal(saved.Job, &job); err != nil {
		t.Fatal(err)
	}
	if job.State != nil || len(job.Graph.Facts)+len(job.Graph.Intents)+len(job.Graph.Hints) != 0 || job.InputSnapshot == nil || job.PreparationKey == "" || len(saved.Job) > 2*board.DefaultContextViewBytes {
		t.Fatalf("prepared job did not retain only its input reference: bytes=%d state=%v snapshot=%+v", len(saved.Job), job.State != nil, job.InputSnapshot)
	}
	return saved, job
}

func TestPrepareSnapshotCapturesServerRevisionAndReplaysExactInput(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			template := snapshotTemplate(f, kind)
			f.request("POST", f.base()+"/hints", map[string]string{"content": "accepted before preparation", "creator": "fixture"}, false, http.StatusCreated, nil)
			captured := f.state()
			saved, job := prepareSnapshot(t, f, template)
			ref := job.InputSnapshot
			if ref.ProjectID != f.project || ref.Generation != captured.Graph.Project.Generation || ref.Revision != captured.Revision || ref.DecisionRevision != captured.DecisionRevision || ref.StateVersion != board.DecisionStateVersion(captured) || ref.HintCount != 1 {
				t.Fatalf("snapshot metadata did not describe the Server's captured input: %+v", ref)
			}
			if kind == "reason" && (job.Decision == nil || job.Decision.ToRevision != captured.Revision || job.Decision.StateVersion != ref.StateVersion) {
				t.Fatal("Decide view is not bound to the captured revision")
			}
			var identity board.ExecutionSummary
			f.request("GET", f.base()+"/executions/"+f.run+"/identity?namespace=protocol-test", nil, false, http.StatusOK, &identity)
			if !identity.HasState || identity.InputRevision != ref.Revision || identity.StateVersion != ref.StateVersion || identity.HintCount != ref.HintCount {
				t.Fatalf("execution metadata disagrees with snapshot: %+v", identity)
			}
			f.request("POST", f.base()+"/hints", map[string]string{"content": "new input after preparation", "creator": "fixture"}, false, http.StatusCreated, nil)
			var replay board.Execution
			f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusOK, &replay)
			if !reflect.DeepEqual(saved, replay) {
				t.Fatal("same preparation request recaptured newer input")
			}
			var changed map[string]any
			if err := json.Unmarshal(template.Job, &changed); err != nil {
				t.Fatal(err)
			}
			changed["workspace"] = "/different-workspace"
			template.Job, _ = json.Marshal(changed)
			f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusConflict, nil)
		})
	}
}

func TestSnapshotReadRemainsFrozenWhileLiveFactsAdvance(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	_, job := prepareSnapshot(t, f, snapshotTemplate(f, "explore"))
	added := f.action("fact", "after-snapshot", map[string]any{"description": "ONLY_IN_LIVE_GRAPH", "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": []board.EvidenceRef{{RunID: f.run, Path: "new.txt", Excerpt: "NEW_EVIDENCE"}}})
	read := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_snapshot", Section: "facts", IDs: []string{"origin", added.ID}, ExpectedVersion: job.InputSnapshot.StateVersion}
	var page struct {
		Items        []board.FactRecord `json:"items"`
		MissingIDs   []string           `json:"missing_ids"`
		Revision     int64              `json:"revision"`
		StateVersion string             `json:"state_version"`
	}
	raw := f.request("POST", f.base()+"/executions/"+f.run+"/input/read", read, true, http.StatusOK, &page)
	if page.Revision != job.InputSnapshot.Revision || page.StateVersion != job.InputSnapshot.StateVersion || len(page.Items) != 1 || page.Items[0].ID != "origin" || !reflect.DeepEqual(page.MissingIDs, []string{added.ID}) || strings.Contains(raw, "ONLY_IN_LIVE_GRAPH") {
		t.Fatalf("snapshot read substituted newer live facts: %s", raw)
	}
	read.Op, read.ExpectedVersion = "read_graph", ""
	live := f.request("POST", f.base()+"/state/read", read, true, http.StatusOK, nil)
	if !strings.Contains(live, "ONLY_IN_LIVE_GRAPH") || !strings.Contains(live, "NEW_EVIDENCE") {
		t.Fatal("live graph did not retain the new observation")
	}
}

func TestSnapshotReadFailsClosedWhenStoredInputIsMissingOrCorrupt(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		status      int
	}{
		{"missing", "DELETE FROM xloom_input_snapshots WHERE project_id=? AND id=?", http.StatusNotFound},
		{"corrupt", "UPDATE xloom_input_snapshots SET state='{}' WHERE project_id=? AND id=?", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, store := newSnapshotHTTPFixture(t)
			_, job := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
			// Fault-inject unavailable storage after genuine HTTP preparation;
			// the read must fail instead of rebuilding input from current FGS.
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				_, err := tx.Exec(tc.query, f.project, job.InputSnapshot.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			f.request("POST", f.base()+"/executions/"+f.run+"/input/read", worker.GraphRequest{RequestID: strings.Repeat("b", 32), Op: "read_snapshot", Section: "overview"}, true, tc.status, nil)
		})
	}
}

func TestPrepareSnapshotRejectsClientSuppliedInputAndCrossRoundTemplates(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	template := snapshotTemplate(f, "reason")
	for name, value := range map[string]any{
		"state": f.state(), "input_snapshot": map[string]any{"id": strings.Repeat("f", 64)},
		"input_view": map[string]string{"claimed": "client-view"}, "preparation_key": "forged",
		"graph": map[string]any{"project": f.state().Graph.Project, "facts": []board.Fact{{ID: "forged", Description: "client fact"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]any
			_ = json.Unmarshal(template.Job, &fields)
			fields[name] = value
			forged := template
			forged.Job, _ = json.Marshal(fields)
			f.request("POST", f.base()+"/executions/prepare", forged, true, http.StatusUnprocessableEntity, nil)
		})
	}
	saved, _ := prepareSnapshot(t, f, template)
	// Even a structurally valid reference cannot be supplied to the legacy
	// registration endpoint; only server-side preparation may create one.
	f.request("POST", f.base()+"/executions", saved, true, http.StatusUnprocessableEntity, nil)
	f.request("POST", f.base()+"/restart", map[string]int{"expected_generation": 0}, false, http.StatusOK, nil)
	oldRun := f.run
	f.run, f.lease = "new-round-run", "planner@new-round-run"
	f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": f.lease, "trigger": "initial"}, false, http.StatusOK, nil)
	var stale worker.Job
	_ = json.Unmarshal(template.Job, &stale)
	stale.RunID = f.run
	template.ID, template.Lease = f.run, f.lease
	template.Job, _ = json.Marshal(stale)
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusConflict, nil)
	f.request("POST", f.base()+"/executions/"+oldRun+"/input/read", worker.GraphRequest{RequestID: strings.Repeat("c", 32), Op: "read_snapshot", Section: "overview"}, true, http.StatusNotFound, nil)
}

func TestSnapshotMarkersCannotDowngradeToLegacyRegistrationWithoutSnapshot(t *testing.T) {
	for _, marker := range []string{"input_view", "preparation_key"} {
		t.Run(marker, func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			template := snapshotTemplate(f, "explore")
			template.RetryKey = "explore:" + f.intent
			var fields map[string]any
			_ = json.Unmarshal(template.Job, &fields)
			if marker == "input_view" {
				fields[marker] = map[string]int{"version": 1}
			} else {
				fields[marker] = strings.Repeat("e", 64)
			}
			template.Job, _ = json.Marshal(fields)
			f.request("POST", f.base()+"/executions", template, true, http.StatusUnprocessableEntity, nil)
		})
	}
}

func TestPrepareSnapshotKeepsRegistrationSmallBeyondEightMiBEvidence(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	const facts, refs = 70, 15
	excerpt := strings.Repeat("x", 8192)
	var lastFact string
	for n := 0; n < facts; n++ {
		evidence := make([]board.EvidenceRef, refs)
		for k := range evidence {
			evidence[k] = board.EvidenceRef{RunID: f.run, Path: fmt.Sprintf("evidence/%d/%d.txt", n, k), Excerpt: excerpt}
		}
		payload, _ := json.Marshal(map[string]any{"description": fmt.Sprintf("Observation %d", n), "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": evidence})
		action := board.StateAction{Op: "fact", IdempotencyKey: fmt.Sprintf("large-fact-%d", n), Payload: payload}
		request := worker.GraphRequest{RequestID: fmt.Sprintf("%032x", n+1), Op: "graph_action", Action: action}
		if err := worker.ValidateGraphRequest(worker.Job{Kind: "explore", GraphRPC: true, ResultContractVersion: 2}, request); err != nil {
			t.Fatal(err)
		}
		frame, _ := json.Marshal(worker.GraphRequestEvent{Type: "graph_request", Request: request})
		if len(frame) > worker.MaxGraphRPCBytes {
			t.Fatalf("fixture exceeds real Worker RPC boundary: %d", len(frame))
		}
		var receipt board.StateActionResult
		f.request("POST", f.base()+"/state/actions", action, true, http.StatusOK, &receipt)
		lastFact = receipt.ID
	}
	final, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": lastFact}})
	f.pending(string(final))
	f.apply(http.StatusOK)
	f.run, f.lease = "after-large-graph", "planner@after-large-graph"
	template := snapshotTemplate(f, "reason")
	state := f.state()
	stateBytes, _ := json.Marshal(state)
	if len(stateBytes) <= 8<<20 || len(state.FactRecords) != facts+2 {
		t.Fatalf("fixture did not accumulate the original failure: bytes=%d facts=%d", len(stateBytes), len(state.FactRecords))
	}
	saved, job := prepareSnapshot(t, f, template)
	if job.InputSnapshot.Revision != state.Revision || job.InputSnapshot.FactCount != facts+2 || job.Decision == nil || job.Decision.ToRevision != state.Revision {
		t.Fatal("large input registration lost its captured revision")
	}
	var page struct {
		Items []board.EvidenceRef `json:"items"`
		Total int                 `json:"total"`
	}
	response := f.request("POST", f.base()+"/executions/"+f.run+"/input/read", worker.GraphRequest{RequestID: strings.Repeat("d", 32), Op: "read_snapshot", Section: "evidence", IDs: []string{lastFact}, Limit: 1, ExpectedVersion: job.InputSnapshot.StateVersion}, true, http.StatusOK, &page)
	if len(response) > worker.MaxGraphRPCBytes || page.Total != refs || len(page.Items) != 1 || page.Items[0].Excerpt != excerpt {
		t.Fatal("large snapshot evidence was not readable through bounded pages")
	}
	t.Logf("state_bytes=%d prepared_job_bytes=%d evidence_page_bytes=%d", len(stateBytes), len(saved.Job), len(response))
}
