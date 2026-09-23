package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

// Use the production graph bridge and HTTP boundary with a scripted runner.
// Type go selects protocol 2; this runner never starts a process or model.
type batchProtocolRunner struct {
	handler     func(context.Context, worker.Job, worker.GraphRequest) (any, error)
	directions  int
	lateError   bool
	afterCommit func()
	mu          sync.Mutex
	jobs        []worker.Job
	requests    int
}

func (r *batchProtocolRunner) SetGraphHandler(fn func(context.Context, worker.Job, worker.GraphRequest) (any, error)) {
	r.handler = fn
}
func (*batchProtocolRunner) Cleanup(context.Context, string, string) error { return nil }
func (*batchProtocolRunner) Projects(context.Context) ([]string, error)    { return nil, nil }
func (r *batchProtocolRunner) graph(ctx context.Context, job worker.Job, request worker.GraphRequest) (any, error) {
	r.mu.Lock()
	r.requests++
	request.RequestID = fmt.Sprintf("%032x", r.requests)
	r.mu.Unlock()
	if err := worker.ValidateGraphRequest(job, request); err != nil {
		return nil, err
	}
	return r.handler(ctx, job, request)
}
func (r *batchProtocolRunner) Run(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	r.mu.Lock()
	r.jobs = append(r.jobs, job)
	r.mu.Unlock()
	if !job.GraphRPC || job.ResultContractVersion != 2 {
		return worker.Result{}, errors.New("new run did not bind the version 2 graph protocol")
	}
	if job.Kind == "reason" {
		if job.Decision == nil || job.Decision.Version != 2 {
			return worker.Result{}, errors.New("planner was not registered with decision version 2")
		}
		actions := []board.DecisionAction{}
		steps, open, facts := fixturePlanInput(job)
		if steps == 0 {
			for n := 0; n < r.directions; n++ {
				payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": fmt.Sprintf("Check independent fixture %d", n)})
				actions = append(actions, board.DecisionAction{Op: "step", Ref: fmt.Sprintf("check%d", n), Payload: payload})
			}
		} else if open == 0 {
			sources := []string{}
			for _, fact := range facts {
				if fact.SourceStepID != "" {
					sources = append(sources, fact.ID)
				}
			}
			payload, _ := json.Marshal(map[string]any{"from": sources, "description": "Each requested fixture now has a supported observation"})
			actions = append(actions, board.DecisionAction{Op: "complete", Payload: payload})
		}
		batch := &board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: actions}
		preview, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_preview", Batch: batch})
		if err != nil {
			return worker.Result{}, err
		}
		previewReceipt := preview.(board.DecisionReceipt)
		if previewReceipt.Committed || previewReceipt.Completed || previewReceipt.ValidationScope != "protocol_only" {
			return worker.Result{}, errors.New("preview published the plan")
		}
		if len(actions) != 0 && actions[len(actions)-1].Op == "complete" {
			review := previewReceipt.CompletionReview
			if review == nil || review.Acceptance != "not_checked" || review.StateVersion != batch.ExpectedVersion || len(review.UserInputs) != 2 || len(review.FactRecords) != len(review.From) || len(review.FactRecords) == 0 {
				return worker.Result{}, fmt.Errorf("completion preview lost authoritative review at the graph bridge: %+v", review)
			}
		} else if previewReceipt.CompletionReview != nil {
			return worker.Result{}, errors.New("ordinary planning preview unexpectedly added completion review")
		} else if len(previewReceipt.Results) != len(actions) {
			return worker.Result{}, errors.New("ordinary planning preview lost its projected results")
		}
		committed, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: batch})
		if err != nil {
			return worker.Result{}, err
		}
		if !committed.(board.DecisionReceipt).Committed {
			return worker.Result{}, errors.New("commit did not return a durable receipt")
		}
		compact := committed.(board.DecisionReceipt)
		if compact.Results != nil || compact.ChangedActions != len(actions) || compact.CompletionReview != nil || compact.ValidationScope != "" {
			return worker.Result{}, fmt.Errorf("compact commit lost changed-action count: got %+v; want %d", compact, len(actions))
		}
		saved, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_receipt"})
		if err != nil {
			return worker.Result{}, err
		}
		receipt := saved.(board.DecisionReceipt)
		if !receipt.Committed || receipt.Results != nil || receipt.ChangedActions != compact.ChangedActions || receipt.CompletionReview != nil || receipt.ValidationScope != "" {
			return worker.Result{}, fmt.Errorf("compact recovery receipt differs from commit: %+v", receipt)
		}
		if r.afterCommit != nil {
			r.afterCommit()
			<-ctx.Done()
			return worker.Result{}, ctx.Err()
		}
		if r.lateError {
			return worker.Result{}, errors.New("synthetic process error after durable commit")
		}
		return worker.Result{Status: "success", Type: "result", Text: `{"accepted":true,"data":{"decided":true}}`}, nil
	}
	if job.Kind != "explore" || job.Intent == nil {
		return worker.Result{}, errors.New("unexpected bootstrap or missing Step")
	}
	payload, _ := json.Marshal(map[string]any{
		"description": "The requested fixture rejected unauthenticated access",
		"scope":       job.Intent.Description, "observed_at": time.Now().UTC().Format(time.RFC3339),
		"evidence": []board.EvidenceRef{{RunID: job.RunID, Path: "retained/fixture-response.txt", Excerpt: "HTTP 401"}},
	})
	response, err := r.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "fact", IdempotencyKey: job.RunID + ":fixture-observation", Payload: payload}})
	if err != nil {
		return worker.Result{}, err
	}
	receipt := response.(board.StateActionResult)
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact_id": receipt.ID}})
	return worker.Result{Status: "success", Type: "result", Text: string(raw)}, nil
}

