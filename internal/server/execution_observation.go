package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	b "xloom/internal/board"
	"xloom/internal/worker"
)

func (s *Server) registerObservationRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/observation", s.wrap(s.executionObservation))
}

func (s *Server) executionObservation(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if len(q.fields) != 1 {
		return 0, nil, b.Err(422, "observation accepts only metrics")
	}
	raw, err := json.Marshal(q.fields["metrics"])
	if err != nil || len(raw) > 16<<10 || bytes.Equal(raw, []byte("null")) {
		return 0, nil, b.Err(422, "invalid metrics")
	}
	var metrics worker.DecisionMetrics
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&metrics) != nil || metrics.Version != 1 {
		return 0, nil, b.Err(422, "invalid decision metrics")
	}
	raw, err = json.Marshal(metrics)
	if err != nil {
		return 0, nil, err
	}
	err = t.RecordExecutionObservation(r.PathValue("pid"), r.PathValue("rid"), decisionFence(r), raw)
	return http.StatusOK, map[string]bool{"recorded": err == nil}, err
}
