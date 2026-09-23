package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ExecutionSummary is the scheduling identity and immutable input boundary.
// It deliberately contains neither the saved Job nor the business Result.
type ExecutionSummary struct {
	ProjectID        string `json:"project_id"`
	ID               string `json:"id"`
	Namespace        string `json:"namespace"`
	Backend          string `json:"backend"`
	Kind             string `json:"kind"`
	Intent           string `json:"intent"`
	Lease            string `json:"lease"`
	RetryKey         string `json:"retry_key"`
	Status           string `json:"status"`
	Resumes          int    `json:"resumes"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	Generation       int64  `json:"generation"`
	InputRevision    int64  `json:"input_revision"`
	DecisionRevision int64  `json:"decision_revision"`
	StateVersion     string `json:"state_version"`
	HasState         bool   `json:"has_state"`
	FactCount        int    `json:"fact_count"`
	HintCount        int    `json:"hint_count"`
	OpenCount        int    `json:"open_count"`
}

func (e ExecutionSummary) Fence() ExecutionFence {
	return ExecutionFence{Run: e.Lease, Lease: e.Kind, Intent: e.Intent}
}

func (e ExecutionSummary) Pending() bool {
	return e.Status == "prepared" || e.Status == "running" || e.Status == "retryable" || e.Status == "result_pending"
}

type ExecutionPage struct {
	Items      []ExecutionSummary `json:"items"`
	NextCursor int64              `json:"next_cursor,omitempty"`
}

type ExecutionCheckQuery struct {
	ProjectID, Namespace, Kind, Intent, RetryKey, StateVersion string
	Generation                                                 int64
}

type ExecutionCheck struct {
	Pending          bool              `json:"pending"`
	Blocked          bool              `json:"blocked"`
	PreviousRunID    string            `json:"previous_run_id"`
	Attempts         int               `json:"attempts"`
	AutomaticRetryID string            `json:"automatic_retry_id"`
	Repeated         bool              `json:"repeated"`
	LatestDecision   *ExecutionSummary `json:"latest_decision,omitempty"`
}

const executionMetadataColumns = `generation,input_revision,decision_revision,state_version,has_state,fact_count,hint_count,open_count`
const executionSummaryColumns = `project_id,id,namespace,backend,kind,intent,lease,retry_key,status,resumes,created_at,updated_at,` + executionMetadataColumns
const pendingExecutionSQL = `status IN ('prepared','running','retryable','result_pending')`
const executionFailureColumns = `COALESCE(CASE WHEN json_valid(result) THEN json_extract(result,'$.status') END,''),COALESCE(CASE WHEN json_valid(result) THEN json_extract(result,'$.failure_kind') END,'')`

func scanExecutionSummary(s scanner, prefix ...any) (ExecutionSummary, error) {
	var e ExecutionSummary
	args := append(prefix, &e.ProjectID, &e.ID, &e.Namespace, &e.Backend, &e.Kind, &e.Intent, &e.Lease, &e.RetryKey, &e.Status, &e.Resumes, &e.CreatedAt, &e.UpdatedAt,
		&e.Generation, &e.InputRevision, &e.DecisionRevision, &e.StateVersion, &e.HasState, &e.FactCount, &e.HintCount, &e.OpenCount)
	err := s.Scan(args...)
	return e, err
}

func executionInputMetadata(raw json.RawMessage) (ExecutionSummary, error) {
	var job struct {
		Graph            Graph          `json:"graph"`
		State            *State         `json:"state"`
		DecisionRevision int64          `json:"decision_revision"`
		InputSnapshot    *InputSnapshot `json:"input_snapshot"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return ExecutionSummary{}, err
	}
	e := ExecutionSummary{Generation: job.Graph.Project.Generation, DecisionRevision: job.DecisionRevision,
		FactCount: len(job.Graph.Facts), HintCount: len(job.Graph.Hints), OpenCount: job.Graph.OpenCount(), HasState: job.State != nil}
	if job.State != nil {
		e.InputRevision = job.State.Revision
		// Legacy jobs can contain State without Decision. Repeated-input checks
		// must hash that state too, rather than depending on a protocol marker.
		e.StateVersion = DecisionStateVersion(*job.State)
	}
	if ref := job.InputSnapshot; ref != nil {
		e.Generation, e.InputRevision, e.DecisionRevision = ref.Generation, ref.Revision, ref.DecisionRevision
		e.StateVersion, e.HasState = ref.StateVersion, true
		e.FactCount, e.HintCount, e.OpenCount = ref.FactCount, ref.HintCount, ref.OpenCount
	}
	return e, nil
}

