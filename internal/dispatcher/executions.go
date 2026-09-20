package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *Scheduler) namespace() string {
	if s.Config.Container.Namespace == "" {
		return "xloom"
	}
	return s.Config.Container.Namespace
}
func (s *Scheduler) environmentID(w config.Worker) string {
	// Credentials are intentionally excluded; rotating a token must not change
	// the task. Provider/model/container configuration is part of its identity.
	env := map[string]string{}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "XLOOM_REASONING_EFFORT", "XLOOM_MAX_OUTPUT_TOKENS", "XLOOM_CONTEXT_BYTES", "XLOOM_REQUEST_TIMEOUT"} {
		env[key] = w.Env[key]
	}
	return digest(struct {
		Type    string
		Image   string
		Network string
		Caps    []string
		Env     map[string]string
	}{w.Type, s.Config.Container.Image, s.Config.Container.Network, s.Config.Container.CapAdd, env})
}

func (s *Scheduler) releaseIdleAdmissions() {
	for project := range s.admitted {
		busy := false
		for _, t := range s.running {
			if t.Job.Graph.Project.ID == project {
				busy = true
				break
			}
		}
		if !busy {
			delete(s.admitted, project)
		}
	}
}
func (s *Scheduler) retryKey(g board.Graph, kind string, intent *board.Intent) string {
	if kind != "reason" {
		if intent == nil {
			return kind
		}
		return kind + ":" + intent.ID
	}
	ended := []string{}
	for _, i := range g.Intents {
		if i.To != nil || i.ConcludedAt != nil {
			ended = append(ended, i.ID)
		}
	}
	return kind + ":" + digest(struct {
		Facts    []board.Fact
		Hints    []board.Hint
		Ended    []string
		Revision int64
	}{g.Facts, g.Hints, ended, s.stateRevisions[g.Project.ID]})
}
func (s *Scheduler) previous(g board.Graph, kind string, intent *board.Intent) string {
	iid := ""
	if intent != nil {
		iid = intent.ID
	}
	id := ""
	for _, e := range s.executions {
		if e.ProjectID == g.Project.ID && e.Kind == kind && e.Intent == iid && e.Status == "retry_requested" {
			id = e.ID
		}
	}
	return id
}
func (s *Scheduler) executionBlocked(g board.Graph, kind string, intent *board.Intent) bool {
	key := s.retryKey(g, kind, intent)
	explicit := s.previous(g, kind, intent) != ""
	for _, e := range s.executions {
		if e.ProjectID != g.Project.ID || e.Kind != kind {
			continue
		}
		same := kind == "reason" || (intent != nil && e.Intent == intent.ID)
		if same && e.Pending() {
			return true
		}
		if e.RetryKey == key && e.Status != "retry_requested" && e.Status != "retried" && !explicit {
			return true
		}
	}
	return false
}
func (s *Scheduler) loadExecutions(ctx context.Context) error {
	var list []board.Execution
	if err := s.Client.Do(ctx, "GET", "/executions?namespace="+url.QueryEscape(s.namespace()), nil, &list, nil); err != nil {
		return err
	}
	s.executions = list
	// A restart must not forget the last decision's input boundary. New evidence
	// received during that decision is deliberately not swallowed by this point.
	for _, e := range list {
		if e.Kind != "reason" || e.Status != "succeeded" {
			continue
		}
		var j worker.Job
		if json.Unmarshal(e.Job, &j) != nil {
			continue
		}
		s.checkpoints[e.ProjectID] = checkpoint{len(j.Graph.Facts), len(j.Graph.Hints), j.Graph.OpenCount()}
		s.decisionRevisions[e.ProjectID] = j.DecisionRevision
	}
	return nil
}
func (s *Scheduler) register(ctx context.Context, t *task) error {
	t.Job.PreviousRunID = s.previous(t.Job.Graph, t.Job.Kind, t.Job.Intent)
	raw, err := json.Marshal(t.Job)
	if err != nil {
		return err
	}
	e := board.Execution{ProjectID: t.Job.Graph.Project.ID, ID: t.Job.RunID, Namespace: s.namespace(), Backend: t.Worker.Name, Kind: t.Job.Kind, Intent: t.Lease.Intent, Lease: t.Lease.Run, Job: raw, RetryKey: s.retryKey(t.Job.Graph, t.Job.Kind, t.Job.Intent)}
	if err = s.Client.Do(ctx, "POST", projectPath(e.ProjectID)+"/executions", e, &t.Execution, &t.Lease); err != nil {
		return err
	}
	s.executions = append(s.executions, t.Execution)
	return nil
}
func (s *Scheduler) start(ctx context.Context, t *task) {
	// Dispatcher shutdown is a recoverable interruption. A project stop or
	// lease loss is a hard cancellation. Detach parent cancellation only long
	// enough to relay this distinction to the Docker process controller.
	runCtx, cancelCause := context.WithCancelCause(context.WithoutCancel(ctx))
	cancel := func() {
		if ctx.Err() != nil {
			cancelCause(worker.ErrInterrupted)
		} else {
			cancelCause(context.Canceled)
		}
	}
	t.Cancel = cancel
	t.Root = ctx
	t.LeaseTimeout = s.leaseTimeout
	go func() {
		select {
		case <-ctx.Done():
			cancelCause(worker.ErrInterrupted)
		case <-runCtx.Done():
		}
	}()
	s.running[t.Job.RunID] = t
	s.admitted[t.Job.Graph.Project.ID] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		outcome, err := s.runTask(runCtx, t)
		s.done <- finished{t, outcome, err}
	}()
}
func executionPath(t *task) string {
	return projectPath(t.Job.Graph.Project.ID) + "/executions/" + url.PathEscape(t.Job.RunID)
}
func (s *Scheduler) status(ctx context.Context, t *task, status string, result worker.Result) error {
	return s.Client.Do(ctx, "POST", executionPath(t)+"/status", map[string]any{"status": status, "result": result}, nil, &t.Lease)
}
func (s *Scheduler) terminal(t *task, status string, result worker.Result) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.status(ctx, t, status, result)
}
func (s *Scheduler) recoverExecutions(ctx context.Context, states map[string]string) error {
	for _, e := range s.executions {
		if !e.Pending() || s.running[e.ID] != nil {
			continue
		}
		var j worker.Job
		if err := json.Unmarshal(e.Job, &j); err != nil {
			return fmt.Errorf("execution %s has invalid job: %w", e.ID, err)
		}
		t := &task{Job: j, Execution: e, Lease: Lease{Run: e.Lease, Kind: e.Kind, Intent: e.Intent}}
		if states[e.ProjectID] != "active" {
			s.terminal(t, "cancelled", worker.Result{Status: "failed", FailureKind: "hard_cancelled", Error: "project is not active"})
			continue
		}
		if len(s.running) >= s.Config.Runtime.MaxWorkers {
			break
		}
		if !s.admitted[e.ProjectID] && len(s.admitted) >= s.Config.Runtime.MaxProjects {
			continue
		}
		projectCount, backendCount := 0, 0
		for _, running := range s.running {
			if running.Job.Graph.Project.ID == e.ProjectID {
				projectCount++
			}
			if running.Worker.Name == e.Backend {
				backendCount++
			}
		}
		if projectCount >= s.Config.Runtime.MaxProjectWorkers {
			continue
		}
		found := false
		for _, w := range s.Config.Workers {
			if w.Name == e.Backend {
				t.Worker = w
				found = true
				break
			}
		}
		if !found || !slices.Contains(t.Worker.TaskTypes, e.Kind) || j.EnvironmentID != s.environmentID(t.Worker) {
			s.terminal(t, "failed", worker.Result{Status: "failed", FailureKind: "configuration", Error: "registered execution environment changed"})
			continue
		}
		if backendCount >= t.Worker.MaxRunning || time.Now().Before(s.unhealthy[t.Worker.Name]) {
			continue
		}
		err := s.Client.Do(ctx, "POST", executionPath(t)+"/resume", map[string]any{}, &t.Execution, &t.Lease)
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404 || pe.Status == 409) {
				s.terminal(t, "failed", worker.Result{Status: "failed", FailureKind: "session_invalid", Error: err.Error()})
				continue
			}
			return err
		}
		s.start(ctx, t)
	}
	return nil
}
func (s *Scheduler) runRegistered(ctx context.Context, t *task, stopHeartbeat func()) (string, error) {
	if t.Execution.Status != "result_pending" {
		if err := s.status(ctx, t, "running", worker.Result{}); err != nil {
			return "interrupted", err
		}
		var result worker.Result
		for attempt := 0; attempt <= 2; attempt++ {
			var err error
			result, err = s.Runner.Run(ctx, t.Worker, t.Job)
			if ctx.Err() != nil {
				// Process-wide shutdown leaves a recoverable registry entry. Explicit
				// project/lease cancellation is terminal and cannot mint a fresh attempt.
				if t.Root != nil && t.Root.Err() != nil {
					return "interrupted", ctx.Err()
				}
				s.terminal(t, "cancelled", worker.Result{Status: "failed", FailureKind: "hard_cancelled", Error: ctx.Err().Error()})
				return "cancelled", ctx.Err()
			}
			if err != nil {
				result = worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: err.Error()}
			}
			if result.Status == "success" {
				break
			}
			if !result.Retryable || attempt == 2 {
				result.Retryable = false
				s.terminal(t, "failed", result)
				return "failed", errors.New(result.Error)
			}
			if err = s.status(ctx, t, "retryable", result); err != nil {
				return "interrupted", err
			}
			// The Worker persists its own recovery counter before issuing a request.
			// This short backoff never releases the step lease or resets its deadline.
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "interrupted", ctx.Err()
			case <-timer.C:
			}
		}
		if err := s.status(ctx, t, "result_pending", result); err != nil {
			return "interrupted", err
		}
	}
	// Finish the business transaction before another heartbeat can observe a
	// concluded bootstrap step and cancel its project completion.
	stopHeartbeat()
	var receipt struct {
		Status string `json:"status"`
	}
	err := s.Client.Do(ctx, "POST", executionPath(t)+"/apply", map[string]any{}, &receipt, &t.Lease)
	if err != nil {
		var pe *ProtocolError
		if errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 {
			s.terminal(t, "failed", worker.Result{Status: "failed", FailureKind: "invalid_output", Error: err.Error()})
			return "failed", err
		}
		return "interrupted", err
	}
	if receipt.Status == "rejected" {
		return "rejected", nil
	}
	return "success", nil
}

