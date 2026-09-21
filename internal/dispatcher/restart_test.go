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

func TestRestartDrainsOldWorkerBeforeFreshBootstrap(t *testing.T) {
	started := make(chan worker.Job, 4)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runner := &controlledRunner{run: func(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
		started <- job
		<-ctx.Done()
		if job.Graph.Project.Generation == 0 {
			close(cancelled)
			<-release
			return worker.Result{Status: "success", Text: `{"description":"late old result"}`}, nil
		}
		return worker.Result{}, ctx.Err()
	}}
	s, ctx := scenario(t, runner)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	g := createProject(t, s, ctx, true)
	createIntent(t, s, ctx, g.Project.ID, "old running step")
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	old := <-started
	if old.Kind != "explore" {
		t.Fatalf("old kind: %s", old.Kind)
	}
	var restarted board.Graph
	if err := s.Client.Do(ctx, "POST", projectPath(g.Project.ID)+"/restart", map[string]int{"expected_generation": 0}, &restarted, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("old round not cancelled")
	}
	if len(s.running) != 1 {
		t.Fatalf("new round overlapped draining task: %d", len(s.running))
	}
	select {
	case job := <-started:
		t.Fatalf("started before drain: %+v", job)
	default:
	}
	once.Do(func() { close(release) })
	finished := waitFinished(t, s)
	if finished.Outcome != "cancelled" {
		t.Fatalf("late worker outcome: %s %v", finished.Outcome, finished.Err)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	// A successful process cancellation is followed by a confirmed container
	// stop, retaining the workspace but excluding orphaned old process groups.
	select {
	case cleanup := <-s.cleanupDone:
		if cleanup.State != "restart:1" || cleanup.Err != nil {
			t.Fatalf("restart cleanup: %+v", cleanup)
		}
		s.cleanupDone <- cleanup
	case <-time.After(3 * time.Second):
		t.Fatal("restart container was not stopped")
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case fresh := <-started:
		if fresh.Kind != "bootstrap" || fresh.Graph.Project.Generation != 1 || fresh.Intent.ID == old.Intent.ID {
			t.Fatalf("fresh round did not initialize safely: %+v", fresh)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fresh bootstrap not launched")
	}
	current, err := s.Client.Get(ctx, g.Project.ID)
	if err != nil || len(current.Facts) != 2 {
		t.Fatalf("late result persisted: %+v %v", current, err)
	}
}

func TestRestartBlocksOnContainerStopFailure(t *testing.T) {
	stops := 0
	runner := &controlledRunner{cleanup: func(_ context.Context, _ string, state string) error {
		if state != "restart:1" {
			return errors.New("unexpected cleanup state")
		}
		stops++
		if stops == 1 {
			return errors.New("Docker unavailable")
		}
		return nil
	}}
	s, ctx := scenario(t, runner)
	g := createProject(t, s, ctx, true)
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/restart", map[string]int{"expected_generation": 0})
	for _, wantFailure := range []bool{true, false} {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
		if len(s.running) != 0 {
			t.Fatal("new worker started before confirmed stop")
		}
		select {
		case cleanup := <-s.cleanupDone:
			if (cleanup.Err != nil) != wantFailure {
				t.Fatalf("cleanup: %+v", cleanup)
			}
			s.cleanupDone <- cleanup
		case <-time.After(3 * time.Second):
			t.Fatal("cleanup did not complete")
		}
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.running) != 1 {
		t.Fatalf("new round did not launch after recovery: %d", len(s.running))
	}
}
