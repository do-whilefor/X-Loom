package board

import (
	"context"
	"errors"
	"testing"
)

func TestRestartRollsBackAtomically(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.Do(ctx, func(tx *Tx) error {
		return tx.Save(Graph{Project: Project{ID: "proj_001", Title: "original", Status: "stopped", CreatedAt: tx.Now}, Facts: []Fact{{ID: "origin", Description: "input"}, {ID: "goal", Description: "goal"}, {ID: "f001", Description: "existing evidence"}}, Intents: []Intent{{ID: "i001", From: []string{"origin"}, Description: "step", Creator: "old", Worker: Ptr("old"), CreatedAt: tx.Now}}})
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort after restart")
	if err := store.Do(ctx, func(tx *Tx) error {
		if _, err := tx.RestartProject("proj_001", nil); err != nil {
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
		if g.Project.Status != "stopped" || g.Project.Generation != 0 || g.Project.RestartedAt != "" || len(g.Facts) != 3 || len(g.Intents) != 1 {
			t.Fatalf("partial restart survived rollback: %+v", g)
		}
		revoked, err := tx.RunRevoked("proj_001", "old")
		if revoked {
			t.Fatal("lease tombstone survived failed restart")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
