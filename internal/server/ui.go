package server

import (
	"net/http"
	"strconv"

	b "xloom/internal/board"
)

func (s *Server) registerUIRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/executions", s.wrap(s.projectExecutions))
	m.HandleFunc("GET /ui/overview", s.wrap(s.uiOverview))
}

func (s *Server) projectExecutions(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	query := r.URL.Query()
	if query.Has("limit") || query.Has("cursor") || query.Has("through") {
		limit := int64(20)
		cursor, through := int64(0), int64(0)
		for key, target := range map[string]*int64{"limit": &limit, "cursor": &cursor, "through": &through} {
			if query.Has(key) {
				value, err := strconv.ParseInt(query.Get(key), 10, 64)
				if err != nil || value < 0 || (key == "limit" && (value == 0 || value > 100)) {
					return 0, nil, b.Err(422, "invalid execution page "+key)
				}
				*target = value
			}
		}
		page, err := t.ProjectExecutionPage(r.PathValue("pid"), cursor, through, int(limit))
		return http.StatusOK, page, err
	}
	views, err := t.ProjectExecutions(r.PathValue("pid"))
	return http.StatusOK, views, err
}

func (s *Server) uiOverview(t *b.Tx, _ *request, _ *http.Request) (int, any, error) {
	overview, err := t.UIOverview()
	return http.StatusOK, overview, err
}
