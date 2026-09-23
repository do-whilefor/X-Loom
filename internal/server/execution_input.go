package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	b "xloom/internal/board"
	"xloom/internal/worker"
)

func (s *Server) schedulingInput(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, nil, b.Err(422, "invalid scheduling offset")
		}
		offset = n
	}
	p, err := t.ScheduleInput(r.PathValue("pid"), offset, r.URL.Query().Get("expected_version"))
	if err == nil && r.URL.Query().Has("namespace") {
		var namespace string
		namespace, err = executionNamespace(r)
		if err == nil {
			p.ExecutionChecks, err = t.ScheduleExecutionChecks(p.Project.ID, namespace, p.Intents)
		}
	}
	return 200, p, err
}

// Freeze, project and register under one transaction. The request carries only
// runtime settings and identities; a growing FGS never enters the HTTP body.
func (s *Server) prepareExecution(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	var e b.Execution
	if err := decodeFields(q, &e); err != nil {
		return 0, nil, b.Err(422, "invalid execution template")
	}
	e.ProjectID = r.PathValue("pid")
	// Inspect presence before decoding into Go scalar fields: an explicit null
	// must not become false/0 and silently select a compatibility protocol.
	var protocol struct {
		GraphRPC              json.RawMessage `json:"graph_rpc"`
		ResultContractVersion json.RawMessage `json:"result_contract_version"`
	}
	if err := json.Unmarshal(e.Job, &protocol); err != nil {
		return 0, nil, b.Err(422, "invalid execution template")
	}
	if len(protocol.GraphRPC) != 0 {
		var enabled bool
		if strings.TrimSpace(string(protocol.GraphRPC)) == "null" || json.Unmarshal(protocol.GraphRPC, &enabled) != nil {
			return 0, nil, b.Err(422, "graph_rpc must be a boolean")
		}
	}
	if len(protocol.ResultContractVersion) != 0 {
		var version int
		if strings.TrimSpace(string(protocol.ResultContractVersion)) == "null" || json.Unmarshal(protocol.ResultContractVersion, &version) != nil || version < 0 || version > 2 {
			return 0, nil, b.Err(422, "result_contract_version must be 0, 1 or 2")
		}
	}
	var j worker.Job
	if json.Unmarshal(e.Job, &j) != nil || j.State != nil || j.InputSnapshot != nil || j.Decision != nil || len(j.InputView) != 0 || j.PreparationKey != "" || j.Graph.Project.ID != e.ProjectID || len(j.Graph.Facts)+len(j.Graph.Intents)+len(j.Graph.Hints) != 0 {
		return 0, nil, b.Err(422, "prepare requires a small job template without graph data")
	}
	if j.Kind == "reason" && j.Intent != nil {
		return 0, nil, b.Err(422, "Decide preparation must not carry an intent")
	}
	if r.Header.Get("X-Xloom-Run") != e.Lease || r.Header.Get("X-Xloom-Lease") != e.Kind || r.Header.Get("X-Xloom-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Execution preparation requires its lease")
	}
	canonical, err := json.Marshal(e)
	if err != nil {
		return 0, nil, err
	}
	sum := sha256.Sum256(canonical)
	key := hex.EncodeToString(sum[:])
	prior, err := t.Execution(e.ProjectID, e.ID)
	if err == nil {
		var old worker.Job
		if json.Unmarshal(prior.Job, &old) != nil || old.PreparationKey != key {
			return 0, nil, b.Err(409, "Execution input is immutable")
		}
		return 200, prior, nil
	}
	var ae *b.APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		return 0, nil, err
	}
	state, err := t.State(e.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	if state.Graph.Project.Generation != j.Graph.Project.Generation {
		return 0, nil, b.Err(409, "state_changed: project round changed before preparation")
	}
	if err = state.Graph.RequireActive(); err != nil {
		return 0, nil, err
	}
	if err = t.CheckExecution(state.Graph, e.Fence()); err != nil {
		return 0, nil, err
	}
	e.RetryKey = e.Kind + ":" + e.Intent
	if e.Kind == "reason" {
		e.RetryKey = b.DecisionRetryKey(state.Graph, state.DecisionRevision)
	}
	check, err := t.CheckExecutions(b.ExecutionCheckQuery{ProjectID: e.ProjectID, Namespace: e.Namespace, Generation: state.Graph.Project.Generation, Kind: e.Kind, Intent: e.Intent, RetryKey: e.RetryKey, StateVersion: b.DecisionStateVersion(state)})
	if err != nil {
		return 0, nil, err
	}
	j.PreviousRunID = check.PreviousRunID
	j.Graph = b.Graph{Project: state.Graph.Project}
	j.DecisionRevision = state.DecisionRevision
	if e.Kind == "reason" {
		var cursor *b.DecisionCursor
		var events []b.StateEvent
		if previous := check.LatestDecision; previous != nil && previous.HasState {
			cursor = &b.DecisionCursor{ProjectID: previous.ProjectID, Generation: previous.Generation, Revision: previous.InputRevision}
			if cursor.Revision <= state.Revision {
				changes, changeErr := t.StateChanges(e.ProjectID, cursor.Revision, state.Revision)
				if changeErr != nil {
					return 0, nil, changeErr
				}
				for _, change := range changes {
					events = append(events, b.StateEvent{Revision: change.Revision, Op: change.Op, ID: change.ID})
				}
			}
		}
		j.Decision, err = b.BuildDecisionContextFromCursor(state, cursor, events, b.DefaultContextViewBytes)
		if err != nil {
			return 0, nil, b.Err(422, err.Error())
		}
		if j.GraphRPC {
			j.Decision.Version = 2
		}
		j.DecisionRepeated = check.Repeated
		j.DecisionTriggers = preparedDecisionTriggers(state, check.LatestDecision, j.DecisionTrigger)
	} else {
		j.Intent = nil
		for _, i := range state.Graph.Intents {
			if i.ID == e.Intent {
				v := i
				j.Intent = &v
				break
			}
		}
		if j.Intent == nil {
			return 0, nil, b.Err(404, "Step not found")
		}
		step := ""
		if e.Kind == "explore" {
			step = e.Intent
		}
		j.InputView, err = b.ContextView(state, step, b.DefaultContextViewBytes)
		if err != nil {
			return 0, nil, b.Err(422, err.Error())
		}
	}
	j.InputSnapshot, err = t.FreezeInput(state)
	if err != nil {
		return 0, nil, err
	}
	j.PreparationKey = key
	e.Job, err = json.Marshal(j)
	if err != nil {
		return 0, nil, err
	}
	return s.registerExecution(t, e, r, true)
}

