package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings(intent_timeout INTEGER NOT NULL DEFAULT 15,reason_timeout INTEGER NOT NULL DEFAULT 15);
INSERT OR IGNORE INTO settings(rowid,intent_timeout,reason_timeout) VALUES(1,15,15);
CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,title TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',bootstrap_enabled INTEGER NOT NULL DEFAULT 1,created_at TEXT NOT NULL,reason_worker TEXT,reason_trigger TEXT,reason_started_at TEXT,reason_last_heartbeat_at TEXT);
CREATE TABLE IF NOT EXISTS facts(id TEXT NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,description TEXT NOT NULL,PRIMARY KEY(id,project_id));
CREATE TABLE IF NOT EXISTS intents(id TEXT NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,to_fact_id TEXT,description TEXT NOT NULL,creator TEXT NOT NULL,worker TEXT,last_heartbeat_at TEXT,created_at TEXT NOT NULL,concluded_at TEXT,PRIMARY KEY(id,project_id));
CREATE TABLE IF NOT EXISTS intent_sources(intent_id TEXT NOT NULL,project_id TEXT NOT NULL,fact_id TEXT NOT NULL,PRIMARY KEY(intent_id,project_id,fact_id),FOREIGN KEY(intent_id,project_id) REFERENCES intents(id,project_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS hints(id TEXT NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,content TEXT NOT NULL,creator TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(id,project_id));
CREATE TABLE IF NOT EXISTS counters(name TEXT PRIMARY KEY,value INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO counters(name,value) VALUES('project',0);
CREATE TABLE IF NOT EXISTS scoped_counters(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,kind TEXT NOT NULL,value INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(project_id,kind));`

type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	Now func() time.Time
}
type Tx struct {
	*sql.Tx
	Now string
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Now: time.Now}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", schema} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	rows, err := db.Query("PRAGMA table_info(projects)")
	if err != nil {
		db.Close()
		return nil, err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		db.Close()
		return nil, err
	}
	if !columns["bootstrap_enabled"] {
		_, err = db.Exec("ALTER TABLE projects ADD COLUMN bootstrap_enabled INTEGER NOT NULL DEFAULT 1")
		if err == nil && columns["bootstrap_mode"] {
			_, err = db.Exec("UPDATE projects SET bootstrap_enabled=CASE WHEN bootstrap_mode='disabled' THEN 0 ELSE 1 END")
		}
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Serialize graph transactions. SQLite remains the final transaction boundary.
func (s *Store) Do(ctx context.Context, fn func(*Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	x := &Tx{Tx: t, Now: s.Now().UTC().Format(time.RFC3339)}
	if err = fn(x); err != nil {
		return err
	}
	return t.Commit()
}
func (t *Tx) Settings() (Settings, error) {
	var s Settings
	err := t.QueryRow("SELECT intent_timeout,reason_timeout FROM settings WHERE rowid=1").Scan(&s.IntentTimeout, &s.ReasonTimeout)
	return s, err
}
func (t *Tx) Expire() error {
	s, err := t.Settings()
	if err != nil {
		return err
	}
	_, err = t.Exec(`UPDATE intents SET worker=NULL WHERE to_fact_id IS NULL AND worker IS NOT NULL AND (julianday(?)-julianday(last_heartbeat_at))*86400>?`, t.Now, s.IntentTimeout)
	if err != nil {
		return err
	}
	_, err = t.Exec(`UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE reason_worker IS NOT NULL AND (julianday(?)-julianday(reason_last_heartbeat_at))*86400>?`, t.Now, s.ReasonTimeout)
	return err
}
func (t *Tx) Next(project, kind string) (string, error) {
	var n int
	var err error
	if kind == "project" {
		err = t.QueryRow("UPDATE counters SET value=value+1 WHERE name='project' RETURNING value").Scan(&n)
		return fmt.Sprintf("proj_%03d", n), err
	}
	_, err = t.Exec("INSERT OR IGNORE INTO scoped_counters(project_id,kind,value) VALUES(?,?,0)", project, kind)
	if err != nil {
		return "", err
	}
	err = t.QueryRow("UPDATE scoped_counters SET value=value+1 WHERE project_id=? AND kind=? RETURNING value", project, kind).Scan(&n)
	return fmt.Sprintf("%s%03d", kind[:1], n), err
}
func (t *Tx) IDs() ([]string, error) {
	rows, err := t.Query("SELECT id FROM projects ORDER BY created_at,rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (t *Tx) Load(id string) (Graph, error) {
	g := Graph{Facts: []Fact{}, Intents: []Intent{}, Hints: []Hint{}}
	var rw, rt, rs, rh *string
	err := t.QueryRow("SELECT id,title,status,bootstrap_enabled,created_at,reason_worker,reason_trigger,reason_started_at,reason_last_heartbeat_at FROM projects WHERE id=?", id).Scan(&g.Project.ID, &g.Project.Title, &g.Project.Status, &g.Project.Bootstrap, &g.Project.CreatedAt, &rw, &rt, &rs, &rh)
	if errors.Is(err, sql.ErrNoRows) {
		return g, Err(404, "Project not found")
	}
	if err != nil {
		return g, err
	}
	if rw != nil {
		g.Project.Reason = &Reason{*rw, Value(rt), Value(rs), Value(rh)}
	}
	rows, err := t.Query("SELECT id,description FROM facts WHERE project_id=? ORDER BY rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var f Fact
		if err = rows.Scan(&f.ID, &f.Description); err != nil {
			rows.Close()
			return g, err
		}
		g.Facts = append(g.Facts, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	rows, err = t.Query("SELECT id,content,creator,created_at FROM hints WHERE project_id=? ORDER BY created_at,rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var h Hint
		if err = rows.Scan(&h.ID, &h.Content, &h.Creator, &h.CreatedAt); err != nil {
			rows.Close()
			return g, err
		}
		g.Hints = append(g.Hints, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	rows, err = t.Query("SELECT id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at FROM intents WHERE project_id=? ORDER BY created_at,rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		i := Intent{From: []string{}}
		if err = rows.Scan(&i.ID, &i.To, &i.Description, &i.Creator, &i.Worker, &i.Heartbeat, &i.CreatedAt, &i.ConcludedAt); err != nil {
			rows.Close()
			return g, err
		}
		g.Intents = append(g.Intents, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	for n := range g.Intents {
		rows, err = t.Query("SELECT fact_id FROM intent_sources WHERE project_id=? AND intent_id=? ORDER BY rowid", id, g.Intents[n].ID)
		if err != nil {
			return g, err
		}
		for rows.Next() {
			var fid string
			if err = rows.Scan(&fid); err != nil {
				rows.Close()
				return g, err
			}
			g.Intents[n].From = append(g.Intents[n].From, fid)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return g, err
		}
	}
	return g, nil
}
func (t *Tx) Save(g Graph) error {
	p := g.Project
	var rw, rt, rs, rh *string
	if p.Reason != nil {
		r := p.Reason
		rw, rt, rs, rh = &r.Worker, &r.Trigger, &r.StartedAt, &r.Heartbeat
	}
	_, err := t.Exec(`INSERT INTO projects(id,title,status,bootstrap_enabled,created_at,reason_worker,reason_trigger,reason_started_at,reason_last_heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET title=excluded.title,status=excluded.status,bootstrap_enabled=excluded.bootstrap_enabled,reason_worker=excluded.reason_worker,reason_trigger=excluded.reason_trigger,reason_started_at=excluded.reason_started_at,reason_last_heartbeat_at=excluded.reason_last_heartbeat_at`, p.ID, p.Title, p.Status, p.Bootstrap, p.CreatedAt, rw, rt, rs, rh)
	if err != nil {
		return err
	}
	for _, f := range g.Facts {
		if _, err = t.Exec("INSERT OR IGNORE INTO facts(id,project_id,description) VALUES(?,?,?)", f.ID, p.ID, f.Description); err != nil {
			return err
		}
	}
	for _, h := range g.Hints {
		if _, err = t.Exec("INSERT OR IGNORE INTO hints(id,project_id,content,creator,created_at) VALUES(?,?,?,?,?)", h.ID, p.ID, h.Content, h.Creator, h.CreatedAt); err != nil {
			return err
		}
	}
	for _, i := range g.Intents {
		_, err = t.Exec(`INSERT INTO intents(id,project_id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id,project_id) DO UPDATE SET to_fact_id=excluded.to_fact_id,worker=excluded.worker,last_heartbeat_at=excluded.last_heartbeat_at,concluded_at=excluded.concluded_at`, i.ID, p.ID, i.To, i.Description, i.Creator, i.Worker, i.Heartbeat, i.CreatedAt, i.ConcludedAt)
		if err != nil {
			return err
		}
		for _, fid := range i.From {
			if _, err = t.Exec("INSERT OR IGNORE INTO intent_sources(intent_id,project_id,fact_id) VALUES(?,?,?)", i.ID, p.ID, fid); err != nil {
				return err
			}
		}
	}
	return nil
}

type APIError struct {
	Status int
	Detail any
}

func (e *APIError) Error() string         { return fmt.Sprint(e.Detail) }
func Err(status int, detail string) error { return &APIError{status, detail} }
