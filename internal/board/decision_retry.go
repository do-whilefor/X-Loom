package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// AutomaticDecisionRetryEligible distinguishes exhausted infrastructure/time
// recovery from a refusal, invalid plan, configuration error or human stop.
func AutomaticDecisionRetryEligible(e Execution) bool {
	if e.Kind != "reason" || e.Status != "failed" {
		return false
	}
	var result struct {
		Status      string `json:"status"`
		FailureKind string `json:"failure_kind"`
	}
	if json.Unmarshal(e.Result, &result) != nil || result.Status != "failed" {
		return false
	}
	return automaticDecisionRetryEligible(e.Kind, e.Status, result.Status, result.FailureKind)
}

func automaticDecisionRetryEligible(kind, status, resultStatus, failureKind string) bool {
	return kind == "reason" && status == "failed" && resultStatus == "failed" && slices.Contains([]string{"recovery_exhausted", "budget_exhausted", "request_timeout", "transient_infrastructure", "transport", "rate_limit", "unavailable"}, failureKind)
}

// RequestAutomaticDecisionRetry grants one successor for unchanged input. The
// existing registry is the durable allowance: a second registered attempt with
// this retry key exhausts it, even after dispatcher restart. Human retry remains
// a separate operation and does not replenish the automatic allowance.
func (t *Tx) RequestAutomaticDecisionRetry(e Execution) error {
	var resultStatus, failureKind string
	current, err := scanExecutionSummary(t.QueryRow("SELECT "+executionFailureColumns+","+executionSummaryColumns+" FROM xloom_executions WHERE project_id=? AND id=?", e.ProjectID, e.ID), &resultStatus, &failureKind)
	if errors.Is(err, sql.ErrNoRows) {
		return Err(404, "Execution not found")
	}
	if err != nil {
		return err
	}
	status := current.Status
	if status == "retry_requested" {
		status = "failed" // A lost grant response may be retried.
	}
	if !automaticDecisionRetryEligible(current.Kind, status, resultStatus, failureKind) {
		return Err(409, "Decision failure does not allow automatic retry")
	}
	var projectStatus string
	var generation int64
	var reasonWorker *string
	if err = t.QueryRow(`SELECT p.status,COALESCE(round.generation,0),p.reason_worker FROM projects p LEFT JOIN xloom_project_rounds round ON round.project_id=p.id WHERE p.id=?`, current.ProjectID).Scan(&projectStatus, &generation, &reasonWorker); err != nil {
		return err
	}
	if projectStatus != "active" {
		return Err(403, "Project is "+projectStatus)
	}
	if current.Generation != generation {
		return Err(409, "Execution input belongs to a previous project round")
	}
	var attempts int
	if err = t.QueryRow("SELECT COUNT(*) FROM xloom_executions WHERE project_id=? AND namespace=? AND kind='reason' AND retry_key=? AND generation=?", current.ProjectID, current.Namespace, current.RetryKey, generation).Scan(&attempts); err != nil {
		return err
	}
	if attempts != 1 {
		return Err(409, "Automatic decision retry allowance exhausted for this input")
	}
	var pending bool
	if err = t.QueryRow("SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE project_id=? AND id!=? AND kind='reason' AND status IN ('prepared','running','retryable','result_pending','retry_requested'))", current.ProjectID, current.ID).Scan(&pending); err != nil {
		return err
	}
	if pending || reasonWorker != nil && *reasonWorker != current.Lease {
		return Err(409, "Another decision is active or authorized")
	}
	// Terminal failures are already revoked. Revoke defensively and release
	// only this attempt's lease in the same transaction as its one-use grant.
	if _, err = t.Exec("INSERT OR IGNORE INTO xloom_revoked_runs(project_id,worker) VALUES(?,?)", current.ProjectID, current.Lease); err != nil {
		return err
	}
	if reasonWorker != nil {
		if _, err = t.Exec("UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE id=?", current.ProjectID); err != nil {
			return err
		}
	}
	// Eligibility and the grant share this transaction. Preserve the saved Job
	// and Result in place: authorizing a successor never restores the old run.
	if current.Status == "retry_requested" {
		return nil
	}
	_, err = t.Exec("UPDATE xloom_executions SET status='retry_requested',updated_at=? WHERE project_id=? AND id=?", t.Now, current.ProjectID, current.ID)
	return err
}
