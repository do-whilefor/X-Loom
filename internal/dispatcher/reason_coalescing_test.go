package dispatcher

import (
	"context"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func coalescingFixture() (*Scheduler, board.Graph, board.SchedulePage, time.Time) {
	s := New(config.Config{}, nil)
	g := board.Graph{Project: board.Project{ID: "coalesce"}, Intents: []board.Intent{
		{ID: "working", Worker: board.Ptr("execute@one")}, {ID: "queued"},
	}}
	s.checkpoints[g.Project.ID] = checkpoint{Facts: 2, Open: 2}
	s.decisionRevisions[g.Project.ID] = 1
	s.stateRevisions[g.Project.ID] = 2
	previous := board.SchedulePage{FactCount: 2, OpenCount: 2, DecisionRevision: 1}
	s.schedules[g.Project.ID] = board.SchedulePage{FactCount: 3, OpenCount: 2, DecisionRevision: 2}
	return s, g, previous, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
}

func TestReasonCoalescesFactsAndPartialCompletionsUntilQuiet(t *testing.T) {
	s, g, previous, start := coalescingFixture()
	if got := s.trigger(g, board.ExecutionCheck{}, previous, start); got != "" {
		t.Fatalf("intermediate fact started Decide while Execute was running: %q", got)
	}
	previous = s.schedules[g.Project.ID]
	input := previous
	// One direction finishes while another remains active. Its terminal event
	// extends the quiet window even when it did not add another fact.
	input.OpenCount, input.DecisionRevision = 1, 3
	s.schedules[g.Project.ID] = input
	s.stateRevisions[g.Project.ID] = input.DecisionRevision
	if got := s.trigger(g, board.ExecutionCheck{}, previous, start.Add(10*time.Second)); got != "" {
		t.Fatalf("partial completion escaped coalescing: %q", got)
	}
	previous = input
	// Lease/revision churn without a business change must not reset the timer.
	input.Revision++
	s.schedules[g.Project.ID] = input
	if got := s.trigger(g, board.ExecutionCheck{}, previous, start.Add(24*time.Second)); got != "" {
		t.Fatalf("Decide did not wait for the most recent completion to settle: %q", got)
	}
	if got := s.trigger(g, board.ExecutionCheck{}, input, start.Add(25*time.Second)); got == "" {
		t.Fatal("stable input never became eligible for Decide")
	}
}

func TestReasonContinuousUpdatesHaveBoundedWait(t *testing.T) {
	s, g, previous, start := coalescingFixture()
	for second := 0; second <= 60; second += 10 {
		input := s.schedules[g.Project.ID]
		input.FactCount++
		input.DecisionRevision++
		s.schedules[g.Project.ID] = input
		s.stateRevisions[g.Project.ID] = input.DecisionRevision
		got := s.trigger(g, board.ExecutionCheck{}, previous, start.Add(time.Duration(second)*time.Second))
		if (got != "") != (second == 60) {
			t.Fatalf("continuous updates at second %d: trigger=%q; expected only at the maximum wait", second, got)
		}
		previous = input
	}
}

func TestReasonUrgentInputsAndIdleExecutionBypassCoalescing(t *testing.T) {
	for _, name := range []string{"hint", "initial", "explicit_retry", "idle", "drained", "invalid_dependency"} {
		t.Run(name, func(t *testing.T) {
			s, g, previous, now := coalescingFixture()
			if got := s.trigger(g, board.ExecutionCheck{}, previous, now); got != "" {
				t.Fatal("fixture was not waiting")
			}
			previous = s.schedules[g.Project.ID]
			input, check := previous, board.ExecutionCheck{}
			switch name {
			case "hint":
				input.HintCount++
			case "initial":
				delete(s.checkpoints, g.Project.ID)
			case "explicit_retry":
				check.PreviousRunID = "authorized-attempt"
			case "idle":
				g.Intents[0].Worker = nil
			case "drained":
				input.OpenCount = 0
			case "invalid_dependency":
				input.Steps = []board.Step{{ID: "queued", InvalidSources: []string{"refuted"}}}
			}
			s.schedules[g.Project.ID] = input
			if got := s.trigger(g, check, previous, now.Add(time.Second)); got == "" {
				t.Fatalf("%s was delayed behind ordinary Execute updates", name)
			}
		})
	}
}

func TestReasonOldInvalidDependencyDoesNotDisableCoalescing(t *testing.T) {
	s, g, previous, now := coalescingFixture()
	previous.Steps = []board.Step{{ID: "old", InvalidSources: []string{"already-refuted"}}}
	input := s.schedules[g.Project.ID]
	input.Steps = previous.Steps
	s.schedules[g.Project.ID] = input
	if got := s.trigger(g, board.ExecutionCheck{}, previous, now); got != "" {
		t.Fatalf("previously observed invalid dependency made an ordinary update urgent: %q", got)
	}
	input.Steps = append(input.Steps, board.Step{ID: "new", InvalidSources: []string{"new-refutation"}})
	s.schedules[g.Project.ID] = input
	if got := s.trigger(g, board.ExecutionCheck{}, previous, now.Add(time.Second)); got == "" {
		t.Fatal("new invalid dependency did not wake Decide")
	}
	if got := s.trigger(g, board.ExecutionCheck{}, input, now.Add(2*time.Second)); got == "" {
		t.Fatal("urgent input was lost while waiting for an available planner")
	}
}

func TestReasonInvalidationObservedDuringPlannerCancellationStaysUrgent(t *testing.T) {
	s, g, previous, now := coalescingFixture()
	input := s.schedules[g.Project.ID]
	input.Steps = []board.Step{{ID: "queued", InvalidSources: []string{"refuted"}}}
	// dispatch observes this page while an old planner is still running, so
	// it records the invalidation but does not try to launch another Decide.
	s.noteInvalidDependencies(g.Project.ID, previous, input)
	s.schedules[g.Project.ID] = input
	// The next page has identical invalid_sources. The pending urgent signal
	// must survive until that old planner exits, despite ordinary Execute work.
	if got := s.trigger(g, board.ExecutionCheck{}, input, now); got == "" {
		t.Fatal("dependency invalidation was lost during planner cancellation")
	}
	// A successful decision consumes the event. A later fact must once again
	// use normal coalescing even though the old invalid Step remains visible.
	s.checkpoints[g.Project.ID] = checkpoint{Facts: input.FactCount, Open: input.OpenCount}
	s.decisionRevisions[g.Project.ID] = input.DecisionRevision
	if got := s.trigger(g, board.ExecutionCheck{}, input, now); got != "" {
		t.Fatalf("consumed invalidation started another planner: %q", got)
	}
	previous = input
	input.FactCount++
	input.DecisionRevision++
	s.stateRevisions[g.Project.ID] = input.DecisionRevision
	s.schedules[g.Project.ID] = input
	if got := s.trigger(g, board.ExecutionCheck{}, previous, now.Add(time.Second)); got != "" {
		t.Fatalf("consumed invalidation disabled later coalescing: %q", got)
	}
}

func TestReasonWaitDoesNotCrossProjectGeneration(t *testing.T) {
	s, g, previous, now := coalescingFixture()
	s.trigger(g, board.ExecutionCheck{}, previous, now)
	g.Project.Generation++
	s.observeGeneration(g.Project)
	if len(s.reasonWaits) != 0 {
		t.Fatal("project restart retained the old input's scheduling deadline")
	}
	if got := s.trigger(g, board.ExecutionCheck{}, board.SchedulePage{}, now); got != "initial" {
		t.Fatalf("new project generation did not get an immediate initial decision: %q", got)
	}
}

type publishingExecuteRunner struct {
	batchProtocolRunner
	published chan worker.Job
	release   chan struct{}
}

func (r *publishingExecuteRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	result, err := r.batchProtocolRunner.Run(ctx, backend, job)
	if err != nil || job.Kind != "explore" {
		return result, err
	}
	r.published <- job
	select {
	case <-r.release:
		return result, nil
	case <-ctx.Done():
		return worker.Result{}, ctx.Err()
	}
}

func TestDispatchUsesFreeSlotsForExecuteDuringFactBurst(t *testing.T) {
	fixture, _, _, graph, store := batchSchedulerFixture(t, 2)
	runner := &publishingExecuteRunner{batchProtocolRunner: batchProtocolRunner{directions: 2}, published: make(chan worker.Job, 2), release: make(chan struct{})}
	fixture.Config.Runtime.MaxWorkers = 3
	s := New(fixture.Config, runner)
	s.Config.Runtime.MaxWorkers = 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer func() { cancel(); s.wg.Wait() }()
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait() // Initial Decide publishes the two directions.
	s.reap()
	s.Config.Runtime.MaxWorkers, s.Config.Runtime.MaxProjectWorkers = 3, 3
	s.Config.Workers[0].MaxRunning = 3
	// Dispatch separately to ensure the second scheduling pass sees the first
	// Execute's fact while that producer still owns its execution lease.
	for range 2 {
		if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
			t.Fatalf("Execute did not start: started=%v err=%v", ok, err)
		}
		select {
		case job := <-runner.published:
			if job.Kind != "explore" {
				t.Fatalf("unexpected task: %s", job.Kind)
			}
		case <-ctx.Done():
			t.Fatal("an intermediate fact consumed the free slot with Decide instead of Execute")
		}
	}
	for range 3 {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	reasons := 0
	for _, run := range testExecutions(t, store) {
		if run.Kind == "reason" {
			reasons++
		}
	}
	if reasons != 1 {
		t.Fatalf("fact burst launched a speculative planner: %d Decide runs", reasons)
	}
	close(runner.release)
	s.wg.Wait()
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	var state board.State
	if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if state.Graph.Project.Status != "completed" {
		t.Fatal("last Execute completion did not trigger immediate final review")
	}
}
