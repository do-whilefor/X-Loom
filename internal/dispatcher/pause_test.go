package dispatcher

import (
	"context"
	"sync"
	"testing"
	"time"

	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestContinueRetriesOnlyThePausedExecution(t *testing.T) {
	for _, taskCase := range []struct {
		kind    string
		partial bool
	}{{"reason", false}, {"reason", true}, {"bootstrap", false}, {"explore", false}} {
		kind := taskCase.kind
		for _, early := range []bool{false, true} {
			name := kind + "/after-cancel"
			if taskCase.partial {
				name = "reason-with-directions/after-cancel"
			}
			if early {
				name = kind + "/before-cancel"
				if taskCase.partial {
					name = "reason-with-directions/before-cancel"
				}
			}
			t.Run(name, func(t *testing.T) {
				started := make(chan worker.Job, 4)
				cancelled := make(chan struct{})
				release := make(chan struct{})
				var once sync.Once
				runner := &controlledRunner{run: func(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
					started <- job
					<-ctx.Done()
					if job.PreviousRunID == "" {
						close(cancelled)
						<-release
					}
					return worker.Result{}, ctx.Err()
				}}
				s, ctx := scenario(t, runner)
				t.Cleanup(func() { once.Do(func() { close(release) }) })
				s.Config.Workers[0].TaskTypes = []string{kind}
				g := createProject(t, s, ctx, kind == "bootstrap")
				if kind == "explore" {
					createIntent(t, s, ctx, g.Project.ID, "paused direction")
				}
				if err := s.Step(ctx); err != nil {
					t.Fatal(err)
				}
				var old worker.Job
				select {
				case old = <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("initial execution not started")
				}
				if taskCase.partial {
					// The initial planner has already externalized a direction.
					// This makes the graph non-initial, and a fresh scheduler may
					// seed its checkpoint from that graph without a new revision.
					createIntent(t, s, ctx, g.Project.ID, "direction written before pause")
				}
				mustDo(t, s, ctx, "PUT", projectPath(g.Project.ID)+"/status", map[string]string{"status": "stopped"})
				if err := s.Step(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case <-cancelled:
				case <-time.After(3 * time.Second):
					t.Fatal("execution not cancelled")
				}
				if early {
					mustDo(t, s, ctx, "PUT", projectPath(g.Project.ID)+"/status", map[string]string{"status": "active"})
				}
				once.Do(func() { close(release) })
				waitFinished(t, s)
				if !early {
					mustDo(t, s, ctx, "PUT", projectPath(g.Project.ID)+"/status", map[string]string{"status": "active"})
				}
				if err := s.Step(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case fresh := <-started:
					if fresh.Kind != kind || fresh.PreviousRunID != old.RunID || fresh.RunID == old.RunID {
						t.Fatalf("resume did not create linked fresh attempt: %+v", fresh)
					}
				case <-time.After(300 * time.Millisecond):
					t.Fatal("continue left the paused execution permanently blocked")
				}
			})
		}
	}
}
