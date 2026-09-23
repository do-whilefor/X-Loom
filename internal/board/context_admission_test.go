package board

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStepAdmissionIncludesFullGoalAncestry(t *testing.T) {
	f := newPlanFixture(t)
	parent := f.action("goal", "parent", map[string]any{"action": "add", "condition": strings.Repeat("a", 8000)}).ID
	child := f.action("goal", "child", map[string]any{"action": "add", "parent_id": parent, "condition": strings.Repeat("b", 8000)}).ID
	before := f.state()
	raw, _ := json.Marshal(stepInput(strings.Repeat("s", 16000), []string{"origin"}, child, 0))
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("proj_001", f.fence, StateAction{Op: "step", IdempotencyKey: "oversized-step", Payload: raw})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "input_context_limit") {
		t.Fatalf("expected context admission failure, got %v", err)
	}
	after := f.state()
	if len(after.Steps) != 0 || after.Revision != before.Revision {
		t.Fatal("rejected Step left partial graph or event state")
	}
	step := f.action("step", "small-step", stepInput("Observe this endpoint", []string{"origin"}, child, 0))
	if step.ID != "i001" {
		t.Fatal("rejected Step consumed its ID")
	}
	if _, err := ContextView(f.state(), step.ID, DefaultContextViewBytes); err != nil {
		t.Fatal(err)
	}
}

func TestContextAdmissionAllowsTerminalActionsOnHistoricalOversize(t *testing.T) {
	for _, transition := range []string{"abandon", "withdraw", "achieve"} {
		t.Run(transition, func(t *testing.T) {
			f := newPlanFixture(t)
			op := "goal"
			var id string
			if transition == "abandon" {
				op = "step"
				id = f.action("step", "initial", stepInput("Inspect fixture", []string{"origin"}, "goal", 0)).ID
			} else {
				id = f.action("goal", "initial", map[string]any{"action": "add", "condition": "Inspect fixture"}).ID
			}
			// Older versions accepted human requirements beyond the context budget.
			// Releasing obsolete work must not require dropping those requirements.
			content := strings.Repeat("x", 32768)
			f.tx(func(tx *Tx) error {
				g, err := tx.Load("proj_001")
				if err != nil {
					return err
				}
				g.Hints = append(g.Hints, Hint{ID: "h001", Content: content, Creator: "user", CreatedAt: tx.Now})
				return tx.Save(g)
			})
			if err := ValidateContextCapacity(f.state()); err == nil {
				t.Fatal("fixture does not reproduce historical oversized input")
			}
			before := f.state().Revision
			payload := map[string]any{"action": transition, "id": id, "reason": "Resolve obsolete work"}
			if transition == "achieve" {
				payload["sources"] = []string{"f001"}
			}
			f.action(op, "terminal", payload)
			after := f.state()
			if len(after.Graph.Hints) != 1 || after.Graph.Hints[0].Content != content || after.Revision != before+1 {
				t.Fatal("terminal transition changed human input or lost its event")
			}
			status := ""
			for _, step := range after.Steps {
				if step.ID == id {
					status = step.Status
				}
			}
			for _, goal := range after.Goals {
				if goal.ID == id {
					status = goal.Status
				}
			}
			want := map[string]string{"abandon": "abandoned", "withdraw": "withdrawn", "achieve": "achieved"}[transition]
			if status != want {
				t.Fatalf("terminal state=%q, want %q", status, want)
			}
		})
	}
}