func executionMetadataValues(e ExecutionSummary) []any {
	return []any{e.Generation, e.InputRevision, e.DecisionRevision, e.StateVersion, e.HasState, e.FactCount, e.HintCount, e.OpenCount}
}

// Backfill once in Open's migration transaction. Read only one historical Job
// at a time and never change its bytes. Later opens and scheduling queries use
// the indexed metadata even if a completed Job is no longer decodable.
func migrateExecutionMetadata(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA table_info(xloom_executions)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"generation", "INTEGER NOT NULL DEFAULT 0"}, {"input_revision", "INTEGER NOT NULL DEFAULT 0"},
		{"decision_revision", "INTEGER NOT NULL DEFAULT 0"}, {"state_version", "TEXT NOT NULL DEFAULT ''"},
		{"has_state", "INTEGER NOT NULL DEFAULT 0"}, {"fact_count", "INTEGER NOT NULL DEFAULT 0"},
		{"hint_count", "INTEGER NOT NULL DEFAULT 0"}, {"open_count", "INTEGER NOT NULL DEFAULT 0"},
		{"metadata_version", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !columns[column.name] {
			if _, err = tx.Exec("ALTER TABLE xloom_executions ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(`CREATE INDEX IF NOT EXISTS xloom_execution_metadata_backfill ON xloom_executions(metadata_version) WHERE metadata_version=0`); err != nil {
		return err
	}
	var after int64
	for {
		var id int64
		var raw []byte
		err = tx.QueryRow("SELECT rowid,job FROM xloom_executions WHERE metadata_version=0 AND rowid>? ORDER BY rowid LIMIT 1", after).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		metadata, err := executionInputMetadata(raw)
		if err != nil {
			return fmt.Errorf("execution metadata migration row %d: %w", id, err)
		}
		args := append(executionMetadataValues(metadata), id)
		if _, err = tx.Exec(`UPDATE xloom_executions SET generation=?,input_revision=?,decision_revision=?,state_version=?,has_state=?,fact_count=?,hint_count=?,open_count=?,metadata_version=1 WHERE rowid=?`, args...); err != nil {
			return err
		}
		after = id
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS xloom_execution_pending ON xloom_executions(namespace) WHERE ` + pendingExecutionSQL + `;
CREATE INDEX IF NOT EXISTS xloom_execution_task ON xloom_executions(namespace,project_id,kind,intent,status,created_at);
CREATE INDEX IF NOT EXISTS xloom_execution_retry ON xloom_executions(namespace,project_id,retry_key,generation);
CREATE INDEX IF NOT EXISTS xloom_execution_decision ON xloom_executions(namespace,project_id,generation,kind,status,created_at);
CREATE INDEX IF NOT EXISTS xloom_execution_version ON xloom_executions(namespace,project_id,kind,state_version);
CREATE INDEX IF NOT EXISTS xloom_execution_step ON xloom_executions(project_id,intent) WHERE kind!='reason';`)
	return err
}

func (t *Tx) PendingExecutions(namespace string, after int64, limit int) (ExecutionPage, error) {
	page := ExecutionPage{Items: []ExecutionSummary{}}
	if namespace == "" || after < 0 || limit < 1 || limit > 100 {
		return page, Err(422, "namespace, nonnegative after and limit between 1 and 100 are required")
	}
	rows, err := t.Query("SELECT rowid,"+executionSummaryColumns+" FROM xloom_executions WHERE namespace=? AND rowid>? AND "+pendingExecutionSQL+" ORDER BY rowid LIMIT ?", namespace, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var rowid int64
		e, err := scanExecutionSummary(rows, &rowid)
		if err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			page.NextCursor = last
			break
		}
		page.Items = append(page.Items, e)
		last = rowid
	}
	return page, rows.Err()
}

func (t *Tx) GetExecutionSummary(namespace, project, id string) (ExecutionSummary, error) {
	e, err := scanExecutionSummary(t.QueryRow("SELECT "+executionSummaryColumns+" FROM xloom_executions WHERE namespace=? AND project_id=? AND id=?", namespace, project, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = Err(404, "Execution not found")
	}
	return e, err
}

func (t *Tx) CheckExecutions(q ExecutionCheckQuery) (ExecutionCheck, error) {
	var out ExecutionCheck
	// Existing pending work and human grants remain visible across generations;
	// recovery is responsible for cancelling stale pending work.
	err := t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE namespace=? AND project_id=? AND kind=? AND (?='reason' OR intent=?) AND `+pendingExecutionSQL+`)`, q.Namespace, q.ProjectID, q.Kind, q.Kind, q.Intent).Scan(&out.Pending)
	if err != nil {
		return out, err
	}
	err = t.QueryRow(`SELECT id FROM xloom_executions WHERE namespace=? AND project_id=? AND kind=? AND intent=? AND status='retry_requested' ORDER BY created_at DESC,rowid DESC LIMIT 1`, q.Namespace, q.ProjectID, q.Kind, q.Intent).Scan(&out.PreviousRunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	var tried bool
	err = t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE namespace=? AND project_id=? AND kind=? AND retry_key=? AND status NOT IN ('retry_requested','retried'))`, q.Namespace, q.ProjectID, q.Kind, q.RetryKey).Scan(&tried)
	if err != nil {
		return out, err
	}
	out.Blocked = out.Pending || (tried && out.PreviousRunID == "")
	err = t.QueryRow(`SELECT COUNT(*) FROM xloom_executions WHERE namespace=? AND project_id=? AND kind=? AND generation=? AND retry_key=?`, q.Namespace, q.ProjectID, q.Kind, q.Generation, q.RetryKey).Scan(&out.Attempts)
	if err != nil {
		return out, err
	}
	if q.StateVersion != "" {
		// The state hash already binds its own generation. Old jobs without a
		// Decision marker could carry a State whose graph differs from Job.Graph;
		// repeated-input detection historically followed that State, not the job.
		err = t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE namespace=? AND project_id=? AND kind='reason' AND state_version=?)`, q.Namespace, q.ProjectID, q.StateVersion).Scan(&out.Repeated)
		if err != nil {
			return out, err
		}
	}
	if q.Kind == "reason" {
		latest, err := scanExecutionSummary(t.QueryRow(`SELECT `+executionSummaryColumns+` FROM xloom_executions WHERE namespace=? AND project_id=? AND generation=? AND kind='reason' AND status='succeeded' ORDER BY created_at DESC,rowid DESC LIMIT 1`, q.Namespace, q.ProjectID, q.Generation))
		if err == nil {
			out.LatestDecision = &latest
		} else if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	if q.Kind != "reason" || out.Attempts != 1 || out.Pending || out.PreviousRunID != "" {
		return out, nil
	}
	// Extract only the small failure classification. Never materialize Result
	// or Job just to decide whether this one candidate merits a retry request.
	var candidate Execution
	var status, failureKind string
	err = t.QueryRow(`SELECT id,status,`+executionFailureColumns+` FROM xloom_executions WHERE namespace=? AND project_id=? AND generation=? AND kind='reason' AND retry_key=? LIMIT 1`, q.Namespace, q.ProjectID, q.Generation, q.RetryKey).Scan(&candidate.ID, &candidate.Status, &status, &failureKind)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if automaticDecisionRetryEligible("reason", candidate.Status, status, failureKind) {
		out.AutomaticRetryID = candidate.ID
	}
	return out, nil
}

