package board

import "encoding/json"

// Keep optional presentation metadata outside Cairn's projects table so both
// legacy schemas and clients continue to work without inventing a scenario.
const projectMetadataSchema = `CREATE TABLE IF NOT EXISTS xloom_project_metadata(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 scenario TEXT NOT NULL CHECK(scenario IN ('ctf','pentest','audit')));`

func ValidScenario(scenario string) bool {
	return scenario == "ctf" || scenario == "pentest" || scenario == "audit"
}

// ExecutionView is a read-only projection. It deliberately cannot serialize
// dispatch inputs, lease identities, retry authorization, or environment data.
type ExecutionView struct {
	ID        string               `json:"id"`
	ProjectID string               `json:"project_id"`
	Kind      string               `json:"kind"`
	Backend   string               `json:"backend"`
	Intent    string               `json:"intent"`
	Status    string               `json:"status"`
	Resumes   int                  `json:"resumes"`
	CreatedAt string               `json:"created_at"`
	UpdatedAt string               `json:"updated_at"`
	Result    *ExecutionResultView `json:"result,omitempty"`
}

type ExecutionResultView struct {
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func executionResultView(raw []byte) *ExecutionResultView {
	var result ExecutionResultView
	if json.Unmarshal(raw, &result) != nil || (result.Text == "" && result.Error == "") {
		return nil
	}
	result.Truncated = false
	for _, field := range []struct {
		text  *string
		limit int
	}{{&result.Text, 64 * 1024}, {&result.Error, 4 * 1024}} {
		if runes := []rune(*field.text); len(runes) > field.limit {
			*field.text = string(runes[:field.limit])
			result.Truncated = true
		}
	}
	return &result
}

func (t *Tx) ProjectExecutions(project string) ([]ExecutionView, error) {
	var exists bool
	if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, Err(404, "Project not found")
	}
	rows, err := t.Query(`SELECT id,project_id,kind,backend,intent,status,resumes,created_at,updated_at,result FROM xloom_executions WHERE project_id=? ORDER BY created_at,rowid`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExecutionView{}
	for rows.Next() {
		var entry ExecutionView
		var result []byte
		if err := rows.Scan(&entry.ID, &entry.ProjectID, &entry.Kind, &entry.Backend, &entry.Intent, &entry.Status, &entry.Resumes, &entry.CreatedAt, &entry.UpdatedAt, &result); err != nil {
			return nil, err
		}
		entry.Result = executionResultView(result)
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ActiveWorkers counts currently held, non-revoked project leases after
// expiration. It is not a claim about running Docker processes or configured
// dispatcher limits, neither of which the HTTP server can observe directly.
type UIOverview struct {
	ActiveWorkers   int            `json:"active_workers"`
	ExecutionCounts map[string]int `json:"execution_counts"`
	ObservedAt      string         `json:"observed_at"`
}

func (t *Tx) UIOverview() (UIOverview, error) {
	out := UIOverview{ExecutionCounts: map[string]int{}, ObservedAt: t.Now}
	err := t.QueryRow(`SELECT COUNT(*) FROM (
 SELECT id AS project_id,reason_worker AS worker FROM projects WHERE status='active' AND reason_worker IS NOT NULL
 UNION
 SELECT i.project_id,i.worker FROM intents i JOIN projects p ON p.id=i.project_id
 WHERE p.status='active' AND i.to_fact_id IS NULL AND i.concluded_at IS NULL AND i.worker IS NOT NULL
) owners WHERE NOT EXISTS(SELECT 1 FROM xloom_revoked_runs r WHERE r.project_id=owners.project_id AND r.worker=owners.worker)`).Scan(&out.ActiveWorkers)
	if err != nil {
		return out, err
	}
	rows, err := t.Query("SELECT status,COUNT(*) FROM xloom_executions GROUP BY status")
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return out, err
		}
		out.ExecutionCounts[status] = count
	}
	return out, rows.Err()
}
