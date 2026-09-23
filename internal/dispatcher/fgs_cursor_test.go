package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

func TestDecisionPreparationQueriesItsCapturedInputRevision(t *testing.T) {
	state := board.State{Graph: board.Graph{Project: board.Project{ID: "p", Generation: 1}, Facts: []board.Fact{{ID: "origin", Description: "Input"}, {ID: "goal", Description: "Finish"}}}, Revision: 9, DecisionRevision: 7}
	var expectedKey string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/p/state" {
			_ = json.NewEncoder(w).Encode(state)
			return
		}
		if r.URL.Path != "/projects/p/executions/check" || r.URL.Query().Get("retry_key") != expectedKey || r.URL.Query().Get("state_version") != board.DecisionStateVersion(state) {
			t.Errorf("decision query did not bind its fresh input: %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(board.ExecutionCheck{})
	}))
	defer api.Close()
	s := New(config.Config{Server: api.URL}, &batchProtocolRunner{})
	s.stateRevisions["p"] = state.DecisionRevision
	expectedKey = s.retryKey(state.Graph, "reason", nil)
	s.stateRevisions["p"] = 1 // Last dispatch preceded the freshly captured evidence.
	run := &task{Job: worker.Job{Kind: "reason", Graph: state.Graph}}
	if err := s.prepareDecision(context.Background(), run, "state_changed"); err != nil {
		t.Fatal(err)
	}
	if run.Job.DecisionRevision != state.DecisionRevision || run.Job.State.Revision != state.Revision {
		t.Fatal("prepared input lost the queried revision")
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
