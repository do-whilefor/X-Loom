package dispatcher

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/contract"
	"xloom/internal/server"
	"xloom/internal/worker"
)

type controlledRunner struct {
	run      func(context.Context, config.Worker, worker.Job) (worker.Result, error)
	projects []string
	cleanup  func(context.Context, string, string) error
}

func (r *controlledRunner) Run(ctx context.Context, w config.Worker, j worker.Job) (worker.Result, error) {
	if r.run != nil {
		return r.run(ctx, w, j)
	}
	<-ctx.Done()
	return worker.Result{}, ctx.Err()
}
func (r *controlledRunner) Projects(context.Context) ([]string, error) { return r.projects, nil }
func (r *controlledRunner) Cleanup(ctx context.Context, id, state string) error {
	if r.cleanup != nil {
		return r.cleanup(ctx, id, state)
	}
	return nil
}

func scenario(t *testing.T, r Runner) (*Scheduler, context.Context) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := httptest.NewServer(server.New(store))
	t.Cleanup(h.Close)
	c := config.Config{Server: h.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 6, MaxProjects: 3, MaxProjectWorkers: 4, HealthMode: "disabled", HealthTimeout: 1}, Tasks: config.Tasks{Bootstrap: config.Task{ConcludeTimeout: 10}, Explore: config.Task{ConcludeTimeout: 10}, Reason: config.Task{MaxIntents: 3}}, Workers: []config.Worker{{Name: "mock", Type: "mock", TaskTypes: []string{"bootstrap", "reason", "explore"}, MaxRunning: 6}}}
	s := New(c, r)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		for _, task := range s.running {
			task.Cancel()
		}
		s.wg.Wait()
	})
	return s, ctx
}
func createProject(t *testing.T, s *Scheduler, ctx context.Context, bootstrap bool) board.Graph {
	t.Helper()
	var g board.Graph
	if err := s.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "task", "origin": "start", "goal": "finish", "bootstrap_enabled": bootstrap}, &g, nil); err != nil {
		t.Fatal(err)
	}
	return g
}
func createIntent(t *testing.T, s *Scheduler, ctx context.Context, id, desc string) board.Intent {
	t.Helper()
	var i board.Intent
	if err := s.Client.Do(ctx, "POST", projectPath(id)+"/intents", map[string]any{"from": []string{"origin"}, "description": desc, "creator": "test"}, &i, nil); err != nil {
		t.Fatal(err)
	}
	return i
}
func mustDo(t *testing.T, s *Scheduler, ctx context.Context, method, route string, input any) {
	t.Helper()
	if err := s.Client.Do(ctx, method, route, input, nil, nil); err != nil {
		t.Fatal(err)
	}
}
func waitFinished(t *testing.T, s *Scheduler) finished {
	t.Helper()
	select {
	case f := <-s.done:
		delete(s.running, f.Task.Job.RunID)
		return f
	case <-time.After(4 * time.Second):
		t.Fatal("task did not finish")
		return finished{}
	}
}

func TestAllConcurrencyLimits(t *testing.T) {
	for _, tt := range []struct {
		name                                    string
		global, project, worker, projects, want int
	}{
		{"global", 2, 4, 6, 3, 2}, {"per project", 6, 1, 6, 1, 1}, {"per worker", 6, 4, 2, 3, 2}, {"project admission", 6, 4, 6, 1, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx := scenario(t, &controlledRunner{})
			s.Config.Runtime.MaxWorkers = tt.global
			s.Config.Runtime.MaxProjectWorkers = tt.project
			s.Config.Runtime.MaxProjects = tt.projects
			s.Config.Workers[0].MaxRunning = tt.worker
			for p := 0; p < 2; p++ {
				g := createProject(t, s, ctx, false)
				for n := 0; n < 3; n++ {
					createIntent(t, s, ctx, g.Project.ID, "direction")
				}
			}
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if len(s.running) != tt.want {
				t.Fatalf("running=%d want=%d", len(s.running), tt.want)
			}
			byProject := map[string]int{}
			for _, task := range s.running {
				byProject[task.Job.Graph.Project.ID]++
			}
			if len(byProject) > tt.projects {
				t.Fatal("project admission exceeded")
			}
			for _, n := range byProject {
				if n > tt.project {
					t.Fatal("project concurrency exceeded")
				}
			}
		})
	}
}

func TestReasonCanOverlapExploreButNotAnotherReason(t *testing.T) {
	s, ctx := scenario(t, &controlledRunner{})
	g := createProject(t, s, ctx, false)
	createIntent(t, s, ctx, g.Project.ID, "one")
	createIntent(t, s, ctx, g.Project.ID, "two")
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.running) != 2 {
		t.Fatalf("expected concurrent explores: %d", len(s.running))
	}
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/hints", map[string]string{"content": "new evidence", "creator": "test"})
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	reasons, explores := 0, 0
	var reasonRun string
	for _, task := range s.running {
		switch task.Job.Kind {
		case "reason":
			reasons++
			reasonRun = task.Lease.Run
		case "explore":
			explores++
		}
	}
	if reasons != 1 || explores != 2 {
		t.Fatalf("reason=%d explore=%d", reasons, explores)
	}
	// Even if the server lease disappears between ticks, the still-running
	// local reason must finish cancellation before a replacement can launch.
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/reason/release", map[string]string{"worker": reasonRun})
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.running) != 3 {
		t.Fatalf("duplicate reason launched: %d", len(s.running))
	}
}

