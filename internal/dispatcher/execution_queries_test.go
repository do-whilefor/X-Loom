package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestDispatcherIgnoresLargeUnrelatedExecutionHistory(t *testing.T) {
	s, runner, store, graph := automaticRetryFixture(t, 1, "transport")
	// A retained terminal Job in the same namespace already exceeds the HTTP
	// client's limit. Neither scheduling nor any live graph tool needs it.
	err := store.Do(context.Background(), func(tx *board.Tx) error {
		unrelated := board.Graph{Project: board.Project{ID: "unrelated-history", Status: "stopped", Title: "Retained history", CreatedAt: tx.Now}}
		if err := tx.Save(unrelated); err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"run_id": "old", "kind": "reason", "graph": unrelated, "retained": strings.Repeat("x", 33<<20)})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO xloom_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, unrelated.Project.ID, "old", "xloom", "old", "reason", "", "old@old", raw, "old-input", "succeeded", tx.Now, tx.Now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var oldHistory []board.Execution
	err = s.Client.Do(context.Background(), "GET", "/executions?namespace=xloom", nil, &oldHistory, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds 33554432-byte limit") {
		t.Fatalf("fixture did not exceed the unchanged response limit: %v", err)
	}
	state := finishBatchFixture(t, s, graph.Project.ID)
	if state.Graph.Project.Status != "completed" || len(runner.seen) < 4 {
		t.Fatal("large unrelated history blocked the Decide/Execute graph flow")
	}
	if runner.seen[1].PreviousRunID != runner.seen[0].RunID {
		t.Fatal("history-free scheduling lost the one-use automatic retry grant")
	}
}

func TestPendingExecutionPagesDoNotFetchUnselectedJobs(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/executions/pending" || r.URL.Query().Get("namespace") != "xloom" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		page := board.ExecutionPage{Items: []board.ExecutionSummary{{ProjectID: "p", ID: "one", Kind: "reason", Status: "running"}}, NextCursor: 10}
		if r.URL.Query().Get("after") == "10" {
			page.Items[0].ID, page.Items[0].Status = "two", "result_pending"
			page.NextCursor = 0
		} else if r.URL.Query().Get("after") != "0" {
			t.Errorf("unexpected pending cursor: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer api.Close()
	s := New(config.Config{Server: api.URL, Runtime: config.Runtime{MaxWorkers: 0}}, &batchProtocolRunner{})
	if err := s.loadExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.pendingExecutions) != 2 {
		t.Fatalf("lost a pending page: %+v", s.pendingExecutions)
	}
	if err := s.recoverExecutions(context.Background(), map[string]string{"p": "active"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("recovery downloaded Jobs without an available worker slot")
	}
}

func TestPendingExecutionPagesRejectNonAdvancingCursor(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(board.ExecutionPage{NextCursor: 10})
	}))
	defer api.Close()
	s := New(config.Config{Server: api.URL}, &batchProtocolRunner{})
	if err := s.loadExecutions(context.Background()); err == nil || !strings.Contains(err.Error(), "cursor did not advance") || requests.Load() != 2 {
		t.Fatalf("pending recovery did not stop at a broken cursor: %v (%d requests)", err, requests.Load())
	}
}

