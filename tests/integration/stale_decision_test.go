//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/dispatcher"
	"xloom/internal/docker"
	"xloom/internal/server"
	"xloom/internal/worker"
)

// Exercise the real cancellation path while the provider is still streaming,
// before it can return a tool call that discovers a commit conflict. Another
// Execute in the same container must retain its own in-flight model request.
func TestDockerStaleDecisionCancelsModelWithoutStoppingExecute(t *testing.T) {
	image := os.Getenv("XLOOM_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set XLOOM_DOCKER_TEST_IMAGE for stale Decide cancellation acceptance")
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	client := &dispatcher.Client{Base: api.URL}
	var project board.Graph
	if err := client.Do(context.Background(), "POST", "/projects", map[string]any{
		"title": "Stale Decide cancellation", "origin": "Use local synthetic files only.",
		"goal": "Preserve a sibling Execute while replacing a stale decision.", "bootstrap_enabled": false,
	}, &project, nil); err != nil {
		t.Fatal(err)
	}
	base := "/projects/" + project.Project.ID
	var intent board.Intent
	if err := client.Do(context.Background(), "POST", base+"/intents", map[string]any{
		"from": []string{"origin"}, "description": "Write and retain the synthetic sibling proof.", "creator": "fixture",
	}, &intent, nil); err != nil {
		t.Fatal(err)
	}
	decideStarted, executeStarted := make(chan string, 1), make(chan string, 1)
	staleDisconnected, siblingDisconnected := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var mu sync.Mutex
	turns := map[string]int{}
	staleRun := ""
	var modelErrors []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []agent.Definition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid controlled request", http.StatusBadRequest)
			return
		}
		run := r.Header.Get("x-opencode-session")
		decide := true
		for _, tool := range request.Tools {
			if tool.Name == "bash" {
				decide = false
			}
		}
		mu.Lock()
		turns[run]++
		turn := turns[run]
		if decide && staleRun == "" {
			staleRun = run
		}
		stale := decide && run == staleRun
		mu.Unlock()
		respond := func(block agent.Block, stop string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
		}
		call := func(name string, input any) {
			raw, _ := json.Marshal(input)
			respond(agent.Block{Type: "tool_use", ID: fmt.Sprintf("call-%d", turn), Name: name, Input: raw}, "tool_use")
		}
		if stale {
			if turn == 1 {
				// Send response headers and one valid SSE event, then hold the body
				// open. Cancellation must close a request already being consumed.
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n")
				w.(http.Flusher).Flush()
				decideStarted <- run
				<-r.Context().Done()
				staleDisconnected <- struct{}{}
				return
			}
			mu.Lock()
			modelErrors = append(modelErrors, "stale Decide made another model request")
			mu.Unlock()
			http.Error(w, "stale request replayed", http.StatusBadRequest)
			return
		}
		if decide {
			call("graph_action", map[string]any{"op": "commit", "idempotency_key": "empty-plan", "payload": map[string]any{}})
			return
		}
		path := "/workspace/.xloom/runs/" + run + "/output-sibling.txt"
		switch turn {
		case 1:
			executeStarted <- run
			select {
			case <-release:
			case <-r.Context().Done():
				siblingDisconnected <- struct{}{}
				return
			}
			call("write", map[string]string{"path": path, "content": "sibling-proof-preserved\n"})
		case 2:
			raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{
				"fact": map[string]any{"description": "The synthetic sibling file was written after stale Decide cancellation.",
					"scope": "local synthetic fixture", "observed_at": time.Now().UTC().Format(time.RFC3339),
					"evidence": []map[string]string{{"path": path}}},
			}})
			respond(agent.Block{Type: "text", Text: string(raw)}, "end_turn")
		default:
			mu.Lock()
			modelErrors = append(modelErrors, fmt.Sprintf("unexpected Execute model turn %d", turn))
			mu.Unlock()
			http.Error(w, "unexpected Execute continuation", http.StatusBadRequest)
		}
	}))
	defer model.Close()
	defer releaseOnce.Do(func() { close(release) })
	c := config.Config{
		Server:    api.URL,
		Runtime:   config.Runtime{Interval: 1, MaxWorkers: 2, MaxProjects: 1, MaxProjectWorkers: 2, HealthMode: "disabled", HealthTimeout: 10},
		Tasks:     config.Tasks{Reason: config.Task{Timeout: 120, MaxIntents: 3}, Explore: config.Task{Timeout: 0, ConcludeTimeout: 10}},
		Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("xloom-stale-%d", time.Now().UnixNano()), CompletedAction: "stop"},
		Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 2,
			Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "XLOOM_REQUEST_TIMEOUT": "120"}}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := docker.New(c.Container)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, project.Project.ID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	done := make(chan error, 1)
	go func() { done <- dispatcher.New(c, runner).Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("scheduler shutdown timed out")
		}
	}()
	awaitRun := func(ch <-chan string) string {
		t.Helper()
		select {
		case run := <-ch:
			return run
		case <-ctx.Done():
			t.Fatal("model request did not start")
			return ""
		}
	}
	executeID := awaitRun(executeStarted)
	if err := client.Do(ctx, "POST", base+"/hints", map[string]string{"content": "Review the synthetic plan while its Step is running.", "creator": "fixture"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	staleID := awaitRun(decideStarted)
	if err := client.Do(ctx, "POST", base+"/hints", map[string]string{"content": "A new synthetic observation invalidates the current decision.", "creator": "fixture"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-staleDisconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("stale Decide did not cancel its in-flight model stream before request timeout")
	}
	readRuns := func() []board.Execution {
		t.Helper()
		runs, err := testExecutions(ctx, store, c.Container.Namespace)
		if err != nil {
			t.Fatal(err)
		}
		return runs
	}
	waitFor := func(check func([]board.Execution) bool, failure string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !check(readRuns()) {
			if time.Now().After(deadline) {
				t.Fatal(failure)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor(func(runs []board.Execution) bool {
		var old, replacement *board.Execution
		for n := range runs {
			if runs[n].ID == staleID {
				old = &runs[n]
			} else if runs[n].Kind == "reason" && runs[n].Status == "succeeded" {
				replacement = &runs[n]
			}
		}
		if old == nil || old.Status != "failed" || replacement == nil {
			return false
		}
		var result worker.Result
		var oldJob, freshJob worker.Job
		if json.Unmarshal(old.Result, &result) != nil || result.FailureKind != "state_changed" || result.Retryable {
			t.Fatalf("stale Decide lost its terminal classification: %s", old.Result)
		}
		if json.Unmarshal(old.Job, &oldJob) != nil || json.Unmarshal(replacement.Job, &freshJob) != nil || oldJob.Decision == nil || freshJob.Decision == nil || oldJob.Decision.StateVersion == freshJob.Decision.StateVersion || freshJob.PreviousRunID != "" {
			t.Fatal("replacement reused the stale decision input or same-run recovery")
		}
		return true
	}, "stale Decide did not yield to a successful fresh decision")
	select {
	case <-siblingDisconnected:
		t.Fatal("cancelling stale Decide disconnected the sibling Execute request")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	waitFor(func(runs []board.Execution) bool {
		for _, run := range runs {
			if run.ID == executeID && run.Status == "succeeded" {
				return true
			}
		}
		return false
	}, "sibling Execute failed to complete after stale Decide cancellation")
	mu.Lock()
	defer mu.Unlock()
	if turns[staleID] != 1 || turns[executeID] != 2 || len(modelErrors) != 0 {
		t.Fatalf("unexpected model continuation: stale=%d execute=%d errors=%v", turns[staleID], turns[executeID], modelErrors)
	}
}
