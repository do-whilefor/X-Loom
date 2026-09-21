package server

import (
	"net/http"

	b "xloom/internal/board"
)

func (s *Server) registerUIRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/executions", s.wrap(s.projectExecutions))
	m.HandleFunc("GET /ui/overview", s.wrap(s.uiOverview))
}

func (s *Server) projectExecutions(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	views, err := t.ProjectExecutions(r.PathValue("pid"))
	return http.StatusOK, views, err
}

func (s *Server) uiOverview(t *b.Tx, _ *request, _ *http.Request) (int, any, error) {
	overview, err := t.UIOverview()
	return http.StatusOK, overview, err
}