func TestRecoveryCancelsInactiveExecutionFromMetadata(t *testing.T) {
	var cancelled atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/projects/stopped/executions/r/status" || r.Header.Get("X-Xloom-Run") != "backend@r" {
			t.Errorf("inactive recovery downloaded a Job or lost its lease: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body struct {
			Status string `json:"status"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil && body.Status == "cancelled" {
			cancelled.Store(true)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer api.Close()
	s := New(config.Config{Server: api.URL}, &batchProtocolRunner{})
	s.pendingExecutions = []board.ExecutionSummary{{ProjectID: "stopped", ID: "r", Namespace: "xloom", Kind: "reason", Lease: "backend@r", Status: "result_pending"}}
	if err := s.recoverExecutions(context.Background(), map[string]string{"stopped": "stopped"}); err != nil {
		t.Fatal(err)
	}
	if !cancelled.Load() {
		t.Fatal("inactive pending execution was stranded")
	}
}

func TestRecoveryFetchesOnlyTheSelectedImmutableJob(t *testing.T) {
	s, runner, _, graph := automaticRetryFixture(t, 0, "")
	ctx := context.Background()
	backend := s.Config.Workers[0]
	run := &task{Job: worker.Job{RunID: "interrupted", Kind: "reason", Graph: graph, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", EnvironmentID: s.environmentID(backend), Budget: s.Config.Task("reason")}, Worker: backend, Lease: Lease{Run: backend.Name + "@interrupted", Kind: "reason"}}
	if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/reason/claim", map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareDecision(ctx, run, "initial"); err != nil {
		t.Fatal(err)
	}
	if err := s.register(ctx, run); err != nil {
		t.Fatal(err)
	}
	// Interrupt after registration, before any Worker starts. Recovery must
	// reopen only this Job without requesting the execution history.
	original := run.Job
	s = New(s.Config, runner)
	var detailReads atomic.Int32
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Path == "/executions" {
			t.Error("recovery downloaded complete execution history")
		}
		if r.Method == "GET" && r.URL.Path == projectPath(graph.Project.ID)+"/executions/"+original.RunID {
			detailReads.Add(1)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	retryTicks(t, s, 1)
	if detailReads.Load() != 1 || len(runner.seen) != 1 || digest(runner.seen[0]) != digest(original) {
		t.Fatalf("recovery did not reopen exactly the selected immutable Job: reads=%d runs=%d", detailReads.Load(), len(runner.seen))
	}
}

func TestGraphHandlerRejectsMismatchedRegisteredIdentity(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/projects/p/executions/r/identity" || r.URL.Query().Get("namespace") != "xloom" {
			t.Errorf("unexpected graph identity request: %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(board.ExecutionSummary{ProjectID: "p", ID: "r", Namespace: "other", Kind: "reason", Lease: "backend@r"})
	}))
	defer api.Close()
	runner := &batchProtocolRunner{}
	New(config.Config{Server: api.URL}, runner)
	_, err := runner.handler(context.Background(), worker.Job{RunID: "r", Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "p"}}}, worker.GraphRequest{Op: "decision_receipt"})
	if err == nil || requests.Load() != 1 {
		t.Fatal("graph operation accepted an identity from another namespace")
	}
}

func TestGraphHandlerRejectsIdentityFromAnotherGeneration(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(board.ExecutionSummary{ProjectID: "p", ID: "r", Namespace: "xloom", Kind: "reason", Generation: 2, Lease: "backend@r"})
	}))
	defer api.Close()
	runner := &batchProtocolRunner{}
	New(config.Config{Server: api.URL}, runner)
	_, err := runner.handler(context.Background(), worker.Job{RunID: "r", Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "p", Generation: 1}}}, worker.GraphRequest{Op: "read_graph"})
	if err == nil || requests.Load() != 1 {
		t.Fatal("graph operation accepted a different registered input generation")
	}
}

func TestLiveGraphReadPagesBeforeDispatcherResponseLimit(t *testing.T) {
	s, runner, store, graph := automaticRetryFixture(t, 0, "")
	// Exercise response byte boundaries, not throughput: race instrumentation
	// and parallel package tests can make this synthetic 33 MiB input slow.
	if s.Client.HTTP == nil {
		s.Client.HTTP = &http.Client{}
	}
	s.Client.HTTP.Timeout = 60 * time.Second
	now := time.Now().UTC()
	store.Now = func() time.Time { return now } // Keep the read lease live while encoding large pages.
	ctx := context.Background()
	lease := Lease{Run: "retry-fixture@large-graph", Kind: "reason"}
	if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	job := worker.Job{RunID: "large-graph", Kind: "reason", Graph: graph, GraphRPC: true, Workspace: "/workspace"}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	execution := board.Execution{ProjectID: graph.Project.ID, ID: job.RunID, Namespace: "xloom", Backend: "retry-fixture", Kind: "reason", Lease: lease.Run, Job: raw, RetryKey: "reason:large-graph"}
	if err = s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/executions", execution, nil, &lease); err != nil {
		t.Fatal(err)
	}
	// Existing executions must still read small graph/evidence pages after the
	// live FGS grows beyond the whole-response limit. The immutable Job is small.
	evidence := board.EvidenceRef{RunID: "source", Path: "retained/large.txt", Excerpt: strings.Repeat("x", 33<<20)}
	err = store.Do(ctx, func(tx *board.Tx) error {
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		g.Facts = append(g.Facts, board.Fact{ID: "large", Description: "Large retained evidence"})
		if err = tx.Save(g); err != nil {
			return err
		}
		data, err := json.Marshal(map[string]any{"facts": []board.FactRecord{{ID: "large", Description: "Large retained evidence", Status: "valid", Evidence: []board.EvidenceRef{evidence}}}})
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,1,1) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=1,decision_revision=1", graph.Project.ID, string(data))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var whole board.State
	if err = s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/state", nil, &whole, &lease); err == nil || !strings.Contains(err.Error(), "exceeds 33554432-byte limit") {
		t.Fatalf("current-state fixture did not reach the response limit: %v", err)
	}
	read := worker.GraphRequest{RequestID: "00000000000000000000000000000001", Op: "read_graph", Section: "evidence", IDs: []string{"large"}, Limit: 1}
	result, err := runner.handler(ctx, job, read)
	if err != nil {
		t.Fatal(err)
	}
	pageBytes, err := json.Marshal(result)
	if err != nil || len(pageBytes) >= worker.MaxGraphRPCBytes {
		t.Fatalf("unbounded graph response: %d bytes, %v", len(pageBytes), err)
	}
	var page struct {
		StateVersion string `json:"state_version"`
		Items        []struct {
			RecordOmitted bool   `json:"record_omitted"`
			RecordBytes   int    `json:"record_bytes"`
			RecordVersion string `json:"record_version"`
		} `json:"items"`
	}
	if err = json.Unmarshal(pageBytes, &page); err != nil || len(page.Items) != 1 || !page.Items[0].RecordOmitted || page.Items[0].RecordBytes <= 32<<20 {
		t.Fatalf("oversized evidence has no continuation: %s, %v", pageBytes, err)
	}
	// Reach the end directly without transferring all prior evidence bytes.
	tail := page.Items[0].RecordBytes - 32
	read.ExpectedVersion, read.ByteOffset = page.StateVersion, &tail
	read.RecordVersion = page.Items[0].RecordVersion
	result, err = runner.handler(ctx, job, read)
	if err != nil {
		t.Fatal(err)
	}
	pageBytes, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fragment struct {
		Content        string `json:"content"`
		NextByteOffset *int   `json:"next_byte_offset"`
	}
	if err = json.Unmarshal(pageBytes, &fragment); err != nil || len(fragment.Content) != 32 || fragment.NextByteOffset != nil || !strings.HasSuffix(fragment.Content, `"}`) {
		t.Fatalf("large evidence tail cannot be read: %s, %v", pageBytes, err)
	}
}

