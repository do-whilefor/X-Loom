package board

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCairnDatabaseMigrationAndCounterRecovery(t *testing.T) {
	for _, mode := range []string{"legacy", "disabled", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			extra := ""
			if mode != "legacy" {
				extra = ",bootstrap_mode TEXT NOT NULL DEFAULT '" + mode + "'"
			}
			_, err = db.Exec(`CREATE TABLE projects(id TEXT PRIMARY KEY,title TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',created_at TEXT NOT NULL,reason_worker TEXT,reason_trigger TEXT,reason_started_at TEXT,reason_last_heartbeat_at TEXT` + extra + `); INSERT INTO projects(id,title,created_at) VALUES('proj_041','legacy','2026-01-01T00:00:00Z')`)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			err = s.Do(context.Background(), func(tx *Tx) error {
				g, err := tx.Load("proj_041")
				if err != nil {
					return err
				}
				if g.Project.Bootstrap != (mode != "disabled") {
					t.Fatalf("bootstrap: %+v", g.Project)
				}
				id, err := tx.Next("", "project")
				if id != "proj_042" {
					t.Fatalf("counter collision: %s", id)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTransactionRollbackAndFactsAreAppendOnly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	g := Graph{Project: Project{ID: "proj_001", Title: "test", Status: "active", CreatedAt: "2026-01-01T00:00:00Z"}, Facts: []Fact{{ID: "origin", Description: "original"}}}
	if err := s.Do(ctx, func(tx *Tx) error { return tx.Save(g) }); err != nil {
		t.Fatal(err)
	}
	rolledBack := errors.New("cancel write")
	err := s.Do(ctx, func(tx *Tx) error {
		if _, err := tx.Next(g.Project.ID, "fact"); err != nil {
			return err
		}
		g.Facts = append(g.Facts, Fact{ID: "f001", Description: "uncommitted"})
		if err := tx.Save(g); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal(err)
	}
	g.Facts = []Fact{{ID: "origin", Description: "replacement attempt"}}
	err = s.Do(ctx, func(tx *Tx) error {
		if err := tx.Save(g); err != nil {
			return err
		}
		loaded, err := tx.Load(g.Project.ID)
		if err != nil {
			return err
		}
		if len(loaded.Facts) != 1 || loaded.Facts[0].Description != "original" {
			t.Fatalf("facts mutated: %+v", loaded.Facts)
		}
		id, err := tx.Next(g.Project.ID, "fact")
		if id != "f001" {
			t.Fatalf("counter not rolled back: %s", id)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWaitingTransactionHonorsCancellation(t *testing.T) {
	s := openTestStore(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- s.Do(context.Background(), func(tx *Tx) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := s.Do(ctx, func(tx *Tx) error { called = true; return nil })
	close(release)
	if firstErr := <-finished; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancellation ignored: %v called=%v", err, called)
	}
}

func TestLeaseExpirationPreservesHeartbeatAndConcludedOwner(t *testing.T) {
	s := openTestStore(t)
	s.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC) }
	old := "2026-01-01T00:00:00Z"
	g := Graph{Project: Project{ID: "proj_001", Title: "test", Status: "active", CreatedAt: old, Reason: &Reason{Worker: "reason", Trigger: "initial", StartedAt: old, Heartbeat: old}}, Intents: []Intent{{ID: "i001", Description: "open", Creator: "r", CreatedAt: old, Worker: Ptr("a"), Heartbeat: &old}, {ID: "i002", Description: "done", Creator: "r", CreatedAt: old, Worker: Ptr("b"), Heartbeat: &old, To: Ptr("f001"), ConcludedAt: &old}}}
	err := s.Do(context.Background(), func(tx *Tx) error {
		if err := tx.Save(g); err != nil {
			return err
		}
		if err := tx.Expire(); err != nil {
			return err
		}
		got, err := tx.Load(g.Project.ID)
		if err != nil {
			return err
		}
		if got.Project.Reason != nil || got.Intents[0].Worker != nil || Value(got.Intents[0].Heartbeat) != old || Value(got.Intents[1].Worker) != "b" {
			t.Fatalf("incorrect expiration: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFailedMigrationRollsBackSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "damaged.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE projects(id TEXT PRIMARY KEY,title TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',created_at TEXT NOT NULL,reason_worker TEXT,reason_trigger TEXT,reason_started_at TEXT,reason_last_heartbeat_at TEXT); CREATE TABLE scoped_counters(unexpected TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("damaged schema unexpectedly accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name='bootstrap_enabled'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left partial schema changes")
	}
}

func TestInMemoryStore(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(tx *Tx) error {
		settings, err := tx.Settings()
		if settings.IntentTimeout != 15 || settings.ReasonTimeout != 15 {
			t.Fatalf("defaults: %+v", settings)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
