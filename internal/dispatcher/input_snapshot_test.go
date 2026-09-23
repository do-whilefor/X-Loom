package dispatcher

import (
	"encoding/json"

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
