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
	expected, err := managementGeneration(q, "restart")
	if err != nil {
		return 0, nil, err
	}
	g, err := t.RestartProject(r.PathValue("pid"), expected)
	return http.StatusOK, g, err
}

func managementGeneration(q *request, action string) (*int64, error) {
	var expected *int64
	for field, raw := range q.fields {
		if field != "expected_generation" {
			return nil, b.Err(422, "unknown "+action+" field: "+field)
		}
		value, ok := raw.(json.Number)
		generation, err := strconv.ParseInt(string(value), 10, 64)
		if !ok || err != nil || generation < 0 {
			return nil, b.Err(422, "expected_generation must be a nonnegative integer")
		}
		expected = &generation
	}
	return expected, nil
}