func preparedDecisionTriggers(state b.State, previous *b.ExecutionSummary, trigger string) []string {
	if previous == nil {
		return []string{trigger}
	}
	causes := []string{}
	if trigger == "explicit_retry" {
		causes = append(causes, trigger)
	}
	if len(state.Graph.Facts) > previous.FactCount {
		causes = append(causes, "new_facts")
	}
	if len(state.Graph.Hints) > previous.HintCount {
		causes = append(causes, "new_hints")
	}
	if previous.OpenCount > 0 && state.Graph.OpenCount() == 0 {
		causes = append(causes, "intents_finished")
	}
	if state.DecisionRevision > previous.DecisionRevision {
		causes = append(causes, "state_changed")
	}
	if len(causes) == 0 {
		causes = append(causes, trigger)
	}
	return causes
}

func (s *Server) snapshotRead(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	var read worker.GraphRequest
	if err := decodeFields(q, &read); err != nil || read.Op != "read_snapshot" {
		return 0, nil, b.Err(422, "expected a read_snapshot request")
	}
	if err := worker.ValidateGraphRequest(worker.Job{}, read); err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	e, err := t.Execution(r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	if r.Header.Get("X-Xloom-Run") != e.Lease || r.Header.Get("X-Xloom-Lease") != e.Kind || r.Header.Get("X-Xloom-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Snapshot read requires its execution lease")
	}
	current, err := t.Load(e.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	if err = guard(t, current, r); err != nil {
		return 0, nil, err
	}
	var j worker.Job
	if err = json.Unmarshal(e.Job, &j); err != nil {
		return 0, nil, err
	}
	if j.InputSnapshot == nil {
		return 0, nil, b.Err(409, "Execution uses a legacy inline input")
	}
	state, err := t.ReadInputSnapshot(e.ProjectID, j.InputSnapshot.ID)
	if err != nil {
		return 0, nil, err
	}
	page, err := worker.GraphPage(state, read)
	if err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	return 200, page, nil
}
