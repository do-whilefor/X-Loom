package board

import (
	"encoding/json"
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
	return slices.Contains([]string{"recovery_exhausted", "budget_exhausted", "request_timeout", "transient_infrastructure", "transport", "rate_limit", "unavailable"}, result.FailureKind)
}

// RequestAutomaticDecisionRetry grants one successor for unchanged input. The
// existing registry is the durable allowance: a second registered attempt with
// this retry key exhausts it, even after dispatcher restart. Human retry remains
// a separate operation and does not replenish the automatic allowance.
func (t *Tx) RequestAutomaticDecisionRetry(e Execution) error {
	current, err := t.Execution(e.ProjectID, e.ID)
	if err != nil {
		return err
	}
	eligible := current
	if eligible.Status == "retry_requested" {
		eligible.Status = "failed" // A lost grant response may be retried.
	}
	if !AutomaticDecisionRetryEligible(eligible) {
		return Err(409, "Decision failure does not allow automatic retry")
	}
	g, err := t.Load(current.ProjectID)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	var job struct {
		Graph Graph `json:"graph"`
	}
	if json.Unmarshal(current.Job, &job) != nil || job.Graph.Project.Generation != g.Project.Generation {
		return Err(409, "Execution input belongs to a previous project round")
	}
	var attempts int
	if err = t.QueryRow("SELECT COUNT(*) FROM xloom_executions WHERE project_id=? AND namespace=? AND retry_key=? AND COALESCE(json_extract(job,'$.graph.project.generation'),0)=?", current.ProjectID, current.Namespace, current.RetryKey, g.Project.Generation).Scan(&attempts); err != nil {
		return err
	}
	if attempts != 1 {
		return Err(409, "Automatic decision retry allowance exhausted for this input")
	}
	var pending bool
	if err = t.QueryRow("SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE project_id=? AND id!=? AND kind='reason' AND status IN ('prepared','running','retryable','result_pending','retry_requested'))", current.ProjectID, current.ID).Scan(&pending); err != nil {
		return err
	}
	if pending || g.Project.Reason != nil && g.Project.Reason.Worker != current.Lease {
		return Err(409, "Another decision is active or authorized")
	}
	// Terminal failures are already revoked. Revoke defensively and release
	// only this attempt's lease in the same transaction as its one-use grant.
	if _, err = t.Exec("INSERT OR IGNORE INTO xloom_revoked_runs(project_id,worker) VALUES(?,?)", current.ProjectID, current.Lease); err != nil {
		return err
	}
	if g.Project.Reason != nil {
		g.Project.Reason = nil
		if err = t.Save(g); err != nil {
			return err
		}
	}
	return t.ExecutionStatus(current, "retry_requested", current.Result)
}
