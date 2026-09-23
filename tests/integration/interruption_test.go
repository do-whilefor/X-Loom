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
	"strings"
	"sync"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/docker"
	"xloom/internal/worker"
)

func TestDockerInterruptedRunResumesWithoutRepeatingSideEffects(t *testing.T) {
	image := os.Getenv("XLOOM_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set XLOOM_DOCKER_TEST_IMAGE for real interruption acceptance")
	}
	const runID = "interrupted-run"
	const projectID = "interruption"
	const runDir = "/workspace/.xloom/runs/" + runID
	var mu sync.Mutex
	requests := 0
	var modelErrors []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var input struct {
			Messages []agent.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			modelErrors = append(modelErrors, err.Error())
			http.Error(w, "bad controlled request", 400)
			return
		}
		requests++
		respond := func(block agent.Block, stop string) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
		}
		tool := func(command string) {
			raw, _ := json.Marshal(map[string]any{"command": command, "timeout": 60})
			respond(agent.Block{Type: "tool_use", ID: fmt.Sprintf("action-%d", requests), Name: "bash", Input: raw}, "tool_use")
		}
		switch requests {
		case 1:
			tool("printf 'completed-once\\n' >> " + runDir + "/side-effects.txt")
		case 2:
			if len(input.Messages) < 3 || input.Messages[len(input.Messages)-1].Content[0].IsError {
				modelErrors = append(modelErrors, "first side effect did not complete")
			}
			tool("sleep 60 & echo $! > " + runDir + "/child.pid; echo $$ > " + runDir + "/shell.pid; wait")
		case 3:
			if len(input.Messages) != 5 {
				modelErrors = append(modelErrors, fmt.Sprintf("restored unexpected history length: %d", len(input.Messages)))
			} else {
				last := input.Messages[4]
				if len(last.Content) != 1 || last.Content[0].ToolUseID != "action-2" || !last.Content[0].IsError {
					modelErrors = append(modelErrors, "uncertain long-running action was replayed or not settled")
				}
			}
			respond(agent.Block{Type: "text", Text: `{"accepted":true,"data":{"description":"A completed synthetic side effect was preserved across infrastructure interruption."}}`}, "end_turn")
		default:
			modelErrors = append(modelErrors, fmt.Sprintf("unexpected model request %d", requests))
			http.Error(w, "extra request", 400)
		}
	}))
	defer model.Close()
	c := config.Container{Socket: "/var/run/docker.sock", Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("xloom-interrupt-%d", time.Now().UnixNano()), CompletedAction: "stop"}
	runner := docker.New(c)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, projectID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	intent := board.Intent{ID: "i001", From: []string{"origin"}, Description: "Preserve a completed synthetic side effect across an execution interruption."}
	job := worker.Job{RunID: runID, Kind: "explore", WorkerType: "go", Workspace: "/workspace", Budget: config.Task{Timeout: 120, ConcludeTimeout: 20}, Intent: &intent, Graph: board.Graph{Project: board.Project{ID: projectID, Title: "Synthetic interruption", Status: "active"}, Facts: []board.Fact{{ID: "origin", Description: "Local synthetic test only"}, {ID: "goal", Description: "Resume without repeating completed writes"}}, Intents: []board.Intent{intent}}}
	backend := config.Worker{Name: "controlled", Type: "go", Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "XLOOM_REQUEST_TIMEOUT": "15"}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runCtx, interrupt := context.WithCancelCause(ctx)
	defer interrupt(worker.ErrInterrupted)
	done := make(chan error, 1)
	go func() { _, err := runner.Run(runCtx, backend, job); done <- err }()
	container := c.Namespace + "-dispatch-" + projectID
	type checkpoint struct {
		RecoveryCount     int             `json:"recovery_count"`
		RepairCount       int             `json:"repair_count"`
		StartedAt         time.Time       `json:"started_at"`
		ExecutionDeadline time.Time       `json:"execution_deadline"`
		History           []agent.Message `json:"history"`
	}
	readSession := func() (checkpoint, error) {
		raw, err := dockerExec(ctx, container, []string{"bash", "-c", `test ! -e "$1/cancelled" && cat "$1/session.json"`, "check", runDir})
		var saved checkpoint
		if err == nil {
			err = json.Unmarshal([]byte(raw), &saved)
		}
		return saved, err
	}
	end := time.Now().Add(25 * time.Second)
	for {
		processes, err := inspectExecutionProcesses(ctx, container, runDir)
		if err == nil && len(processes) == 2 && !processes[0].dead() && !processes[1].dead() {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execution ended before interrupted tool: %v", err)
		default:
		}
		if time.Now().After(end) {
			t.Fatalf("long-running tool never started: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	before, err := readSession()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.History) != 4 {
		t.Fatalf("tool intent was not durably checkpointed: %+v", before)
	}
	launchToken, err := dockerExec(ctx, container, []string{"cat", runDir + "/launch-token"})
	if err != nil || len(launchToken) != 32 {
		t.Fatalf("missing per-launch identity: %q %v", launchToken, err)
	}
	interrupt(worker.ErrInterrupted)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected interruption outcome: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("container worker did not stop")
	}
	processes, err := inspectExecutionProcesses(ctx, container, runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if !process.dead() {
			t.Fatalf("owned process survived interruption: %+v", process)
		}
	}
	if _, err := readSession(); err != nil {
		t.Fatal("interruption persisted cancellation or lost checkpoint", err)
	}
	// Recreate a Docker exec that starts only after its original launch was
	// interrupted. It must fail before touching history or making a model call.
	if _, err := dockerExec(ctx, container, []string{"env", "XLOOM_LAUNCH_TOKEN=" + launchToken, "/usr/local/bin/xloom", "worker", "--job", runDir + "/job.json"}); err == nil || !strings.Contains(err.Error(), "interrupted or superseded") {
		t.Fatalf("late interrupted launch was not rejected: %v", err)
	}
	result, err := runner.Run(ctx, backend, job)
	if err != nil || result.Status != "success" || result.Conclude {
		t.Fatal(result, err)
	}
	after, err := readSession()
	if err != nil {
		t.Fatal(err)
	}
	if after.RecoveryCount != 1 || after.RepairCount != 0 || !after.StartedAt.Equal(before.StartedAt) || !after.ExecutionDeadline.Equal(before.ExecutionDeadline) {
		t.Fatalf("resume reset allowance or original deadline: before=%+v after=%+v", before, after)
	}
	count, err := dockerExec(ctx, container, []string{"cat", runDir + "/side-effects.txt"})
	if err != nil || strings.TrimSpace(count) != "completed-once" {
		t.Fatalf("completed side effect replayed: %q %v", count, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 || len(modelErrors) > 0 {
		t.Fatalf("model history mismatch: requests=%d errors=%v", requests, modelErrors)
	}
}