func TestBootstrapGatesInitialProjectAndFallback(t *testing.T) {
	for _, tt := range []struct {
		name                         string
		enabled, supported, existing bool
		want                         string
	}{
		{"bootstrap", true, true, false, "bootstrap"}, {"disabled", false, true, false, "reason"}, {"unsupported", true, false, false, "reason"}, {"existing unsupported bootstrap", true, false, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx := scenario(t, &controlledRunner{})
			g := createProject(t, s, ctx, tt.enabled)
			if !tt.supported {
				s.Config.Workers[0].TaskTypes = []string{"reason", "explore"}
			}
			if tt.existing {
				mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/intents", map[string]any{"from": []string{"origin"}, "description": "bootstrap", "creator": "dispatcher.bootstrap"})
			}
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(s.running) != 0 {
					t.Fatal("abandoned pending bootstrap")
				}
				return
			}
			if len(s.running) != 1 {
				t.Fatalf("initial project started %d tasks", len(s.running))
			}
			for _, task := range s.running {
				if task.Job.Kind != tt.want {
					t.Fatalf("got %s", task.Job.Kind)
				}
			}
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if len(s.running) != 1 {
				t.Fatal("initial task duplicated")
			}
		})
	}
}

func TestTriggersIgnoreNewIntentButReactToFactsHintsAndDrain(t *testing.T) {
	s := New(config.Config{}, &controlledRunner{})
	base := board.Graph{Project: board.Project{ID: "p"}, Facts: []board.Fact{{ID: "origin"}, {ID: "goal"}}, Intents: []board.Intent{{ID: "i"}}}
	s.checkpoints["p"] = checkpoint{2, 0, 1}
	g := base
	g.Intents = append(append([]board.Intent{}, g.Intents...), board.Intent{ID: "new"})
	if s.trigger(g) != "" {
		t.Fatal("new intent triggered reason")
	}
	g = base
	g.Facts = append(append([]board.Fact{}, g.Facts...), board.Fact{ID: "new"})
	if s.trigger(g) == "" {
		t.Fatal("new fact missed")
	}
	g = base
	g.Hints = []board.Hint{{ID: "h"}}
	if s.trigger(g) == "" {
		t.Fatal("new hint missed")
	}
	g = base
	g.Intents = []board.Intent{{ID: "i", To: board.Ptr("fact")}}
	if s.trigger(g) == "" {
		t.Fatal("open intent drain missed")
	}
}

func TestLateResultAfterStopOrDeleteNeverWrites(t *testing.T) {
	for _, action := range []string{"stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			r := &controlledRunner{run: func(ctx context.Context, _ config.Worker, _ worker.Job) (worker.Result, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					<-release
				}
				return worker.Result{Status: "success", Text: `{"description":"late"}`}, nil
			}}
			s, ctx := scenario(t, r)
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			g := createProject(t, s, ctx, false)
			createIntent(t, s, ctx, g.Project.ID, "one")
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			<-started
			if action == "stop" {
				mustDo(t, s, ctx, "PUT", projectPath(g.Project.ID)+"/status", map[string]string{"status": "stopped"})
			} else {
				mustDo(t, s, ctx, "DELETE", projectPath(g.Project.ID), nil)
			}
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			once.Do(func() { close(release) })
			f := waitFinished(t, s)
			if f.Outcome != "cancelled" {
				t.Fatalf("got %s: %v", f.Outcome, f.Err)
			}
			if action == "stop" {
				current, err := s.Client.Get(ctx, g.Project.ID)
				if err != nil || len(current.Facts) != 2 {
					t.Fatalf("late fact persisted: %+v %v", current, err)
				}
			}
		})
	}
}

