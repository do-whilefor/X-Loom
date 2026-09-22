package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"

	"xloom/internal/board"
	"xloom/internal/worker"
)

// The durable baseline is the successful execution's original input, never
// the graph at its completion time or a page read by the model mid-decision.
func (s *Scheduler) previousDecision(state board.State) *board.State {
	var previous *board.State
	for _, e := range s.executions {
		if e.ProjectID != state.Graph.Project.ID || e.Kind != "reason" || e.Status != "succeeded" {
			continue
		}
		var job worker.Job
		if json.Unmarshal(e.Job, &job) != nil || job.Graph.Project.Generation != state.Graph.Project.Generation {
			continue
		}
		// A newer legacy execution with no full state invalidates an older
		// baseline instead of silently skipping an untraceable decision.
		previous = job.State
	}
	return previous
}

func (s *Scheduler) prepareDecision(ctx context.Context, t *task, trigger string) error {
	var state board.State
	base := projectPath(t.Job.Graph.Project.ID)
	if err := s.Client.Do(ctx, "GET", base+"/state", nil, &state, &t.Lease); err != nil {
		return err
	}
	if state.Graph.Project.ID != t.Job.Graph.Project.ID || state.Graph.Project.Generation != t.Job.Graph.Project.Generation {
		return errors.New("state_changed: project round changed before decision preparation")
	}
	previous := s.previousDecision(state)
	var events []board.StateEvent
	if previous != nil && previous.Revision < state.Revision {
		err := s.Client.Do(ctx, "GET", base+"/state/events?after="+strconv.FormatInt(previous.Revision, 10), nil, &events, &t.Lease)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Missing events force a full bounded projection. The state itself
			// was read successfully and remains an immutable input snapshot.
			events = nil
		}
		for n, event := range events {
			if event.Revision > state.Revision {
				events = events[:n]
				break
			}
		}
	}
	view, err := board.BuildDecisionContext(state, previous, events, board.DefaultContextViewBytes)
	if err != nil {
		return err
	}
	t.Job.Graph = state.Graph
	t.Job.State = &state
	t.Job.Decision = view
	t.Job.DecisionRevision = state.DecisionRevision
	t.Job.DecisionTrigger = trigger
	t.Job.DecisionTriggers = decisionTriggers(state, previous, trigger)
	for _, e := range s.executions {
		if e.ProjectID != state.Graph.Project.ID || e.Kind != "reason" {
			continue
		}
		var job worker.Job
		if json.Unmarshal(e.Job, &job) == nil && job.State != nil && board.DecisionStateVersion(*job.State) == view.StateVersion {
			t.Job.DecisionRepeated = true
			break
		}
	}
	// Registration retry identity and the successful checkpoint share exactly
	// this snapshot; newer evidence remains eligible for a subsequent run.
	s.stateRevisions[state.Graph.Project.ID] = state.DecisionRevision
	slog.Info("decision input", "project", state.Graph.Project.ID, "run", t.Job.RunID,
		"trigger", trigger, "causes", t.Job.DecisionTriggers, "mode", view.Mode,
		"fallback", view.Fallback, "from_revision", view.FromRevision, "to_revision", view.ToRevision,
		"bytes", len(view.View), "baseline_bytes", view.BaselineBytes, "repeated", t.Job.DecisionRepeated)
	return nil
}

func decisionTriggers(state board.State, previous *board.State, trigger string) []string {
	if previous == nil {
		return []string{trigger}
	}
	causes := []string{}
	if trigger == "explicit_retry" {
		causes = append(causes, trigger)
	}
	if len(state.Graph.Facts) > len(previous.Graph.Facts) {
		causes = append(causes, "new_facts")
	}
	if len(state.Graph.Hints) > len(previous.Graph.Hints) {
		causes = append(causes, "new_hints")
	}
	if previous.Graph.OpenCount() > 0 && state.Graph.OpenCount() == 0 {
		causes = append(causes, "intents_finished")
	}
	if state.DecisionRevision > previous.DecisionRevision {
		causes = append(causes, "state_changed")
	}
	if len(causes) == 0 {
		causes = append(causes, trigger)
	}
	return causes
}
