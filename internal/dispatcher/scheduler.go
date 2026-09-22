package dispatcher

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/provider"
	"xloom/internal/worker"
)

type Runner interface {
	Run(context.Context, config.Worker, worker.Job) (worker.Result, error)
	Cleanup(context.Context, string, string) error
	Projects(context.Context) ([]string, error)
}
type checkpoint struct{ Facts, Hints, Open int }
type task struct {
	Job          worker.Job
	Worker       config.Worker
	Lease        Lease
	Cancel       context.CancelFunc
	Root         context.Context
	Execution    board.Execution
	LeaseTimeout time.Duration
}
type finished struct {
	Task    *task
	Outcome string
	Err     error
}
type cleaned struct {
	ID, State string
	Err       error
}
type Scheduler struct {
	Config            config.Config
	Client            *Client
	Runner            Runner
	running           map[string]*task
	admitted          map[string]bool
	checkpoints       map[string]checkpoint
	unhealthy         map[string]time.Time
	rejected          map[string]time.Time
	cleanup           map[string]string
	cleaned           map[string]string
	done              chan finished
	cleanupDone       chan cleaned
	wg                sync.WaitGroup
	cursor            int
	executions        []board.Execution
	decisionRevisions map[string]int64
	stateRevisions    map[string]int64
	states            map[string]board.State
	generations       map[string]int64
	restartCleaned    map[string]int64
	leaseTimeout      time.Duration
	// CheckHealth overrides the model readiness probe in tests or embeddings.
	CheckHealth func(context.Context, config.Worker) error
}

