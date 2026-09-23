package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

// The scripted planner supplies the intended directions, not model quality.
// Execute waits at a barrier so the real scheduler's limits can be checked.
type coldStartRunner struct {
	client         *Client
	directions     int
	executeStarted chan worker.Job
	release        chan struct{}
	mu             sync.Mutex
	kinds          []string
}

func (r *coldStartRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.mu.Lock()
	r.kinds = append(r.kinds, job.Kind)
	r.mu.Unlock()
	data := map[string]any{}
	switch job.Kind {
	case "reason":
		if len(job.Graph.Intents) == 0 {
			directions := []any{}
			for i := 0; i < r.directions; i++ {
				directions = append(directions, map[string]any{"from": []string{"origin"}, "description": fmt.Sprintf("Check independent fixture %d", i)})
			}
			data["intents"] = directions
		} else if job.Graph.OpenCount() == 0 {
			sources := []string{}
			for _, fact := range job.Graph.Facts {
				if fact.ID != "origin" && fact.ID != "goal" {
					sources = append(sources, fact.ID)
				}
			}
			data["complete"] = map[string]any{"from": sources, "description": "All requested synthetic checks are supported"}
		}
	case "explore":
		r.executeStarted <- job
		select {
		case <-r.release:
		case <-ctx.Done():
			return worker.Result{}, ctx.Err()
		}
		data["description"] = "Fixture rejected unauthenticated access during the requested check"
		if job.ResultContractVersion >= 2 {
			lease := Lease{Run: backend.Name + "@" + job.RunID, Kind: job.Kind, Intent: job.Intent.ID}
			payload := map[string]any{
				"description": data["description"], "scope": job.Intent.Description,
				"observed_at": time.Now().UTC().Format(time.RFC3339),
				"evidence":    []board.EvidenceRef{{RunID: job.RunID, Path: "observations.txt", Excerpt: "HTTP 401"}},
			}
			var receipt board.StateActionResult
			if err := r.client.Do(ctx, "POST", projectPath(job.Graph.Project.ID)+"/state/actions", map[string]any{"op": "fact", "idempotency_key": job.RunID + ":observation", "payload": payload}, &receipt, &lease); err != nil {
				return worker.Result{}, err
			}
			data = map[string]any{"fact_id": receipt.ID}
		}
	default:
		return worker.Result{}, fmt.Errorf("unexpected task kind %s", job.Kind)
	}
	response := map[string]any{"accepted": true, "data": data}
	if job.Kind == "explore" {
		response["outcome"] = "completed"
	}
	raw, err := json.Marshal(response)
	result := worker.Result{Status: "success", Type: "result", Text: string(raw)}
	if job.Decision != nil {
		result.StateVersion = job.Decision.StateVersion
	}
	return result, err
}
func (*coldStartRunner) Cleanup(context.Context, string, string) error { return nil }
func (*coldStartRunner) Projects(context.Context) ([]string, error)    { return nil, nil }

func TestNewProjectsStartWithDecideAndRespectExecuteLimits(t *testing.T) {
	for _, directions := range []int{1, 3} {
		t.Run(fmt.Sprintf("directions_%d", directions), func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "cold-start.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			httpServer := httptest.NewServer(server.New(store))
			defer httpServer.Close()
			runner := &coldStartRunner{directions: directions, executeStarted: make(chan worker.Job, 8), release: make(chan struct{})}
			cfg := config.Config{
				Server:    httpServer.URL,
				Runtime:   config.Runtime{Interval: 1, MaxWorkers: 2, MaxProjects: 1, MaxProjectWorkers: 2, HealthTimeout: 5, HealthMode: "disabled"},
				Tasks:     config.Tasks{Reason: config.Task{MaxIntents: 3}, Explore: config.Task{ConcludeTimeout: 60}},
				Container: config.Container{Image: "fixture", Network: "bridge", CompletedAction: "stop"},
				Workers:   []config.Worker{{Name: "fixture", Type: "mock", TaskTypes: []string{"reason", "explore"}, MaxRunning: 2}},
			}
			if err = cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			scheduler := New(cfg, runner)
			runner.client = scheduler.Client
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer func() { cancel(); scheduler.wg.Wait() }()
			var graph board.Graph
			if err = scheduler.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Cold start", "origin": "Synthetic inputs", "goal": "Check all requested fixtures", "bootstrap_enabled": true}, &graph, nil); err != nil {
				t.Fatal(err)
			}
			if graph.Project.Bootstrap {
				t.Fatal("Web creation parameter enabled bootstrap")
			}
			if err = scheduler.Step(ctx); err != nil {
				t.Fatal(err)
			}
			scheduler.wg.Wait()
			runner.mu.Lock()
			firstIsDecide := len(runner.kinds) == 1 && runner.kinds[0] == "reason"
			runner.mu.Unlock()
			if !firstIsDecide {
				t.Fatal("first activity was not a single Decide")
			}
			if err = scheduler.Step(ctx); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < min(directions, 2); i++ {
				select {
				case <-runner.executeStarted:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if len(scheduler.running) != min(directions, 2) {
				t.Fatalf("active tasks exceeded or missed configured capacity: %d", len(scheduler.running))
			}
			close(runner.release)
			scheduler.wg.Wait()
			for round := 0; round < 8; round++ {
				if err = scheduler.Step(ctx); err != nil {
					t.Fatal(err)
				}
				scheduler.wg.Wait()
				if err = scheduler.Client.Do(ctx, "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
					t.Fatal(err)
				}
				if graph.Project.Status == "completed" {
					break
				}
			}
			if graph.Project.Status != "completed" {
				t.Fatalf("Decide/Execute fixture stalled: %+v", graph)
			}
			if len(graph.Intents) != directions+1 || len(graph.Facts) != directions+2 {
				t.Fatalf("unexpected duplicate or missing work: %d intents, %d facts", len(graph.Intents), len(graph.Facts))
			}
			runner.mu.Lock()
			defer runner.mu.Unlock()
			executed := 0
			for _, kind := range runner.kinds {
				if kind == "bootstrap" {
					t.Fatal("new project entered legacy bootstrap")
				}
				if kind == "explore" {
					executed++
				}
			}
			if executed != directions {
				t.Fatalf("Execute ran %d times, want %d", executed, directions)
			}
		})
	}
}
