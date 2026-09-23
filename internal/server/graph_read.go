package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	b "xloom/internal/board"
	"xloom/internal/worker"
)

// Apply continuation boundaries before sending the current FGS over HTTP.
// This keeps one oversized Fact from preventing all live graph reads.
func (s *Server) graphRead(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return 0, nil, err
	}
	var read worker.GraphRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&read); err != nil || read.Op != "read_graph" {
		return 0, nil, b.Err(422, "expected a read_graph request")
	}
	if err = worker.ValidateGraphRequest(worker.Job{}, read); err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	state, err := t.State(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if err = guard(t, state.Graph, r); err != nil {
		return 0, nil, err
	}
	page, err := worker.GraphPage(state, read)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if strings.HasPrefix(err.Error(), "state_changed:") {
			status = http.StatusConflict
		}
		return 0, nil, b.Err(status, err.Error())
	}
	return http.StatusOK, page, nil
}