func New(c config.Config, r Runner) *Scheduler {
	s := &Scheduler{Config: c, Runner: r, Client: &Client{Base: c.Server}, running: map[string]*task{}, admitted: map[string]bool{}, checkpoints: map[string]checkpoint{}, unhealthy: map[string]time.Time{}, rejected: map[string]time.Time{}, cleanup: map[string]string{}, cleaned: map[string]string{}, done: make(chan finished, c.Runtime.MaxWorkers), cleanupDone: make(chan cleaned, c.Runtime.MaxProjects+8), decisionRevisions: map[string]int64{}, stateRevisions: map[string]int64{}, states: map[string]board.State{}}
	s.configureGraphHandler()
	s.generations = map[string]int64{}
	s.restartCleaned = map[string]int64{}
	return s
}
func (s *Scheduler) Health(ctx context.Context, force bool) error {
	if s.Config.Runtime.HealthMode == "disabled" && !force {
		return nil
	}
	var result error
	for _, w := range s.Config.Workers {
		if err := s.health(ctx, w); err != nil {
			s.unhealthy[w.Name] = time.Now().Add(5 * time.Second)
			result = errors.Join(result, fmt.Errorf("worker %s: %w", w.Name, err))
			slog.Warn("worker health failed", "worker", w.Name, "error", err)
		}
	}
	return result
}
func (s *Scheduler) health(ctx context.Context, w config.Worker) error {
	if s.CheckHealth != nil {
		return s.CheckHealth(ctx, w)
	}
	if w.Type == "mock" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.Runtime.HealthTimeout)*time.Second)
	defer cancel()
	p := provider.Anthropic{BaseURL: w.Env["ANTHROPIC_BASE_URL"], Token: w.Env["ANTHROPIC_AUTH_TOKEN"], Model: w.Env["ANTHROPIC_MODEL"], MaxTokens: 10}
	_, err := p.Generate(ctx, []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	return err
}
func (s *Scheduler) Run(ctx context.Context) error {
	var settings board.Settings
	if err := s.Client.Do(ctx, "GET", "/settings", nil, &settings, nil); err != nil {
		return err
	}
	leaseTimeout := min(settings.IntentTimeout, settings.ReasonTimeout)
	s.leaseTimeout = time.Duration(leaseTimeout) * time.Second
	if s.Config.Runtime.Interval >= leaseTimeout {
		return errors.New("heartbeat interval must be shorter than each server lease timeout")
	}
	if leaseTimeout < 2*s.Config.Runtime.Interval {
		slog.Warn("server lease timeout leaves little heartbeat slack", "interval", s.Config.Runtime.Interval, "lease_timeout", leaseTimeout)
	}
	_ = s.Health(ctx, false)
	// With one dispatcher, old managed executions cannot survive a restart and
	// keep exploring after their leases are reassigned. Keep workspace contents.
	old, err := s.Runner.Projects(ctx)
	if err != nil {
		return err
	}
	for _, id := range old {
		if err = s.Runner.Cleanup(ctx, id, "stopped"); err != nil {
			return err
		}
	}
	defer func() {
		for _, t := range s.running {
			t.Cancel()
		}
		s.wg.Wait()
	}()
	tick := time.NewTicker(time.Duration(s.Config.Runtime.Interval) * time.Second)
	defer tick.Stop()
	for {
		if err := s.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("dispatcher tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
func (s *Scheduler) reap() {
	for {
		select {
		case f := <-s.done:
			delete(s.running, f.Task.Job.RunID)
			key := s.rejectKey(f.Task.Job.Graph.Project.ID, f.Task.Job.Kind, f.Task.Worker.Name)
			if f.Outcome == "unhealthy" {
				s.unhealthy[f.Task.Worker.Name] = time.Now().Add(5 * time.Second)
			} else {
				delete(s.unhealthy, f.Task.Worker.Name)
			}
			if f.Outcome == "rejected" {
				s.rejected[key] = time.Now().Add(5 * time.Second)
			} else {
				delete(s.rejected, key)
			}
			if f.Outcome == "success" && f.Task.Job.Kind == "reason" && f.Task.Job.Graph.Project.Generation == s.generations[f.Task.Job.Graph.Project.ID] {
				g := f.Task.Job.Graph
				s.checkpoints[g.Project.ID] = checkpoint{len(g.Facts), len(g.Hints), g.OpenCount()}
				s.decisionRevisions[g.Project.ID] = f.Task.Job.DecisionRevision
			}
			slog.Info("task finished", "project", f.Task.Job.Graph.Project.ID, "run", f.Task.Job.RunID, "task", f.Task.Job.Kind, "outcome", f.Outcome, "error", f.Err)
		default:
			goto cleanup
		}
	}
cleanup:
	for {
		select {
		case f := <-s.cleanupDone:
			delete(s.cleanup, f.ID)
			if f.Err == nil {
				s.cleaned[f.ID] = f.State
				if raw, ok := strings.CutPrefix(f.State, "restart:"); ok {
					generation, _ := strconv.ParseInt(raw, 10, 64)
					s.restartCleaned[f.ID] = generation
				}
			} else {
				slog.Warn("container cleanup failed", "project", f.ID, "error", f.Err)
			}
		default:
			return
		}
	}
}
func (s *Scheduler) Step(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.reap()
	if s.leaseTimeout == 0 {
		var settings board.Settings
		if err := s.Client.Do(ctx, "GET", "/settings", nil, &settings, nil); err != nil {
			return err
		}
		s.leaseTimeout = time.Duration(min(settings.IntentTimeout, settings.ReasonTimeout)) * time.Second
		if s.leaseTimeout <= time.Duration(s.Config.Runtime.Interval)*time.Second {
			return errors.New("heartbeat interval must be shorter than each server lease timeout")
		}
	}
	summaries, err := s.Client.List(ctx)
	if err != nil {
		return err
	}
	states := map[string]string{}
	active := []board.Summary{}
	for _, p := range summaries {
		s.observeGeneration(p.Project)
		states[p.ID] = p.Status
		if p.Status == "active" {
			active = append(active, p)
			delete(s.cleaned, p.ID)
			if _, ok := s.checkpoints[p.ID]; !ok && p.Working+p.Unclaimed > 0 {
				s.checkpoints[p.ID] = checkpoint{p.FactCount, p.HintCount, p.Working + p.Unclaimed}
			}
		}
	}
	for id := range s.admitted {
		if states[id] != "active" {
			delete(s.admitted, id)
			delete(s.checkpoints, id)
		}
	}
	for _, t := range s.running {
		if states[t.Job.Graph.Project.ID] != "active" || t.Job.Graph.Project.Generation != s.generations[t.Job.Graph.Project.ID] {
			t.Cancel()
		}
	}
	if err := s.loadExecutions(ctx); err != nil {
		return err
	}
	s.releaseIdleAdmissions()
	if err := s.recoverExecutions(ctx, states); err != nil {
		return err
	}
	managed, err := s.Runner.Projects(ctx)
	if err != nil {
		return err
	}
	for _, id := range managed {
		state := states[id]
		if state == "active" {
			continue
		}
		if state == "" {
			state = "deleted"
		}
		if s.cleaned[id] == state || s.cleanup[id] != "" {
			continue
		}
		s.queueCleanup(ctx, id, state)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	if len(active) > 0 {
		offset := s.cursor % len(active)
		active = append(active[offset:], active[:offset]...)
		s.cursor++
	}
	for len(s.running) < s.Config.Runtime.MaxWorkers {
		launched := false
		for _, p := range active {
			if !s.admitted[p.ID] {
				continue
			}
			ok, err := s.dispatch(ctx, p.ID)
			if err != nil {
				slog.Warn("dispatch skipped", "project", p.ID, "error", err)
			}
			launched = launched || ok
			if len(s.running) >= s.Config.Runtime.MaxWorkers {
				return nil
			}
		}
		if launched {
			continue
		}
		if len(s.admitted) >= s.Config.Runtime.MaxProjects {
			return nil
		}
		for _, p := range active {
			if s.admitted[p.ID] {
				continue
			}
			ok, err := s.dispatch(ctx, p.ID)
			if err != nil {
				slog.Warn("dispatch skipped", "project", p.ID, "error", err)
			}
			if ok {
				launched = true
				break
			}
		}
		if !launched {
			break
		}
	}
	return nil
}
func bootstrap(i board.Intent) bool {
	return i.To == nil && i.ConcludedAt == nil && i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin"
}
func initial(g board.Graph) bool {
	if len(g.Facts) != 2 {
		return false
	}
	ids := map[string]bool{}
	for _, f := range g.Facts {
		ids[f.ID] = true
	}
	if !ids["origin"] || !ids["goal"] {
		return false
	}
	for _, i := range g.Intents {
		if !bootstrap(i) {
			return false
		}
	}
	return true
}
func (s *Scheduler) trigger(g board.Graph) string {
	if s.previous(g, "reason", nil) != "" {
		return "explicit_retry"
	}
	p, ok := s.checkpoints[g.Project.ID]
	if !ok {
		return "initial"
	}
	// Legacy To-based draining excludes the planner's own abandonment (To=nil).
	// Actual scheduling separately filters State.Steps and ConcludedAt.
	if len(g.Facts) > p.Facts || len(g.Hints) > p.Hints || (p.Open > 0 && g.OpenCount() == 0) || s.stateRevisions[g.Project.ID] > s.decisionRevisions[g.Project.ID] {
		return "new_facts_or_hints_or_finished_intents"
	}
	return ""
}

// Restart keeps the project ID but replaces its execution round. Forget the
// prior planner boundary, including when a restart raced the list request.
func (s *Scheduler) observeGeneration(project board.Project) {
	if s.generations == nil {
		s.generations = map[string]int64{}
	}
	if s.generations[project.ID] != project.Generation {
		delete(s.checkpoints, project.ID)
		delete(s.decisionRevisions, project.ID)
		delete(s.stateRevisions, project.ID)
		delete(s.states, project.ID)
		s.generations[project.ID] = project.Generation
	}
}

func (s *Scheduler) queueCleanup(ctx context.Context, id, state string) {
	s.cleanup[id] = state
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := s.Runner.Cleanup(cleanupCtx, id, state)
		select {
		case s.cleanupDone <- cleaned{id, state, err}:
		case <-ctx.Done():
		}
	}()
}

// Individual process cancellation is best effort during Docker outages. A
// restart additionally requires a confirmed container stop before any worker
// may enter the new round; this retains the container and workspace files.
func (s *Scheduler) restartReady(ctx context.Context, project board.Project) bool {
	if project.Generation == 0 || s.restartCleaned[project.ID] == project.Generation {
		return true
	}
	for _, task := range s.running {
		if task.Job.Graph.Project.ID == project.ID {
			return false
		}
	}
	if s.cleanup[project.ID] == "" {
		s.queueCleanup(ctx, project.ID, "restart:"+strconv.FormatInt(project.Generation, 10))
	}
	return false
}

func (s *Scheduler) dispatch(ctx context.Context, id string) (bool, error) {
	if s.cleanup[id] != "" {
		return false, nil
	}
	running := 0
	localReason, localBootstrap := false, false
	for _, t := range s.running {
		if t.Job.Graph.Project.ID == id {
			running++
			localReason = localReason || t.Job.Kind == "reason"
			localBootstrap = localBootstrap || t.Job.Kind == "bootstrap"
		}
	}
	if running >= s.Config.Runtime.MaxProjectWorkers {
		return false, nil
	}
	var state board.State
	err := s.Client.Do(ctx, "GET", projectPath(id)+"/state", nil, &state, nil)
	if err != nil {
		return false, err
	}
	g := state.Graph
	s.observeGeneration(g.Project)
	// Cancellation may take time in a container. Do not start the new round
	// in the same workspace until every old local execution has actually left.
	stale := false
	for _, task := range s.running {
		if task.Job.Graph.Project.ID == id && task.Job.Graph.Project.Generation != g.Project.Generation {
			task.Cancel()
			stale = true
		}
	}
	if stale {
		return false, nil
	}
	if !s.restartReady(ctx, g.Project) {
		return false, nil
	}
	s.stateRevisions[id] = state.DecisionRevision
	s.states[id] = state
	if g.Project.Status != "active" {
		return false, nil
	}
	// A failed bootstrap may already have handed planning to Decide. Explicit
	// retry still refers to that same Step, even after normal steps were added.
	// Drain current project work first, then run the authorized initialization
	// attempt alone, just as the original bootstrap gate does.
	for n := range g.Intents {
		i := &g.Intents[n]
		if bootstrap(*i) && s.previous(g, "bootstrap", i) != "" {
			if running > 0 || g.Project.Reason != nil || i.Worker != nil {
				return false, nil
			}
			return s.launch(ctx, g, "bootstrap", i, "")
		}
	}
	bootstrapFailed := false
	for _, step := range state.Steps {
		if step.Status == "failed" {
			for _, intent := range g.Intents {
				if intent.ID == step.ID && bootstrap(intent) {
					bootstrapFailed = true
				}
			}
		}
	}
	if initial(g) && !bootstrapFailed {
		if g.Project.Reason != nil || localReason || localBootstrap {
			return false, nil
		}
		var boot *board.Intent
		for n := range g.Intents {
			i := &g.Intents[n]
			if bootstrap(*i) && (boot == nil || bootstrapBefore(*i, *boot)) {
				boot = i
			}
		}
		supported := false
		for _, w := range s.Config.Workers {
			supported = supported || slices.Contains(w.TaskTypes, "bootstrap")
		}
		if !g.Project.Bootstrap || (boot == nil && !supported) {
			return s.launch(ctx, g, "reason", nil, "initial")
		}
		if boot == nil {
			var i board.Intent
			err = s.Client.Do(ctx, "POST", projectPath(id)+"/intents", map[string]any{"from": []string{"origin"}, "description": "bootstrap", "creator": "dispatcher.bootstrap"}, &i, nil)
			if err != nil {
				return false, err
			}
			boot = &i
			g.Intents = append(g.Intents, i)
		}
		if boot.Worker != nil {
			return false, nil
		}
		return s.launch(ctx, g, "bootstrap", boot, "")
	}
	if g.Project.Reason == nil && !localReason {
		if trigger := s.trigger(g); trigger != "" {
			if ok, err := s.launch(ctx, g, "reason", nil, trigger); ok || err != nil {
				return ok, err
			}
		}
	}
	stepState := map[string]board.Step{}
	for _, step := range state.Steps {
		stepState[step.ID] = step
	}
	var newest *board.Intent
	for n := range g.Intents {
		i := &g.Intents[n]
		if i.To != nil || i.ConcludedAt != nil || i.Worker != nil || bootstrap(*i) || stepState[i.ID].Status == "abandoned" || s.executionBlocked(g, "explore", i) {
			continue
		}
		local := false
		for _, t := range s.running {
			if t.Job.Graph.Project.ID == id && t.Lease.Intent == i.ID {
				local = true
			}
		}
		if !local && (newest == nil || stepState[i.ID].Priority > stepState[newest.ID].Priority || (stepState[i.ID].Priority == stepState[newest.ID].Priority && i.CreatedAt > newest.CreatedAt)) {
			newest = i
		}
	}
	if newest != nil {
		return s.launch(ctx, g, "explore", newest, "")
	}
	return false, nil
}

func bootstrapBefore(a, b board.Intent) bool {
	if (a.Worker == nil) != (b.Worker == nil) {
		return a.Worker == nil
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.ID < b.ID
}
func (s *Scheduler) rejectKey(project, kind, name string) string {
	return project + "\x00" + kind + "\x00" + name
}
func (s *Scheduler) choose(project, kind string) *config.Worker {
	counts := map[string]int{}
	for _, t := range s.running {
		counts[t.Worker.Name]++
	}
	candidates := []config.Worker{}
	now := time.Now()
	for _, w := range s.Config.Workers {
		if !slices.Contains(w.TaskTypes, kind) || counts[w.Name] >= w.MaxRunning || now.Before(s.unhealthy[w.Name]) || now.Before(s.rejected[s.rejectKey(project, kind, w.Name)]) {
			continue
		}
		candidates = append(candidates, w)
	}
	mrand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return counts[a.Name] < counts[b.Name]
	})
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0]
}
func (s *Scheduler) launch(ctx context.Context, g board.Graph, kind string, intent *board.Intent, trigger string) (bool, error) {
	if s.executionBlocked(g, kind, intent) {
		return false, nil
	}
	w := s.choose(g.Project.ID, kind)
	if w == nil {
		return false, nil
	}
	var bytes [16]byte
	if _, err := crand.Read(bytes[:]); err != nil {
		return false, err
	}
	id := hex.EncodeToString(bytes[:])
	lease := Lease{Run: w.Name + "@" + id, Kind: kind}
	claim := projectPath(g.Project.ID)
	body := map[string]string{"worker": lease.Run}
	if kind == "reason" {
		claim += "/reason/claim"
		body["trigger"] = trigger
	} else {
		lease.Intent = intent.ID
		claim += "/intents/" + intent.ID + "/heartbeat"
	}
	if err := s.Client.Do(ctx, "POST", claim, body, nil, nil); err != nil {
		return false, err
	}
	budget := s.Config.Task(kind)
	// Worker owns its execution budget and the separate conclusion deadline.
	// A dispatcher deadline measured from container startup could abort before
	// a long current turn reaches the boundary where soft conclusion begins.
	t := &task{Job: worker.Job{RunID: id, Kind: kind, WorkerType: w.Type, Graph: g, Intent: intent, Budget: budget, Workspace: "/workspace", GraphRPC: w.Type != "mock", ResultContractVersion: 1, DecisionRevision: s.stateRevisions[g.Project.ID], EnvironmentID: s.environmentID(*w)}, Worker: *w, Lease: lease}
	if state, ok := s.states[g.Project.ID]; ok {
		state.Graph = g
		t.Job.State = &state
	}
	if kind == "reason" {
		if err := s.prepareDecision(ctx, t, trigger); err != nil {
			_ = s.Client.Do(ctx, "POST", s.leasePath(t)+"/release", map[string]string{"worker": lease.Run}, nil, nil)
			return false, err
		}
	}
	if err := s.register(ctx, t); err != nil {
		_ = s.Client.Do(ctx, "POST", s.leasePath(t)+"/release", map[string]string{"worker": lease.Run}, nil, nil)
		return false, err
	}
	s.start(ctx, t)
	return true, nil
}
func (s *Scheduler) runTask(ctx context.Context, t *task) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseCtx, stopLease := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() { defer close(heartbeatDone); s.heartbeat(leaseCtx, t, cancel) }()
	defer func() {
		stopLease()
		<-heartbeatDone
		releaseCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = s.Client.Do(releaseCtx, "POST", s.leasePath(t)+"/release", map[string]string{"worker": t.Lease.Run}, nil, nil)
	}()
	if s.Config.Runtime.HealthMode == "startup_and_task" {
		if err := s.health(ctx, t.Worker); err != nil {
			if ctx.Err() != nil {
				return "cancelled", ctx.Err()
			}
			return "unhealthy", err
		}
	}
	return s.runRegistered(ctx, t, func() { stopLease(); <-heartbeatDone })
}
func (s *Scheduler) leasePath(t *task) string {
	base := projectPath(t.Job.Graph.Project.ID)
	if t.Job.Kind == "reason" {
		return base + "/reason"
	}
	return base + "/intents/" + t.Lease.Intent
}
func (s *Scheduler) heartbeat(ctx context.Context, t *task, cancel context.CancelFunc) {
	interval := time.Duration(s.Config.Runtime.Interval) * time.Second
	tick := time.NewTicker(interval)
	defer tick.Stop()
	last := time.Now()
	timeout := t.LeaseTimeout
	if timeout <= interval {
		timeout = 2 * interval
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Queueing behind other transactions must not turn a healthy busy
			// Server into a hard cancellation after only two heartbeat intervals.
			// Stop before the actual Server lease expires, including queue time.
			callCtx, c := context.WithDeadline(ctx, last.Add(timeout-interval))
			err := s.Client.Do(callCtx, "POST", s.leasePath(t)+"/heartbeat", map[string]string{"worker": t.Lease.Run}, nil, &t.Lease)
			c()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				last = time.Now()
				continue
			}
			var pe *ProtocolError
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404 || pe.Status == 409) || time.Since(last) >= timeout-interval {
				cancel()
				return
			}
		}
	}
}
