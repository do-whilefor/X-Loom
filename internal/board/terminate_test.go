package board

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminationIsAtomicAndPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	now := time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC)
	store.Now = func() time.Time { return now }
	ctx := context.Background()
	if err := store.Do(ctx, func(tx *Tx) error {
		return tx.Save(Graph{Project: Project{ID: "proj_001", Title: "unfinished", Status: "active", CreatedAt: tx.Now}, Facts: []Fact{{ID: "origin", Description: "input"}, {ID: "goal", Description: "goal"}}})
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort termination transaction")
	if err := store.Do(ctx, func(tx *Tx) error {
		if _, err := tx.TerminateProject("proj_001", nil); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("rollback: %v", err)
	}
	if err := store.Do(ctx, func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		if g.Project.Status != "active" || g.Project.TerminatedAt != "" {
			t.Fatal("failed termination partially committed")
		}
		_, err = tx.TerminateProject("proj_001", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Do(ctx, func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		if g.Project.Status != "terminated" || g.Project.TerminatedAt != now.Format(time.RFC3339) {
			t.Fatalf("termination did not persist: %+v", g.Project)
		}
		// Legacy graph writers may omit newly added presentation metadata.
		g.Project.TerminatedAt = ""
		if err = tx.Save(g); err != nil {
			return err
		}
		g, err = tx.Load("proj_001")
		if err != nil {
			return err
		}
		if g.Project.TerminatedAt != now.Format(time.RFC3339) {
			t.Fatal("legacy save erased termination time")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
