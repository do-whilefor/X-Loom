//go:build linux

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

type updateProvider func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error)

func (p updateProvider) Generate(ctx context.Context, messages []agent.Message, definitions []agent.Definition, emit agent.Emit) (agent.Message, error) {
	return p(ctx, messages, definitions, emit)
}

// Only the process launch is replaced. Worker, graph files, Dispatcher identity
// lookup, HTTP permissions, snapshot preparation and SQLite remain real.
type updateLocalRunner struct {
	handler func(context.Context, worker.Job, worker.GraphRequest) (any, error)
	options worker.Options
}

func (r *updateLocalRunner) SetGraphHandler(fn func(context.Context, worker.Job, worker.GraphRequest) (any, error)) {
	r.handler = fn
}
func (*updateLocalRunner) Cleanup(context.Context, string, string) error { return nil }
func (*updateLocalRunner) Projects(context.Context) ([]string, error)    { return nil, nil }
func (r *updateLocalRunner) Run(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	opts := r.options
	opts.Output = &updateFileBridge{ctx: ctx, dir: opts.RunDir, job: job, handler: r.handler}
	return worker.Run(ctx, job, opts)
}

type updateFileBridge struct {
	ctx     context.Context
	dir     string
	job     worker.Job
	handler func(context.Context, worker.Job, worker.GraphRequest) (any, error)
}

func (b *updateFileBridge) Write(raw []byte) (int, error) {
	var event worker.GraphRequestEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return 0, err
	}
	if event.Type != "graph_request" {
		return len(raw), nil
	}
	response := worker.GraphResponse{RequestID: event.Request.RequestID}
	err := worker.ValidateGraphRequest(b.job, event.Request)
	var result any
	if err == nil {
		result, err = b.handler(b.ctx, b.job, event.Request)
	}
	if err != nil {
		response.Error = err.Error()
	} else if response.Result, err = json.Marshal(result); err != nil {
		return 0, err
	}
	data, err := json.Marshal(response)
	if err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(b.dir, "graph-response-"+event.Request.RequestID+".json"), data, 0600); err != nil {
		return 0, err
	}
	return len(raw), nil
}

type updateRequestFixture struct {
	t         *testing.T
	ctx       context.Context
	scheduler *Scheduler
	runner    *updateLocalRunner
	project   board.Project
	workspace string
	seq       int
}

