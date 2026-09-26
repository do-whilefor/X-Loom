package board

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

type DecisionAction struct {
	Op      string          `json:"op"`
	Ref     string          `json:"ref,omitempty"`
	Payload json.RawMessage `json:"payload"`
}
type DecisionBatch struct {
	ExpectedVersion string           `json:"expected_version"`
	Actions         []DecisionAction `json:"actions"`
}
type DecisionReceipt struct {
	Committed        bool                `json:"committed"`
	StateVersion     string              `json:"state_version,omitempty"`
	Results          []StateActionResult `json:"results"`
	ChangedActions   int                 `json:"changed_actions,omitempty"`
	IDs              map[string]string   `json:"ids"`
	Completed        bool                `json:"completed"`
	ValidationScope  string              `json:"validation_scope,omitempty"`
	CompletionReview *CompletionReview   `json:"completion_review,omitempty"`
}

// CheckDirectDecisionWrite keeps old jobs' protocol while fencing all writes
// by a registered version 2 planner into its one transactional commit.
func (t *Tx) CheckDirectDecisionWrite(project string, fence ExecutionFence) error {
	if t.inDecisionBatch || fence.Run == "" || fence.Lease != "reason" {
		return nil
	}
	var raw []byte
	err := t.QueryRow("SELECT job FROM xloom_executions WHERE project_id=? AND lease=? AND kind='reason'", project, fence.Run).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var job struct {
		Decision *struct {
			Version int `json:"version"`
		} `json:"decision"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		return err
	}
	if job.Decision != nil && job.Decision.Version >= 2 {
		return Err(403, "Decide version 2 must submit its plan through a decision batch")
	}
	return nil
}

func (t *Tx) batchExecution(project string, fence ExecutionFence) (Execution, error) {
	if fence.Run == "" || fence.Lease != "reason" || fence.Intent != "" {
		return Execution{}, Err(403, "decision batches require a registered Decide lease")
	}
	e, err := scanExecution(t.QueryRow("SELECT "+executionColumns+" FROM xloom_executions WHERE project_id=? AND lease=? AND kind='reason'", project, fence.Run))
	if errors.Is(err, sql.ErrNoRows) {
		return Execution{}, Err(403, "decision batches require a registered Decide execution")
	}
	if err != nil {
		return e, err
	}
	var job struct {
		Graph    Graph `json:"graph"`
		Decision *struct {
			Version int `json:"version"`
		} `json:"decision"`
	}
	if json.Unmarshal(e.Job, &job) != nil || job.Decision == nil || job.Decision.Version != 2 {
		return e, Err(403, "decision batches require Decide protocol version 2")
	}
	if _, err = DecisionJobVersion(e.Job); err != nil {
		return e, err
	}
	g, err := t.Load(project)
	if err != nil {
		return e, err
	}
	if job.Graph.Project.Generation != g.Project.Generation {
		return e, Err(409, "decision belongs to a previous project round")
	}
	return e, nil
}

func emptyDecisionReceipt() DecisionReceipt {
	return DecisionReceipt{Results: []StateActionResult{}, IDs: map[string]string{}}
}

func (t *Tx) savedDecision(project, lease string) (DecisionReceipt, string, error) {
	out := emptyDecisionReceipt()
	var request, response string
	err := t.QueryRow("SELECT request,response FROM xloom_state_actions WHERE project_id=? AND idempotency_key=?", project, "decision:"+lease).Scan(&request, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return out, "", nil
	}
	if err != nil {
		return out, "", err
	}
	err = json.Unmarshal([]byte(response), &out)
	return out, request, err
}

func (t *Tx) DecisionReceipt(project string, fence ExecutionFence) (DecisionReceipt, error) {
	e, err := t.batchExecution(project, fence)
	if err != nil {
		return emptyDecisionReceipt(), err
	}
	out, _, err := t.savedDecision(project, fence.Run)
	if err != nil {
		return out, err
	}
	if out.Committed && e.Status == "succeeded" {
		return out, nil
	}
	g, err := t.Load(project)
	if err == nil {
		err = t.CheckExecution(g, fence)
	}
	if err == nil {
		err = g.RequireActive()
	}
	return out, err
}

func (t *Tx) PreviewDecision(project string, fence ExecutionFence, batch DecisionBatch) (DecisionReceipt, error) {
	return t.decisionBatch(project, fence, batch, false)
}
func (t *Tx) CommitDecision(project string, fence ExecutionFence, batch DecisionBatch) (DecisionReceipt, error) {
	return t.decisionBatch(project, fence, batch, true)
}

var decisionAlias = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func normalizeDecisionActions(actions []DecisionAction) ([]DecisionAction, error) {
	if len(actions) > 64 {
		return nil, Err(422, "decision batch may contain at most 64 actions")
	}
	normalized := make([]DecisionAction, len(actions))
	refs := map[string]bool{}
	for n, action := range actions {
		if !slices.Contains([]string{"goal", "step", "fact_relation", "complete"}, action.Op) {
			return nil, Err(422, "invalid decision action "+action.Op)
		}
		if action.Op == "complete" && n != len(actions)-1 {
			return nil, Err(422, "complete must be the last decision action")
		}
		payload, err := decisionPayload(action.Payload)
		if err != nil {
			return nil, err
		}
		if action.Ref != "" {
			if !decisionAlias.MatchString(action.Ref) || refs[action.Ref] || (action.Op != "goal" && action.Op != "step") || payload["action"] != "add" {
				return nil, Err(422, "ref must uniquely name a newly added goal or step")
			}
			refs[action.Ref] = true
		}
		action.Payload, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		normalized[n] = action
	}
	return normalized, nil
}

func decisionPayload(raw json.RawMessage) (map[string]any, error) {
	var payload map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&payload) != nil || payload == nil {
		return nil, Err(422, "decision payload must be an object")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, Err(422, "decision payload must contain one JSON object")
	}
	return payload, nil
}

func resolveDecisionReferences(raw json.RawMessage, ids map[string]string) (json.RawMessage, error) {
	payload, err := decisionPayload(raw)
	if err != nil {
		return nil, err
	}
	resolve := func(value any) (any, error) {
		id, ok := value.(string)
		if !ok || !strings.HasPrefix(id, "$") {
			return value, nil
		}
		if saved, ok := ids[strings.TrimPrefix(id, "$")]; ok {
			return saved, nil
		}
		return nil, Err(422, "unknown or forward decision reference "+id)
	}
	for _, key := range []string{"id", "parent_id", "goal_id", "source", "target"} {
		if value, ok := payload[key]; ok {
			payload[key], err = resolve(value)
			if err != nil {
				return nil, err
			}
		}
	}
	for _, key := range []string{"from", "sources"} {
		if values, ok := payload[key].([]any); ok {
			for n, value := range values {
				values[n], err = resolve(value)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return json.Marshal(payload)
}

func (t *Tx) decisionBatch(project string, fence ExecutionFence, batch DecisionBatch, commit bool) (out DecisionReceipt, err error) {
	out = emptyDecisionReceipt()
	e, err := t.batchExecution(project, fence)
	if err != nil {
		return out, err
	}
	actions, err := normalizeDecisionActions(batch.Actions)
	if err != nil {
		return out, err
	}
	canonical, _ := json.Marshal(actions)
	prior, request, err := t.savedDecision(project, fence.Run)
	if err != nil {
		return out, err
	}
	if prior.Committed {
		if request != string(canonical) {
			return out, Err(409, "this Decide execution already committed a different batch")
		}
		if e.Status != "succeeded" {
			return out, Err(409, "decision receipt has no successful execution")
		}
		return prior, nil
	}
	state, err := t.State(project)
	if err != nil {
		return out, err
	}
	if err = state.Graph.RequireActive(); err != nil {
		return out, err
	}
	if !e.Pending() {
		return out, Err(409, "decision execution is terminal")
	}
	if err = t.CheckExecution(state.Graph, fence); err != nil {
		return out, err
	}
	if err = t.CheckDecisionStateVersion(state, fence, batch.ExpectedVersion); err != nil {
		return out, err
	}
	if t.inDecisionBatch {
		return out, Err(409, "nested decision batch is not allowed")
	}
	if _, err = t.Exec("SAVEPOINT xloom_decision_batch"); err != nil {
		return out, err
	}
	keep := false
	t.inDecisionBatch = true
	defer func() {
		t.inDecisionBatch = false
		if !keep {
			_, rollbackErr := t.Exec("ROLLBACK TO xloom_decision_batch")
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := t.Exec("RELEASE xloom_decision_batch")
		err = errors.Join(err, releaseErr)
	}()
	for n, action := range actions {
		payload, resolveErr := resolveDecisionReferences(action.Payload, out.IDs)
		if resolveErr != nil {
			return out, resolveErr
		}
		before := state
		result, actionErr := t.stateAction(&state, fence, StateAction{Op: action.Op, Payload: payload, IdempotencyKey: fmt.Sprintf("decision:%s:%d", fence.Run, n)})
		if actionErr != nil {
			var api *APIError
			if errors.As(actionErr, &api) {
				return out, &APIError{Status: api.Status, Detail: map[string]any{"action": n + 1, "op": action.Op, "error": api.Detail}}
			}
			return out, fmt.Errorf("decision action %d (%s): %w", n+1, action.Op, actionErr)
		}
		if !commit && action.Op == "complete" {
			out.CompletionReview, err = buildCompletionReview(before, batch.ExpectedVersion, payload, MaxCompletionReviewBytes)
			if err != nil {
				return out, err
			}
		}
		out.Results = append(out.Results, result)
		if !result.Unchanged {
			out.ChangedActions++
		}
		if action.Ref != "" {
			out.IDs[action.Ref] = result.ID
		}
	}
	if len(actions) == 0 && !slices.ContainsFunc(state.Steps, func(step Step) bool {
		return (step.Status == "open" || step.Status == "running") && len(step.InvalidSources) == 0
	}) {
		return out, Err(422, "empty decision would leave the project idle: add an executable Step or propose complete with supporting facts and proof; draft unchanged")
	}
	out.StateVersion = DecisionStateVersion(state)
	if !commit {
		out.ValidationScope = "protocol_only"
		return out, nil
	}
	out.Committed = true
	out.Completed = state.Graph.Project.Status == "completed"
	response, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = t.Exec("INSERT INTO xloom_state_actions(project_id,idempotency_key,request,response) VALUES(?,?,?,?)", project, "decision:"+fence.Run, string(canonical), string(response)); err != nil {
		return out, err
	}
	result, _ := json.Marshal(map[string]any{"status": "success", "type": "result", "text": `{"accepted":true,"data":{"decided":true}}`, "state_version": out.StateVersion})
	if err = t.ExecutionStatus(e, "result_pending", result); err != nil {
		return out, err
	}
	if err = t.ExecutionStatus(e, "succeeded", result); err != nil {
		return out, err
	}
	// The execution receipt is the durable delivery acknowledgement. Release
	// its planner lease now; process cancellation happens outside this transaction.
	if state.Graph.Project.Reason != nil && state.Graph.Project.Reason.Worker == fence.Run {
		state.Graph.Project.Reason = nil
		if _, err = t.Exec("UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE id=?", project); err != nil {
			return out, err
		}
	}
	keep = true
	return out, nil
}

func (t *Tx) CompleteProject(project string, fence ExecutionFence, from []string, description string) (Intent, error) {
	if fence.Lease != "reason" && fence.Lease != "bootstrap" {
		return Intent{}, Err(403, "only Decide or a legacy bootstrap can complete the project")
	}
	if err := t.CheckDirectDecisionWrite(project, fence); err != nil {
		return Intent{}, err
	}
	if !required(description, 16384) {
		return Intent{}, Err(422, "completion description is required")
	}
	s, err := t.State(project)
	if err != nil {
		return Intent{}, err
	}
	if err = s.Graph.RequireActive(); err != nil {
		return Intent{}, err
	}
	if err = t.CheckExecution(s.Graph, fence); err != nil {
		return Intent{}, err
	}
	if err = s.ValidateFactSources(from, true); err != nil {
		return Intent{}, err
	}
	if err = t.ValidateStateCompletion(project, from); err != nil {
		return Intent{}, err
	}
	for _, step := range s.Steps {
		if step.Status == "open" || step.Status == "running" || step.Status == "needs_review" {
			return Intent{}, Err(409, "Project has an active step; resolve it explicitly before completing")
		}
	}
	id, err := t.Next(project, "intent")
	if err != nil {
		return Intent{}, err
	}
	i := Intent{ID: id, From: from, To: Ptr("goal"), Description: strings.TrimSpace(description), Creator: fence.Run, Worker: Ptr(fence.Run), Heartbeat: Ptr(t.Now), CreatedAt: t.Now, ConcludedAt: Ptr(t.Now)}
	s.Graph.Intents = append(s.Graph.Intents, i)
	s.Graph.Project.Status, s.Graph.Project.Reason = "completed", nil
	return i, t.Save(s.Graph)
}
