package board

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLegacyMutationIgnoresPersistedNoopsAndRuntimeLeases(t *testing.T) {
	f := newPlanFixture(t)
	f.action("step", "planned", stepInput("Observe fixture", []string{"origin"}, "goal", 0))
	before := f.state()
	f.tx(func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		g.Project.Reason.Heartbeat = "2026-09-22T10:01:00Z"
		g.Intents[0].Worker, g.Intents[0].Heartbeat = Ptr("executor@lease"), Ptr(tx.Now)
		// Existing facts are immutable in the persistence layer; an attempted
		// in-memory edit must not produce an event for a change never saved.
		g.Facts[0].Description = "ignored edit"
		return tx.SaveLegacyMutation(g, "step", g.Intents[0].ID, "", nil, g.Intents[0])
	})
	after := f.state()
	if after.Revision != before.Revision || after.DecisionRevision != before.DecisionRevision || DecisionStateVersion(after) != DecisionStateVersion(before) {
		t.Fatal("runtime-only or ignored changes emitted a business revision")
	}
	if Value(after.Graph.Intents[0].Worker) != "executor@lease" {
		t.Fatal("suppressing events also discarded the lease update")
	}
	f.tx(func(tx *Tx) error {
		events, err := tx.StateEvents("proj_001", 0)
		if err == nil && len(events) != 1 {
			t.Fatalf("modern action or legacy no-op emitted duplicate events: %+v", events)
		}
		return err
	})
}

func TestLegacyMutationPreservesExtendedMetadataAndEventSequence(t *testing.T) {
	f := newPlanFixture(t)
	f.action("goal", "child", map[string]any{"action": "add", "parent_id": "goal", "condition": "Verify one observation"})
	before := f.state()
	f.tx(func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		h := Hint{ID: "h001", Content: "Use the revised fixture", Creator: "user", CreatedAt: tx.Now}
		g.Hints = append(g.Hints, h)
		return tx.SaveLegacyMutation(g, "hint", h.ID, "", h, h)
	})
	after := f.state()
	if len(after.Goals) != 2 || after.Goals[1].Condition != before.Goals[1].Condition || after.Revision != before.Revision+1 || after.DecisionRevision != before.DecisionRevision+1 {
		t.Fatalf("legacy event lost extended state or counters: %+v", after)
	}
	f.tx(func(tx *Tx) error {
		events, err := tx.StateEvents("proj_001", 0)
		if err == nil && (len(events) != 2 || events[0].Revision != 1 || events[1].Revision != 2 || events[1].Op != "hint") {
			t.Fatalf("mixed events are not contiguous: %+v", events)
		}
		if err == nil && tx.ValidateStateCompletion("proj_001", []string{"f001"}) == nil {
			t.Fatal("legacy event weakened the unresolved child goal completion check")
		}
		return err
	})
}

func TestLegacyEventFailureRollsBackGraphAndRevision(t *testing.T) {
	f := newPlanFixture(t)
	before, _ := json.Marshal(f.state())
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_legacy_event BEFORE INSERT ON xloom_state_events BEGIN SELECT RAISE(ABORT, 'event unavailable'); END")
		return err
	})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		h := Hint{ID: "h001", Content: "Must roll back", Creator: "user", CreatedAt: tx.Now}
		g.Hints = append(g.Hints, h)
		return tx.SaveLegacyMutation(g, "hint", h.ID, "", h, h)
	})
	if err == nil {
		t.Fatal("event failure did not abort the transaction")
	}
	after, _ := json.Marshal(f.state())
	if string(after) != string(before) {
		t.Fatal("failed event left a partial graph mutation or revision")
	}
}