func newUpdateRequestFixture(t *testing.T) *updateRequestFixture {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "updates.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(server.New(store))
	t.Cleanup(httpServer.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	runner := &updateLocalRunner{}
	scheduler := New(config.Config{Server: httpServer.URL, Runtime: config.Runtime{MaxWorkers: 1, Interval: 1, HealthMode: "disabled"}}, runner)
	f := &updateRequestFixture{t: t, ctx: ctx, scheduler: scheduler, runner: runner, workspace: t.TempDir()}
	var graph board.Graph
	f.do("POST", "/projects", map[string]any{"title": "Execute updates", "origin": "Synthetic local request fixture", "goal": "Review corrected premises", "bootstrap_enabled": false}, &graph, nil)
	f.project = graph.Project
	return f
}

func (f *updateRequestFixture) do(method, path string, input, output any, lease *Lease) {
	f.t.Helper()
	if err := f.scheduler.Client.Do(f.ctx, method, path, input, output, lease); err != nil {
		f.t.Fatal(err)
	}
}

func (f *updateRequestFixture) prepare(kind string, from []string, description string) *task {
	f.t.Helper()
	f.seq++
	id := fmt.Sprintf("updates-%d", f.seq)
	lease := Lease{Run: "fixture@" + id, Kind: kind}
	var intent *board.Intent
	base := projectPath(f.project.ID)
	if kind == "reason" {
		f.do("POST", base+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil)
	} else {
		var created board.Intent
		f.do("POST", base+"/intents", map[string]any{"from": from, "description": description, "creator": "fixture"}, &created, nil)
		lease.Intent = created.ID
		f.do("POST", base+"/intents/"+created.ID+"/heartbeat", map[string]string{"worker": lease.Run}, &created, nil)
		intent = &created
	}
	job := worker.Job{RunID: id, Kind: kind, WorkerType: "go", Workspace: f.workspace, GraphRPC: true, ResultContractVersion: 2, Graph: board.Graph{Project: f.project}, Intent: intent, Budget: config.Task{Timeout: 60, ConcludeTimeout: 10}}
	raw, err := json.Marshal(job)
	if err != nil {
		f.t.Fatal(err)
	}
	execution := board.Execution{ProjectID: f.project.ID, ID: id, Namespace: "xloom", Backend: "fixture", Kind: kind, Intent: lease.Intent, Lease: lease.Run, Job: raw}
	f.do("POST", base+"/executions/prepare", execution, &execution, &lease)
	if err = json.Unmarshal(execution.Job, &job); err != nil {
		f.t.Fatal(err)
	}
	return &task{Job: job, Worker: config.Worker{Name: "fixture", Type: "go"}, Lease: lease, Execution: execution, LeaseTimeout: 30 * time.Second}
}

func (f *updateRequestFixture) start(run *task) {
	f.t.Helper()
	f.do("POST", executionPath(run)+"/status", map[string]string{"status": "running"}, nil, &run.Lease)
}

func (f *updateRequestFixture) publish(run *task, description string) string {
	f.t.Helper()
	var receipt board.StateActionResult
	f.do("POST", projectPath(f.project.ID)+"/state/actions", map[string]any{
		"op": "fact", "idempotency_key": run.Job.RunID + ":observation",
		"payload": map[string]any{"description": description, "scope": "synthetic local fixture", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []board.EvidenceRef{{RunID: run.Job.RunID, Path: "retained/response.txt", Excerpt: description}}},
	}, &receipt, &run.Lease)
	return receipt.ID
}

func (f *updateRequestFixture) decide(op string, payload any) {
	f.t.Helper()
	run := f.prepare("reason", nil, "")
	f.start(run)
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	batch := board.DecisionBatch{ExpectedVersion: run.Job.Decision.StateVersion, Actions: []board.DecisionAction{{Op: op, Payload: raw}}}
	f.do("POST", projectPath(f.project.ID)+"/state/decisions/commit", batch, nil, &run.Lease)
}

const updateOriginal = "Initial observation used an authenticated session"
const updateCorrected = "Fresh unauthenticated fixture request is denied"
const updateReason = "The original authenticated observation does not establish public access"

func (f *updateRequestFixture) dependentRun() (*task, string) {
	f.t.Helper()
	source := f.prepare("explore", []string{"origin"}, "Observe the initial fixture")
	f.start(source)
	id := f.publish(source, updateOriginal)
	run := f.prepare("explore", []string{id}, "Inspect the initially public fixture")
	f.start(run)
	return run, id
}

func (f *updateRequestFixture) correct(source string) string {
	f.t.Helper()
	observer := f.prepare("explore", []string{"origin"}, "Independently check unauthenticated access")
	f.start(observer)
	correction := f.publish(observer, updateCorrected)
	f.decide("fact_relation", map[string]string{"kind": "refutes", "source": correction, "target": source, "reason": updateReason})
	return correction
}

func updateToolCall(id, name, input string) agent.Message {
	return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}}}
}

func updateStop() agent.Message {
	return agent.Text("assistant", `{"accepted":false,"reason":"The deterministic request fixture is complete"}`)
}

func updateHistoryCopy(messages []agent.Message) []agent.Message {
	raw, _ := json.Marshal(messages)
	var copied []agent.Message
	_ = json.Unmarshal(raw, &copied)
	return copied
}

func updateHistoryText(messages []agent.Message) string {
	raw, _ := json.Marshal(messages)
	return string(raw)
}

type updateRunResult struct {
	result worker.Result
	err    error
}

func (f *updateRequestFixture) await(done <-chan updateRunResult) updateRunResult {
	f.t.Helper()
	select {
	case result := <-done:
		return result
	case <-f.ctx.Done():
		f.t.Fatal("request fixture did not finish")
		return updateRunResult{}
	}
}

func (f *updateRequestFixture) awaitTool(started <-chan struct{}, done <-chan updateRunResult) {
	f.t.Helper()
	select {
	case <-started:
	case result := <-done:
		f.t.Fatalf("worker ended before controlled tool: %+v %v", result.result, result.err)
	case <-f.ctx.Done():
		f.t.Fatal("controlled tool never started")
	}
}