// Drop only successful commit response bodies, after the real server already
// committed. Track forbidden status/apply retries after that durable boundary.
type batchFaultTransport struct {
	base         http.RoundTripper
	loseResponse bool
	mu           sync.Mutex
	committed    map[string]bool
	lateWrites   int
	lost         int
}

func (f *batchFaultTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	lease := request.Header.Get("X-Xloom-Run")
	f.mu.Lock()
	if f.committed[lease] && request.Method == "POST" && (strings.HasSuffix(request.URL.Path, "/status") || strings.HasSuffix(request.URL.Path, "/apply")) {
		f.lateWrites++
	}
	f.mu.Unlock()
	response, err := f.base.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusOK && strings.HasSuffix(request.URL.Path, "/state/decisions/commit") {
		f.mu.Lock()
		f.committed[lease] = true
		if f.loseResponse {
			f.lost++
		}
		f.mu.Unlock()
		if f.loseResponse {
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(""))
		}
	}
	return response, err
}

func batchSchedulerFixture(t *testing.T, directions int) (*Scheduler, *batchProtocolRunner, *batchFaultTransport, board.Graph) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "decision-flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(server.New(store))
	t.Cleanup(httpServer.Close)
	runner := &batchProtocolRunner{directions: directions}
	cfg := config.Config{
		Server:    httpServer.URL,
		Runtime:   config.Runtime{Interval: 1, MaxWorkers: 1, MaxProjects: 1, MaxProjectWorkers: 1, HealthTimeout: 5, HealthMode: "disabled"},
		Tasks:     config.Tasks{Reason: config.Task{MaxIntents: 3}, Explore: config.Task{ConcludeTimeout: 60}},
		Container: config.Container{Image: "synthetic", Network: "bridge", CompletedAction: "stop"},
		Workers:   []config.Worker{{Name: "scripted", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 1, Env: map[string]string{"ANTHROPIC_BASE_URL": "http://unused.invalid", "ANTHROPIC_AUTH_TOKEN": "synthetic-test-token", "ANTHROPIC_MODEL": "synthetic"}}},
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	scheduler := New(cfg, runner)
	transport := &batchFaultTransport{base: http.DefaultTransport, committed: map[string]bool{}}
	scheduler.Client.HTTP = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var graph board.Graph
	if err = scheduler.Client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Batch fixture", "origin": "Synthetic target fixtures", "goal": "Check every requested fixture", "bootstrap_enabled": true}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	return scheduler, runner, transport, graph
}

