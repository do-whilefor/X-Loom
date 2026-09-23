package server

import (
	"net/http"
	"strconv"

	b "xloom/internal/board"
)

func executionNamespace(r *http.Request) (string, error) {
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" || len(namespace) > 128 {
		return "", b.Err(422, "namespace is required and must not exceed 128 bytes")
	}
	return namespace, nil
}

func executionQueryInt(r *http.Request, key string, fallback int64) (int64, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, b.Err(422, key+" must be a nonnegative integer")
	}
	return n, nil
}

func (s *Server) pendingExecutions(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	namespace, err := executionNamespace(r)
	if err != nil {
		return 0, nil, err
	}
	after, err := executionQueryInt(r, "after", 0)
	if err != nil {
		return 0, nil, err
	}
	limit, err := executionQueryInt(r, "limit", 100)
	if err != nil || limit < 1 || limit > 100 {
		return 0, nil, b.Err(422, "limit must be between 1 and 100")
	}
	page, err := t.PendingExecutions(namespace, after, int(limit))
	return 200, page, err
}

func (s *Server) executionDetail(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	namespace, err := executionNamespace(r)
	if err != nil {
		return 0, nil, err
	}
	// Check the small identity first so a mismatched namespace cannot cause a
	// full immutable input/result read or reveal the execution's presence.
	identity, err := t.GetExecutionSummary(namespace, r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	if r.PathValue("execution_identity") == "true" {
		return 200, identity, nil
	}
	e, err := t.Execution(identity.ProjectID, identity.ID)
	return 200, e, err
}

func (s *Server) executionCheck(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	namespace, err := executionNamespace(r)
	if err != nil {
		return 0, nil, err
	}
	generation, err := executionQueryInt(r, "generation", 0)
	if err != nil {
		return 0, nil, err
	}
	values := r.URL.Query()
	kind, intent, key := values.Get("kind"), values.Get("intent"), values.Get("retry_key")
	if kind != "reason" && kind != "bootstrap" && kind != "explore" {
		return 0, nil, b.Err(422, "kind must be reason, bootstrap or explore")
	}
	if (kind == "reason" && intent != "") || (kind != "reason" && intent == "") || len(intent) > 128 || len(key) > 1024 || len(values.Get("state_version")) > 64 {
		return 0, nil, b.Err(422, "invalid execution query")
	}
	check, err := t.CheckExecutions(b.ExecutionCheckQuery{ProjectID: r.PathValue("pid"), Namespace: namespace, Generation: generation, Kind: kind, Intent: intent, RetryKey: key, StateVersion: values.Get("state_version")})
	return 200, check, err
}