func assertUpdateDelivered(t *testing.T, history []agent.Message, source, correction string) {
	t.Helper()
	resultIndex, correctionIndex := -1, -1
	uses, results := 0, 0
	var update board.ExecuteUpdates
	for i, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ID == "wait-for-correction" {
				uses++
			}
			if block.Type == "tool_result" && block.ToolUseID == "wait-for-correction" {
				resultIndex = i
				results++
			}
		}
		if strings.HasPrefix(message.Text(), "Shared graph update:") && strings.Contains(message.Text(), updateCorrected) {
			correctionIndex = i
			_, payload, _ := strings.Cut(message.Text(), "\n")
			if err := json.Unmarshal([]byte(payload), &update); err != nil {
				t.Fatalf("graph update is not structured task data: %v", err)
			}
		}
	}
	if correctionIndex < 0 {
		t.Error("next Provider request omitted the corrective observation")
		return
	}
	invalid, fact, relation := false, false, false
	for _, id := range update.InvalidSources {
		invalid = invalid || id == source
	}
	for _, record := range update.Facts {
		fact = fact || record.ID == correction && record.Description == updateCorrected
	}
	for _, record := range update.Relations {
		relation = relation || record.Source == correction && record.Target == source && record.Kind == "refutes" && record.Reason == updateReason
	}
	if !invalid || !fact || !relation {
		t.Errorf("delivered notice omitted invalid source, correction, or reason: %+v", update)
	}
	if uses != 0 && (uses != 1 || results != 1) {
		t.Errorf("correction checkpoint broke tool result pairing: uses=%d results=%d", uses, results)
	}
	if resultIndex >= 0 && correctionIndex <= resultIndex {
		t.Errorf("correction was not appended after the complete tool result: result=%d correction=%d", resultIndex, correctionIndex)
	}
}

func TestExecuteCorrectionReachesNextProviderRequest(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	started, release := make(chan struct{}), make(chan struct{})
	var requests [][]agent.Message
	f.runner.options = worker.Options{RunDir: t.TempDir(), Tools: []agent.Tool{{Definition: agent.Definition{Name: "wait", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		select {
		case <-release:
			return "harmless fixture wait completed", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}}, Provider: updateProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		requests = append(requests, updateHistoryCopy(history))
		if len(requests) == 1 {
			return updateToolCall("wait-for-correction", "wait", `{}`), nil
		}
		return updateStop(), nil
	})}
	done := make(chan updateRunResult, 1)
	go func() { result, err := f.runner.Run(f.ctx, run.Worker, run.Job); done <- updateRunResult{result, err} }()
	f.awaitTool(started, done)
	correction := f.correct(source)
	close(release)
	result := f.await(done)
	if result.err != nil || result.result.Status != "success" || len(requests) != 2 {
		t.Fatalf("request run failed: %+v err=%v requests=%d", result.result, result.err, len(requests))
	}
	if strings.Contains(updateHistoryText(requests[0]), updateCorrected) {
		t.Fatal("correction was present before the controlled publication")
	}
	assertUpdateDelivered(t, requests[1], source, correction)
}

func TestExecuteCorrectionBeforeFirstRequestPreservesSnapshot(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	correction := f.correct(source)
	var history []agent.Message
	f.runner.options = worker.Options{RunDir: t.TempDir(), Provider: updateProvider(func(_ context.Context, messages []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		history = updateHistoryCopy(messages)
		return updateStop(), nil
	})}
	result, err := f.runner.Run(f.ctx, run.Worker, run.Job)
	if err != nil || result.Status != "success" {
		t.Fatalf("first request failed: %+v %v", result, err)
	}
	assertUpdateDelivered(t, history, source, correction)
	value, err := f.runner.handler(f.ctx, run.Job, worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_snapshot", Section: "facts", IDs: []string{source, correction}, ExpectedVersion: run.Job.InputSnapshot.StateVersion})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var page struct {
		StateVersion string             `json:"state_version"`
		Items        []board.FactRecord `json:"items"`
		Missing      []string           `json:"missing"`
	}
	if err = json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.StateVersion != run.Job.InputSnapshot.StateVersion || len(page.Items) != 1 || page.Items[0].ID != source || page.Items[0].Description != updateOriginal || page.Items[0].Status != "valid" || strings.Contains(string(raw), updateCorrected) {
		t.Fatalf("automatic updates changed the original input: %s", raw)
	}
}