func TestCommittedDecisionFinishGraceHonorsProjectFence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		generation int64
		expired    bool
		allowed    bool
	}{
		{"active", "active", 3, false, true},
		{"completed", "completed", 3, false, true},
		{"stopped", "stopped", 3, false, false},
		{"terminated", "terminated", 3, false, false},
		{"restarted", "active", 4, false, false},
		{"expired", "completed", 3, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/receipt") {
					_ = json.NewEncoder(w).Encode(board.DecisionReceipt{Committed: true})
					return
				}
				_ = json.NewEncoder(w).Encode(board.Graph{Project: board.Project{ID: "p", Status: tc.status, Generation: tc.generation}})
			}))
			defer api.Close()
			s := New(config.Config{Server: api.URL}, &batchProtocolRunner{})
			run := &task{Job: worker.Job{Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "p", Generation: 3}}, Decision: &board.DecisionContext{Version: 2}}}
			if tc.expired {
				run.committedAt.Store(time.Now().Add(-decisionFinishGrace).UnixNano())
			}
			if got := s.decisionFinishAllowed(context.Background(), run); got != tc.allowed {
				t.Fatalf("finish allowed = %v, want %v", got, tc.allowed)
			}
		})
	}
}

type finishingDecisionRunner struct {
	batchProtocolRunner
	committed chan worker.Job
	finish    chan struct{}
	cancelled atomic.Bool
	managed   string
	cleanups  atomic.Int32
}

