package board

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestEvidenceConclusionRollsBackWithItsOwningTransaction(t *testing.T) {
	f := newPlanFixture(t)
	fence := ExecutionFence{Run: "executor@run", Lease: "explore", Intent: "i001"}
	f.tx(func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		// The plan fixture seeds f001/f002 directly rather than allocating IDs.
		for n := 0; n < 2; n++ {
			if _, err := tx.Next("proj_001", "fact"); err != nil {
				return err
			}
		}
		g.Intents = []Intent{{ID: fence.Intent, From: []string{"origin"}, Description: "Check fixture authentication", Creator: "planner", Worker: Ptr(fence.Run), Heartbeat: Ptr(tx.Now), CreatedAt: tx.Now}}
		return tx.Save(g)
	})
	payload, _ := json.Marshal(map[string]any{"description": "Fixture refused the request", "scope": "one request", "observed_at": "2026-09-22T10:00:00Z", "evidence": []EvidenceRef{{RunID: "run", Path: "/run/frozen.raw", Excerpt: "401 Unauthorized"}}})
	before := f.state()
	injected := errors.New("execution receipt could not be saved")
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.ConcludeEvidenceStep("proj_001", fence, "", payload); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("did not reach receipt boundary: %v", err)
	}
	after := f.state()
	if after.Revision != before.Revision || len(after.Graph.Facts) != len(before.Graph.Facts) || after.Steps[0].Result != nil {
		t.Fatal("failed receipt leaked final fact or step completion")
	}
	f.tx(func(tx *Tx) error { _, err := tx.ConcludeEvidenceStep("proj_001", fence, "", payload); return err })
	after = f.state()
	if len(after.Graph.Facts) != len(before.Graph.Facts)+1 || after.Steps[0].Result == nil {
		t.Fatal("valid conclusion failed")
	}
}
