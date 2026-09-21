package dispatcher

import (
	"context"
	"sync"
	"testing"
	"time"

	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestTerminateCancelsWorkerAndStopsWithoutRedispatch(t *testing.T) {
	started := make(chan struct{}, 2)
	cleaned := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	runner := &controlledRunner{
		run: func(ctx context.Context, _ config.Worker, _ worker.Job) (worker.Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			<-release
			return worker.Result{Status: "success", Text: `{"description":"late old result"}`}, nil
		},
		cleanup: func(_ context.Context, _ string, state string) error { cleaned <- state; return nil },
	}
	s, ctx := scenario(t, runner)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	g := createProject(t, s, ctx, false)
	runner.projects = []string{g.Project.ID}
	createIntent(t, s, ctx, g.Project.ID, "unfinished direction")
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker not started")
	}
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/terminate", map[string]int{"expected_generation": 0})
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	finished := waitFinished(t, s)
	if finished.Outcome != "cancelled" {
		t.Fatalf("terminated worker outcome: %s %v", finished.Outcome, finished.Err)
	}
	select {
	case state := <-cleaned:
		if state != "terminated" {
			t.Fatalf("cleanup state: %s", state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminated container not stopped")
	}
	for range 2 {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.running) != 0 {
		t.Fatal("terminated project was dispatched again")
	}
	current, err := s.Client.Get(ctx, g.Project.ID)
	if err != nil || current.Project.Status != "terminated" || len(current.Facts) != 2 {
		t.Fatalf("late result persisted: %+v %v", current, err)
	}
}
