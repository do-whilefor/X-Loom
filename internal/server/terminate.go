package server

import (
	"net/http"

	b "xloom/internal/board"
)

func (s *Server) terminate(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if r.Header.Get("X-Xloom-Run") != "" {
		return 0, nil, b.Err(403, "terminate is a project-management operation")
	}
	expected, err := managementGeneration(q, "terminate")
	if err != nil {
		return 0, nil, err
	}
	g, err := t.TerminateProject(r.PathValue("pid"), expected)
	return http.StatusOK, g, err
}
