package board

import "encoding/json"

// SaveLegacyMutation adds the same ordered change history to compatibility
// writes as StateAction. Save itself remains a persistence primitive: modern
// commands already own their events and must not receive a second one here.
// The caller's transaction commits the graph, counters and event together.
func (t *Tx) SaveLegacyMutation(g Graph, op, id, run string, payload, result any) error {
	decisionChange := false
	switch op {
	case "step", "complete":
		// A planner's own proposed work must not trigger another planning turn.
	case "hint", "step_completed", "reopen":
		decisionChange = true
	default:
		return Err(422, "unsupported legacy mutation event")
	}
	before, err := t.Load(g.Project.ID)
	if err != nil {
		return err
	}
	if err = t.Save(g); err != nil {
		return err
	}
	if op == "hint" || op == "step" || op == "reopen" {
		if err = t.CheckContextCapacity(g.Project.ID); err != nil {
			return err
		}
	}
	after, err := t.Load(g.Project.ID)
	if err != nil {
		return err
	}
	// Compare persisted shared content, not the input graph: Save intentionally
	// ignores edits to immutable facts and ignores omitted optional metadata.
	// Heartbeats and lease ownership are not domain changes.
	if DecisionStateVersion(State{Graph: before}) == DecisionStateVersion(State{Graph: after}) {
		return nil
	}
	revision, err := t.advanceStateRevision(g.Project.ID, decisionChange)
	if err != nil {
		return err
	}
	input, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	output, err := json.Marshal(result)
	if err != nil {
		return err
	}
	event, err := json.Marshal(StateEvent{Revision: revision, Op: op, ID: id, RunID: run, CreatedAt: t.Now, Payload: input, Result: output})
	if err != nil {
		return err
	}
	_, err = t.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES(?,?,?)", g.Project.ID, revision, string(event))
	return err
}
