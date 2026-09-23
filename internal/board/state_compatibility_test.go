package board

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCompactStepMetadataLifecycleAndRollbackRead(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "add", stepInput("Current task", []string{"f002", "f001"}, "goal", 7)).ID
	check := func(status string, priority int, reason string) {
		t.Helper()
		state := f.state()
		step := state.Steps[0]
		if step.ID != id || step.Status != status || step.Priority != priority || step.Reason != reason || step.Description != "Current task" || !reflect.DeepEqual(step.From, []string{"f002", "f001"}) {
			t.Fatalf("unexpected Step projection: %+v", step)
		}
		f.tx(func(tx *Tx) error {
			var raw []byte
			if err := tx.QueryRow("SELECT data FROM xloom_state WHERE project_id='proj_001'").Scan(&raw); err != nil {
				return err
			}
			var stored struct {
				Steps []map[string]json.RawMessage `json:"steps"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				return err
			}
			for key := range stored.Steps[0] {
				if key != "id" && key != "goal_id" && key != "priority" && key != "reason" && key != "status" {
					t.Fatalf("duplicate Step field persisted: %s", key)
				}
			}
			if marker := string(stored.Steps[0]["status"]); marker != "" && marker != `"abandoned"` {
				t.Fatalf("runtime status persisted: %s", marker)
			}
			// The old version decoded public Step values and projected these exact
			// five fields onto Intent. New JSON must remain usable by that reader.
			var legacy struct {
				Steps []Step `json:"steps"`
			}
			if err := json.Unmarshal(raw, &legacy); err != nil {
				return err
			}
			old := legacy.Steps[0]
			if old.ID != step.ID || old.GoalID != step.GoalID || old.Priority != step.Priority || old.Reason != step.Reason || (old.Status == "abandoned") != (status == "abandoned") {
				t.Fatalf("rollback reader lost metadata: %+v", old)
			}
			return nil
		})
	}
	check("open", 7, "")
	f.action("step", "priority", map[string]any{"action": "priority", "id": id, "priority": 19, "reason": "Prioritize observation"})
	check("open", 19, "Prioritize observation")
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE intents SET worker='runner@current',last_heartbeat_at=? WHERE project_id='proj_001' AND id=?", tx.Now, id)
		return err
	})
	check("running", 19, "Prioritize observation")
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "No longer needed"})
	check("abandoned", 19, "No longer needed")
	completedID := f.action("step", "add-completed", stepInput("Second task", []string{"origin"}, "goal", 3)).ID
	var databasePath string
	f.tx(func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE intents SET worker='runner@completed',to_fact_id='f001',concluded_at=? WHERE project_id='proj_001' AND id=?", tx.Now, completedID); err != nil {
			return err
		}
		var sequence int
		var name string
		return tx.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &databasePath)
	})
	before := f.state()
	if before.Steps[1].Status != "completed" || Value(before.Steps[1].Result) != "f001" || Value(before.Steps[1].Worker) != "runner@completed" {
		t.Fatalf("completed projection: %+v", before.Steps[1])
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store = reopened
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatalf("reopen changed projection: before=%+v after=%+v", before, after)
	}
}

func TestLegacyStepMetadataProjectionAfterReopen(t *testing.T) {
	// This is the old persisted public Step shape. Runtime and task fields
	// intentionally disagree with the authoritative Intent below.
	const legacy = `{"steps":[{"id":"i001","goal_id":"goal","priority":7,"reason":"saved rationale","status":"completed","from":["stale"],"description":"obsolete task","worker":"obsolete worker","result":"obsolete result","created_at":"obsolete date"}]}`
	for _, tc := range []struct {
		name, metadata, status, goal, reason string
		worker, result                       *string
		priority                             int
	}{
		{name: "open", metadata: legacy, status: "open", goal: "goal", reason: "saved rationale", priority: 7},
		{name: "claimed", metadata: legacy, status: "running", goal: "goal", reason: "saved rationale", worker: Ptr("runner@current"), priority: 7},
		{name: "completed", metadata: legacy, status: "completed", goal: "goal", reason: "saved rationale", worker: Ptr("runner@current"), result: Ptr("f001"), priority: 7},
		{name: "abandoned", metadata: `{"steps":[{"id":"i001","goal_id":"goal","status":"abandoned","reason":"cancelled plan"}]}`, status: "abandoned", goal: "goal", reason: "cancelled plan"},
		{name: "missing goal stays missing", metadata: `{"steps":[{"id":"i001","status":"failed"}]}`, status: "open"},
		{name: "historical intent defaults to root", metadata: `{"steps":[]}`, status: "open", goal: "goal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			const created = "2026-09-23T00:00:00Z"
			err = store.Do(context.Background(), func(tx *Tx) error {
				if err := tx.Save(Graph{
					Project: Project{ID: "proj_001", Title: "Legacy fixture", Status: "active", CreatedAt: created},
					Facts:   []Fact{{ID: "origin", Description: "Original requirement"}, {ID: "goal", Description: "Root goal"}, {ID: "f001", Description: "Observed result"}},
					Intents: []Intent{{ID: "i001", From: []string{"origin"}, Description: "Current task", Creator: "planner", Worker: tc.worker, To: tc.result, CreatedAt: created}},
				}); err != nil {
					return err
				}
				_, err := tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES('proj_001',?,4,2)", tc.metadata)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			err = reopened.Do(context.Background(), func(tx *Tx) error {
				state, err := tx.State("proj_001")
				if err != nil {
					return err
				}
				want := Step{ID: "i001", From: []string{"origin"}, GoalID: tc.goal, Description: "Current task", Status: tc.status, Worker: tc.worker, Result: tc.result, CreatedAt: created, Priority: tc.priority, Reason: tc.reason}
				if !reflect.DeepEqual(state.Steps, []Step{want}) || state.Revision != 4 || state.DecisionRevision != 2 {
					t.Fatalf("legacy projection changed: steps=%+v, revision=%d/%d; want %+v, revision=4/2", state.Steps, state.Revision, state.DecisionRevision, want)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
