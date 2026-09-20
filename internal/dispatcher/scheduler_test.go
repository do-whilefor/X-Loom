package dispatcher

import (
	"context"
	"encoding/json"
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

type fakeRunner struct {
	mu    sync.Mutex
	kinds []string
}

func (f *fakeRunner) Projects(context.Context) ([]string, error)    { return nil, nil }
func (f *fakeRunner) Cleanup(context.Context, string, string) error { return nil }
func (f *fakeRunner) Run(_ context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
	f.mu.Lock()
	f.kinds = append(f.kinds, j.Kind)
	f.mu.Unlock()
	data := map[string]any{}
	switch j.Kind {
	case "bootstrap":
		data["fact"] = map[string]string{"description": "known"}
		data["complete"] = map[string]string{"description": "done"}
	case "explore":
		data["description"] = "confirmed"
	case "reason":
		if len(j.Graph.Facts) > 2 {
			data["complete"] = map[string]any{"from": []string{"f001"}, "description": "done"}
		} else {
			data["intents"] = []any{map[string]any{"from": []string{"origin"}, "description": "look"}}
		}
	}
	raw, _ := json.Marshal(map[string]any{"accepted": true, "data": data})
	return worker.Result{Type: "result", Status: "success", Text: string(raw)}, nil
}
func TestBusinessChains(t *testing.T) {
	for _, boot := range []bool{true, false} {
		t.Run(map[bool]string{true: "bootstrap", false: "reason-explore-reason"}[boot], func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			httpServer := httptest.NewServer(server.New(store))
			defer httpServer.Close()
			client := &Client{Base: httpServer.URL}
			var graph board.Graph
			err = client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "test", "origin": "start", "goal": "goal", "bootstrap_enabled": boot}, &graph, nil)
			if err != nil {
				t.Fatal(err)
			}
			c := config.Config{Server: httpServer.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 3, MaxProjects: 2, MaxProjectWorkers: 3, HealthMode: "disabled"}, Tasks: config.Tasks{Bootstrap: config.Task{Timeout: 30, ConcludeTimeout: 10}, Reason: config.Task{Timeout: 30, MaxIntents: 2}, Explore: config.Task{Timeout: 30, ConcludeTimeout: 10}}, Workers: []config.Worker{{Name: "mock", Type: "mock", TaskTypes: []string{"bootstrap", "reason", "explore"}, MaxRunning: 3}}}
			fake := &fakeRunner{}
			s := New(c, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); s.wg.Wait() }()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err = s.Step(ctx); err != nil {
					t.Fatal(err)
				}
				g, err := client.Get(ctx, graph.Project.ID)
				if err != nil {
					t.Fatal(err)
				}
				if g.Project.Status == "completed" {
					if len(g.Facts) != 3 {
						t.Fatal(g)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("chain did not complete")
		})
	}
}
func TestReasonTriggerAndWorkerSelection(t *testing.T) {
	s := New(config.Config{Runtime: config.Runtime{MaxWorkers: 4}, Workers: []config.Worker{{Name: "low", Priority: 2, MaxRunning: 1, TaskTypes: []string{"reason"}}, {Name: "high", Priority: 0, MaxRunning: 1, TaskTypes: []string{"reason"}}}}, &fakeRunner{})
	g := board.Graph{Project: board.Project{ID: "p"}, Facts: []board.Fact{{ID: "origin"}, {ID: "goal"}}, Intents: []board.Intent{{ID: "i"}}}
	s.checkpoints["p"] = checkpoint{2, 0, 1}
	if s.trigger(g) != "" {
		t.Fatal("unchanged graph retriggers reason")
	}
	g.Hints = append(g.Hints, board.Hint{ID: "h"})
	if s.trigger(g) == "" {
		t.Fatal("hint failed to trigger reason")
	}
	if s.choose("p", "reason").Name != "high" {
		t.Fatal("priority not honored")
	}
	s.unhealthy["high"] = time.Now().Add(time.Minute)
	if s.choose("p", "reason").Name != "low" {
		t.Fatal("unhealthy worker selected")
	}
}