func TestExecuteUnrelatedChangesAndRepeatedCorrectionsStayQuiet(t *testing.T) {
	for _, initialCorrection := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrected=%t", initialCorrection), func(t *testing.T) {
			f := newUpdateRequestFixture(t)
			run, source := f.dependentRun()
			if initialCorrection {
				f.correct(source)
			}
			started, release := make(chan struct{}), make(chan struct{})
			var requests [][]agent.Message
			f.runner.options = worker.Options{RunDir: t.TempDir(), Tools: []agent.Tool{
				{Definition: agent.Definition{Name: "wait", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
					close(started)
					select {
					case <-release:
						return "fixture released", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}},
				{Definition: agent.Definition{Name: "noop", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) { return "no graph changes", nil }},
			}, Provider: updateProvider(func(_ context.Context, messages []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				requests = append(requests, updateHistoryCopy(messages))
				switch len(requests) {
				case 1:
					return updateToolCall("wait-unrelated", "wait", `{}`), nil
				case 2:
					return updateToolCall("repeat-checkpoint", "noop", `{}`), nil
				default:
					return updateStop(), nil
				}
			})}
			done := make(chan updateRunResult, 1)
			go func() { result, err := f.runner.Run(f.ctx, run.Worker, run.Job); done <- updateRunResult{result, err} }()
			f.awaitTool(started, done)
			unrelated := f.prepare("explore", []string{"origin"}, "Check a separate fixture branch")
			f.start(unrelated)
			f.publish(unrelated, "Unrelated branch has a different observation")
			close(release)
			result := f.await(done)
			if result.err != nil || result.result.Status != "success" || len(requests) != 3 {
				t.Fatalf("quiet-update run failed: %+v %v requests=%d", result.result, result.err, len(requests))
			}
			want := 0
			if initialCorrection {
				want = 1
			}
			for i, request := range requests {
				text := updateHistoryText(request)
				if got := strings.Count(text, "Shared graph update:"); got != want {
					t.Errorf("request %d has %d notices, want %d", i+1, got, want)
				}
				if strings.Contains(text, "Unrelated branch has a different observation") {
					t.Errorf("request %d received an unrelated observation", i+1)
				}
			}
		})
	}
}

func updateResultText(history []agent.Message, id string) string {
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID == id {
				var text string
				if json.Unmarshal(block.Content, &text) == nil {
					return text
				}
				return string(block.Content)
			}
		}
	}
	return ""
}

func TestExecuteCanActivelyReadCorrectionAndOriginalSnapshot(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	correction := f.correct(source)
	var requests [][]agent.Message
	f.runner.options = worker.Options{RunDir: t.TempDir(), Provider: updateProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		requests = append(requests, updateHistoryCopy(history))
		if len(requests) == 1 {
			message := updateToolCall("active-relations", "read_graph", fmt.Sprintf(`{"section":"relations","ids":[%q]}`, source))
			message.Content = append(message.Content,
				updateToolCall("active-correction", "read_graph", fmt.Sprintf(`{"section":"facts","ids":[%q]}`, correction)).Content[0],
				updateToolCall("frozen-input", "read_snapshot", fmt.Sprintf(`{"section":"facts","ids":[%q,%q]}`, source, correction)).Content[0])
			return message, nil
		}
		return updateStop(), nil
	})}
	result, err := f.runner.Run(f.ctx, run.Worker, run.Job)
	if err != nil || result.Status != "success" || len(requests) != 2 {
		t.Fatalf("active reads failed: %+v %v requests=%d", result, err, len(requests))
	}
	relations := updateResultText(requests[1], "active-relations")
	facts := updateResultText(requests[1], "active-correction")
	frozen := updateResultText(requests[1], "frozen-input")
	if !strings.Contains(relations, updateReason) || !strings.Contains(relations, "refutes") || !strings.Contains(facts, updateCorrected) {
		t.Fatalf("active reads did not return the correction: relations=%s facts=%s", relations, facts)
	}
	if !strings.Contains(frozen, updateOriginal) || strings.Contains(frozen, updateCorrected) || !strings.Contains(frozen, `"status":"valid"`) {
		t.Fatalf("read_snapshot lost the immutable premise: %s", frozen)
	}
}

