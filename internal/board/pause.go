package board

import "encoding/json"

// Pause records only executions interrupted by this human operation. A later
// Continue must not grant blanket retries to unrelated failures or abandoned
// directions. The marker survives dispatcher shutdown and cascades on restart.
func (t *Tx) markPausedExecutions(g Graph) error {
	_, err := t.Exec(`INSERT OR IGNORE INTO xloom_paused_executions(project_id,execution_id)
 SELECT e.project_id,e.id FROM xloom_executions e
 WHERE e.project_id=? AND e.status IN ('prepared','running','retryable','result_pending')
 AND NOT EXISTS(SELECT 1 FROM xloom_revoked_runs r WHERE r.project_id=e.project_id AND r.worker=e.lease)`, g.Project.ID)
	return err
}

func (t *Tx) continuePausedExecutions(g Graph) error {
	rows, err := t.Query("SELECT execution_id FROM xloom_paused_executions WHERE project_id=?", g.Project.ID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	d, _, _, err := t.stateData(g.Project.ID)
	if err != nil {
		return err
	}
	abandoned := map[string]bool{}
	for _, step := range d.Steps {
		abandoned[step.ID] = step.Status == "abandoned"
	}
	for _, id := range ids {
		e, err := t.Execution(g.Project.ID, id)
		if err != nil {
			return err
		}
		if !e.Pending() && e.Status != "cancelled" {
			continue
		}
		if e.Kind != "reason" {
			available := false
			for _, intent := range g.Intents {
				if intent.ID == e.Intent && intent.To == nil && intent.ConcludedAt == nil && !abandoned[intent.ID] {
					available = true
					break
				}
			}
			if !available {
				continue
			}
		}
		// Continue may arrive before the old worker acknowledges cancellation.
		// Terminalize the revoked attempt here; its late callback cannot revoke
		// this one-use grant. ExecutionStatus retains immutable pending results.
		if e.Pending() {
			failure := json.RawMessage(`{"status":"failed","failure_kind":"project_paused","error":"project paused by user"}`)
			if err = t.ExecutionStatus(e, "cancelled", failure); err != nil {
				return err
			}
		}
		if err = t.ExecutionStatus(e, "retry_requested", nil); err != nil {
			return err
		}
	}
	_, err = t.Exec("DELETE FROM xloom_paused_executions WHERE project_id=?", g.Project.ID)
	return err
}
