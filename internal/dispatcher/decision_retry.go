package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"

	"xloom/internal/board"
	"xloom/internal/worker"
)

// Only the scheduler goroutine reads/updates its registry snapshot. The server
// grants and consumes retries durably, including when this dispatcher restarts
// between authorization and launch.
func (s *Scheduler) automaticDecisionRetry(ctx context.Context, g *board.Graph) error {
	key := s.retryKey(*g, "reason", nil)
	attempts, candidate := 0, -1
	for n, e := range s.executions {
		if e.ProjectID != g.Project.ID || e.Kind != "reason" {
			continue
		}
		var job worker.Job
		if json.Unmarshal(e.Job, &job) != nil || job.Graph.Project.Generation != g.Project.Generation {
			continue
		}
		if e.Pending() || e.Status == "retry_requested" {
			return nil
		}
		if e.RetryKey != key {
			continue
		}
		attempts++
		if board.AutomaticDecisionRetryEligible(e) {
			candidate = n
		}
	}
	if attempts != 1 || candidate < 0 {
		return nil
	}
	e := &s.executions[candidate]
	err := s.Client.Do(ctx, "POST", projectPath(g.Project.ID)+"/executions/"+url.PathEscape(e.ID)+"/retry", map[string]bool{"automatic": true}, nil, nil)
	if err != nil {
		var protocol *ProtocolError
		if errors.As(err, &protocol) && (protocol.Status == 403 || protocol.Status == 404 || protocol.Status == 409) {
			return nil // Project/lease changed; the next tick reads the new state.
		}
		return err
	}
	e.Status = "retry_requested"
	if g.Project.Reason != nil && g.Project.Reason.Worker == e.Lease {
		g.Project.Reason = nil
	}
	slog.Info("decision automatic retry authorized", "project", e.ProjectID, "previous_run", e.ID)
	return nil
}