func TestExecuteCorrectionRecoveryRetainsNoticeAndDoesNotRepeatTool(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	started, release := make(chan struct{}), make(chan struct{})
	toolCalls := 0
	var requests [][]agent.Message
	runDir := t.TempDir()
	f.runner.options = worker.Options{RunDir: runDir, Tools: []agent.Tool{{Definition: agent.Definition{Name: "wait", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
		toolCalls++
		close(started)
		select {
		case <-release:
			return "completed exactly once", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}}, Provider: updateProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		requests = append(requests, updateHistoryCopy(history))
		switch len(requests) {
		case 1:
			return updateToolCall("wait-for-correction", "wait", `{}`), nil
		case 2:
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("controlled request interruption after durable correction")}
		default:
			return updateStop(), nil
		}
	})}
	done := make(chan updateRunResult, 1)
	go func() { result, err := f.runner.Run(f.ctx, run.Worker, run.Job); done <- updateRunResult{result, err} }()
	f.awaitTool(started, done)
	correction := f.correct(source)
	close(release)
	first := f.await(done)
	if first.err != nil || !first.result.Retryable || first.result.FailureKind != string(agent.ErrorTransport) {
		t.Fatalf("did not reach retryable transport boundary: %+v %v", first.result, first.err)
	}
	type checkpoint struct {
		RunID             string          `json:"run_id"`
		ExecutionDeadline time.Time       `json:"execution_deadline"`
		RecoveryCount     int             `json:"recovery_count"`
		History           []agent.Message `json:"history"`
		Updates           json.RawMessage `json:"execute_updates"`
	}
	readCheckpoint := func() checkpoint {
		raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
		if err != nil {
			t.Fatal(err)
		}
		var saved checkpoint
		if err = json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	before := readCheckpoint()
	if len(before.Updates) == 0 {
		t.Fatal("history has no durable update checkpoint")
	}
	assertUpdateDelivered(t, before.History, source, correction)
	last, err := f.runner.Run(f.ctx, run.Worker, run.Job)
	after := readCheckpoint()
	if err != nil || last.Status != "success" || toolCalls != 1 || len(requests) != 3 {
		t.Fatalf("recovery replayed tools or failed: %+v %v tools=%d requests=%d", last, err, toolCalls, len(requests))
	}
	assertUpdateDelivered(t, requests[2], source, correction)
	if strings.Count(updateHistoryText(requests[2]), "Shared graph update:") != 1 || before.RunID != after.RunID || !before.ExecutionDeadline.Equal(after.ExecutionDeadline) || before.RecoveryCount+1 != after.RecoveryCount {
		t.Fatalf("recovery duplicated notice or changed the execution budget: before=%+v after=%+v", before, after)
	}
}

func TestExecuteExplicitAbandonCancelsWorkerAndKeepsIndependentObservation(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	started := make(chan struct{})
	requests := 0
	f.runner.options = worker.Options{RunDir: t.TempDir(), Tools: []agent.Tool{{Definition: agent.Definition{Name: "wait", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}}, Provider: updateProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		requests++
		if requests > 1 {
			return agent.Message{}, errors.New("abandoned execution reached another Provider request")
		}
		return updateToolCall("wait-for-abandon", "wait", `{}`), nil
	})}
	done := make(chan updateRunResult, 1)
	go func() {
		status, err := f.scheduler.runTask(f.ctx, run)
		done <- updateRunResult{worker.Result{Status: status}, err}
	}()
	f.awaitTool(started, done)
	f.correct(source)
	observation := f.publish(run, "Independent response retained a rate-limit header")
	f.decide("step", map[string]string{"action": "abandon", "id": run.Job.Intent.ID, "reason": "The corrected premise makes further fixture work unnecessary"})
	// The production heartbeat observes revoked ownership and cancels the
	// running Worker. The test never calls its execution cancel function.
	result := f.await(done)
	if result.result.Status != "cancelled" || !errors.Is(result.err, context.Canceled) || requests != 1 {
		t.Fatalf("abandon did not cancel through its lease: %+v %v requests=%d", result.result, result.err, requests)
	}
	for _, key := range []string{run.Job.RunID + ":observation", "late-observation"} {
		err := f.scheduler.Client.Do(f.ctx, "POST", projectPath(f.project.ID)+"/state/actions", map[string]any{"op": "fact", "idempotency_key": key, "payload": map[string]any{"description": "Late result must be rejected"}}, nil, &run.Lease)
		var protocol *ProtocolError
		if !errors.As(err, &protocol) || protocol.Status != 409 {
			t.Fatalf("late observation was not fenced: %v", err)
		}
	}
	var state board.State
	f.do("GET", projectPath(f.project.ID)+"/state", nil, &state, nil)
	if err := state.ValidateFactSources([]string{observation}, true); err != nil {
		t.Fatalf("abandon invalidated an independent observation: %v", err)
	}
	for _, step := range state.Steps {
		if step.ID == run.Job.Intent.ID {
			if step.Status != "abandoned" || step.Result != nil {
				t.Fatalf("cancelled worker completed or changed its abandoned Step: %+v", step)
			}
			return
		}
	}
	t.Fatal("abandoned Step disappeared")
}

