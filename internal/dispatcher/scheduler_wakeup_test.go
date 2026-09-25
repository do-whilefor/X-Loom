package dispatcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

type wakeupRunner struct {
	batchProtocolRunner
	started        chan worker.Job
	completed      chan worker.Job
	executeRelease <-chan struct{}
	waitAll        bool
	cleanupStarted chan string
	cleanupRelease <-chan struct{}
	cleanupError   error
}

func (r *wakeupRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.started <- job
	if r.waitAll || job.Kind == "explore" && r.executeRelease != nil {
		select {
		case <-r.executeRelease:
		case <-ctx.Done():
			return worker.Result{}, ctx.Err()
		}
	}
	result, err := r.batchProtocolRunner.Run(ctx, backend, job)
	if err == nil {
		r.completed <- job
	}
	return result, err
}

func (r *wakeupRunner) Cleanup(ctx context.Context, _ string, state string) error {
	if r.cleanupStarted != nil {
		r.cleanupStarted <- state
	}
	if r.cleanupRelease != nil {
		select {
		case <-r.cleanupRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.cleanupError
}

func wakeupFixture(t *testing.T, directions int) (*Scheduler, *wakeupRunner, board.Graph, *board.Store) {
	t.Helper()
	fixture, _, _, graph, store := batchSchedulerFixture(t, directions)
	// A lost wakeup takes a full minute, far beyond each bounded assertion.
	fixture.Config.Runtime.Interval = 60
	fixture.Config.Runtime.MaxWorkers = 3
	fixture.Config.Runtime.MaxProjectWorkers = 3
	fixture.Config.Workers[0].MaxRunning = 3
	if err := fixture.Client.Do(context.Background(), "PUT", "/settings", board.Settings{IntentTimeout: 180, ReasonTimeout: 180}, nil, nil); err != nil {
		t.Fatal(err)
	}
	runner := &wakeupRunner{
		batchProtocolRunner: batchProtocolRunner{directions: directions},
		started:             make(chan worker.Job, 16),
		completed:           make(chan worker.Job, 16),
	}
	return New(fixture.Config, runner), runner, graph, store
}

func runWakeupScheduler(t *testing.T, scheduler *Scheduler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("scheduler shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("scheduler did not join its workers during shutdown")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func nextWakeupJob(t *testing.T, jobs <-chan worker.Job) worker.Job {
	t.Helper()
	select {
	case job := <-jobs:
		return job
	case <-time.After(5 * time.Second):
		t.Fatal("task transition waited for the minute-long scheduler tick")
		return worker.Job{}
	}
}

func TestRunWakesOnTaskCompletionWithoutLosingConcurrentOutcomes(t *testing.T) {
	scheduler, runner, graph, store := wakeupFixture(t, 3)
	release := make(chan struct{})
	runner.executeRelease = release
	stop := runWakeupScheduler(t, scheduler)
	if job := nextWakeupJob(t, runner.started); job.Kind != "reason" {
		t.Fatalf("first task = %s", job.Kind)
	}
	if job := nextWakeupJob(t, runner.completed); job.Kind != "reason" {
		t.Fatalf("first completion = %s", job.Kind)
	}
	intents := map[string]bool{}
	for range 3 {
		job := nextWakeupJob(t, runner.started)
		if job.Kind != "explore" || job.Intent == nil || intents[job.Intent.ID] {
			t.Fatalf("missing or duplicate Execute claim: %+v", job)
		}
		intents[job.Intent.ID] = true
	}
	close(release) // Several outcomes can share the one pending notification.
	for range 3 {
		if job := nextWakeupJob(t, runner.completed); job.Kind != "explore" {
			t.Fatalf("planner overtook pending Execute results: %s", job.Kind)
		}
	}
	if job := nextWakeupJob(t, runner.started); job.Kind != "reason" {
		t.Fatalf("final task = %s", job.Kind)
	}
	if job := nextWakeupJob(t, runner.completed); job.Kind != "reason" {
		t.Fatalf("final completion = %s", job.Kind)
	}
	stop()
	// Run has joined every producer; inspect queues and maps without races.
	scheduler.reap()
	if len(scheduler.running) != 0 || len(runner.started) != 0 || len(runner.completed) != 0 {
		t.Fatal("completion was lost or a task was launched twice")
	}
	executions := testExecutions(t, store)
	if len(executions) != 5 {
		t.Fatalf("executions = %d, want one initial plan, three Executes and one final plan", len(executions))
	}
	for _, execution := range executions {
		if execution.Status != "succeeded" {
			t.Fatalf("execution did not finish: %s %s", execution.Kind, execution.Status)
		}
	}
	if err := scheduler.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	if graph.Project.Status != "completed" {
		t.Fatal("completion chain did not reach the project goal")
	}
}

func TestRunWakesAfterRestartCleanupAndCancelsActiveWorker(t *testing.T) {
	scheduler, runner, graph, _ := wakeupFixture(t, 1)
	release := make(chan struct{})
	runner.waitAll = true
	runner.cleanupStarted = make(chan string, 2)
	runner.cleanupRelease = release
	if err := scheduler.Client.Do(context.Background(), "POST", projectPath(graph.Project.ID)+"/restart", map[string]any{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	stop := runWakeupScheduler(t, scheduler)
	select {
	case state := <-runner.cleanupStarted:
		if state != "restart:1" {
			t.Fatalf("cleanup state = %q", state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart cleanup did not start")
	}
	select {
	case <-runner.started:
		t.Fatal("new generation started before cleanup finished")
	default:
	}
	close(release)
	if job := nextWakeupJob(t, runner.started); job.Kind != "reason" || job.Graph.Project.Generation != 1 {
		t.Fatalf("cleanup resumed an unexpected task: %+v", job)
	}
	stop() // The active runner waits solely for context cancellation.
	scheduler.reap()
	if len(scheduler.running) != 0 || len(runner.cleanupStarted) != 0 {
		t.Fatal("shutdown lost a completion or repeated successful cleanup")
	}
}

func TestCleanupFailureDoesNotCreateImmediateRetryLoop(t *testing.T) {
	scheduler, runner, graph, _ := wakeupFixture(t, 1)
	runner.cleanupStarted = make(chan string, 16)
	runner.cleanupError = errors.New("synthetic Docker outage")
	if err := scheduler.Client.Do(context.Background(), "POST", projectPath(graph.Project.ID)+"/restart", map[string]any{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	stop := runWakeupScheduler(t, scheduler)
	select {
	case <-runner.cleanupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("restart cleanup did not start")
	}
	select {
	case <-runner.cleanupStarted:
		t.Fatal("failed cleanup bypassed the ticker retry delay")
	case <-runner.started:
		t.Fatal("failed cleanup admitted a new worker")
	case <-time.After(100 * time.Millisecond):
	}
	stop()
}
