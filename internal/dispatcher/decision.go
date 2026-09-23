package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"xloom/internal/board"
)

func (s *Scheduler) prepareDecision(ctx context.Context, t *task, trigger string) error {
	var state board.State
	base := projectPath(t.Job.Graph.Project.ID)
	if err := s.Client.Do(ctx, "GET", base+"/state", nil, &state, &t.Lease); err != nil {
		return err
	}
	if state.Graph.Project.ID != t.Job.Graph.Project.ID || state.Graph.Project.Generation != t.Job.Graph.Project.Generation {
		return errors.New("state_changed: project round changed before decision preparation")
	}
	// The claim and state read can race new evidence. Query retry metadata for
	// the same immutable input that registration will persist below.
	s.stateRevisions[state.Graph.Project.ID] = state.DecisionRevision
	query, err := s.executionCheck(ctx, state.Graph, "reason", nil, board.DecisionStateVersion(state))
	if err != nil {
		return err
	}
	previous := query.LatestDecision
	var cursor *board.DecisionCursor
	if previous != nil && previous.HasState {
		cursor = &board.DecisionCursor{ProjectID: previous.ProjectID, Generation: previous.Generation, Revision: previous.InputRevision}
	}
	var events []board.StateEvent
	if cursor != nil && cursor.Revision < state.Revision {
		err := s.Client.Do(ctx, "GET", base+"/state/changes?after="+strconv.FormatInt(cursor.Revision, 10)+"&through="+strconv.FormatInt(state.Revision, 10), nil, &events, &t.Lease)
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
	view, err := board.BuildDecisionContextFromCursor(state, cursor, events, board.DefaultContextViewBytes)
	if err != nil {
		return err
	}
	t.Job.Graph = state.Graph
	t.Job.State = &state
	t.Job.Decision = view
	if t.Job.GraphRPC {
		t.Job.Decision.Version = 2
	}
	t.Job.DecisionRevision = state.DecisionRevision
	t.Job.DecisionTrigger = trigger
	t.Job.DecisionTriggers = decisionTriggers(state, previous, trigger)
	t.Job.DecisionRepeated = query.Repeated

	// Registration retry identity and the successful checkpoint share exactly
	// this snapshot; newer evidence remains eligible for a subsequent run.
	slog.Info("decision input", "project", state.Graph.Project.ID, "run", t.Job.RunID,
		"trigger", trigger, "causes", t.Job.DecisionTriggers, "mode", view.Mode,
		"fallback", view.Fallback, "from_revision", view.FromRevision, "to_revision", view.ToRevision,
		"bytes", len(view.View), "baseline_bytes", view.BaselineBytes, "repeated", t.Job.DecisionRepeated)
	return nil
}

func decisionTriggers(state board.State, previous *board.ExecutionSummary, trigger string) []string {
	if previous == nil {
		return []string{trigger}
	}
	causes := []string{}
	if trigger == "explicit_retry" {
		causes = append(causes, trigger)
	}
	if len(state.Graph.Facts) > previous.FactCount {
		causes = append(causes, "new_facts")
	}
	if len(state.Graph.Hints) > previous.HintCount {
		causes = append(causes, "new_hints")
	}
	if previous.OpenCount > 0 && state.Graph.OpenCount() == 0 {
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