func (r *finishingDecisionRunner) Projects(context.Context) ([]string, error) {
	if r.managed == "" {
		return nil, nil
	}
	return []string{r.managed}, nil
}

func (r *finishingDecisionRunner) Cleanup(context.Context, string, string) error {
	r.cleanups.Add(1)
	return nil
}

func (r *finishingDecisionRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	result, err := r.batchProtocolRunner.Run(ctx, backend, job)
	if err != nil || job.Kind != "reason" || len(job.Graph.Intents) == 0 || job.Graph.OpenCount() != 0 {
		return result, err
	}
	r.committed <- job
	select {
	case <-ctx.Done():
		r.cancelled.Store(true)
		return worker.Result{}, ctx.Err()
	case <-r.finish:
		result.Metrics = &worker.DecisionMetrics{Version: 1, ModelCalls: 1, CompletedCalls: 1, Committed: true, CommittedActions: 1, Outcome: "success"}
		return result, nil
	}
}

func TestCompletedDecisionReturnsObservationBeforeCancellation(t *testing.T) {
	s, _, transport, graph := batchSchedulerFixture(t, 1)
	runner := &finishingDecisionRunner{batchProtocolRunner: batchProtocolRunner{directions: 1}, committed: make(chan worker.Job, 1), finish: make(chan struct{}), managed: graph.Project.ID}
	s.Runner = runner
	s.configureGraphHandler()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer func() { cancel(); s.wg.Wait() }()
	for range 2 { // Publish one direction, then finish its evidence gathering.
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
		s.wg.Wait()
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var job worker.Job
	select {
	case job = <-runner.committed:
	case <-ctx.Done():
		t.Fatal("final decision did not commit")
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	// Both the completed-project observation above and the next heartbeat used
	// to cancel this process before its final metrics could be delivered.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if runner.cancelled.Load() {
		t.Fatal("authoritative completion cancelled its own result delivery")
	}
	if runner.cleanups.Load() != 0 {
		t.Fatal("container cleanup interrupted the committed planner's delivery grace")
	}
	close(runner.finish)
	s.wg.Wait()
	var execution board.Execution
	if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/executions/"+job.RunID+"?namespace=xloom", nil, &execution, nil); err != nil {
		t.Fatal(err)
	}
	var result worker.Result
	if err := json.Unmarshal(execution.Result, &result); err != nil {
		t.Fatal(err)
	}
	if execution.Status != "succeeded" || result.Metrics == nil || result.Metrics.ModelCalls != 1 {
		t.Fatalf("committed execution lost its observation: status=%s metrics=%+v", execution.Status, result.Metrics)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	if runner.cleanups.Load() != 1 {
		t.Fatal("completed project was not cleaned after the planner finished")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.lateWrites != 0 {
		t.Fatal("observation delivery retried the business result")
	}
}

type executionQueryTransport func(*http.Request) (*http.Response, error)

func (f executionQueryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestObservationFailureNeverRetriesCommittedBusinessResult(t *testing.T) {
	s, _, transport, graph := batchSchedulerFixture(t, 1)
	runner := &finishingDecisionRunner{batchProtocolRunner: batchProtocolRunner{directions: 1}, committed: make(chan worker.Job, 1), finish: make(chan struct{})}
	close(runner.finish)
	s.Runner = runner
	s.configureGraphHandler()
	var observations atomic.Int32
	s.Client.HTTP.Transport = executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/observation") {
			observations.Add(1)
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic metrics delivery failure"))}, nil
		}
		return transport.RoundTrip(r)
	})
	finishBatchFixture(t, s, graph.Project.ID)
	assertBatchExecutionReceipts(t, s, transport, graph.Project.ID)
	if observations.Load() != 1 {
		t.Fatalf("observation failure retried the execution: %d observation attempts", observations.Load())
	}
}