func finishBatchFixture(t *testing.T, scheduler *Scheduler, project string) board.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer func() { cancel(); scheduler.wg.Wait() }()
	for turn := 0; turn < 24; turn++ {
		if err := scheduler.Step(ctx); err != nil {
			t.Fatal(err)
		}
		scheduler.wg.Wait()
		var state board.State
		if err := scheduler.Client.Do(ctx, "GET", projectPath(project)+"/state", nil, &state, nil); err != nil {
			t.Fatal(err)
		}
		if state.Graph.Project.Status == "completed" {
			return state
		}
	}
	t.Fatal("scripted version 2 decision flow did not complete")
	return board.State{}
}

func assertBatchExecutionReceipts(t *testing.T, scheduler *Scheduler, transport *batchFaultTransport, project string) {
	t.Helper()
	var executions []board.Execution
	if err := scheduler.Client.Do(context.Background(), "GET", "/executions?namespace=xloom", nil, &executions, nil); err != nil {
		t.Fatal(err)
	}
	for _, execution := range executions {
		if execution.ProjectID == project && execution.Status != "succeeded" {
			t.Fatalf("commit receipt was overwritten or execution retried: %s %s", execution.Kind, execution.Status)
		}
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.lateWrites != 0 {
		t.Fatalf("dispatcher posted %d status/apply mutations after commit", transport.lateWrites)
	}
}

func TestDecisionBatchDispatcherEndToEndWithDeliveryFaults(t *testing.T) {
	for _, directions := range []int{1, 3} {
		for _, mode := range []string{"normal", "runner_error_after_commit", "commit_response_lost"} {
			t.Run(fmt.Sprintf("%d_directions/%s", directions, mode), func(t *testing.T) {
				scheduler, runner, transport, graph := batchSchedulerFixture(t, directions)
				runner.lateError = mode == "runner_error_after_commit"
				transport.loseResponse = mode == "commit_response_lost"
				state := finishBatchFixture(t, scheduler, graph.Project.ID)
				if graph.Project.Bootstrap || len(state.Steps) != directions+1 || len(state.FactRecords) != directions+2 {
					t.Fatalf("cold-start/plan/fact counts changed: %+v", state)
				}
				for _, fact := range state.FactRecords {
					if fact.ID != "origin" && fact.ID != "goal" && (fact.Legacy || fact.SourceStepID == "" || len(fact.Evidence) != 1) {
						t.Fatalf("new observation lost structured evidence: %+v", fact)
					}
				}
				runner.mu.Lock()
				if len(runner.jobs) == 0 || runner.jobs[0].Kind != "reason" {
					t.Fatal("new project did not begin with Decide")
				}
				explores := 0
				for _, job := range runner.jobs {
					if job.Kind == "explore" {
						explores++
					}
				}
				runner.mu.Unlock()
				if explores != directions {
					t.Fatalf("Execute ran %d times, want %d", explores, directions)
				}
				assertBatchExecutionReceipts(t, scheduler, transport, graph.Project.ID)
				if mode == "commit_response_lost" && transport.lost == 0 {
					t.Fatal("commit response loss was not exercised")
				}
			})
		}
	}
}

func TestDecisionBatchReceiptSurvivesCancellationAndDispatcherRestart(t *testing.T) {
	scheduler, runner, transport, graph := batchSchedulerFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.afterCommit = cancel
	if err := scheduler.Step(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	scheduler.wg.Wait()
	assertBatchExecutionReceipts(t, scheduler, transport, graph.Project.ID)
	var before board.State
	if err := scheduler.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &before, nil); err != nil {
		t.Fatal(err)
	}
	if len(before.Steps) != 1 || before.Graph.Project.Status != "active" {
		t.Fatal("committed plan was lost when the runner was cancelled")
	}
	runner.afterCommit = nil
	restarted := New(scheduler.Config, runner)
	restarted.Client.HTTP = scheduler.Client.HTTP
	state := finishBatchFixture(t, restarted, graph.Project.ID)
	if len(state.Steps) != 2 || len(state.FactRecords) != 3 {
		t.Fatal("restart replayed the already committed decision")
	}
	assertBatchExecutionReceipts(t, restarted, transport, graph.Project.ID)
}
