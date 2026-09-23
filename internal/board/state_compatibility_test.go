package board

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

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