func (s *Scheduler) configureGraphHandler() {
	setter, ok := s.Runner.(interface {
		SetGraphHandler(func(context.Context, worker.Job, worker.GraphRequest) (any, error))
	})
	if !ok {
		return
	}
	setter.SetGraphHandler(func(ctx context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
		// The job, not model-generated fields, supplies project and lease identity.
		backend := ""
		// Worker identity is registered on the Server; avoid reading Scheduler maps
		// from Runner goroutines, which would race with dispatch/reaping.
		var entries []board.Execution
		if err := s.Client.Do(ctx, "GET", "/executions?namespace="+url.QueryEscape(s.namespace()), nil, &entries, nil); err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.ProjectID == j.Graph.Project.ID && e.ID == j.RunID {
				backend = e.Backend
				break
			}
		}
		if backend == "" {
			return nil, errors.New("graph request has no registered execution")
		}
		lease := Lease{Run: backend + "@" + j.RunID, Kind: j.Kind}
		if j.Intent != nil {
			lease.Intent = j.Intent.ID
		}
		base := projectPath(j.Graph.Project.ID)
		switch request.Op {
		case "read_graph":
			var state board.State
			if err := s.Client.Do(ctx, "GET", base+"/state", nil, &state, &lease); err != nil {
				return nil, err
			}
			return worker.GraphPage(state, request)
		case "graph_action":
			var result board.StateActionResult
			err := s.Client.Do(ctx, "POST", base+"/state/actions", request.Action, &result, &lease)
			return result, err
		default:
			return nil, errors.New("unknown graph request")
		}
	})
}
