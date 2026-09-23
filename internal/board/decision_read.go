package board

import (
	"database/sql"
	"encoding/json"
	"errors"
)

const decisionReadSchema = `CREATE TABLE IF NOT EXISTS xloom_decision_read_views(
 project_id TEXT NOT NULL,execution_id TEXT NOT NULL,state_version TEXT NOT NULL,state BLOB NOT NULL,
 PRIMARY KEY(project_id,execution_id),
 FOREIGN KEY(project_id,execution_id) REFERENCES xloom_executions(project_id,id) ON DELETE CASCADE);`

// DecisionReadState pins a version 2 planner's detail reads to its last
// overview, or its registered input before the first refresh. Only one refreshed
// view is retained per execution. Writes still validate against current State.
func (t *Tx) DecisionReadState(current State, fence ExecutionFence, refresh bool) (State, error) {
	if fence.Run == "" || fence.Lease != "reason" {
		return current, nil
	}
	e, err := scanExecution(t.QueryRow("SELECT "+executionColumns+" FROM xloom_executions WHERE project_id=? AND lease=? AND kind='reason'", current.Graph.Project.ID, fence.Run))
	if errors.Is(err, sql.ErrNoRows) {
		return current, nil
	}
	if err != nil {
		return State{}, err
	}
	version, err := DecisionJobVersion(e.Job)
	if err != nil || version == "" {
		return current, err
	}
	var job struct {
		Graph         Graph          `json:"graph"`
		State         *State         `json:"state"`
		InputSnapshot *InputSnapshot `json:"input_snapshot"`
		Decision      *struct {
			Version int `json:"version"`
		} `json:"decision"`
	}
	if err = json.Unmarshal(e.Job, &job); err != nil {
		return State{}, err
	}
	// Version 1 publishes individual actions and advances its read version
	// after each write. Preserve that live protocol for existing workers.
	if job.Decision == nil || job.Decision.Version != 2 {
		return current, nil
	}
	if job.Graph.Project.Generation != current.Graph.Project.Generation {
		return State{}, Err(409, "decision belongs to a previous project round")
	}
	if refresh {
		raw, err := json.Marshal(current)
		if err != nil {
			return State{}, err
		}
		_, err = t.Exec("INSERT INTO xloom_decision_read_views(project_id,execution_id,state_version,state) VALUES(?,?,?,?) ON CONFLICT(project_id,execution_id) DO UPDATE SET state_version=excluded.state_version,state=excluded.state", e.ProjectID, e.ID, DecisionStateVersion(current), raw)
		return current, err
	}
	var raw []byte
	err = t.QueryRow("SELECT state_version,state FROM xloom_decision_read_views WHERE project_id=? AND execution_id=?", e.ProjectID, e.ID).Scan(&version, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		if job.InputSnapshot != nil {
			return t.ReadInputSnapshot(e.ProjectID, job.InputSnapshot.ID)
		}
		return *job.State, nil // DecisionJobVersion validated the legacy input.
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return State{}, err
	}
	if state.Graph.Project.ID != e.ProjectID || state.Graph.Project.Generation != current.Graph.Project.Generation || DecisionStateVersion(state) != version {
		return State{}, errors.New("decision read view binding mismatch")
	}
	return state, nil
}
