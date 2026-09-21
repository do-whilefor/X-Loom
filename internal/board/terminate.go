package board

import "encoding/json"

const terminationSchema = `CREATE TABLE IF NOT EXISTS xloom_project_termination(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 terminated_at TEXT NOT NULL);`

// Termination ends an unfinished project without asserting that its Goal was
// achieved. The graph, evidence, history and workspace survive for inspection.
// Only an explicit restart may start another execution round afterward.
func (t *Tx) TerminateProject(project string, expected *int64) (Graph, error) {
	g, err := t.Load(project)
	if err != nil {
		return Graph{}, err
	}
	if expected != nil && *expected != g.Project.Generation {
		return Graph{}, Err(409, "Project was restarted; refresh before terminating")
	}
	if g.Project.Status == "terminated" {
		return g, nil
	}
	if g.Project.Status != "active" && g.Project.Status != "stopped" {
		return Graph{}, Err(409, "Only active or stopped projects can be terminated")
	}
	if err = t.RevokeRuns(project); err != nil {
		return Graph{}, err
	}
	rows, err := t.Query("SELECT id FROM xloom_executions WHERE project_id=? AND status IN ('prepared','running','retryable','result_pending')", project)
	if err != nil {
		return Graph{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return Graph{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Graph{}, err
	}
	for _, id := range ids {
		e, err := t.Execution(project, id)
		if err != nil {
			return Graph{}, err
		}
		failure := json.RawMessage(`{"status":"failed","failure_kind":"project_terminated","error":"project terminated by user"}`)
		// A result_pending execution keeps its immutable saved result. Its
		// cancellation diagnostic is added to history, never applied as a Fact.
		if err = t.ExecutionStatus(e, "cancelled", failure); err != nil {
			return Graph{}, err
		}
	}
	if _, err = t.Exec("DELETE FROM xloom_paused_executions WHERE project_id=?", project); err != nil {
		return Graph{}, err
	}
	// Cancel outstanding one-use retry grants as well as running work, while
	// retaining the attempt's stored result and original failure history.
	if _, err = t.Exec("UPDATE xloom_executions SET status='cancelled',updated_at=? WHERE project_id=? AND status='retry_requested'", t.Now, project); err != nil {
		return Graph{}, err
	}
	g.Project.Status, g.Project.Reason, g.Project.TerminatedAt = "terminated", nil, t.Now
	for index := range g.Intents {
		if g.Intents[index].ConcludedAt == nil {
			g.Intents[index].Worker = nil
		}
	}
	if _, err = t.Exec("INSERT INTO xloom_project_termination(project_id,terminated_at) VALUES(?,?)", project, t.Now); err != nil {
		return Graph{}, err
	}
	return g, t.Save(g)
}
