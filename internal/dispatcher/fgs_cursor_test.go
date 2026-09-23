package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"xloom/internal/board"
	"xloom/internal/server"
)

func TestDecisionPreparationQueriesItsCapturedInputRevision(t *testing.T) {
	s, runner, _, graph := batchSchedulerFixture(t, 1)
	ctx := context.Background()
	inserted := false
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.Path == projectPath(graph.Project.ID)+"/executions/prepare" && !inserted {
			inserted = true
			// A new human requirement arrives after the dispatch read, but before
			// the server transaction captures and registers the immutable input.
			if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "A newly confirmed requirement", "creator": "user"}, nil, nil); err != nil {
				return nil, err
			}
		}
		if r.URL.Path == projectPath(graph.Project.ID)+"/state" {
			t.Error("preparing a new run downloaded the complete State")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	launched, err := s.dispatch(ctx, graph.Project.ID)
	if err != nil || !launched {
		t.Fatalf("dispatch: %v %v", launched, err)
	}
	s.wg.Wait()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if !inserted || len(runner.jobs) != 1 {
		t.Fatal("fresh input was not captured")
	}
	job := runner.jobs[0]
	if job.State != nil || job.InputSnapshot == nil || job.InputSnapshot.Revision != 1 || job.DecisionRevision != 1 || job.Decision.ToRevision != 1 || job.InputSnapshot.HintCount != 1 {
		t.Fatal("registered input lost the server-captured revision")
	}
	raw, _ := json.Marshal(job)
	if len(raw) > 64<<10 {
		t.Fatal("prepared Job was not bounded")
	}
}

func TestNewDecisionDoesNotReadCompletedJobSnapshot(t *testing.T) {
	template, _, _, _ := batchSchedulerFixture(t, 1)
	store, err := board.Open(filepath.Join(t.TempDir(), "fgs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	host := httptest.NewServer(server.New(store))
	defer host.Close()
	cfg := template.Config
	cfg.Server = host.URL
	runner := &batchProtocolRunner{directions: 1}
	scheduler := New(cfg, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); scheduler.wg.Wait() }()
	var graph board.Graph
	if err := scheduler.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "FGS fixture", "origin": "Synthetic fixture", "goal": "Check every requested fixture"}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Step(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.wg.Wait()
	// Simulate archived/unavailable historical inputs. The graph, immutable
	// registration metadata and outcome still exist; new planning must not
	// deserialize this completed run or fall back to a private transcript.
	if err := store.Do(ctx, func(tx *board.Tx) error {
		result, err := tx.Exec("UPDATE xloom_executions SET job='{}' WHERE project_id=? AND kind='reason' AND status='succeeded'", graph.Project.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n != 1 {
			t.Fatalf("expected one completed planner, got %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state := finishBatchFixture(t, scheduler, graph.Project.ID)
	if state.Graph.Project.Status != "completed" {
		t.Fatal("new planning depended on archived job")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	last := runner.jobs[len(runner.jobs)-1]
	if last.Kind != "reason" || last.Decision == nil || last.Decision.Mode != "changes" || last.Decision.FromRevision != 0 || last.Decision.ToRevision == 0 {
		t.Fatalf("new run did not use FGS revision cursor: %+v", last.Decision)
	}
}
