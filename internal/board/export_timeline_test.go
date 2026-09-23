package board

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestGraphOnlyExportCannotSilentlyDropFGSHistory(t *testing.T) {
	if _, err := Export(Graph{}, "timeline"); err == nil {
		t.Fatal("timeline must require FGS state and events")
	}
}

func TestTimelinePreservesCorrectionsAndWithdrawnPlans(t *testing.T) {
	f := newFindingFixture(t)
	finding := f.action("finding", "first-finding", findingInput(f.first))
	f.correctFirst()
	f.fence = ExecutionFence{Run: "planner@first", Lease: "reason"}
	goal := f.action("goal", "goal", map[string]any{"action": "add", "condition": "WITHDRAWN_CONDITION"})
	step := f.action("step", "step", stepInput("ABANDONED_DIRECTION", []string{"origin"}, goal.ID, 1))
	f.action("step", "priority", map[string]any{"action": "priority", "id": step.ID, "priority": 10, "reason": "REORDER_REASON"})
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": step.ID, "reason": "ABANDON_REASON"})
	f.action("goal", "withdraw", map[string]any{"action": "withdraw", "id": goal.ID, "reason": "WITHDRAW_REASON"})
	f.tx(func(tx *Tx) error {
		timeline, err := tx.ExportTimeline("proj_001")
		if err != nil {
			return err
		}
		for _, content := range []string{"first observation", "controlled replacement", "/runs/first.raw", "/runs/second.raw", "FACT_RELATION", "supersedes", "WITHDRAWN_CONDITION", "WITHDRAW_REASON", "ABANDONED_DIRECTION", "ABANDON_REASON", "REORDER_REASON", "FACT " + f.first.ID + " status=superseded", "FINDING " + finding.ID + " status=verified support_valid=false", "GOAL " + goal.ID + " status=withdrawn"} {
			if !strings.Contains(timeline, content) {
				t.Errorf("timeline omitted %q", content)
			}
		}
		if strings.Index(timeline, "REORDER_REASON") > strings.Index(timeline, "ABANDON_REASON") {
			t.Fatal("equal-timestamp mutations lost revision order")
		}
		return nil
	})
}

func TestTimelineContinuesAfterThousandEventsAndKeepsLegacyFacts(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		for n := 1; n <= 1001; n++ {
			result, _ := json.Marshal(map[string]string{"reason": fmt.Sprintf("event-%04d", n)})
			// Older diagnostic events can have a payload without a result.
			event, _ := json.Marshal(StateEvent{Revision: int64(n), Op: "execution_failed", ID: "legacy-step", CreatedAt: tx.Now, Payload: result})
			if _, err := tx.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES('proj_001',?,?)", n, string(event)); err != nil {
				return err
			}
		}
		timeline, err := tx.ExportTimeline("proj_001")
		if err == nil && (!strings.Contains(timeline, "event-1001") || !strings.Contains(timeline, "First observation") || !strings.Contains(timeline, "snapshot (no creation event recorded)")) {
			t.Fatal("timeline lost later events or pre-event legacy records")
		}
		return err
	})
}
