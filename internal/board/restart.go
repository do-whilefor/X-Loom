package board

const restartSchema = `CREATE TABLE IF NOT EXISTS xloom_project_rounds(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL DEFAULT 0,restarted_at TEXT NOT NULL);` + roundHistorySchema

// RestartProject begins a fresh board round, preserving human inputs. Lease
// tombstones and ID counters deliberately survive: late workers must never
// regain authority or accidentally address a replacement step with an old ID.
// The prior round is archived before resetting its current projections. This
// operation never changes project workspace files or historical snapshots.
func (t *Tx) RestartProject(project string, expected *int64) (Graph, error) {
	g, err := t.Load(project)
	if err != nil {
		return Graph{}, err
	}
	if expected != nil && *expected != g.Project.Generation {
		return Graph{}, Err(409, "Project was already restarted; refresh before retrying")
	}
	inputs := []Fact{}
	for _, fact := range g.Facts {
		if fact.ID == "origin" || fact.ID == "goal" {
			inputs = append(inputs, fact)
		}
	}
	if len(inputs) != 2 {
		return Graph{}, Err(409, "Project is missing its original input or goal")
	}
	if err = t.archiveRound(project); err != nil {
		return Graph{}, err
	}
	if err = t.RevokeRuns(project); err != nil {
		return Graph{}, err
	}
	for _, query := range []string{
		"DELETE FROM xloom_project_termination WHERE project_id=?",
		"DELETE FROM xloom_state_actions WHERE project_id=?",
		"DELETE FROM xloom_state_events WHERE project_id=?",
		"DELETE FROM xloom_state WHERE project_id=?",
		"DELETE FROM xloom_executions WHERE project_id=?",
		"DELETE FROM intents WHERE project_id=?",
		"DELETE FROM facts WHERE project_id=? AND id NOT IN ('origin','goal')",
	} {
		if _, err = t.Exec(query, project); err != nil {
			return Graph{}, err
		}
	}
	g.Project.Status, g.Project.Reason = "active", nil
	g.Project.Generation++
	g.Project.RestartedAt = t.Now
	g.Project.TerminatedAt = ""
	g.Facts, g.Intents = inputs, []Intent{}
	if _, err = t.Exec(`INSERT INTO xloom_project_rounds(project_id,generation,restarted_at) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET generation=excluded.generation,restarted_at=excluded.restarted_at`, project, g.Project.Generation, t.Now); err != nil {
		return Graph{}, err
	}
	return g, t.Save(g)
}