func TestLostLeaseCancelsOnlyItsTaskAndCannotReleaseNewOwner(t *testing.T) {
	started := make(chan worker.Job, 4)
	r := &controlledRunner{run: func(ctx context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
		started <- j
		<-ctx.Done()
		return worker.Result{}, ctx.Err()
	}}
	s, ctx := scenario(t, r)
	g := createProject(t, s, ctx, false)
	createIntent(t, s, ctx, g.Project.ID, "one")
	createIntent(t, s, ctx, g.Project.ID, "two")
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := <-started, <-started
	var task *task
	for _, candidate := range s.running {
		if candidate.Job.RunID == first.RunID {
			task = candidate
		}
	}
	base := projectPath(g.Project.ID) + "/intents/" + first.Intent.ID
	mustDo(t, s, ctx, "POST", base+"/release", map[string]string{"worker": task.Lease.Run})
	mustDo(t, s, ctx, "POST", base+"/heartbeat", map[string]string{"worker": "replacement"})
	f := waitFinished(t, s)
	if f.Task.Job.RunID != first.RunID || f.Outcome != "cancelled" {
		t.Fatalf("wrong execution cancelled: %+v", f)
	}
	if _, ok := s.running[second.RunID]; !ok {
		t.Fatal("sibling task cancelled")
	}
	current, err := s.Client.Get(ctx, g.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range current.Intents {
		if i.ID == first.Intent.ID && board.Value(i.Worker) != "replacement" {
			t.Fatal("old release cleared replacement")
		}
	}
	if err = s.apply(ctx, task, contract.Result{Kind: "fact", Fact: "late"}); err == nil {
		t.Fatal("lost lease wrote a fact")
	}
}

func TestRejectedInvalidAndTimedOutTasksReleaseClaim(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		runErr     error
		want       string
	}{
		{"rejected", `{"accepted":false}`, nil, "rejected"}, {"bad contract", `{"description":123}`, nil, "failed"}, {"timeout", "", context.DeadlineExceeded, "failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &controlledRunner{run: func(context.Context, config.Worker, worker.Job) (worker.Result, error) {
				return worker.Result{Status: "success", Text: tt.text}, tt.runErr
			}}
			s, ctx := scenario(t, r)
			g := createProject(t, s, ctx, false)
			i := createIntent(t, s, ctx, g.Project.ID, "one")
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			f := waitFinished(t, s)
			if f.Outcome != tt.want {
				t.Fatalf("got %s %v", f.Outcome, f.Err)
			}
			current, err := s.Client.Get(ctx, g.Project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(current.Facts) != 2 {
				t.Fatal("invalid result became fact")
			}
			for _, got := range current.Intents {
				if got.ID == i.ID && got.Worker != nil {
					t.Fatal("claim not released")
				}
			}
		})
	}
}

func TestBootstrapConcludeSavesOnlyFact(t *testing.T) {
	r := &controlledRunner{run: func(context.Context, config.Worker, worker.Job) (worker.Result, error) {
		return worker.Result{Status: "success", Conclude: true, Text: `{"fact":{"description":"confirmed"},"complete":{"description":"must be ignored"}}`}, nil
	}}
	s, ctx := scenario(t, r)
	g := createProject(t, s, ctx, true)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFinished(t, s)
	if f.Outcome != "success" {
		t.Fatalf("%s %v", f.Outcome, f.Err)
	}
	current, err := s.Client.Get(ctx, g.Project.ID)
	if err != nil || current.Project.Status != "active" || len(current.Facts) != 3 {
		t.Fatalf("%+v %v", current, err)
	}
}

func TestReasonKeepsValidDirectionsWhenSiblingRejected(t *testing.T) {
	r := &controlledRunner{run: func(context.Context, config.Worker, worker.Job) (worker.Result, error) {
		return worker.Result{Status: "success", Text: `{"intents":[{"from":null,"description":42},{"from":["origin"],"description":"valid"}]}`}, nil
	}}
	s, ctx := scenario(t, r)
	g := createProject(t, s, ctx, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFinished(t, s)
	if f.Outcome != "success" {
		t.Fatalf("%s %v", f.Outcome, f.Err)
	}
	current, err := s.Client.Get(ctx, g.Project.ID)
	if err != nil || len(current.Intents) != 1 || current.Intents[0].Description != "valid" {
		t.Fatalf("%+v %v", current, err)
	}
}

func TestRestartStopsManagedExecutionsBeforeDispatch(t *testing.T) {
	var cleaned []string
	r := &controlledRunner{projects: []string{"old-a", "old-b"}}
	s, parent := scenario(t, r)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	r.cleanup = func(_ context.Context, id, state string) error {
		if state != "stopped" {
			t.Errorf("restart cleanup=%s", state)
		}
		cleaned = append(cleaned, id)
		if len(cleaned) == 2 {
			cancel()
		}
		return nil
	}
	if err := s.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(cleaned) != 2 || len(s.running) != 0 {
		t.Fatalf("cleanup=%v running=%d", cleaned, len(s.running))
	}
}

func TestHealthModes(t *testing.T) {
	s, ctx := scenario(t, &controlledRunner{})
	calls := 0
	s.CheckHealth = func(context.Context, config.Worker) error { calls++; return errors.New("offline") }
	if err := s.Health(ctx, false); err != nil || calls != 0 {
		t.Fatal("disabled health probe ran")
	}
	if err := s.Health(ctx, true); err == nil || calls != 1 {
		t.Fatal("forced health probe missing")
	}
	if s.choose("p", "reason") != nil {
		t.Fatal("unhealthy worker selected")
	}
}
