package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"net/url"

	"xloom/internal/board"
)

// The Server counts every historical attempt and grants one successor
// transactionally. A dispatcher restart cannot replenish that allowance.
func (s *Scheduler) automaticDecisionRetry(ctx context.Context, g *board.Graph, check *board.ExecutionCheck) error {
	// Let recovery settle before authorizing a fresh attempt. Its terminal
	// failure is observed on the next tick, just as with normal worker reaping.
	for _, pending := range s.pendingExecutions {
		if pending.ProjectID == g.Project.ID && pending.Kind == "reason" && pending.Generation == g.Project.Generation {
			return nil
		}
	}
	if check.Pending || check.PreviousRunID != "" || check.Attempts != 1 || check.AutomaticRetryID == "" {
		return nil
	}
	id := check.AutomaticRetryID
	err := s.Client.Do(ctx, "POST", projectPath(g.Project.ID)+"/executions/"+url.PathEscape(id)+"/retry", map[string]bool{"automatic": true}, nil, nil)
	if err != nil {
		var protocol *ProtocolError
		if errors.As(err, &protocol) && (protocol.Status == 403 || protocol.Status == 404 || protocol.Status == 409) {
			return nil // Project/lease changed; the next tick reads the new state.
		}
		return err
	}
	check.PreviousRunID, check.Blocked = id, false
	// Successful authorization already checked and released this attempt's
	// lease in the same transaction. A competing lease would have rejected it.
	g.Project.Reason = nil
	slog.Info("decision automatic retry authorized", "project", g.Project.ID, "previous_run", id)
	return nil
}
