package dispatcher

import (
	"context"
	"encoding/json"
	"testing"

	"xloom/internal/board"
	"xloom/internal/worker"
)

// Scripted planners read the same bounded input that the model receives.
// They must not depend on a second, hidden full graph inside new Jobs.
func fixturePlanInput(job worker.Job) (steps, open int, facts []board.FactRecord) {
	if ref := job.InputSnapshot; ref != nil {
		var view struct {
			Facts []board.FactRecord `json:"fact_records"`
		}
		if job.Decision != nil {
			_ = json.Unmarshal(job.Decision.View, &view)
		}
		return ref.StepCount, ref.OpenCount, view.Facts
	}
	if job.State != nil {
		facts = job.State.FactRecords
	} else {
		for _, fact := range job.Graph.Facts {
			facts = append(facts, board.FactRecord{ID: fact.ID, Description: fact.Description})
		}
	}
	return len(job.Graph.Intents), job.Graph.OpenCount(), facts
}

// Inspect retained fixture records without restoring the retired whole-history
// HTTP endpoint. Scheduling and graph operations still use the real server.
func testExecutions(t *testing.T, store *board.Store) []board.Execution {
	t.Helper()
	var executions []board.Execution
	err := store.Do(context.Background(), func(tx *board.Tx) error {
		rows, err := tx.Query("SELECT project_id,id FROM xloom_executions WHERE namespace=? ORDER BY created_at,rowid", "xloom")
		if err != nil {
			return err
		}
		var identities [][2]string
		for rows.Next() {
			var identity [2]string
			if err := rows.Scan(&identity[0], &identity[1]); err != nil {
				rows.Close()
				return err
			}
			identities = append(identities, identity)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, identity := range identities {
			execution, err := tx.Execution(identity[0], identity[1])
			if err != nil {
				return err
			}
			executions = append(executions, execution)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return executions
}

// Preserve historical job shapes for recovery/bridge fixtures. New execution
// registration tests use the production /executions/prepare path instead.
func registerLegacyExecution(t *testing.T, store *board.Store, execution board.Execution) board.Execution {
	t.Helper()
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		if err := tx.RegisterExecution(execution); err != nil {
			return err
		}
		var err error
		execution, err = tx.Execution(execution.ProjectID, execution.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return execution
}
