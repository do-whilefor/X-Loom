package board

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestStateNodeAddsWriteOnlyNewNode(t *testing.T) {
	for _, op := range []string{"fact", "step"} {
		t.Run(op, func(t *testing.T) {
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 4}, 1)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				for _, statement := range []string{
					`CREATE TRIGGER guard_project BEFORE UPDATE ON projects BEGIN SELECT RAISE(ABORT,'unrelated project write'); END`,
					`CREATE TRIGGER guard_fact BEFORE INSERT ON facts WHEN NEW.id != 'f017' BEGIN SELECT RAISE(ABORT,'unrelated fact write'); END`,
					`CREATE TRIGGER guard_intent BEFORE INSERT ON intents WHEN NEW.id != 'i017' BEGIN SELECT RAISE(ABORT,'unrelated intent write'); END`,
					`CREATE TRIGGER guard_sources BEFORE INSERT ON intent_sources WHEN NEW.intent_id != 'i017' BEGIN SELECT RAISE(ABORT,'unrelated source write'); END`,
				} {
					if _, err := tx.Exec(statement); err != nil {
						return err
					}
				}
				fence, want := stateBenchmarkPlanner, "i017"
				if op == "fact" {
					fence, want = stateBenchmarkWorker, "f017"
				}
				result, err := tx.StateAction(stateBenchmarkProject, fence, f.action(op, 0))
				if err != nil {
					return err
				}
				if result.ID != want || result.Revision != 1 {
					t.Fatalf("unexpected addition: %+v", result)
				}
				state, err := tx.State(stateBenchmarkProject)
				if err != nil {
					return err
				}
				if op == "step" {
					step := state.Steps[len(state.Steps)-1]
					if step.ID != want || len(step.From) != 4 {
						t.Fatalf("sources were not saved: %+v", step)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStateNodeFailureRollsBackAllPersistence(t *testing.T) {
	for _, op := range []string{"fact", "step"} {
		t.Run(op, func(t *testing.T) {
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 4}, 1)
			snapshot := func() string {
				t.Helper()
				var raw string
				if err := f.store.Do(context.Background(), func(tx *Tx) error {
					state, err := tx.State(stateBenchmarkProject)
					if err != nil {
						return err
					}
					var metadata string
					var facts, intents, sources, actions, events, factCounter, intentCounter int
					if err = tx.QueryRow(`SELECT data,(SELECT COUNT(*) FROM facts),(SELECT COUNT(*) FROM intents),(SELECT COUNT(*) FROM intent_sources),(SELECT COUNT(*) FROM xloom_state_actions),(SELECT COUNT(*) FROM xloom_state_events),(SELECT value FROM scoped_counters WHERE project_id=? AND kind='fact'),(SELECT value FROM scoped_counters WHERE project_id=? AND kind='intent') FROM xloom_state WHERE project_id=?`, stateBenchmarkProject, stateBenchmarkProject, stateBenchmarkProject).Scan(&metadata, &facts, &intents, &sources, &actions, &events, &factCounter, &intentCounter); err != nil {
						return err
					}
					encoded, err := json.Marshal(state)
					raw = fmt.Sprintf("%s|%s|%d,%d,%d,%d,%d,%d,%d", encoded, metadata, facts, intents, sources, actions, events, factCounter, intentCounter)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			before := snapshot()
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_node_event BEFORE INSERT ON xloom_state_events BEGIN SELECT RAISE(ABORT,'event unavailable'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fence := stateBenchmarkPlanner
			if op == "fact" {
				fence = stateBenchmarkWorker
			}
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction(stateBenchmarkProject, fence, f.action(op, 0))
				return err
			}); err == nil {
				t.Fatal("injected event failure succeeded")
			}
			if after := snapshot(); after != before {
				t.Fatal("failed node addition changed graph, metadata, counters, receipt or event")
			}
		})
	}
}
