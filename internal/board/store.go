package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
CREATE TABLE IF NOT EXISTS scoped_counters(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,kind TEXT NOT NULL,value INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(project_id,kind));
CREATE TABLE IF NOT EXISTS xloom_revoked_runs(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,worker TEXT NOT NULL,PRIMARY KEY(project_id,worker));`

type Store struct {
	db  *sql.DB
	Now func() time.Time
}
type Tx struct {
	*sql.Tx
	Now             string
	inDecisionBatch bool
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	// Acquire the write reservation at BEGIN, including when a second process
	// opens this database. This keeps check-and-claim atomic across connections.
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite", path+separator+"_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Now: time.Now}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Migrate atomically: a crash cannot add bootstrap_enabled without mapping
	// a legacy disabled bootstrap_mode, or leave only some counters repaired.
	migration, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	defer migration.Rollback()
	if _, err = migration.Exec(schema + stateSchema + executionSchema + projectMetadataSchema + restartSchema + terminationSchema); err != nil {
		migration.Rollback()
		db.Close()
		return nil, err
	}
	rows, err := migration.Query("PRAGMA table_info(projects)")
	if err != nil {
		migration.Rollback()
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
			migration.Rollback()
			db.Close()
			return nil, err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		migration.Rollback()
		db.Close()
		return nil, err
	}
	if !columns["bootstrap_enabled"] {
		_, err = migration.Exec("ALTER TABLE projects ADD COLUMN bootstrap_enabled INTEGER NOT NULL DEFAULT 1")
		if err == nil && columns["bootstrap_mode"] {
			_, err = migration.Exec("UPDATE projects SET bootstrap_enabled=CASE WHEN bootstrap_mode='disabled' THEN 0 ELSE 1 END")
		}
		if err != nil {
			migration.Rollback()
			db.Close()
			return nil, err
		}
	}
	// Older or partially restored Cairn databases may omit their counters. Never
	// let a newly allocated ID overwrite a project or reference an old Fact.
	if _, err = migration.Exec(`UPDATE counters SET value=MAX(value,COALESCE((SELECT MAX(CAST(SUBSTR(id,6) AS INTEGER)) FROM projects WHERE id GLOB 'proj_[0-9]*'),0)) WHERE name='project'`); err != nil {
		migration.Rollback()
		db.Close()
		return nil, err
	}
	for _, item := range []struct{ table, kind, prefix string }{{"facts", "fact", "f"}, {"intents", "intent", "i"}, {"hints", "hint", "h"}} {
		query := fmt.Sprintf(`INSERT INTO scoped_counters(project_id,kind,value) SELECT project_id,?,MAX(CAST(SUBSTR(id,2) AS INTEGER)) FROM %s WHERE id GLOB ? GROUP BY project_id ON CONFLICT(project_id,kind) DO UPDATE SET value=MAX(value,excluded.value)`, item.table)
		if _, err = migration.Exec(query, item.kind, item.prefix+"[0-9]*"); err != nil {
			migration.Rollback()
			db.Close()
			return nil, err
		}
	}
	if err = migrateExecutionMetadata(migration); err != nil {
		migration.Rollback()
		db.Close()
		return nil, err
	}
	if err = migration.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// The one-connection pool serializes transactions with context-aware waiting.
// SQLite's immediate transactions also serialize other Store instances.
func (s *Store) Do(ctx context.Context, fn func(*Tx) error) error {
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

// RevokeRuns invalidates execution identities without changing Cairn's retained
// worker and heartbeat metadata on concluded intents. In particular, a stopped
// and then resumed project must reject a delayed bootstrap completion.
func (t *Tx) RevokeRuns(project string) error {
	_, err := t.Exec(`INSERT OR IGNORE INTO xloom_revoked_runs(project_id,worker)
SELECT project_id,worker FROM intents WHERE project_id=? AND worker IS NOT NULL
UNION SELECT id,reason_worker FROM projects WHERE id=? AND reason_worker IS NOT NULL
UNION SELECT project_id,lease FROM xloom_executions WHERE project_id=?`, project, project, project)
	return err
}

func (t *Tx) RunRevoked(project, worker string) (bool, error) {
	var revoked bool
	err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM xloom_revoked_runs WHERE project_id=? AND worker=?)", project, worker).Scan(&revoked)
	return revoked, err
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
	if kind != "project" && kind != "fact" && kind != "intent" && kind != "hint" {
		return "", fmt.Errorf("unknown ID kind %q", kind)
	}
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
	err := t.QueryRow(`SELECT p.id,p.title,p.status,p.bootstrap_enabled,p.created_at,p.reason_worker,p.reason_trigger,p.reason_started_at,p.reason_last_heartbeat_at,COALESCE(m.scenario,''),COALESCE(round.generation,0),COALESCE(round.restarted_at,''),COALESCE(termination.terminated_at,'') FROM projects p LEFT JOIN xloom_project_metadata m ON m.project_id=p.id LEFT JOIN xloom_project_rounds round ON round.project_id=p.id LEFT JOIN xloom_project_termination termination ON termination.project_id=p.id WHERE p.id=?`, id).Scan(&g.Project.ID, &g.Project.Title, &g.Project.Status, &g.Project.Bootstrap, &g.Project.CreatedAt, &rw, &rt, &rs, &rh, &g.Project.Scenario, &g.Project.Generation, &g.Project.RestartedAt, &g.Project.TerminatedAt)
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
	if p.Scenario != "" && !ValidScenario(p.Scenario) {
		return Err(422, "scenario must be ctf, pentest or audit")
	}
	var rw, rt, rs, rh *string
	if p.Reason != nil {
		r := p.Reason
		rw, rt, rs, rh = &r.Worker, &r.Trigger, &r.StartedAt, &r.Heartbeat
	}
	_, err := t.Exec(`INSERT INTO projects(id,title,status,bootstrap_enabled,created_at,reason_worker,reason_trigger,reason_started_at,reason_last_heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET title=excluded.title,status=excluded.status,bootstrap_enabled=excluded.bootstrap_enabled,reason_worker=excluded.reason_worker,reason_trigger=excluded.reason_trigger,reason_started_at=excluded.reason_started_at,reason_last_heartbeat_at=excluded.reason_last_heartbeat_at`, p.ID, p.Title, p.Status, p.Bootstrap, p.CreatedAt, rw, rt, rs, rh)
	if err != nil {
		return err
	}
	// Older graph callers omit this optional metadata. Keep their updates from
	// silently clearing the scenario chosen by the project creator.
	if p.Scenario != "" {
		if _, err = t.Exec(`INSERT INTO xloom_project_metadata(project_id,scenario) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET scenario=excluded.scenario`, p.ID, p.Scenario); err != nil {
			return err
		}
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
		if err = t.saveIntent(p.ID, i); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tx) saveIntent(project string, i Intent) error {
	_, err := t.Exec(`INSERT INTO intents(id,project_id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id,project_id) DO UPDATE SET to_fact_id=excluded.to_fact_id,worker=excluded.worker,last_heartbeat_at=excluded.last_heartbeat_at,concluded_at=excluded.concluded_at`, i.ID, project, i.To, i.Description, i.Creator, i.Worker, i.Heartbeat, i.CreatedAt, i.ConcludedAt)
	if err != nil {
		return err
	}
	for _, fid := range i.From {
		if _, err = t.Exec("INSERT OR IGNORE INTO intent_sources(intent_id,project_id,fact_id) VALUES(?,?,?)", i.ID, project, fid); err != nil {
			return err
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
