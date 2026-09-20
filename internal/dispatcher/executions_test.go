package dispatcher

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestTerminalFailureDoesNotMintFreshRun(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	r := &controlledRunner{run: func(context.Context, config.Worker, worker.Job) (worker.Result, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return worker.Result{Status: "failed", FailureKind: "invalid_output", Error: "invalid contract"}, nil
	}}
	s, ctx := scenario(t, r)
	createProject(t, s, ctx, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFinished(t, s)
	if f.Outcome != "failed" {
		t.Fatal(f)
	}
	for n := 0; n < 4; n++ {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || len(s.running) != 0 {
		t.Fatalf("failure automatically retried: calls=%d running=%d", calls, len(s.running))
	}
}

func TestFailedActiveProjectReleasesAdmission(t *testing.T) {
	s, ctx := scenario(t, &controlledRunner{run: func(context.Context, config.Worker, worker.Job) (worker.Result, error) {
		return worker.Result{Status: "failed", Error: "terminal"}, nil
	}})
	s.Config.Runtime.MaxProjects = 1
	first := createProject(t, s, ctx, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	waitFinished(t, s)
	second := createProject(t, s, ctx, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFinished(t, s)
	if f.Task.Job.Graph.Project.ID != second.Project.ID || s.admitted[first.Project.ID] {
		t.Fatal("failed active project starved another project's admission")
	}
}

func TestEnvironmentIdentityCoversEffectiveConfiguration(t *testing.T) {
	s, _ := scenario(t, &controlledRunner{})
	w := s.Config.Workers[0]
	w.Env = map[string]string{"ANTHROPIC_AUTH_TOKEN": "before"}
	initial := s.environmentID(w)
	w.Env["ANTHROPIC_AUTH_TOKEN"] = "rotated"
	if s.environmentID(w) != initial {
		t.Fatal("credential rotation changed task identity")
	}
	for _, key := range []string{"XLOOM_REASONING_EFFORT", "XLOOM_CONTEXT_BYTES", "XLOOM_MAX_OUTPUT_TOKENS", "XLOOM_REQUEST_TIMEOUT"} {
		w.Env[key] = "different"
		if s.environmentID(w) == initial {
			t.Fatalf("%s did not bind environment", key)
		}
		delete(w.Env, key)
	}
}

func TestFailedBootstrapAllowsDecisionWithoutRepeatingBootstrap(t *testing.T) {
	r := &controlledRunner{run: func(_ context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
		if j.Kind == "bootstrap" {
			return worker.Result{Status: "failed", Error: "initial method unavailable"}, nil
		}
		return worker.Result{Status: "success", Text: `{"intents":[{"from":["origin"],"description":"alternative method"}]}`}, nil
	}}
	s, ctx := scenario(t, r)
	createProject(t, s, ctx, true)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	first := waitFinished(t, s)
	if first.Task.Job.Kind != "bootstrap" || first.Outcome != "failed" {
		t.Fatal(first)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	second := waitFinished(t, s)
	if second.Task.Job.Kind != "reason" || second.Outcome != "success" {
		t.Fatal("bootstrap failure did not reach the planner", second)
	}
	// The new normal Step means initial(g) is now false. A previously accepted
	// explicit bootstrap retry must still be consumed instead of hanging forever.
	mustDo(t, s, ctx, "POST", executionPath(first.Task)+"/retry", map[string]any{})
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	third := waitFinished(t, s)
	if third.Task.Job.Kind != "bootstrap" || third.Task.Job.PreviousRunID != first.Task.Job.RunID {
		t.Fatal("explicit bootstrap retry was stranded after planning", third)
	}
}

func TestTransientRecoveryUsesOriginalRun(t *testing.T) {
	var jobs []worker.Job
	r := &controlledRunner{run: func(_ context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
		jobs = append(jobs, j)
		if len(jobs) < 3 {
			return worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: "503"}, nil
		}
		return worker.Result{Status: "success", Text: `{"intents":[{"from":["origin"],"description":"check"}]}`}, nil
	}}
	s, ctx := scenario(t, r)
	g := createProject(t, s, ctx, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFinished(t, s)
	if f.Outcome != "success" {
		t.Fatalf("%s %v", f.Outcome, f.Err)
	}
	if len(jobs) != 3 {
		t.Fatalf("attempts=%d", len(jobs))
	}
	for _, j := range jobs {
		if j.RunID != jobs[0].RunID || j.Budget != jobs[0].Budget {
			t.Fatal("retry changed identity or budget")
		}
	}
	if err := s.loadExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.executions) != 1 || s.executions[0].Status != "succeeded" {
		t.Fatal(s.executions)
	}
	graph, err := s.Client.Get(ctx, g.Project.ID)
	if err != nil || len(graph.Intents) != 1 {
		t.Fatalf("%+v %v", graph, err)
	}
}

func TestRetryableFailureHasBoundAndExplicitRetryLink(t *testing.T) {
	var jobs []worker.Job
	r := &controlledRunner{run: func(_ context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
		jobs = append(jobs, j)
		return worker.Result{Status: "failed", Retryable: true, Error: "temporary"}, nil
	}}
	s, ctx := scenario(t, r)
	g := createProject(t, s, ctx, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFinished(t, s)
	if f.Outcome != "failed" || len(jobs) != 3 {
		t.Fatalf("%+v attempts=%d", f, len(jobs))
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.running) != 0 {
		t.Fatal("failure bypassed retry bound")
	}
	mustDo(t, s, ctx, "POST", executionPath(f.Task)+"/retry", map[string]any{})
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	next := waitFinished(t, s)
	if next.Task.Job.RunID == f.Task.Job.RunID || next.Task.Job.PreviousRunID != f.Task.Job.RunID || next.Task.Job.Graph.Project.ID != g.Project.ID {
		t.Fatal("explicit retry did not link original step/run")
	}
}

func TestDispatcherRestartRecoversOriginalJob(t *testing.T) {
	started := make(chan worker.Job, 1)
	r := &controlledRunner{run: func(ctx context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
		started <- j
		<-ctx.Done()
		return worker.Result{}, ctx.Err()
	}}
	s, parent := scenario(t, r)
	createProject(t, s, parent, false)
	ctx, cancel := context.WithCancel(parent)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var original worker.Job
	select {
	case original = <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	f := waitFinished(t, s)
	if f.Outcome != "interrupted" {
		t.Fatal(f)
	}
	var recovered worker.Job
	next := New(s.Config, &controlledRunner{run: func(_ context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
		recovered = j
		return worker.Result{Status: "success", Text: `{"intents":[{"from":["origin"],"description":"resumed"}]}`}, nil
	}})
	t.Cleanup(func() {
		for _, task := range next.running {
			task.Cancel()
		}
		next.wg.Wait()
	})
	if err := next.Step(parent); err != nil {
		t.Fatal(err)
	}
	done := waitFinished(t, next)
	if done.Outcome != "success" {
		t.Fatalf("%s %v", done.Outcome, done.Err)
	}
	before, _ := json.Marshal(original)
	after, _ := json.Marshal(recovered)
	if string(before) != string(after) {
		t.Fatalf("recovered input changed: %s != %s", before, after)
	}
	if done.Task.Execution.Resumes != 1 {
		t.Fatal("recovery allowance not persisted")
	}
}

func TestRecoveryAppliesPendingResultWithoutRerunningWorker(t *testing.T) {
	s, ctx := scenario(t, &controlledRunner{})
	g := createProject(t, s, ctx, true)
	i := createIntent(t, s, ctx, g.Project.ID, "bootstrap")
	lease := Lease{Run: "mock@pending", Kind: "bootstrap", Intent: i.ID}
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/intents/"+i.ID+"/heartbeat", map[string]string{"worker": lease.Run})
	task := &task{Job: worker.Job{RunID: "pending", Kind: "bootstrap", Graph: g, Intent: &i, WorkerType: "mock", Workspace: "/workspace", Budget: s.Config.Tasks.Bootstrap, EnvironmentID: s.environmentID(s.Config.Workers[0])}, Worker: s.Config.Workers[0], Lease: lease}
	if err := s.register(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.status(ctx, task, "result_pending", worker.Result{Status: "success", Text: `{"fact":{"description":"evidence"},"complete":{"description":"done"}}`}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	next := New(s.Config, &controlledRunner{run: func(context.Context, config.Worker, worker.Job) (worker.Result, error) {
		calls++
		return worker.Result{}, nil
	}})
	t.Cleanup(func() {
		for _, task := range next.running {
			task.Cancel()
		}
		next.wg.Wait()
	})
	if err := next.Step(ctx); err != nil {
		t.Fatal(err)
	}
	done := waitFinished(t, next)
	if done.Outcome != "success" || calls != 0 {
		t.Fatalf("outcome=%s error=%v worker_calls=%d", done.Outcome, done.Err, calls)
	}
	result, err := next.Client.Get(ctx, g.Project.ID)
	if err != nil || result.Project.Status != "completed" || len(result.Facts) != 3 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestStateStepPriorityAndAbandonment(t *testing.T) {
	s, ctx := scenario(t, &controlledRunner{})
	s.Config.Runtime.MaxProjectWorkers = 1
	g := createProject(t, s, ctx, false)
	low := createIntent(t, s, ctx, g.Project.ID, "low")
	high := createIntent(t, s, ctx, g.Project.ID, "high")
	abandoned := createIntent(t, s, ctx, g.Project.ID, "abandoned")
	lease := Lease{Run: "planner", Kind: "reason"}
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "test"})
	action := func(key, payload string) {
		t.Helper()
		if err := s.Client.Do(ctx, "POST", projectPath(g.Project.ID)+"/state/actions", board.StateAction{Op: "step", IdempotencyKey: key, Payload: json.RawMessage(payload)}, nil, &lease); err != nil {
			t.Fatal(err)
		}
	}
	action("priority", `{"action":"priority","id":"`+high.ID+`","priority":99,"reason":"most relevant"}`)
	action("abandon", `{"action":"abandon","id":"`+abandoned.ID+`","reason":"obsolete"}`)
	mustDo(t, s, ctx, "POST", projectPath(g.Project.ID)+"/reason/release", map[string]string{"worker": lease.Run})
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.running) != 1 {
		t.Fatal("unexpected task count")
	}
	for _, task := range s.running {
		if task.Lease.Intent != high.ID {
			t.Fatalf("picked %s; want %s (low=%s)", task.Lease.Intent, high.ID, low.ID)
		}
	}
}
