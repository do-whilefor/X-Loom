package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	b "xloom/internal/board"
)

func (s *Server) restart(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if r.Header.Get("X-Xloom-Run") != "" {
		return 0, nil, b.Err(403, "restart is a project-management operation")
	}
	var expected *int64
	for field, raw := range q.fields {
		if field != "expected_generation" {
			return 0, nil, b.Err(422, "unknown restart field: "+field)
		}
		value, ok := raw.(json.Number)
		generation, err := strconv.ParseInt(string(value), 10, 64)
		if !ok || err != nil || generation < 0 {
			return 0, nil, b.Err(422, "expected_generation must be a nonnegative integer")
		}
		expected = &generation
	}
	g, err := t.RestartProject(r.PathValue("pid"), expected)
	return http.StatusOK, g, err
}
