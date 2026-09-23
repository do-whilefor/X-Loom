package dispatcher

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

// This runner records the immutable job and stops immediately. It never starts
// a process, calls a model, or touches a container.
type protocolRunner struct{ jobs chan worker.Job }

func (r *protocolRunner) Run(_ context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	r.jobs <- job
	return worker.Result{Status: "failed", Error: "fixture finished"}, nil
}
func (*protocolRunner) Cleanup(context.Context, string, string) error { return nil }
func (*protocolRunner) Projects(context.Context) ([]string, error)    { return nil, nil }

func TestNewJobsRegisterTheCurrentResultProtocol(t *testing.T) {
	for _, backend := range []string{"go", "mock"} {
		for _, kind := range []string{"reason", "explore", "bootstrap"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				store, err := board.Open(filepath.Join(t.TempDir(), "protocol.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				httpServer := httptest.NewServer(server.New(store))
				defer httpServer.Close()
				runner := &protocolRunner{jobs: make(chan worker.Job, 1)}
				cfg := config.Config{
					Server:  httpServer.URL,
					Runtime: config.Runtime{Interval: 60, MaxWorkers: 1, MaxProjects: 1, HealthMode: "disabled"},
					Tasks:   config.Tasks{Reason: config.Task{MaxIntents: 3}},
					Workers: []config.Worker{{Name: "fixture", Type: backend, TaskTypes: []string{kind}, MaxRunning: 1}},
				}
				scheduler := New(cfg, runner)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var graph board.Graph
				if err = scheduler.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Protocol fixture", "origin": "Synthetic input", "goal": "Synthetic goal", "bootstrap_enabled": false}, &graph, nil); err != nil {
					t.Fatal(err)
				}
				var intent *board.Intent
				if kind != "reason" {
					intent = &board.Intent{}
					if err = scheduler.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/intents", map[string]any{"from": []string{"origin"}, "description": "Observe fixture", "creator": "fixture"}, intent, nil); err != nil {
						t.Fatal(err)
					}
					graph.Intents = append(graph.Intents, *intent)
				}
				started, err := scheduler.launch(ctx, graph, kind, intent, "initial")
				if err != nil || !started {
					t.Fatalf("launch: started=%v, err=%v", started, err)
				}
				defer scheduler.wg.Wait()
				var job worker.Job
				select {
				case job = <-runner.jobs:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if job.ResultContractVersion != 2 || job.GraphRPC != (backend != "mock") {
					t.Fatalf("wrong protocol for %s %s: version=%d graph_rpc=%v", backend, kind, job.ResultContractVersion, job.GraphRPC)
				}
				var executions []board.Execution
				if err = scheduler.Client.Do(ctx, "GET", "/executions?namespace=xloom", nil, &executions, nil); err != nil {
					t.Fatal(err)
				}
				if len(executions) != 1 {
					t.Fatalf("registered executions: %d", len(executions))
				}
				var persisted worker.Job
				if err = json.Unmarshal(executions[0].Job, &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted.ResultContractVersion != job.ResultContractVersion || persisted.GraphRPC != job.GraphRPC {
					t.Fatal("worker protocol differed from immutable registered job")
				}
			})
		}
	}
}
