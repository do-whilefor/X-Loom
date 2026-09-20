//go:build linux

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
	"sync"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/contract"
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
	Job    worker.Job
	Worker config.Worker
	Lease  Lease
	Cancel context.CancelFunc
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
	Config      config.Config
	Client      *Client
	Runner      Runner
	running     map[string]*task
	admitted    map[string]bool
	checkpoints map[string]checkpoint
	unhealthy   map[string]time.Time
	rejected    map[string]time.Time
	cleanup     map[string]string
	cleaned     map[string]string
	done        chan finished
	cleanupDone chan cleaned
	wg          sync.WaitGroup
	cursor      int
}

func New(c config.Config, r Runner) *Scheduler {
	return &Scheduler{Config: c, Runner: r, Client: &Client{Base: c.Server}, running: map[string]*task{}, admitted: map[string]bool{}, checkpoints: map[string]checkpoint{}, unhealthy: map[string]time.Time{}, rejected: map[string]time.Time{}, cleanup: map[string]string{}, cleaned: map[string]string{}, done: make(chan finished, c.Runtime.MaxWorkers), cleanupDone: make(chan cleaned, c.Runtime.MaxProjects+8)}
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
	if s.Config.Runtime.Interval*2 >= min(settings.IntentTimeout, settings.ReasonTimeout) {
		return errors.New("heartbeat interval must leave at least two ticks within each server lease timeout")
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
			if f.Outcome == "success" && f.Task.Job.Kind == "reason" {
				g := f.Task.Job.Graph
				s.checkpoints[g.Project.ID] = checkpoint{len(g.Facts), len(g.Hints), g.OpenCount()}
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
			} else {
				slog.Warn("container cleanup failed", "project", f.ID, "error", f.Err)
			}
		default:
			return
		}
	}
}
func (s *Scheduler) Step(ctx context.Context) error {
	s.reap()
	summaries, err := s.Client.List(ctx)
	if err != nil {
		return err
	}
	states := map[string]string{}
	active := []board.Summary{}
	for _, p := range summaries {
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
		if states[t.Job.Graph.Project.ID] != "active" {
			t.Cancel()
		}
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
		s.cleanup[id] = state
		s.wg.Add(1)
		go func(id, state string) {
			defer s.wg.Done()
			cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			err := s.Runner.Cleanup(cleanupCtx, id, state)
			select {
			case s.cleanupDone <- cleaned{id, state, err}:
			case <-ctx.Done():
			}
		}(id, state)
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
	return i.To == nil && i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin"
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
	p, ok := s.checkpoints[g.Project.ID]
	if !ok {
		return "initial"
	}
	if len(g.Facts) > p.Facts || len(g.Hints) > p.Hints || (p.Open > 0 && g.OpenCount() == 0) {
		return "new_facts_or_hints_or_finished_intents"
	}
	return ""
}
func (s *Scheduler) dispatch(ctx context.Context, id string) (bool, error) {
	if s.cleanup[id] != "" {
		return false, nil
	}
	running := 0
	for _, t := range s.running {
		if t.Job.Graph.Project.ID == id {
			running++
		}
	}
	if running >= s.Config.Runtime.MaxProjectWorkers {
		return false, nil
	}
	g, err := s.Client.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if g.Project.Status != "active" {
		return false, nil
	}
	if initial(g) {
		if g.Project.Reason != nil {
			return false, nil
		}
		var boot *board.Intent
		for n := range g.Intents {
			if bootstrap(g.Intents[n]) && (boot == nil || g.Intents[n].Worker == nil) {
				boot = &g.Intents[n]
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
	if g.Project.Reason == nil {
		if trigger := s.trigger(g); trigger != "" {
			return s.launch(ctx, g, "reason", nil, trigger)
		}
	}
	var newest *board.Intent
	for n := range g.Intents {
		i := &g.Intents[n]
		if i.To != nil || i.Worker != nil || bootstrap(*i) {
			continue
		}
		local := false
		for _, t := range s.running {
			if t.Job.Graph.Project.ID == id && t.Lease.Intent == i.ID {
				local = true
			}
		}
		if !local && (newest == nil || i.CreatedAt > newest.CreatedAt) {
			newest = i
		}
	}
	if newest != nil {
		return s.launch(ctx, g, "explore", newest, "")
	}
	return false, nil
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
	total := budget.Timeout + budget.ConcludeTimeout + 30
	var taskCtx context.Context
	var cancel context.CancelFunc
	if budget.Timeout > 0 {
		taskCtx, cancel = context.WithTimeout(ctx, time.Duration(total)*time.Second)
	} else {
		taskCtx, cancel = context.WithCancel(ctx)
	}
	t := &task{Job: worker.Job{RunID: id, Kind: kind, WorkerType: w.Type, Graph: g, Intent: intent, Budget: budget, Workspace: "/workspace"}, Worker: *w, Lease: lease, Cancel: cancel}
	s.running[id] = t
	s.admitted[g.Project.ID] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		outcome, err := s.runTask(taskCtx, t)
		s.done <- finished{t, outcome, err}
	}()
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
	result, err := s.Runner.Run(ctx, t.Worker, t.Job)
	stopLease()
	<-heartbeatDone
	if ctx.Err() != nil {
		return "cancelled", ctx.Err()
	}
	if err != nil {
		return "failed", err
	}
	if result.Status != "success" {
		return "failed", errors.New(result.Error)
	}
	r, err := contract.Parse(result.Text, t.Job.Kind, result.Conclude, t.Job.Graph.OpenCount(), s.Config.Tasks.Reason.MaxIntents)
	if err != nil {
		return "failed", err
	}
	if r.Kind == "rejected" {
		return "rejected", nil
	}
	if err = s.apply(ctx, t, r); err != nil {
		return "failed", err
	}
	return "success", nil
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			callCtx, c := context.WithTimeout(ctx, interval)
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
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404 || pe.Status == 409) || time.Since(last) >= 2*interval {
				cancel()
				return
			}
		}
	}
}
func (s *Scheduler) apply(ctx context.Context, t *task, r contract.Result) error {
	base := projectPath(t.Job.Graph.Project.ID)
	post := func(path string, input, output any) error {
		return s.Client.Do(ctx, "POST", base+path, input, output, &t.Lease)
	}
	if t.Job.Kind == "reason" {
		switch r.Kind {
		case "noop":
			return nil
		case "complete":
			err := post("/complete", map[string]any{"from": r.Complete.From, "description": r.Complete.Description, "worker": t.Lease.Run}, nil)
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Status == 403 {
				return nil
			}
			return err
		case "intents":
			created := 0
			var lastErr error
			for _, i := range r.Intents {
				err := post("/intents", map[string]any{"from": i.From, "description": i.Description, "creator": t.Lease.Run}, nil)
				if err != nil {
					lastErr = err
					var pe *ProtocolError
					if errors.As(err, &pe) && pe.Status == 403 {
						return nil
					}
					continue
				}
				created++
			}
			if created == 0 {
				if lastErr != nil {
					return lastErr
				}
				return errors.New("reason created no intents")
			}
			return nil
		}
		return errors.New("invalid reason outcome")
	}
	var concluded board.Conclusion
	if err := post("/intents/"+t.Lease.Intent+"/conclude", map[string]string{"description": r.Fact, "worker": t.Lease.Run}, &concluded); err != nil {
		return err
	}
	if t.Job.Kind == "bootstrap" && r.Kind == "complete" {
		return post("/complete", map[string]any{"from": []string{concluded.Fact.ID}, "description": r.Complete.Description, "worker": t.Lease.Run}, nil)
	}
	return nil
}
