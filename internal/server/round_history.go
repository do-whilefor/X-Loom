package server

import (
	"net/http"
	"strconv"

	b "xloom/internal/board"
)

func (s *Server) registerRoundRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/rounds", s.wrap(s.rounds))
	m.HandleFunc("GET /projects/{pid}/rounds/{generation}/entries", s.wrap(s.roundEntries))
	m.HandleFunc("GET /projects/{pid}/rounds/{generation}/entries/{entry}", s.wrap(s.roundEntry))
}

func roundManagementRead(r *http.Request) error {
	if r.Header.Get("X-Xloom-Run") != "" {
		return b.Err(403, "archived rounds are a project-management view, not current execution input")
	}
	return nil
}

func roundQueryInt(r *http.Request, field string, fallback int64) (int64, error) {
	if raw := r.URL.Query().Get(field); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, b.Err(422, field+" must be an integer")
		}
		return value, nil
	}
	return fallback, nil
}

func roundPathInt(r *http.Request, field string) (int64, error) {
	value, err := strconv.ParseInt(r.PathValue(field), 10, 64)
	if err != nil || value < 0 {
		return 0, b.Err(422, field+" must be a nonnegative integer")
	}
	return value, nil
}

func (s *Server) rounds(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	if err := roundManagementRead(r); err != nil {
		return 0, nil, err
	}
	after, err := roundQueryInt(r, "cursor", -1)
	if err != nil {
		return 0, nil, err
	}
	limit, err := roundQueryInt(r, "limit", 20)
	if err != nil || limit < 1 || limit > 100 {
		return 0, nil, b.Err(422, "limit must be between 1 and 100")
	}
	page, err := t.RoundHistory(r.PathValue("pid"), after, int(limit))
	return http.StatusOK, page, err
}

func (s *Server) roundEntries(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	if err := roundManagementRead(r); err != nil {
		return 0, nil, err
	}
	generation, err := roundPathInt(r, "generation")
	if err != nil {
		return 0, nil, err
	}
	after, err := roundQueryInt(r, "cursor", 0)
	if err != nil {
		return 0, nil, err
	}
	limit, err := roundQueryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 100 {
		return 0, nil, b.Err(422, "limit must be between 1 and 100")
	}
	page, err := t.RoundEntries(r.PathValue("pid"), generation, after, int(limit))
	return http.StatusOK, page, err
}

func (s *Server) roundEntry(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	if err := roundManagementRead(r); err != nil {
		return 0, nil, err
	}
	generation, err := roundPathInt(r, "generation")
	if err != nil {
		return 0, nil, err
	}
	entry, err := roundPathInt(r, "entry")
	if err != nil {
		return 0, nil, err
	}
	offset, err := roundQueryInt(r, "offset", 0)
	if err != nil {
		return 0, nil, err
	}
	limit, err := roundQueryInt(r, "limit", 32<<10)
	if err != nil || limit < 1 || limit > 32<<10 {
		return 0, nil, b.Err(422, "limit must be between 1 and 32768")
	}
	page, err := t.ReadRoundEntry(r.PathValue("pid"), generation, entry, offset, int(limit))
	return http.StatusOK, page, err
}
