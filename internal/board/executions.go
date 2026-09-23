package board

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

const executionSchema = `CREATE TABLE IF NOT EXISTS xloom_executions(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,namespace TEXT NOT NULL,backend TEXT NOT NULL,kind TEXT NOT NULL,intent TEXT NOT NULL,
 lease TEXT NOT NULL,job BLOB NOT NULL,retry_key TEXT NOT NULL,status TEXT NOT NULL,result BLOB,
 resumes INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 PRIMARY KEY(project_id,id));
CREATE TABLE IF NOT EXISTS xloom_paused_executions(
 project_id TEXT NOT NULL,execution_id TEXT NOT NULL,
 PRIMARY KEY(project_id,execution_id),
 FOREIGN KEY(project_id,execution_id) REFERENCES xloom_executions(project_id,id) ON DELETE CASCADE);` + inputSnapshotSchema + decisionReadSchema

// Execution contains no backend environment or model credentials. Job is the
// immutable input supplied to the Worker, not a newly loaded project snapshot.
type Execution struct {
	ProjectID string          `json:"project_id"`
	ID        string          `json:"id"`
	Namespace string          `json:"namespace"`
	Backend   string          `json:"backend"`
	Kind      string          `json:"kind"`
	Intent    string          `json:"intent"`
	Lease     string          `json:"lease"`
	Job       json.RawMessage `json:"job"`
	RetryKey  string          `json:"retry_key"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Resumes   int             `json:"resumes"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func (e Execution) Fence() ExecutionFence {
	return ExecutionFence{Run: e.Lease, Lease: e.Kind, Intent: e.Intent}
}
func (e Execution) Pending() bool {
	return e.Status == "prepared" || e.Status == "running" || e.Status == "retryable" || e.Status == "result_pending"
}

const executionColumns = `project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,result,resumes,created_at,updated_at`

type scanner interface{ Scan(...any) error }

func scanExecution(s scanner) (Execution, error) {
	var e Execution
	var job, result []byte
	err := s.Scan(&e.ProjectID, &e.ID, &e.Namespace, &e.Backend, &e.Kind, &e.Intent, &e.Lease, &job, &e.RetryKey, &e.Status, &result, &e.Resumes, &e.CreatedAt, &e.UpdatedAt)
	e.Job, e.Result = job, result
	return e, err
}
func (t *Tx) Execution(project, id string) (Execution, error) {
	e, err := scanExecution(t.QueryRow("SELECT "+executionColumns+" FROM xloom_executions WHERE project_id=? AND id=?", project, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = Err(404, "Execution not found")
	}
	return e, err
}
func (t *Tx) Executions(namespace string) ([]Execution, error) {
	rows, err := t.Query("SELECT "+executionColumns+" FROM xloom_executions WHERE namespace=? ORDER BY created_at,rowid", namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Execution{}
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (t *Tx) RegisterExecution(e Execution) error {
	var snapshotJob struct {
		InputSnapshot *InputSnapshot `json:"input_snapshot"`
	}
	if err := json.Unmarshal(e.Job, &snapshotJob); err != nil {
		return Err(422, "invalid execution job")
	}
	if ref := snapshotJob.InputSnapshot; ref != nil {
		saved, err := t.InputSnapshotMetadata(e.ProjectID, ref.ID)
		if err != nil {
			return err
		}
		if *saved != *ref {
			return Err(422, "execution snapshot binding mismatch")
		}
	}
	if _, err := DecisionJobVersion(e.Job); err != nil {
		return err
	}
	metadata, err := executionInputMetadata(e.Job)
	if err != nil {
		return Err(422, "invalid execution job")
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	// A new lease can race a restart after the dispatcher read its input. Check
	// the immutable job's round, even if that newly claimed lease is valid.
	if metadata.Generation != g.Project.Generation {
		return Err(409, "Execution input belongs to a previous project round")
	}
	if err = t.CheckExecution(g, e.Fence()); err != nil {
		return err
	}
	if e.ID == "" || e.Lease != e.Backend+"@"+e.ID || e.RetryKey == "" || e.Namespace == "" {
		return Err(422, "invalid execution identity")
	}
	if e.Kind != "reason" {
		if e.RetryKey != e.Kind+":"+e.Intent {
			return Err(422, "Execute retry key must be bound to its step")
		}
	}
	prior, err := t.Execution(e.ProjectID, e.ID)
	if err == nil {
		if prior.Namespace != e.Namespace || prior.Backend != e.Backend || prior.Kind != e.Kind || prior.Intent != e.Intent || prior.Lease != e.Lease || string(prior.Job) != string(e.Job) || prior.RetryKey != e.RetryKey {
			return Err(409, "Execution input is immutable")
		}
		return nil
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		return err
	}
	if e.Kind != "reason" {
		if err = t.StepReady(e.ProjectID, e.Intent); err != nil {
			return err
		}
	}
	var job struct {
		PreviousRunID string `json:"previous_run_id"`
	}
	if err = json.Unmarshal(e.Job, &job); err != nil {
		return Err(422, "invalid execution job")
	}
	var grant *ExecutionSummary
	if job.PreviousRunID != "" {
		previous, err := t.GetExecutionSummary(e.Namespace, e.ProjectID, job.PreviousRunID)
		if err != nil {
			return err
		}
		if previous.Status != "retry_requested" || previous.Namespace != e.Namespace || previous.Kind != e.Kind || previous.Intent != e.Intent {
			return Err(409, "previous_run_id does not identify an available retry for this task")
		}
		grant = &previous
	}
	var blocked bool
	err = t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE project_id=? AND namespace=? AND retry_key=? AND status IN ('prepared','running','retryable','result_pending','failed','rejected','cancelled','succeeded','retry_requested','retried'))`, e.ProjectID, e.Namespace, e.RetryKey).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked && grant == nil {
		return Err(409, "Execution requires new task evidence or an explicit retry")
	}
	if grant != nil {
		// Consume the one-use authorization in the same transaction as the new
		// registration. A competing attempt cannot reuse this parent afterward.
		if _, err = t.Exec("UPDATE xloom_executions SET status='retried',updated_at=? WHERE project_id=? AND id=? AND status='retry_requested'", t.Now, grant.ProjectID, grant.ID); err != nil {
			return err
		}
	}
	args := []any{e.ProjectID, e.ID, e.Namespace, e.Backend, e.Kind, e.Intent, e.Lease, []byte(e.Job), e.RetryKey, t.Now, t.Now}
	args = append(args, executionMetadataValues(metadata)...)
	_, err = t.Exec(`INSERT INTO xloom_executions(`+executionColumns+`,`+executionMetadataColumns+`,metadata_version) VALUES(?,?,?,?,?,?,?,?,?,'prepared',NULL,0,?,?,?,?,?,?,?,?,?,?,1)`, args...)
	return err
}
func (t *Tx) ExecutionStatus(e Execution, status string, result json.RawMessage) error {
	// Re-read rather than trusting a caller's earlier snapshot. State transitions
	// and pending-result immutability are enforced inside the same transaction.
	current, err := t.Execution(e.ProjectID, e.ID)
	if err != nil {
		return err
	}
	if len(result) == 0 {
		result = current.Result
	}
	failure := result
	if current.Status == status && bytes.Equal(current.Result, result) {
		return nil
	}
	if !slices.Contains([]string{"running", "retryable", "result_pending", "failed", "cancelled", "rejected", "succeeded", "retry_requested"}, status) {
		return Err(422, "invalid execution status")
	}
	if status == "retry_requested" {
		if current.Status != "failed" && current.Status != "rejected" && current.Status != "cancelled" {
			return Err(409, "only a failed, rejected or cancelled execution can authorize a new attempt")
		}
	} else if !current.Pending() {
		return Err(409, "execution is terminal")
	}
	if current.Status == "result_pending" {
		if status == "failed" || status == "cancelled" {
			// A failed delivery or cancellation terminates this attempt, but
			// cannot replace the already durable business result with an error.
			result = current.Result
		}
		if !bytes.Equal(current.Result, result) || status == "running" || status == "retryable" {
			return Err(409, "pending result is immutable and cannot return to execution")
		}
	}
	if status == "succeeded" && current.Status != "result_pending" {
		return Err(409, "only a pending result can be applied successfully")
	}
	if status == "running" && current.Status != "running" && current.Kind != "reason" {
		if err = t.StepReady(current.ProjectID, current.Intent); err != nil {
			return err
		}
	}
	_, err = t.Exec(`UPDATE xloom_executions SET status=?,result=?,updated_at=? WHERE project_id=? AND id=?`, status, []byte(result), t.Now, e.ProjectID, e.ID)
	if err == nil && slices.Contains([]string{"failed", "rejected", "cancelled", "succeeded"}, status) {
		_, err = t.Exec("INSERT OR IGNORE INTO xloom_revoked_runs(project_id,worker) VALUES(?,?)", e.ProjectID, current.Lease)
	}
	if err == nil && current.Kind != "reason" && slices.Contains([]string{"failed", "rejected", "cancelled"}, status) {
		err = t.recordExecutionFailure(current, status, failure)
	}
	return err
}

// Resume reclaims only an unowned or same-owner lease. A human stop or another
// execution's lease cannot be overridden by a saved transcript.
func (t *Tx) ResumeExecution(e Execution) error {
	current, err := t.Execution(e.ProjectID, e.ID)
	if err != nil {
		return err
	}
	e = current
	if !e.Pending() {
		return Err(409, "Execution is terminal")
	}
	if e.Resumes >= 2 {
		return Err(409, "Dispatcher recovery allowance exhausted")
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	revoked, err := t.RunRevoked(e.ProjectID, e.Lease)
	if err != nil {
		return err
	}
	if revoked {
		return Err(409, "Execution was revoked")
	}
	if e.Kind == "reason" {
		if g.Project.Reason != nil && g.Project.Reason.Worker != e.Lease {
			return Err(409, "Another execution owns the decision")
		}
		if g.Project.Reason == nil {
			g.Project.Reason = &Reason{Worker: e.Lease, Trigger: "recovery", StartedAt: e.CreatedAt, Heartbeat: t.Now}
		} else {
			g.Project.Reason.Heartbeat = t.Now
		}
	} else {
		if err = t.StepAvailable(e.ProjectID, e.Intent); err != nil {
			return err
		}
		if e.Status != "result_pending" {
			if err = t.StepReady(e.ProjectID, e.Intent); err != nil {
				return err
			}
		}
		found := false
		for n := range g.Intents {
			i := &g.Intents[n]
			if i.ID != e.Intent {
				continue
			}
			found = true
			if i.To != nil || i.ConcludedAt != nil {
				return Err(409, "Step has already ended")
			}
			if i.Worker != nil && *i.Worker != e.Lease {
				return Err(409, "Another execution owns the step")
			}
			i.Worker = Ptr(e.Lease)
			i.Heartbeat = Ptr(t.Now)
		}
		if !found {
			return Err(404, "Step not found")
		}
	}
	if err = t.Save(g); err != nil {
		return err
	}
	_, err = t.Exec(`UPDATE xloom_executions SET resumes=resumes+1,updated_at=? WHERE project_id=? AND id=?`, t.Now, e.ProjectID, e.ID)
	return err
}

func (t *Tx) HasDecisionActions(project, run string) (bool, error) {
	var found bool
	err := t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_state_events WHERE project_id=? AND json_extract(event,'$.run_id')=? AND json_extract(event,'$.op') IN ('goal','step','fact_relation'))`, project, run).Scan(&found)
	return found, err
}

// CheckNewStepLimit counts every direction this decision already created,
// including subsequently abandoned steps. Tool submissions and the final
// structured result share one registered budget.
func (t *Tx) CheckNewStepLimit(project, run string) error {
	var raw []byte
	err := t.QueryRow("SELECT job FROM xloom_executions WHERE project_id=? AND lease=? AND kind='reason' ORDER BY rowid DESC LIMIT 1", project, run).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Legacy clients may use leases without execution registration.
	}
	if err != nil {
		return err
	}
	var job struct {
		Budget struct {
			MaxIntents int `json:"max_intents"`
		} `json:"budget"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		return err
	}
	if job.Budget.MaxIntents <= 0 {
		return Err(422, "registered decision has no valid direction budget")
	}
	var count int
	if err = t.QueryRow("SELECT COUNT(*) FROM intents WHERE project_id=? AND creator=? AND (to_fact_id IS NULL OR to_fact_id!='goal')", project, run).Scan(&count); err != nil {
		return err
	}
	if count >= job.Budget.MaxIntents {
		return Err(409, "decision reached its new-step budget")
	}
	return nil
}

func ValidExecutionID(id string) bool {
	return required(id, 128) && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\r\n")
}
