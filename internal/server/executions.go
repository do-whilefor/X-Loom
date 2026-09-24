package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	b "xloom/internal/board"
	"xloom/internal/contract"
)

func (s *Server) registerExecutionRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/scheduling", s.wrap(s.schedulingInput))
	m.HandleFunc("POST /projects/{pid}/executions/prepare", s.wrap(s.prepareExecution))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/input/read", s.wrap(s.snapshotRead))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/updates", s.wrap(s.executionUpdates))
	m.HandleFunc("GET /executions/pending", s.wrap(s.pendingExecutions))
	m.HandleFunc("GET /projects/{pid}/executions/check", s.wrap(s.executionCheck))
	m.HandleFunc("GET /projects/{pid}/executions/{rid}", s.wrap(s.executionDetail))
	m.HandleFunc("GET /projects/{pid}/executions/{rid}/identity", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("execution_identity", "true")
		s.wrap(s.executionDetail)(w, r)
	})
	for _, op := range []string{"status", "resume", "apply", "retry"} {
		m.HandleFunc("POST /projects/{pid}/executions/{rid}/"+op, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("execution_op", op)
			s.wrap(s.executionAction)(w, r)
		})
	}
}
func decodeFields(q *request, v any) error {
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// Production registrations receive the job built by server-side preparation.
func (s *Server) registerExecution(t *b.Tx, e b.Execution, r *http.Request) (int, any, error) {
	var job struct {
		RunID                 string          `json:"run_id"`
		Kind                  string          `json:"kind"`
		Graph                 b.Graph         `json:"graph"`
		Intent                *b.Intent       `json:"intent"`
		Workspace             string          `json:"workspace"`
		GraphRPC              json.RawMessage `json:"graph_rpc"`
		ResultContractVersion json.RawMessage `json:"result_contract_version"`
	}
	if json.Unmarshal(e.Job, &job) != nil || e.ID == "" || e.Namespace == "" || e.Backend == "" || e.Lease == "" || e.RetryKey == "" || job.RunID != e.ID || job.Kind != e.Kind || job.Graph.Project.ID != e.ProjectID || job.Workspace == "" {
		return 0, nil, b.Err(422, "Invalid execution identity")
	}
	// Absence preserves old jobs. Explicit null or a malformed value must not
	// silently select the compatibility protocol or poison a durable result.
	if len(job.GraphRPC) != 0 {
		var enabled bool
		if strings.TrimSpace(string(job.GraphRPC)) == "null" || json.Unmarshal(job.GraphRPC, &enabled) != nil {
			return 0, nil, b.Err(422, "graph_rpc must be a boolean")
		}
	}
	if len(job.ResultContractVersion) != 0 {
		var version int
		if strings.TrimSpace(string(job.ResultContractVersion)) == "null" || json.Unmarshal(job.ResultContractVersion, &version) != nil || version < 0 || version > 2 {
			return 0, nil, b.Err(422, "result_contract_version must be 0, 1 or 2")
		}
	}
	if !b.ValidExecutionID(e.ID) || len(e.Namespace) > 128 || len(e.Backend) > 256 || len(e.RetryKey) > 1024 || e.Lease != e.Backend+"@"+e.ID {
		return 0, nil, b.Err(422, "Invalid execution identity")
	}
	if e.Kind != "reason" && e.Kind != "bootstrap" && e.Kind != "explore" {
		return 0, nil, b.Err(422, "Invalid execution kind")
	}
	if (e.Kind == "reason" && e.Intent != "") || (e.Kind != "reason" && (job.Intent == nil || job.Intent.ID != e.Intent)) {
		return 0, nil, b.Err(422, "Invalid execution step")
	}
	if r.Header.Get("X-Xloom-Run") != e.Lease || r.Header.Get("X-Xloom-Lease") != e.Kind || r.Header.Get("X-Xloom-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Execution registration requires its lease")
	}
	if err := t.RegisterExecution(e); err != nil {
		return 0, nil, err
	}
	saved, err := t.Execution(e.ProjectID, e.ID)
	return 201, saved, err
}
func (s *Server) executionAction(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	op := r.PathValue("execution_op")
	if op == "retry" {
		if r.Header.Get("X-Xloom-Run") != "" {
			return 0, nil, b.Err(403, "retry authorization is a project-management operation")
		}
		if value, exists := q.fields["automatic"]; exists {
			automatic, ok := value.(bool)
			if !ok {
				return 0, nil, b.Err(422, "automatic must be a boolean")
			}
			if automatic {
				e := b.Execution{ProjectID: r.PathValue("pid"), ID: r.PathValue("rid")}
				return 200, map[string]string{"previous_run_id": e.ID}, t.RequestAutomaticDecisionRetry(e)
			}
		}
	}
	e, err := t.Execution(r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	// Retry is a project-management operation, never a Worker tool. Automatic
	// Decide recovery has a separate server-enforced bound; human retry remains
	// explicit. Both retain the old record and enable one subsequent attempt.
	if op == "retry" {
		if e.Status != "failed" && e.Status != "rejected" && e.Status != "cancelled" {
			return 0, nil, b.Err(409, "Only failed, rejected or cancelled executions can be retried")
		}
		g, err := t.Load(e.ProjectID)
		if err != nil {
			return 0, nil, err
		}
		if err = g.RequireActive(); err != nil {
			return 0, nil, err
		}
		if e.Kind != "reason" {
			if err = t.StepAvailable(e.ProjectID, e.Intent); err != nil {
				return 0, nil, err
			}
			for _, intent := range g.Intents {
				if intent.ID == e.Intent && (intent.To != nil || intent.ConcludedAt != nil) {
					return 0, nil, b.Err(409, "An ended step cannot authorize a retry")
				}
			}
		}
		return 200, map[string]string{"previous_run_id": e.ID}, t.ExecutionStatus(e, "retry_requested", e.Result)
	}
	if r.Header.Get("X-Xloom-Run") != e.Lease || r.Header.Get("X-Xloom-Lease") != e.Kind || r.Header.Get("X-Xloom-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Execution lease mismatch")
	}
	if op == "resume" {
		if err = t.ResumeExecution(e); err != nil {
			return 0, nil, err
		}
		e, err = t.Execution(e.ProjectID, e.ID)
		return 200, e, err
	}
	if op == "apply" {
		return applyExecution(t, e)
	}
	status := q.text("status")
	switch status {
	case "running", "retryable", "result_pending", "failed", "cancelled", "rejected":
	default:
		return 0, nil, b.Err(422, "Invalid execution status")
	}
	if !e.Pending() {
		return 0, nil, b.Err(409, "Execution is terminal")
	}
	if e.Status == "result_pending" && (status == "running" || status == "retryable") {
		return 0, nil, b.Err(409, "Pending result cannot return to execution")
	}
	var result json.RawMessage
	if v, ok := q.fields["result"]; ok {
		result, err = json.Marshal(v)
		if err != nil {
			return 0, nil, err
		}
	}
	if status == "running" || status == "retryable" || status == "result_pending" {
		g, err := t.Load(e.ProjectID)
		if err != nil {
			return 0, nil, err
		}
		if err = g.RequireActive(); err != nil {
			return 0, nil, err
		}
		if err = t.CheckExecution(g, e.Fence()); err != nil {
			return 0, nil, err
		}
	}
	if status == "result_pending" {
		var rr struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(result, &rr) != nil || rr.Status != "success" {
			return 0, nil, b.Err(422, "Pending result must be successful")
		}
	}
	if err = t.ExecutionStatus(e, status, result); err != nil {
		return 0, nil, err
	}
	e, err = t.Execution(e.ProjectID, e.ID)
	return 200, e, err
}

