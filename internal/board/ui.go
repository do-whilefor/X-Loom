package board

import (
	"encoding/json"
	"sort"
)

// Keep optional scenario metadata outside Cairn's projects table so both
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
	ID         string               `json:"id"`
	ProjectID  string               `json:"project_id"`
	Generation int64                `json:"generation"`
	Kind       string               `json:"kind"`
	Backend    string               `json:"backend"`
	Intent     string               `json:"intent"`
	Status     string               `json:"status"`
	Resumes    int                  `json:"resumes"`
	CreatedAt  string               `json:"created_at"`
	UpdatedAt  string               `json:"updated_at"`
	Result     *ExecutionResultView `json:"result,omitempty"`
}

type ExecutionResultView struct {
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func executionResultView(text, failure string) *ExecutionResultView {
	result := ExecutionResultView{Text: text, Error: failure}
	if result.Text == "" && result.Error == "" {
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

// The SQL boundary extracts only bounded public result fields. Four UTF-8
// bytes per character plus a sentinel preserve the existing character limits;
// slicing BLOBs also preserves embedded NULs (SQLite text substr does not).
// Historical Job bodies never enter this read.
const executionViewColumns = `rowid,id,project_id,generation,kind,backend,intent,status,resumes,created_at,updated_at,
COALESCE(CASE WHEN json_valid(result) THEN CASE WHEN json_type(result,'$.text')='text' THEN substr(CAST(json_extract(result,'$.text') AS BLOB),1,262148) END END,''),
COALESCE(CASE WHEN json_valid(result) THEN CASE WHEN json_type(result,'$.error')='text' THEN substr(CAST(json_extract(result,'$.error') AS BLOB),1,16388) END END,'')`

const MaxExecutionViewPageBytes = 1 << 20

type ExecutionViewPage struct {
	Items      []ExecutionView `json:"items"`
	NextCursor int64           `json:"next_cursor,omitempty"`
	Through    int64           `json:"through"`
}

// ProjectExecutions preserves the original array response for old clients.
// New clients follow ProjectExecutionPage cursors for bounded responses.
func (t *Tx) ProjectExecutions(project string) ([]ExecutionView, error) {
	out := []ExecutionView{}
	var after, through int64
	for {
		page, err := t.ProjectExecutionPage(project, after, through, 100)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == 0 {
			break
		}
		after, through = page.NextCursor, page.Through
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// Through freezes the set of executions to enumerate, not their changing
// statuses. A fresh refresh starts at zero and sees current runtime metadata.
func (t *Tx) ProjectExecutionPage(project string, after, through int64, limit int) (ExecutionViewPage, error) {
	out := ExecutionViewPage{Items: []ExecutionView{}, Through: through}
	if after < 0 || through < 0 || (through != 0 && after > through) || limit < 1 || limit > 100 {
		return out, Err(422, "invalid execution page boundary (limit must be 1-100)")
	}
	var exists bool
	if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, Err(404, "Project not found")
	}
	if through == 0 {
		if err := t.QueryRow("SELECT COALESCE(MAX(rowid),0) FROM xloom_executions WHERE project_id=?", project).Scan(&out.Through); err != nil {
			return out, err
		}
	}
	rows, err := t.Query("SELECT "+executionViewColumns+" FROM xloom_executions WHERE project_id=? AND rowid>? AND rowid<=? ORDER BY rowid LIMIT ?", project, after, out.Through, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	used, last := 128, after
	for rows.Next() {
		var entry ExecutionView
		var row int64
		var text, failure string
		if err := rows.Scan(&row, &entry.ID, &entry.ProjectID, &entry.Generation, &entry.Kind, &entry.Backend, &entry.Intent, &entry.Status, &entry.Resumes, &entry.CreatedAt, &entry.UpdatedAt, &text, &failure); err != nil {
			return out, err
		}
		entry.Result = executionResultView(text, failure)
		raw, err := json.Marshal(entry)
		if err != nil {
			return out, err
		}
		if len(raw)+128 > MaxExecutionViewPageBytes {
			return out, Err(422, "execution metadata exceeds page size")
		}
		if len(out.Items) == limit || used+len(raw)+1 > MaxExecutionViewPageBytes {
			out.NextCursor = last
			break
		}
		out.Items = append(out.Items, entry)
		used += len(raw) + 1
		last = row
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