// Scheduling needs only admission and retry grants for the current page's
// Execute candidates. Registration still verifies their leases and identity.
func (t *Tx) ScheduleExecutionChecks(project, namespace string, intents []Intent) (map[string]ExecutionCheck, error) {
	checks := make(map[string]ExecutionCheck, len(intents))
	if len(intents) == 0 {
		return checks, nil
	}
	values := make([]string, len(intents))
	args := make([]any, 0, 3*len(intents)+2)
	for n, i := range intents {
		kind := "explore"
		if i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin" {
			kind = "bootstrap"
		}
		values[n] = "(?,?,?)"
		args = append(args, kind, i.ID, kind+":"+i.ID)
	}
	args = append(args, namespace, project)
	rows, err := t.Query(`WITH candidates(kind,intent,retry_key) AS (VALUES `+strings.Join(values, ",")+`),
	registry AS (SELECT rowid AS sequence,* FROM xloom_executions WHERE namespace=? AND project_id=?)
	SELECT c.retry_key,
	EXISTS(SELECT 1 FROM registry e WHERE e.kind=c.kind AND e.intent=c.intent AND `+pendingExecutionSQL+`),
	COALESCE((SELECT e.id FROM registry e WHERE e.kind=c.kind AND e.intent=c.intent AND e.status='retry_requested' ORDER BY e.created_at DESC,e.sequence DESC LIMIT 1),''),
	EXISTS(SELECT 1 FROM registry e WHERE e.kind=c.kind AND e.retry_key=c.retry_key AND e.status NOT IN ('retry_requested','retried'))
	FROM candidates c`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var check ExecutionCheck
		var tried bool
		if err := rows.Scan(&key, &check.Pending, &check.PreviousRunID, &tried); err != nil {
			return nil, err
		}
		check.Blocked = check.Pending || (tried && check.PreviousRunID == "")
		checks[key] = check
	}
	return checks, rows.Err()
}
