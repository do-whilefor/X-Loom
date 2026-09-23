package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	b "xloom/internal/board"
)

func (s *Server) registerStateRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/state", s.wrap(s.projectState))
	m.HandleFunc("POST /projects/{pid}/state/actions", s.wrap(s.stateAction))
	m.HandleFunc("GET /projects/{pid}/state/events", s.wrap(s.stateEvents))
	m.HandleFunc("POST /projects/{pid}/state/decisions/preview", s.wrap(s.decisionPreview))
	m.HandleFunc("POST /projects/{pid}/state/decisions/commit", s.wrap(s.decisionCommit))
	m.HandleFunc("GET /projects/{pid}/state/decisions/receipt", s.wrap(s.decisionReceipt))
}

func decisionFence(r *http.Request) b.ExecutionFence {
	return b.ExecutionFence{Run: r.Header.Get("X-Xloom-Run"), Lease: r.Header.Get("X-Xloom-Lease"), Intent: r.Header.Get("X-Xloom-Intent")}
}
func decodeDecisionBatch(q *request) (b.DecisionBatch, error) {
	var batch b.DecisionBatch
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return batch, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&batch); err != nil {
		return batch, b.Err(422, "Invalid decision batch: "+err.Error())
	}
	if batch.Actions == nil {
		return batch, b.Err(422, "decision actions must be an array")
	}
	return batch, nil
}
func (s *Server) decisionPreview(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	batch, err := decodeDecisionBatch(q)
	if err != nil {
		return 0, nil, err
	}
	receipt, err := t.PreviewDecision(r.PathValue("pid"), decisionFence(r), batch)
	return http.StatusOK, receipt, err
}
func (s *Server) decisionCommit(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	batch, err := decodeDecisionBatch(q)
	if err != nil {
		return 0, nil, err
	}
	receipt, err := t.CommitDecision(r.PathValue("pid"), decisionFence(r), batch)
	return http.StatusOK, receipt, err
}
func (s *Server) decisionReceipt(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	receipt, err := t.DecisionReceipt(r.PathValue("pid"), decisionFence(r))
	return http.StatusOK, receipt, err
}
func (s *Server) projectState(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	state, err := t.State(r.PathValue("pid"))
	return 200, state, err
}
func (s *Server) stateAction(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	for key := range q.fields {
		if key != "op" && key != "idempotency_key" && key != "payload" && key != "expected_version" {
			return 0, nil, b.Err(422, "unknown state action field: "+key)
		}
	}
	op, key := q.text("op"), q.text("idempotency_key")
	expected := ""
	if _, ok := q.fields["expected_version"]; ok {
		expected = q.text("expected_version")
	}
	payload, ok := q.fields["payload"].(map[string]any)
	if !ok {
		return 0, nil, b.Err(422, "payload must be an object")
	}
	if q.err != nil {
		return 0, nil, q.err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	result, err := t.StateAction(r.PathValue("pid"), b.ExecutionFence{Run: r.Header.Get("X-Xloom-Run"), Lease: r.Header.Get("X-Xloom-Lease"), Intent: r.Header.Get("X-Xloom-Intent")}, b.StateAction{Op: op, IdempotencyKey: key, Payload: raw, ExpectedVersion: expected})
	return 200, result, err
}
func (s *Server) stateEvents(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return 0, nil, b.Err(422, "after must be a nonnegative revision")
		}
	}
	events, err := t.StateEvents(r.PathValue("pid"), after)
	return 200, events, err
}
