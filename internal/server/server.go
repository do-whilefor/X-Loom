package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	b "xloom/internal/board"
	"xloom/web"
)

type Server struct{ Store *b.Store }
type request struct {
	fields map[string]any
	err    error
}
type action func(*b.Tx, *request, *http.Request) (int, any, error)

func New(store *b.Store) http.Handler {
	s := &Server{store}
	m := http.NewServeMux()
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web.Files)))
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, err := web.Files.ReadFile("index.html")
		if err != nil {
			http.Error(w, "Web unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	for pattern, fn := range map[string]action{
		"GET /health": s.health, "GET /settings": s.settings, "PUT /settings": s.settings,
		"GET /projects": s.projects, "POST /projects": s.projects,
		"GET /projects/{pid}": s.project, "DELETE /projects/{pid}": s.project,
		"PUT /projects/{pid}/title": s.title, "PUT /projects/{pid}/status": s.status,
		"POST /projects/{pid}/reason/{op}": s.reason,
		"POST /projects/{pid}/hints":       s.hint, "POST /projects/{pid}/intents": s.intent,
		"POST /projects/{pid}/intents/{iid}/{op}": s.intentAction,
		"POST /projects/{pid}/complete":           s.complete, "POST /projects/{pid}/reopen": s.reopen,
		"GET /projects/{pid}/export": s.export,
	} {
		m.HandleFunc(pattern, s.wrap(fn))
	}
	return m
}
func (s *Server) wrap(fn action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := &request{fields: map[string]any{}}
		if r.Method == "POST" || r.Method == "PUT" {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
			dec.UseNumber()
			if err := dec.Decode(&in.fields); err != nil || in.fields == nil {
				writeError(w, &b.APIError{Status: 422, Detail: "Expected a JSON object"})
				return
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				writeError(w, &b.APIError{Status: 422, Detail: "Expected one JSON object"})
				return
			}
		}
		status := 200
		var body any
		err := s.Store.Do(r.Context(), func(t *b.Tx) error {
			if err := t.Expire(); err != nil {
				return err
			}
			var err error
			status, body, err = fn(t, in, r)
			if in.err != nil {
				return in.err
			}
			return err
		})
		if err != nil {
			writeError(w, err)
			return
		}
		if txt, ok := body.(plain); ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			io.WriteString(w, string(txt))
			return
		}
		if status == 204 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.Warn("response write failed", "error", err)
		}
	}
}
func writeError(w http.ResponseWriter, err error) {
	var ae *b.APIError
	if !errors.As(err, &ae) {
		slog.Error("server request failed", "error", err)
		ae = &b.APIError{Status: 500, Detail: "Internal Server Error"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	json.NewEncoder(w).Encode(map[string]any{"detail": ae.Detail})
}
func (r *request) invalid(key, msg string) {
	if r.err == nil {
		r.err = &b.APIError{Status: 422, Detail: []any{map[string]any{"loc": []string{"body", key}, "msg": msg, "type": "value_error"}}}
	}
}
func (r *request) text(key string) string {
	v, ok := r.fields[key].(string)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		r.invalid(key, "must be a non-empty string")
	}
	return v
}
func (r *request) optional(key string) *string {
	if r.fields[key] == nil {
		return nil
	}
	return b.Ptr(r.text(key))
}
func (r *request) sources() []string {
	v := r.fields["from"]
	if v == nil {
		v = r.fields["from_"]
	}
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		r.invalid("from", "must contain at least one fact id")
	}
	out := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		s, ok := item.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" {
			r.invalid("from", "fact ids must not be empty")
		}
		if seen[s] {
			r.invalid("from", "duplicate fact ids")
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
func (r *request) integer(key string) int {
	var str string
	switch v := r.fields[key].(type) {
	case json.Number:
		str = string(v)
	case string:
		str = v
	default:
		r.invalid(key, "must be an integer")
	}
	n, err := strconv.Atoi(str)
	if err != nil || n < 5 {
		r.invalid(key, "must be an integer greater than or equal to 5")
	}
	return n
}
func (r *request) bootstrap() bool {
	v, exists := r.fields["bootstrap_enabled"]
	if !exists {
		return true
	}
	switch x := v.(type) {
	case bool:
		return x
	case json.Number:
		if x == "1" {
			return true
		}
		if x == "0" {
			return false
		}
	case string:
		switch strings.ToLower(x) {
		case "true", "1", "yes", "on", "y", "t":
			return true
		case "false", "0", "no", "off", "n", "f":
			return false
		}
	}
	r.invalid("bootstrap_enabled", "must be a boolean")
	return false
}
func active(g b.Graph) error {
	if g.Project.Status != "active" {
		return b.Err(403, "Project is "+g.Project.Status)
	}
	return nil
}
func sourceFacts(g b.Graph, from []string) error {
	for _, id := range from {
		found := false
		for _, f := range g.Facts {
			if f.ID == id {
				found = true
				break
			}
		}
		if !found {
			return b.Err(404, "Fact "+id+" not found")
		}
	}
	for _, id := range from {
		if id == "goal" {
			return b.Err(400, "goal cannot be used in from")
		}
	}
	return nil
}

// Dispatcher-only conditional writes fence off expired/replaced runs without
// changing the public Cairn API. Worker identity is unique to each execution.
func guard(g b.Graph, r *http.Request) error {
	run := r.Header.Get("X-Xloom-Run")
	if run == "" {
		return nil
	}
	if r.Header.Get("X-Xloom-Lease") == "reason" {
		if g.Project.Reason == nil || g.Project.Reason.Worker != run {
			return b.Err(409, "Reason execution no longer owns its lease")
		}
		return nil
	}
	id := r.Header.Get("X-Xloom-Intent")
	for _, i := range g.Intents {
		if i.ID == id && b.Value(i.Worker) == run {
			return nil
		}
	}
	return b.Err(409, "Intent execution no longer owns its lease")
}
func (s *Server) health(t *b.Tx, _ *request, _ *http.Request) (int, any, error) {
	_, err := t.Settings()
	return 200, map[string]string{"status": "ok"}, err
}
func (s *Server) settings(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if r.Method == "GET" {
		v, err := t.Settings()
		return 200, v, err
	}
	v := b.Settings{IntentTimeout: q.integer("intent_timeout"), ReasonTimeout: q.integer("reason_timeout")}
	if q.err != nil {
		return 0, nil, q.err
	}
	_, err := t.Exec("UPDATE settings SET intent_timeout=?,reason_timeout=? WHERE rowid=1", v.IntentTimeout, v.ReasonTimeout)
	return 200, v, err
}
func (s *Server) projects(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if r.Method == "GET" {
		ids, err := t.IDs()
		if err != nil {
			return 0, nil, err
		}
		out := []b.Summary{}
		for _, id := range ids {
			g, err := t.Load(id)
			if err != nil {
				return 0, nil, err
			}
			out = append(out, g.Summarize())
		}
		return 200, out, nil
	}
	title, origin, goal, bootstrap := q.text("title"), q.text("origin"), q.text("goal"), q.bootstrap()
	if q.err != nil {
		return 0, nil, q.err
	}
	id, err := t.Next("", "project")
	if err != nil {
		return 0, nil, err
	}
	g := b.Graph{Project: b.Project{ID: id, Title: title, Status: "active", Bootstrap: bootstrap, CreatedAt: t.Now}, Facts: []b.Fact{{ID: "origin", Description: origin}, {ID: "goal", Description: goal}}, Intents: []b.Intent{}, Hints: []b.Hint{}}
	if err = t.Save(g); err != nil {
		return 0, nil, err
	}
	if raw := q.fields["hints"]; raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return 0, nil, b.Err(422, "hints must be an array")
		}
		for _, item := range list {
			fields, ok := item.(map[string]any)
			if !ok {
				return 0, nil, b.Err(422, "hint must be an object")
			}
			h := &request{fields: fields}
			content, creator := h.text("content"), h.text("creator")
			if h.err != nil {
				return 0, nil, h.err
			}
			hid, err := t.Next(id, "hint")
			if err != nil {
				return 0, nil, err
			}
			g.Hints = append(g.Hints, b.Hint{ID: hid, Content: content, Creator: creator, CreatedAt: t.Now})
		}
	}
	return 201, g, t.Save(g)
}
func (s *Server) project(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if r.Method == "GET" {
		return 200, g, nil
	}
	_, err = t.Exec("DELETE FROM projects WHERE id=?", g.Project.ID)
	return 204, nil, err
}
func (s *Server) title(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	title := q.text("title")
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	g.Project.Title = title
	return 200, g.Project, t.Save(g)
}
func (s *Server) status(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	status := q.text("status")
	if status != "active" && status != "stopped" {
		q.invalid("status", "must be active or stopped")
	}
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if g.Project.Status == "completed" {
		return 0, nil, b.Err(409, "Completed projects cannot change status")
	}
	if g.Project.Status != status {
		g.Project.Status = status
		if status == "stopped" {
			g.Project.Reason = nil
			for n := range g.Intents {
				if g.Intents[n].To == nil {
					g.Intents[n].Worker = nil
				}
			}
		}
	}
	return 200, g.Project, t.Save(g)
}
func (s *Server) reason(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	op := r.PathValue("op")
	if op != "claim" && op != "heartbeat" && op != "release" {
		return 0, nil, b.Err(404, "Not Found")
	}
	worker := q.text("worker")
	trigger := ""
	if op == "claim" {
		trigger = q.text("trigger")
	}
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if err = active(g); err != nil {
		return 0, nil, err
	}
	lease := g.Project.Reason
	if lease != nil && lease.Worker != worker {
		return 0, nil, b.Err(409, "Project reason is currently claimed by "+lease.Worker)
	}
	switch op {
	case "claim":
		if lease == nil {
			g.Project.Reason = &b.Reason{Worker: worker, Trigger: trigger, StartedAt: t.Now, Heartbeat: t.Now}
		}
	case "heartbeat":
		if lease == nil {
			return 0, nil, b.Err(409, "Project reason is not currently claimed")
		}
		lease.Heartbeat = t.Now
	case "release":
		g.Project.Reason = nil
	}
	return 200, g.Project, t.Save(g)
}
func (s *Server) hint(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	content, creator := q.text("content"), q.text("creator")
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	id, err := t.Next(g.Project.ID, "hint")
	if err != nil {
		return 0, nil, err
	}
	h := b.Hint{ID: id, Content: content, Creator: creator, CreatedAt: t.Now}
	g.Hints = append(g.Hints, h)
	return 201, h, t.Save(g)
}
func (s *Server) intent(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	from, desc, creator, worker := q.sources(), q.text("description"), q.text("creator"), q.optional("worker")
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if err = active(g); err != nil {
		return 0, nil, err
	}
	if err = guard(g, r); err != nil {
		return 0, nil, err
	}
	if err = sourceFacts(g, from); err != nil {
		return 0, nil, err
	}
	if worker != nil && *worker != creator {
		return 0, nil, b.Err(400, "worker must be null or equal to creator")
	}
	id, err := t.Next(g.Project.ID, "intent")
	if err != nil {
		return 0, nil, err
	}
	i := b.Intent{ID: id, From: from, Description: desc, Creator: creator, Worker: worker, CreatedAt: t.Now}
	if worker != nil {
		i.Heartbeat = b.Ptr(t.Now)
	}
	g.Intents = append(g.Intents, i)
	return 201, i, t.Save(g)
}
func (s *Server) intentAction(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	op := r.PathValue("op")
	if op != "heartbeat" && op != "release" && op != "conclude" {
		return 0, nil, b.Err(404, "Not Found")
	}
	worker := q.text("worker")
	desc := ""
	if op == "conclude" {
		desc = q.text("description")
	}
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if err = active(g); err != nil {
		return 0, nil, err
	}
	if err = guard(g, r); err != nil {
		return 0, nil, err
	}
	for n := range g.Intents {
		i := &g.Intents[n]
		if i.ID != r.PathValue("iid") {
			continue
		}
		if i.To != nil {
			return 0, nil, b.Err(409, "Intent already concluded")
		}
		if i.Worker != nil && *i.Worker != worker {
			return 0, nil, b.Err(409, "Intent is currently claimed by "+*i.Worker)
		}
		if op == "release" {
			i.Worker = nil
			return 200, *i, t.Save(g)
		}
		i.Worker = b.Ptr(worker)
		i.Heartbeat = b.Ptr(t.Now)
		if op == "heartbeat" {
			return 200, *i, t.Save(g)
		}
		fid, err := t.Next(g.Project.ID, "fact")
		if err != nil {
			return 0, nil, err
		}
		f := b.Fact{ID: fid, Description: desc}
		i.To = &fid
		i.ConcludedAt = b.Ptr(t.Now)
		g.Facts = append(g.Facts, f)
		return 200, b.Conclusion{Fact: f, Intent: *i}, t.Save(g)
	}
	return 0, nil, b.Err(404, "Intent not found")
}
func (s *Server) complete(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	from, desc, worker := q.sources(), q.text("description"), q.text("worker")
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if err = active(g); err != nil {
		return 0, nil, err
	}
	if err = guard(g, r); err != nil {
		return 0, nil, err
	}
	if err = sourceFacts(g, from); err != nil {
		return 0, nil, err
	}
	id, err := t.Next(g.Project.ID, "intent")
	if err != nil {
		return 0, nil, err
	}
	i := b.Intent{ID: id, From: from, To: b.Ptr("goal"), Description: desc, Creator: worker, Worker: &worker, Heartbeat: b.Ptr(t.Now), CreatedAt: t.Now, ConcludedAt: b.Ptr(t.Now)}
	g.Intents = append(g.Intents, i)
	g.Project.Status = "completed"
	g.Project.Reason = nil
	return 200, i, t.Save(g)
}
func (s *Server) reopen(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	desc, creator := q.text("description"), q.text("creator")
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if g.Project.Status != "completed" {
		return 0, nil, b.Err(403, "Project is "+g.Project.Status)
	}
	index := -1
	for n, i := range g.Intents {
		if b.Value(i.To) == "goal" {
			if index != -1 {
				return 0, nil, b.Err(409, "Completed project has multiple completion intents")
			}
			index = n
		}
	}
	if index < 0 {
		return 0, nil, b.Err(409, "Completed project is missing its completion intent")
	}
	old := g.Intents[index]
	if len(old.From) == 0 {
		return 0, nil, b.Err(409, "Completion intent is missing its source facts")
	}
	fid, err := t.Next(g.Project.ID, "fact")
	if err != nil {
		return 0, nil, err
	}
	iid, err := t.Next(g.Project.ID, "intent")
	if err != nil {
		return 0, nil, err
	}
	if _, err = t.Exec("DELETE FROM intents WHERE project_id=? AND id=?", g.Project.ID, old.ID); err != nil {
		return 0, nil, err
	}
	f := b.Fact{ID: fid, Description: desc}
	i := b.Intent{ID: iid, From: old.From, To: &fid, Description: "external_feedback", Creator: creator, Worker: &creator, Heartbeat: b.Ptr(t.Now), CreatedAt: t.Now, ConcludedAt: b.Ptr(t.Now)}
	g.Intents = append(g.Intents[:index], g.Intents[index+1:]...)
	g.Intents = append(g.Intents, i)
	g.Facts = append(g.Facts, f)
	g.Project.Status = "active"
	g.Project.Reason = nil
	return 200, b.Reopened{Project: g.Project, Fact: f, Intent: i}, t.Save(g)
}

type plain string

func (s *Server) export(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "yaml"
	}
	if format != "yaml" && format != "timeline" {
		return 0, nil, b.Err(400, "Supported formats: yaml, timeline")
	}
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	text, err := b.Export(g, format)
	return 200, plain(text), err
}
