package board

// RequireActive gates exploration writes; project management and human Hints
// have their own state rules and deliberately do not call it.
func (g Graph) RequireActive() error {
	if g.Project.Status != "active" {
		return Err(403, "Project is "+g.Project.Status)
	}
	return nil
}

func (g Graph) ValidateSources(from []string) error {
	for _, id := range from {
		found := false
		for _, f := range g.Facts {
			if f.ID == id {
				found = true
				break
			}
		}
		if !found {
			return Err(404, "Fact "+id+" not found")
		}
	}
	for _, id := range from {
		if id == "goal" {
			return Err(400, "goal cannot be used in from")
		}
	}
	return nil
}

// ExecutionFence adds conditional writes for the dispatcher. An empty Run
// retains the public Cairn semantics for existing clients.
type ExecutionFence struct {
	Run, Lease, Intent string
	AllowConcluded     bool
}

func (t *Tx) CheckExecution(g Graph, fence ExecutionFence) error {
	if fence.Run == "" {
		return nil
	}
	revoked, err := t.RunRevoked(g.Project.ID, fence.Run)
	if err != nil {
		return err
	}
	if revoked {
		return Err(409, "Execution was revoked")
	}
	if fence.Lease == "reason" {
		if g.Project.Reason == nil || g.Project.Reason.Worker != fence.Run {
			return Err(409, "Reason execution no longer owns its lease")
		}
		return nil
	}
	for _, i := range g.Intents {
		// Bootstrap concludes before completing. No other late operation may
		// use an already-concluded intent to authorize new graph writes.
		if i.ID == fence.Intent && (i.To == nil || fence.AllowConcluded) && Value(i.Worker) == fence.Run {
			return nil
		}
	}
	return Err(409, "Intent execution no longer owns its lease")
}

func (t *Tx) SetStatus(g *Graph, status string) error {
	if status != "active" && status != "stopped" {
		return Err(422, "status must be active or stopped")
	}
	if g.Project.Status == "completed" {
		return Err(409, "Completed projects cannot change status")
	}
	if g.Project.Status == status {
		return nil
	}
	if status == "stopped" {
		if err := t.markPausedExecutions(*g); err != nil {
			return err
		}
		if err := t.RevokeRuns(g.Project.ID); err != nil {
			return err
		}
		g.Project.Reason = nil
		for n := range g.Intents {
			if g.Intents[n].ConcludedAt == nil {
				g.Intents[n].Worker = nil
			}
		}
	} else if err := t.continuePausedExecutions(*g); err != nil {
		return err
	}
	g.Project.Status = status
	return nil
}