// Final business mutations and their receipt commit in the same SQLite
// transaction. Retrying after a lost HTTP response cannot append duplicates.
func applyExecution(t *b.Tx, e b.Execution) (int, any, error) {
	if e.Status == "succeeded" || e.Status == "rejected" {
		return 200, map[string]string{"status": e.Status}, nil
	}
	if e.Status != "result_pending" {
		return 0, nil, b.Err(409, "Execution has no pending result")
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	if err = g.RequireActive(); err != nil {
		return 0, nil, err
	}
	if err = t.CheckExecution(g, e.Fence()); err != nil {
		return 0, nil, err
	}
	var result struct {
		Text         string `json:"text"`
		Conclude     bool   `json:"conclude"`
		StateVersion string `json:"state_version"`
	}
	if err = json.Unmarshal(e.Result, &result); err != nil {
		return 0, nil, err
	}
	var job struct {
		GraphRPC              bool `json:"graph_rpc"`
		ResultContractVersion int  `json:"result_contract_version"`
		Budget                struct {
			MaxIntents int `json:"max_intents"`
		} `json:"budget"`
	}
	if err = json.Unmarshal(e.Job, &job); err != nil {
		return 0, nil, err
	}
	requiresBatch := false
	if e.Kind == "reason" {
		var protocol struct {
			Decision *struct {
				Version int `json:"version"`
			} `json:"decision"`
		}
		if err = json.Unmarshal(e.Job, &protocol); err != nil {
			return 0, nil, err
		}
		requiresBatch = protocol.Decision != nil && protocol.Decision.Version == 2
		initialVersion, err := b.DecisionJobVersion(e.Job)
		if err != nil {
			return 0, nil, err
		}
		if initialVersion != "" || result.StateVersion != "" {
			state, err := t.State(e.ProjectID)
			if err != nil {
				return 0, nil, err
			}
			if result.StateVersion == "" || result.StateVersion != b.DecisionStateVersion(state) {
				return 0, nil, b.Err(409, "state_changed: decision result does not match current shared state")
			}
		}
	}
	open := 0
	for _, intent := range g.Intents {
		if intent.To == nil && intent.ConcludedAt == nil {
			open++
		}
	}
	// The registered job fixes the submission protocol. A model response or
	// apply request cannot opt a live graph run into legacy plan creation.
	parsed, err := contract.ParseWithPolicy(result.Text, e.Kind, result.Conclude, open, job.Budget.MaxIntents, contract.Policy{Version: job.ResultContractVersion, GraphRPC: job.GraphRPC})
	if err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	if parsed.Outcome == "continue" || parsed.Outcome == "incomplete" {
		return 0, nil, b.Err(422, "Continuing or incomplete worker output cannot be applied as a successful result")
	}
	if requiresBatch && parsed.Kind != "rejected" {
		// Only a refusal can finish without publishing a plan. A successful
		// batch already wrote its receipt; final JSON cannot replace that commit.
		return 0, nil, b.Err(409, "Decide version 2 requires a committed decision batch")
	}
	status := "succeeded"
	// The legacy HTTP field parser normalized creator/worker strings. Retain
	// that boundary when applying old executions through typed business calls.
	legacyWorker := strings.TrimSpace(e.Lease)
	if parsed.Kind == "rejected" {
		status = "rejected"
	} else if e.Kind == "reason" {
		switch parsed.Kind {
		case "intents":
			created := 0
			for _, direction := range parsed.Intents {
				// Preserve the legacy partial-batch behavior: invalid directions are
				// skipped, but SQL errors abort the entire final-result transaction.
				from := direction.From
				if len(from) == 0 || strings.TrimSpace(direction.Description) == "" {
					continue
				}
				if err = g.ValidateSources(from); err != nil {
					continue
				}
				from, desc, validationErr := legacyDirection(direction)
				if validationErr != nil {
					continue
				}
				if _, _, err = createIntent(t, e.ProjectID, e.Fence(), from, desc, legacyWorker, nil); err != nil {
					var validation *b.APIError
					if errors.As(err, &validation) && (validation.Status == 400 || validation.Status == 404 || validation.Status == 422) {
						continue
					}
					return 0, nil, err
				}
				created++
			}
			if created == 0 {
				return 0, nil, b.Err(422, "Decision created no valid steps")
			}
		case "complete":
			from, desc, validationErr := legacyDirection(parsed.Complete)
			if validationErr != nil {
				return 0, nil, validationErr
			}
			if _, _, err = completeProject(t, e.ProjectID, e.Fence(), from, desc, legacyWorker); err != nil {
				return 0, nil, err
			}
		case "noop":
		case "decided":
			committed, err := t.HasDecisionActions(e.ProjectID, e.Lease)
			if err != nil {
				return 0, nil, err
			}
			if !committed {
				return 0, nil, b.Err(409, "Decision has no committed graph actions")
			}
		default:
			return 0, nil, b.Err(422, "Invalid decision result")
		}
	} else {
		var out any
		if job.ResultContractVersion == 2 {
			out, err = t.ConcludeEvidenceStep(e.ProjectID, e.Fence(), parsed.FactID, parsed.FactPayload)
		} else {
			_, out, err = changeIntent(t, e.ProjectID, e.Intent, e.Fence(), "conclude", legacyWorker, parsed.Fact)
		}
		if err != nil {
			return 0, nil, err
		}
		if e.Kind == "bootstrap" && parsed.Kind == "complete" {
			conclusion := out.(b.Conclusion)
			// The bootstrap lease remains valid after concluding its Intent only
			// for this final completion operation, never for arbitrary writes.
			fence := e.Fence()
			fence.AllowConcluded = true
			if _, _, err = completeProject(t, e.ProjectID, fence, []string{conclusion.Fact.ID}, parsed.Complete.Description, legacyWorker); err != nil {
				return 0, nil, err
			}
		}
	}
	if err = t.ExecutionStatus(e, status, e.Result); err != nil {
		return 0, nil, err
	}
	return 200, map[string]string{"status": status}, nil
}

// Legacy parsing intentionally accepts directions independently. Reapply the
// HTTP field rules to typed values without serializing them back into a body.
func legacyDirection(d contract.Direction) ([]string, string, error) {
	if len(d.From) == 0 {
		return nil, "", invalidField("from", "must contain at least one fact id")
	}
	from := make([]string, len(d.From))
	seen := make(map[string]bool, len(d.From))
	for n, value := range d.From {
		id := strings.TrimSpace(value)
		if id == "" {
			return nil, "", invalidField("from", "fact ids must not be empty")
		}
		if seen[id] {
			return nil, "", invalidField("from", "duplicate fact ids")
		}
		seen[id] = true
		from[n] = id
	}
	desc := strings.TrimSpace(d.Description)
	if desc == "" {
		return nil, "", invalidField("description", "must be a non-empty string")
	}
	return from, desc, nil
}