type updateCompactionProvider struct {
	normal    updateProvider
	summaries int
}

func (p *updateCompactionProvider) Generate(ctx context.Context, history []agent.Message, definitions []agent.Definition, emit agent.Emit) (agent.Message, error) {
	return p.normal.Generate(ctx, history, definitions, emit)
}

func (p *updateCompactionProvider) GenerateSummary(context.Context, []agent.Message, int, agent.Emit) (agent.Message, error) {
	p.summaries++
	// This is a valid summary that deliberately omits every correction. The
	// request must preserve the authoritative update independently of notes.
	return agent.Text("assistant", `{"notes":"A harmless local fixture tool completed.","quotes":[]}`), nil
}

func TestExecuteCorrectionSurvivesCompactionAndRecovery(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	started, release := make(chan struct{}), make(chan struct{})
	const contextBytes = 40000
	const retainedTokens = 10000
	padding := strings.Repeat("Synthetic padding from a harmless fixture tool.\n", 2500)
	initialTools, resumedTools := 0, 0
	var requests [][]agent.Message
	var requestSizes []int
	p := &updateCompactionProvider{normal: updateProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		requests = append(requests, updateHistoryCopy(history))
		raw, err := json.Marshal(struct {
			Messages []agent.Message    `json:"messages"`
			Tools    []agent.Definition `json:"tools,omitempty"`
		}{agent.WireHistory(history), definitions})
		if err != nil {
			return agent.Message{}, err
		}
		requestSizes = append(requestSizes, len(raw))
		switch len(requests) {
		case 1:
			return updateToolCall("wait-for-correction", "wait", `{}`), nil
		case 2:
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("controlled interruption after committed compaction")}
		case 3:
			return updateToolCall("padding-after-recovery", "pad", `{}`), nil
		default:
			return updateStop(), nil
		}
	})}
	runDir := t.TempDir()
	f.runner.options = worker.Options{RunDir: runDir, Provider: p, ContextBytes: contextBytes, ContextTargetTokens: retainedTokens, Tools: []agent.Tool{
		{Definition: agent.Definition{Name: "wait", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			initialTools++
			close(started)
			select {
			case <-release:
				return padding, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		{Definition: agent.Definition{Name: "pad", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) {
			resumedTools++
			return padding, nil
		}},
	}}
	done := make(chan updateRunResult, 1)
	go func() { result, err := f.runner.Run(f.ctx, run.Worker, run.Job); done <- updateRunResult{result, err} }()
	f.awaitTool(started, done)
	correction := f.correct(source)
	close(release)
	first := f.await(done)
	if first.err != nil || !first.result.Retryable || first.result.FailureKind != string(agent.ErrorTransport) || p.summaries != 1 || len(requests) != 2 {
		t.Fatalf("did not reach the post-compaction recovery boundary: %+v %v summaries=%d requests=%d", first.result, first.err, p.summaries, len(requests))
	}
	type checkpoint struct {
		ExecutionDeadline time.Time                `json:"execution_deadline"`
		RecoveryCount     int                      `json:"recovery_count"`
		Context           *agent.ContextCheckpoint `json:"context_checkpoint"`
	}
	readCheckpoint := func() checkpoint {
		raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
		if err != nil {
			t.Fatal(err)
		}
		var saved checkpoint
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	before := readCheckpoint()
	if before.Context == nil || before.Context.LastCompaction == nil || before.Context.LastCompaction.Status != "committed" || strings.Contains(before.Context.LastCompaction.Summary, updateCorrected) {
		t.Fatalf("fixture did not commit an omission-bearing summary: %+v", before.Context)
	}
	assertUpdateDelivered(t, requests[1], source, correction)
	last, err := f.runner.Run(f.ctx, run.Worker, run.Job)
	after := readCheckpoint()
	if err != nil || last.Status != "success" || len(requests) != 4 || p.summaries != 2 || initialTools != 1 || resumedTools != 1 {
		t.Fatalf("compacted recovery failed or repeated a completed tool: %+v %v summaries=%d requests=%d tools=%d/%d", last, err, p.summaries, len(requests), initialTools, resumedTools)
	}
	for i := 1; i < len(requests); i++ {
		assertUpdateDelivered(t, requests[i], source, correction)
		if requestSizes[i] > contextBytes || strings.Count(updateHistoryText(requests[i]), "Shared graph update:") != 1 {
			t.Errorf("request %d exceeded its byte budget or duplicated the retained correction: bytes=%d", i+1, requestSizes[i])
		}
	}
	if !before.ExecutionDeadline.Equal(after.ExecutionDeadline) || before.RecoveryCount+1 != after.RecoveryCount {
		t.Fatal("compacted recovery reset its deadline or allowance")
	}
	value, err := f.runner.handler(f.ctx, run.Job, worker.GraphRequest{RequestID: strings.Repeat("b", 32), Op: "read_snapshot", Section: "facts", IDs: []string{source, correction}, ExpectedVersion: run.Job.InputSnapshot.StateVersion})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if !strings.Contains(string(raw), updateOriginal) || strings.Contains(string(raw), updateCorrected) || !strings.Contains(string(raw), `"status":"valid"`) {
		t.Fatalf("compaction changed the original input: %s", raw)
	}
}

func TestExecuteUpdateReadCrossingDeadlineEntersConclusionBeforeProvider(t *testing.T) {
	for _, mode := range []string{"deadline", "one_shot_soft_stop"} {
		t.Run(mode, func(t *testing.T) {
			f := newUpdateRequestFixture(t)
			run, _ := f.dependentRun()
			started, release := make(chan struct{}), make(chan struct{})
			softStop := make(chan struct{}, 1)
			var now atomic.Int64
			start := time.Now()
			now.Store(start.UnixNano())
			production := f.runner.handler
			reads := 0
			f.runner.handler = func(ctx context.Context, job worker.Job, request worker.GraphRequest) (any, error) {
				if request.Op == "read_updates" {
					reads++
					if reads == 1 {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
				}
				return production(ctx, job, request)
			}
			requests, toolCalls, exposed := 0, 0, false
			f.runner.options = worker.Options{RunDir: t.TempDir(), SoftStop: softStop, Now: func() time.Time { return time.Unix(0, now.Load()) }, Tools: []agent.Tool{{Definition: agent.Definition{Name: "probe", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) { toolCalls++; return "harmless probe", nil }}}, Provider: updateProvider(func(_ context.Context, _ []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				requests++
				if len(definitions) != 0 && requests == 1 {
					exposed = true
					return updateToolCall("too-late-probe", "probe", `{}`), nil
				}
				return agent.Text("assistant", `{"accepted":true,"outcome":"incomplete","reason":"The execution reached its conclusion boundary during the dependency read"}`), nil
			})}
			done := make(chan updateRunResult, 1)
			go func() { result, err := f.runner.Run(f.ctx, run.Worker, run.Job); done <- updateRunResult{result, err} }()
			f.awaitTool(started, done)
			// Change phase only after the automatic read has begun. The one-shot
			// channel stays open: reading the signal twice would silently lose it.
			if mode == "deadline" {
				now.Store(start.Add(61 * time.Second).UnixNano())
			} else {
				softStop <- struct{}{}
			}
			close(release)
			result := f.await(done)
			if result.err != nil || !result.result.Conclude || result.result.FailureKind != "incomplete" || requests != 1 || reads != 1 || exposed || toolCalls != 0 {
				t.Fatalf("dependency read allowed exploration after conclusion was requested: %+v %v requests=%d reads=%d exposed=%t tools=%d", result.result, result.err, requests, reads, exposed, toolCalls)
			}
		})
	}
}
