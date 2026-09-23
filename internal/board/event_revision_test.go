package board

import (
	"encoding/json"
	"testing"
)

func TestEventOnlyChangesPreserveStoredMetadata(t *testing.T) {
	for _, op := range []string{"hint", "failure", "abandoned failure", "conclusion"} {
		t.Run(op, func(t *testing.T) {
			f := newPlanFixture(t)
			id := f.action("step", "add", stepInput("Observe fixture", []string{"origin"}, "goal", 0)).ID
			fence := ExecutionFence{Run: "worker@event", Lease: "explore", Intent: id}
			f.tx(func(tx *Tx) error {
				_, err := tx.Exec("UPDATE intents SET worker=?,last_heartbeat_at=? WHERE project_id='proj_001' AND id=?", fence.Run, tx.Now, id)
				return err
			})
			fact := ""
			if op == "conclusion" {
				f.tx(func(tx *Tx) error {
					// f001/f002 in this fixture were seeded without counters.
					if _, err := tx.Exec("INSERT INTO scoped_counters(project_id,kind,value) VALUES('proj_001','fact',2)"); err != nil {
						return err
					}
					raw := json.RawMessage(`{"description":"Observed response","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"run_id":"worker@event","path":"evidence/response.txt","excerpt":"response"}]}`)
					result, err := tx.StateAction("proj_001", fence, StateAction{Op: "fact", IdempotencyKey: "observation", Payload: raw})
					fact = result.ID
					return err
				})
			}
			if op == "abandoned failure" {
				f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "replaced plan"})
			}
			before := f.state()
			f.tx(func(tx *Tx) error {
				var raw string
				if err := tx.QueryRow("SELECT data FROM xloom_state WHERE project_id='proj_001'").Scan(&raw); err != nil {
					return err
				}
				// Retain even unknown future fields and formatting on version writes.
				raw = "{ \"future_metadata\": {\"preserve\":true}, " + raw[1:]
				if _, err := tx.Exec("UPDATE xloom_state SET data=? WHERE project_id='proj_001'", raw); err != nil {
					return err
				}
				if _, err := tx.Exec(`CREATE TRIGGER guard_metadata BEFORE UPDATE OF data ON xloom_state BEGIN SELECT RAISE(ABORT,'event rewrote metadata'); END`); err != nil {
					return err
				}
				switch op {
				case "hint":
					g, err := tx.Load("proj_001")
					if err != nil {
						return err
					}
					h := Hint{ID: "h001", Content: "Inspect response", Creator: "user", CreatedAt: tx.Now}
					g.Hints = append(g.Hints, h)
					if err := tx.SaveLegacyMutation(g, "hint", h.ID, "", h, h); err != nil {
						return err
					}
				case "conclusion":
					if _, err := tx.ConcludeEvidenceStep("proj_001", fence, fact, nil); err != nil {
						return err
					}
				default:
					if err := tx.recordExecutionFailure(Execution{ProjectID: "proj_001", ID: "event", Intent: id, Lease: fence.Run}, "failed", json.RawMessage(`{"error":"unavailable"}`)); err != nil {
						return err
					}
				}
				var after string
				if err := tx.QueryRow("SELECT data FROM xloom_state WHERE project_id='proj_001'").Scan(&after); err != nil {
					return err
				}
				if after != raw {
					t.Fatal("event changed retained metadata")
				}
				return nil
			})
			after := f.state()
			decisionIncrement := int64(1)
			if op == "abandoned failure" {
				decisionIncrement = 0
			}
			if after.Revision != before.Revision+1 || after.DecisionRevision != before.DecisionRevision+decisionIncrement {
				t.Fatalf("event counters changed: before=%d/%d after=%d/%d", before.Revision, before.DecisionRevision, after.Revision, after.DecisionRevision)
			}
		})
	}
}
