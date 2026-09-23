//go:build linux

package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/provider"
	"xloom/internal/server"
	"xloom/internal/worker"
)

// This is a synthetic presentation ablation. It uses the supported inline v2
// registration contract, production prompts, Worker, graph bridge, Dispatcher,
// Server and SQLite. It does not measure the production initial-view selector.
type p3Limits struct {
	DeadlineSeconds int    `json:"deadline_seconds"`
	MaxCalls        int    `json:"max_logical_calls_including_summary"`
	MaxTokens       int    `json:"max_output_tokens_per_call"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func defaultP3Limits() p3Limits {
	return p3Limits{DeadlineSeconds: 180, MaxCalls: 8, MaxTokens: 8192, ReasoningEffort: "max"}
}

type p3GraphCall struct {
	Request  worker.GraphRequest `json:"request"`
	Response json.RawMessage     `json:"response,omitempty"`
	Error    string              `json:"error,omitempty"`
}

type p3ProviderCall struct {
	Kind       string             `json:"kind"`
	Messages   []agent.Message    `json:"messages"`
	Tools      []agent.Definition `json:"tools"`
	InputBytes int                `json:"input_bytes"`
	InputBasis string             `json:"input_basis"`
	MaxTokens  int                `json:"max_output_tokens"`
	Response   agent.Message      `json:"response"`
	Error      string             `json:"error,omitempty"`
	DurationMS int64              `json:"duration_ms"`
}

type p3Exposure struct {
	IDVisible       bool `json:"id_visible"`
	BodyVisible     bool `json:"complete_description_visible"`
	EvidenceVisible bool `json:"all_evidence_excerpts_visible"`
	BodyFetched     bool `json:"complete_description_fetched"`
	EvidenceFetched bool `json:"all_evidence_excerpts_fetched"`
	BodyRead        bool `json:"complete_description_delivered_in_tool_result"`
	EvidenceRead    bool `json:"all_evidence_excerpts_delivered_in_tool_result"`
}

type p3Measures struct {
	Facts               map[string]p3Exposure `json:"facts"`
	Completed           bool                  `json:"completed"`
	WantComplete        bool                  `json:"want_complete"`
	CompletionMatches   *bool                 `json:"completion_state_matches_expected"`
	Combined            bool                  `json:"combined_completion_proxy"`
	CombinedBasis       string                `json:"combined_basis"`
	CompletionSources   []string              `json:"completion_sources"`
	CompletionProof     string                `json:"completion_proof"`
	NewSteps            []board.Step          `json:"new_investigation_steps"`
	RepeatAssessment    string                `json:"repeat_assessment"`
	DuplicateGraphReads int                   `json:"duplicate_same_version_graph_reads"`
	InputBytes          int64                 `json:"total_logical_input_bytes"`
	Usage               agent.Usage           `json:"reported_usage"`
	UsageStatus         string                `json:"usage_status"`
	CallLimitReached    bool                  `json:"logical_call_limit_reached"`
	Assessment          string                `json:"assessment"`
}

type p3HTTPAttempt struct {
	Status     int   `json:"status"`
	DurationMS int64 `json:"duration_ms"`
	Failed     bool  `json:"failed"`
}

type p3CaseReport struct {
	Version          int              `json:"version"`
	Case             string           `json:"case"`
	Protocol         string           `json:"protocol"`
	Presentation     string           `json:"presentation"`
	Model            string           `json:"model,omitempty"`
	Endpoint         string           `json:"endpoint,omitempty"`
	Limits           p3Limits         `json:"limits"`
	StartedAt        time.Time        `json:"started_at"`
	DurationMS       int64            `json:"duration_ms"`
	InitialState     board.State      `json:"initial_state"`
	InitialView      json.RawMessage  `json:"initial_view"`
	InputGraphSHA256 string           `json:"input_graph_sha256_project_identity_normalized"`
	Requests         []p3ProviderCall `json:"requests"`
	GraphCalls       []p3GraphCall    `json:"graph_calls"`
	HTTPAttempts     []p3HTTPAttempt  `json:"provider_http_attempts,omitempty"`
	Result           worker.Result    `json:"worker_result"`
	RunError         string           `json:"run_error,omitempty"`
	FinalState       board.State      `json:"final_state"`
	Measures         p3Measures       `json:"measures"`
	Qualification    string           `json:"qualification"`
}

type p3EvaluationFixture struct {
	Spec       p3Case
	Store      *board.Store
	Scheduler  *Scheduler
	Runner     *updateLocalRunner
	Task       *task
	Initial    board.State
	GraphCalls []p3GraphCall
	ctx        context.Context
	runDir     string
	workspace  string
}

func newP3EvaluationFixture(t *testing.T, spec p3Case, ctx context.Context) *p3EvaluationFixture {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "p3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Now = func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }
	api := httptest.NewServer(server.New(store))
	t.Cleanup(api.Close)
	runner := &updateLocalRunner{}
	scheduler := New(config.Config{Server: api.URL, Runtime: config.Runtime{MaxWorkers: 1, Interval: 1, HealthMode: "disabled"}}, runner)
	f := &p3EvaluationFixture{Spec: spec, Store: store, Scheduler: scheduler, Runner: runner, ctx: ctx, runDir: t.TempDir(), workspace: t.TempDir()}
	var graph board.Graph
	if err := scheduler.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Synthetic retained observations", "origin": spec.Origin, "goal": spec.Goal, "bootstrap_enabled": false}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	// Synthetic observations are explicit fixture data, not artifacts obtained
	// from a real business system. Their evidence is retained verbatim in FGS.
	err = store.Do(ctx, func(tx *board.Tx) error {
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		facts := append([]board.FactRecord{}, spec.Facts...)
		steps := []board.Step{}
		for i := range facts {
			fact := &facts[i]
			if fact.ID == "" || fact.ID == "origin" || fact.ID == "goal" {
				return errors.New("P3 fixture needs distinct non-input fact IDs")
			}
			if fact.Status == "" {
				fact.Status = "valid"
			}
			if fact.Scope == "" {
				fact.Scope = "Synthetic P3 fixture; no external business validation"
			}
			if fact.ObservedAt == "" {
				fact.ObservedAt = tx.Now
			}
			if fact.RunID == "" {
				fact.RunID = fmt.Sprintf("p3-source-%d", i)
			}
			if fact.SourceStepID == "" {
				fact.SourceStepID = fmt.Sprintf("p3-step-%d", i)
			}
			if len(fact.Evidence) == 0 {
				fact.Evidence = []board.EvidenceRef{{RunID: fact.RunID, Path: "retained/synthetic-observation.txt", Excerpt: fact.Description}}
			}
			for _, ref := range fact.Evidence {
				if filepath.IsAbs(ref.Path) || strings.Contains(ref.Path, "..") || strings.ContainsAny(ref.RunID, "/\\") {
					return errors.New("P3 retained fixture path must remain within its seed directory")
				}
				path := filepath.Join(f.workspace, "seeded-evidence", ref.RunID, filepath.FromSlash(ref.Path))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(ref.Excerpt), 0600); err != nil {
					return err
				}
			}
			g.Facts = append(g.Facts, board.Fact{ID: fact.ID, Description: fact.Description})
			g.Intents = append(g.Intents, board.Intent{ID: fact.SourceStepID, From: []string{"origin"}, To: board.Ptr(fact.ID), Description: "Completed independent observation", Creator: fact.RunID, CreatedAt: tx.Now, ConcludedAt: board.Ptr(tx.Now)})
			steps = append(steps, board.Step{ID: fact.SourceStepID, From: []string{"origin"}, GoalID: "goal", Description: "Completed independent observation", Status: "completed", Result: board.Ptr(fact.ID), CreatedAt: tx.Now})
		}
		if err = tx.Save(g); err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"facts": facts, "steps": steps})
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,1,1)", g.Project.ID, string(raw))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{Run: "p3@planner-run", Kind: "reason"}
	base := projectPath(graph.Project.ID)
	if err := scheduler.Client.Do(ctx, "POST", base+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Client.Do(ctx, "GET", base+"/state", nil, &f.Initial, nil); err != nil {
		t.Fatal(err)
	}
	decision, err := p3InitialView(f.Initial, spec.Presentation)
	if err != nil {
		t.Fatal(err)
	}
	job := worker.Job{RunID: "planner-run", Kind: "reason", WorkerType: "go", GraphRPC: true, ResultContractVersion: 2,
		Graph: f.Initial.Graph, State: &f.Initial, Decision: decision, DecisionRevision: f.Initial.DecisionRevision, DecisionTrigger: "initial", Workspace: f.workspace,
		Budget: config.Task{Timeout: defaultP3Limits().DeadlineSeconds, MaxIntents: 3}}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	e := board.Execution{ProjectID: graph.Project.ID, ID: job.RunID, Namespace: "xloom", Backend: "p3", Kind: "reason", Lease: lease.Run, Job: raw, RetryKey: board.DecisionRetryKey(f.Initial.Graph, f.Initial.DecisionRevision)}
	if err := scheduler.Client.Do(ctx, "POST", base+"/executions", e, &e, &lease); err != nil {
		t.Fatal(err)
	}
	f.Task = &task{Job: job, Lease: lease, Execution: e, Worker: config.Worker{Name: "p3", Type: "go"}}
	if err := scheduler.Client.Do(ctx, "POST", executionPath(f.Task)+"/status", map[string]string{"status": "running"}, nil, &lease); err != nil {
		t.Fatal(err)
	}
	forward := runner.handler
	runner.handler = func(ctx context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
		result, err := forward(ctx, j, request)
		call := p3GraphCall{Request: request}
		if err != nil {
			call.Error = err.Error()
		} else {
			call.Response, err = json.Marshal(result)
		}
		f.GraphCalls = append(f.GraphCalls, call)
		return result, err
	}
	return f
}

type p3RecordedProvider struct {
	base     agent.Provider
	limits   p3Limits
	requests []p3ProviderCall
	reached  bool
}

type p3SizedProvider struct {
	*p3RecordedProvider
	sizer agent.RequestSizer
}

func (p *p3SizedProvider) InputBytes(m []agent.Message, d []agent.Definition) (int, error) {
	return p.sizer.InputBytes(m, d)
}

func (p *p3RecordedProvider) Generate(ctx context.Context, m []agent.Message, d []agent.Definition, emit agent.Emit) (agent.Message, error) {
	return p.generate(ctx, m, d, emit, 0)
}

func (p *p3RecordedProvider) GenerateSummary(ctx context.Context, m []agent.Message, tokens int, emit agent.Emit) (agent.Message, error) {
	return p.generate(ctx, m, nil, emit, min(tokens, p.limits.MaxTokens))
}

func (p *p3RecordedProvider) generate(ctx context.Context, m []agent.Message, d []agent.Definition, emit agent.Emit, summaryTokens int) (agent.Message, error) {
	if len(p.requests) >= p.limits.MaxCalls {
		p.reached = true
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorBudget, Err: errors.New("P3 logical model call limit reached")}
	}
	call := p3ProviderCall{Kind: "turn", MaxTokens: p.limits.MaxTokens}
	if summaryTokens > 0 {
		call.Kind, call.MaxTokens = "summary", summaryTokens
	}
	raw, _ := json.Marshal(m)
	_ = json.Unmarshal(raw, &call.Messages)
	raw, _ = json.Marshal(d)
	_ = json.Unmarshal(raw, &call.Tools)
	if sizer, ok := p.base.(agent.RequestSizer); ok {
		call.InputBytes, _ = sizer.InputBytes(m, d)
		call.InputBasis = "provider_request"
	} else {
		raw, _ = json.Marshal(struct {
			Messages []agent.Message
			Tools    []agent.Definition
		}{agent.WireHistory(m), d})
		call.InputBytes, call.InputBasis = len(raw), "wire_history_and_tools"
	}
	index := len(p.requests)
	p.requests = append(p.requests, call)
	started := time.Now()
	var result agent.Message
	var err error
	if summaryTokens > 0 {
		if summary, ok := p.base.(agent.SummaryProvider); ok {
			result, err = summary.GenerateSummary(ctx, m, summaryTokens, emit)
		} else {
			result, err = p.base.Generate(ctx, m, nil, emit)
		}
	} else {
		result, err = p.base.Generate(ctx, m, d, emit)
	}
	p.requests[index].Response, p.requests[index].DurationMS = result, time.Since(started).Milliseconds()
	if err != nil {
		p.requests[index].Error = err.Error()
	}
	return result, err
}

func (f *p3EvaluationFixture) RunProvider(t *testing.T, base agent.Provider, limits p3Limits) p3CaseReport {
	t.Helper()
	recorded := &p3RecordedProvider{base: base, limits: limits}
	var p agent.Provider = recorded
	if sizer, ok := base.(agent.RequestSizer); ok {
		p = &p3SizedProvider{p3RecordedProvider: recorded, sizer: sizer}
	}
	f.Runner.options = worker.Options{Provider: p, RunDir: f.runDir, ContextBytes: worker.DefaultContextBytes, ContextTokens: worker.DefaultContextTokens, ContextTargetTokens: worker.DefaultContextTargetTokens}
	report := p3CaseReport{Version: 1, Case: f.Spec.Name, Presentation: f.Spec.Presentation, Protocol: "synthetic_presentation_ablation_inline_v2", Limits: limits, StartedAt: time.Now().UTC(), InitialState: f.Initial, InitialView: f.Task.Job.Decision.View,
		Qualification: "Synthetic seeded evidence and controlled presentation; production prompts/read/commit path, not production selector quality. Combined is a completion-support/answer proxy. New steps require manual review before labeling repeated investigation. Reported usage may exclude internal HTTP retries; no rate or cost estimate is implied."}
	normalized := f.Initial
	normalized.Graph.Project.ID, normalized.Graph.Project.Reason = "synthetic-project", nil
	graphBytes, _ := json.Marshal(normalized)
	graphDigest := sha256.Sum256(graphBytes)
	report.InputGraphSHA256 = hex.EncodeToString(graphDigest[:])
	ctx, cancel := context.WithTimeout(f.ctx, time.Duration(limits.DeadlineSeconds)*time.Second)
	defer cancel()
	result, err := f.Runner.Run(ctx, f.Task.Worker, f.Task.Job)
	report.Result, report.DurationMS = result, time.Since(report.StartedAt).Milliseconds()
	if err != nil {
		report.RunError = err.Error()
	}
	report.Requests, report.GraphCalls = recorded.requests, f.GraphCalls
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	if err := f.Scheduler.Client.Do(readCtx, "GET", projectPath(f.Task.Job.Graph.Project.ID)+"/state", nil, &report.FinalState, nil); err != nil {
		t.Fatal(err)
	}
	report.Measures = p3Measure(f.Spec, report)
	report.Measures.CallLimitReached = recorded.reached
	return report
}

func p3ContainsValue(text, value string) bool {
	if value == "" {
		return false
	}
	encoded, _ := json.Marshal(value)
	return strings.Contains(text, value) || strings.Contains(text, string(encoded[1:len(encoded)-1]))
}

func p3Measure(spec p3Case, report p3CaseReport) p3Measures {
	m := p3Measures{Facts: map[string]p3Exposure{}, WantComplete: spec.WantComplete, UsageStatus: "unknown", RepeatAssessment: "Inspect new step descriptions and supports; new steps are not automatically repeated investigation.", CombinedBasis: "Completed graph intent must cite all required fact IDs and contain the expected answer when configured."}
	first := ""
	if len(report.Requests) > 0 {
		for _, message := range report.Requests[0].Messages {
			first += message.Text() + "\n"
		}
	}
	facts := map[string]board.FactRecord{}
	for _, fact := range report.InitialState.FactRecords {
		facts[fact.ID] = fact
	}
	for _, seeded := range spec.Facts {
		id := seeded.ID
		fact := facts[id]
		exposure := p3Exposure{IDVisible: p3ContainsValue(first, id), BodyVisible: p3ContainsValue(first, fact.Description), EvidenceVisible: len(fact.Evidence) > 0}
		for _, evidence := range fact.Evidence {
			exposure.EvidenceVisible = exposure.EvidenceVisible && p3ContainsValue(first, evidence.Excerpt)
		}
		m.Facts[id] = exposure
	}
	seenReads := map[string]bool{}
	for group, calls := range [][]p3GraphCall{report.GraphCalls, p3DeliveredReads(report.Requests)} {
		readEvidence := map[string]map[string]bool{}
		for _, call := range calls {
			if call.Request.Op != "read_graph" || call.Error != "" {
				continue
			}
			var page struct {
				StateVersion string            `json:"state_version"`
				Items        []json.RawMessage `json:"items"`
			}
			if json.Unmarshal(call.Response, &page) != nil {
				continue
			}
			keyRequest := call.Request
			keyRequest.RequestID = ""
			key, _ := json.Marshal(struct {
				Request worker.GraphRequest
				Version string
			}{keyRequest, page.StateVersion})
			if group == 0 && seenReads[string(key)] {
				m.DuplicateGraphReads++
			}
			seenReads[string(key)] = true
			for _, item := range page.Items {
				if call.Request.Section == "facts" {
					var got board.FactRecord
					if json.Unmarshal(item, &got) != nil || got.ID == "" {
						continue
					}
					exposure, tracked := m.Facts[got.ID]
					if !tracked {
						continue
					}
					if group == 0 {
						exposure.BodyFetched = exposure.BodyFetched || got.Description == facts[got.ID].Description
					} else {
						exposure.BodyRead = exposure.BodyRead || got.Description == facts[got.ID].Description
					}
					m.Facts[got.ID] = exposure
					if readEvidence[got.ID] == nil {
						readEvidence[got.ID] = map[string]bool{}
					}
					for _, ref := range got.Evidence {
						readEvidence[got.ID][ref.Excerpt] = true
					}
				} else if call.Request.Section == "evidence" && len(call.Request.IDs) == 1 {
					id := call.Request.IDs[0]
					var ref board.EvidenceRef
					if json.Unmarshal(item, &ref) == nil && ref.Excerpt != "" {
						if readEvidence[id] == nil {
							readEvidence[id] = map[string]bool{}
						}
						readEvidence[id][ref.Excerpt] = true
					}
				}
			}
		}
		for id, exposure := range m.Facts {
			complete := len(facts[id].Evidence) > 0
			for _, ref := range facts[id].Evidence {
				complete = complete && readEvidence[id][ref.Excerpt]
			}
			if group == 0 {
				exposure.EvidenceFetched = complete
			} else {
				exposure.EvidenceRead = complete
			}
			m.Facts[id] = exposure
		}
	}
	initialSteps := map[string]bool{}
	for _, step := range report.InitialState.Steps {
		initialSteps[step.ID] = true
	}
	for _, step := range report.FinalState.Steps {
		if !initialSteps[step.ID] && board.Value(step.Result) != "goal" {
			m.NewSteps = append(m.NewSteps, step)
		}
	}
	m.Completed = report.FinalState.Graph.Project.Status == "completed"
	if m.Completed || (report.RunError == "" && report.Result.Status == "success" && len(report.Requests) > 0) {
		matches := m.Completed == spec.WantComplete
		m.CompletionMatches = &matches
	}
	for _, intent := range report.FinalState.Graph.Intents {
		if board.Value(intent.To) != "goal" {
			continue
		}
		m.CompletionSources, m.CompletionProof = intent.From, intent.Description
		sources := map[string]bool{}
		for _, id := range intent.From {
			sources[id] = true
		}
		combined := len(spec.RequiredIDs) > 0
		for _, id := range spec.RequiredIDs {
			combined = combined && sources[id]
		}
		m.Combined = m.Completed && combined && (spec.ExpectedAnswer == "" || strings.Contains(intent.Description, spec.ExpectedAnswer))
	}
	usageCalls := 0
	for _, call := range report.Requests {
		m.InputBytes += int64(call.InputBytes)
		if usage := call.Response.Usage; usage != nil {
			usageCalls++
			m.Usage.InputTokens += usage.InputTokens
			m.Usage.OutputTokens += usage.OutputTokens
			m.Usage.CacheReadTokens += usage.CacheReadTokens
			m.Usage.CacheWriteTokens += usage.CacheWriteTokens
		}
	}
	if usageCalls > 0 {
		m.UsageStatus = "partial"
		if usageCalls == len(report.Requests) {
			m.UsageStatus = "reported_only"
		}
	}
	switch {
	case m.Completed && !spec.WantComplete:
		m.Assessment = "unexpected_completion_requires_review"
	case m.Combined:
		m.Assessment = "completed_with_required_support_requires_review"
	case m.Completed:
		m.Assessment = "completion_requires_review"
	case report.RunError != "" || report.Result.Status != "success" || len(report.Requests) == 0:
		m.Assessment = "inconclusive_run_failure"
	case len(m.NewSteps) > 0:
		m.Assessment = "followup_proposed_needs_review"
	default:
		m.Assessment = "not_completed_needs_review"
	}
	return m
}

// A successful bridge read is only fetched. It counts as delivered once its
// successful paired tool_result enters a later normal Provider request. Reads
// in a batch that commits immediately can be fetched without ever being read.
func p3DeliveredReads(requests []p3ProviderCall) []p3GraphCall {
	var delivered []p3GraphCall
	for _, request := range requests {
		if request.Kind != "turn" {
			continue
		}
		uses := map[string]worker.GraphRequest{}
		for _, message := range request.Messages {
			for _, block := range message.Content {
				if message.Role == "assistant" && block.Type == "tool_use" && block.Name == "read_graph" {
					var read worker.GraphRequest
					if json.Unmarshal(block.Input, &read) == nil {
						read.Op = "read_graph"
						uses[block.ID] = read
					}
				}
				if block.Type != "tool_result" || block.IsError {
					continue
				}
				read, ok := uses[block.ToolUseID]
				if !ok {
					continue
				}
				var text string
				if json.Unmarshal(block.Content, &text) == nil {
					delivered = append(delivered, p3GraphCall{Request: read, Response: json.RawMessage(text)})
				}
			}
		}
	}
	return delivered
}

type p3AttemptTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	attempts []p3HTTPAttempt
}

func (p *p3AttemptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now()
	response, err := p.base.RoundTrip(request)
	attempt := p3HTTPAttempt{DurationMS: time.Since(started).Milliseconds(), Failed: err != nil}
	if response != nil {
		attempt.Status = response.StatusCode
	}
	p.mu.Lock()
	p.attempts = append(p.attempts, attempt)
	p.mu.Unlock()
	return response, err
}

func p3EnvInt(t *testing.T, key string, fallback, minValue, maxValue int) int {
	t.Helper()
	if raw := os.Getenv(key); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < minValue || n > maxValue {
			t.Fatalf("%s must be between %d and %d", key, minValue, maxValue)
		}
		return n
	}
	return fallback
}

func TestP3LiveEvaluation(t *testing.T) {
	if os.Getenv("XLOOM_P3_LIVE") != "1" {
		t.Skip("set XLOOM_P3_LIVE=1 to run opt-in real-model evaluation")
	}
	token := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if token == "" {
		t.Fatal("ANTHROPIC_AUTH_TOKEN is required for live P3 evaluation")
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	if model == "" {
		model = "step-5-preview"
	}
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		t.Fatal("ANTHROPIC_BASE_URL must be explicit for live P3 evaluation")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatal("invalid ANTHROPIC_BASE_URL")
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	limits := defaultP3Limits()
	limits.DeadlineSeconds = p3EnvInt(t, "XLOOM_P3_DEADLINE", limits.DeadlineSeconds, 10, 180)
	limits.MaxCalls = p3EnvInt(t, "XLOOM_P3_MAX_CALLS", limits.MaxCalls, 1, 16)
	limits.MaxTokens = p3EnvInt(t, "XLOOM_P3_MAX_TOKENS", limits.MaxTokens, 256, 32768)
	if effort := os.Getenv("XLOOM_P3_EFFORT"); effort != "" {
		if effort != "low" && effort != "high" && effort != "max" {
			t.Fatal("XLOOM_P3_EFFORT must be low, high or max")
		}
		limits.ReasoningEffort = effort
	}
	output := os.Getenv("XLOOM_P3_OUTPUT")
	if output == "" {
		output = filepath.Join("..", "..", ".xloom", "p3")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range p3Cases() {
		t.Run(spec.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(limits.DeadlineSeconds+10)*time.Second)
			defer cancel()
			fixture := newP3EvaluationFixture(t, spec, ctx)
			transport := &p3AttemptTransport{base: http.DefaultTransport}
			live := &provider.Anthropic{BaseURL: baseURL, Token: token, Model: model, MaxTokens: limits.MaxTokens, ReasoningEffort: limits.ReasoningEffort, Timeout: time.Duration(limits.DeadlineSeconds) * time.Second, SessionID: fixture.Task.Job.RunID + "-" + fixture.Task.Job.Graph.Project.ID, Client: &http.Client{Transport: transport}}
			report := fixture.RunProvider(t, live, limits)
			report.Model, report.Endpoint = model, parsed.String()
			transport.mu.Lock()
			report.HTTPAttempts = append([]p3HTTPAttempt{}, transport.attempts...)
			transport.mu.Unlock()
			dir := filepath.Join(output, strings.NewReplacer("/", "-", "\\", "-", "..", "-").Replace(spec.Name)+"-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			raw, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"events.jsonl", "session.json"} {
				if raw, err := os.ReadFile(filepath.Join(fixture.runDir, name)); err == nil {
					if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.CopyFS(filepath.Join(dir, "seeded-evidence"), os.DirFS(filepath.Join(fixture.workspace, "seeded-evidence"))); err != nil {
				t.Fatal(err)
			}
			t.Logf("P3 report: %s; calls=%d completed=%v combined_proxy=%v model_error=%v", filepath.Join(dir, "report.json"), len(report.Requests), report.Measures.Completed, report.Measures.Combined, report.RunError != "")
		})
	}
}

func TestP3EvaluationHarnessUsesRealReadAndCommit(t *testing.T) {
	cases := p3Cases()
	if len(cases) == 0 {
		t.Fatal("P3 requires fixed cases")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newP3EvaluationFixture(t, cases[0], ctx)
	turn := 0
	scripted := updateProvider(func(_ context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turn++
		if turn == 1 {
			raw, _ := json.Marshal(map[string]any{"section": "facts", "ids": f.Spec.RequiredIDs, "limit": 50})
			return updateToolCall("read", "read_graph", string(raw)), nil
		}
		if turn == 2 {
			raw, _ := json.Marshal(map[string]any{"op": "complete", "idempotency_key": "complete", "payload": map[string]any{"from": f.Spec.RequiredIDs, "description": "Synthetic supported completion: " + f.Spec.ExpectedAnswer}})
			return updateToolCall("draft", "graph_action", string(raw)), nil
		}
		if turn == 3 {
			return updateToolCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`), nil
		}
		return agent.Message{}, errors.New("unexpected model call after authoritative commit")
	})
	report := f.RunProvider(t, scripted, defaultP3Limits())
	if report.RunError != "" || !report.Measures.Completed || !report.Measures.Combined || len(report.Requests) != 3 || report.Measures.UsageStatus != "unknown" {
		t.Fatalf("real protocol harness failed: completed=%v combined=%v calls=%d error=%s", report.Measures.Completed, report.Measures.Combined, len(report.Requests), report.RunError)
	}
	for _, id := range f.Spec.RequiredIDs {
		if !report.Measures.Facts[id].BodyRead || !report.Measures.Facts[id].EvidenceRead {
			t.Fatalf("successful graph evidence read was not measured for %s", id)
		}
	}
}

type p3SizedScript struct{ updateProvider }

func (p p3SizedScript) InputBytes([]agent.Message, []agent.Definition) (int, error) { return 317, nil }

func TestP3EvaluationCallLimitIncludesSummaryAndPreservesSizer(t *testing.T) {
	called := 0
	base := p3SizedScript{updateProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		called++
		return agent.Text("assistant", "bounded response"), nil
	})}
	recorded := &p3RecordedProvider{base: base, limits: p3Limits{MaxCalls: 2, MaxTokens: 512}}
	p := &p3SizedProvider{p3RecordedProvider: recorded, sizer: base}
	if n, err := p.InputBytes(nil, nil); err != nil || n != 317 {
		t.Fatal("wrapper changed provider request sizing")
	}
	if _, err := p.Generate(context.Background(), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GenerateSummary(context.Background(), nil, 4096, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Generate(context.Background(), nil, nil, nil); err == nil || called != 2 || !recorded.reached {
		t.Fatal("summary bypassed total logical call limit")
	}
	if len(recorded.requests) != 2 || recorded.requests[1].Kind != "summary" || recorded.requests[1].MaxTokens != 512 || !reflect.DeepEqual(recorded.requests[0].Response, agent.Text("assistant", "bounded response")) {
		t.Fatal("request/response trace lost call controls")
	}
}

func TestP3MeasuresSeparateFetchedFromModelDeliveredEvidence(t *testing.T) {
	fact := p3Fact("f001", "Complete source observation", "Exact retained evidence")
	spec := p3Case{Facts: []board.FactRecord{fact}}
	page, _ := json.Marshal(map[string]any{"state_version": strings.Repeat("a", 64), "items": []board.FactRecord{fact}})
	read := worker.GraphRequest{Op: "read_graph", Section: "facts", IDs: []string{fact.ID}}
	report := p3CaseReport{InitialState: board.State{FactRecords: spec.Facts}, GraphCalls: []p3GraphCall{{Request: read, Response: page}}, Result: worker.Result{Status: "success"}, Requests: []p3ProviderCall{{Kind: "turn", Messages: []agent.Message{agent.Text("user", "original task")}}}}
	measures := p3Measure(spec, report)
	got := measures.Facts[fact.ID]
	if !got.BodyFetched || !got.EvidenceFetched || got.BodyRead || got.EvidenceRead {
		t.Fatal("a fetched result with no subsequent request was counted as model-delivered")
	}
	input, _ := json.Marshal(map[string]any{"section": "facts", "ids": []string{fact.ID}})
	content, _ := json.Marshal(string(page))
	report.Requests = append(report.Requests, p3ProviderCall{Kind: "turn", Messages: []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "read", Name: "read_graph", Input: input}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "read", Content: content}}},
	}})
	got = p3Measure(spec, report).Facts[fact.ID]
	if !got.BodyRead || !got.EvidenceRead {
		t.Fatal("a successful read result in a real request was not measured as delivered")
	}
	report.Requests[1].Messages[1].Content[0].IsError = true
	got = p3Measure(spec, report).Facts[fact.ID]
	if got.BodyRead || got.EvidenceRead {
		t.Fatal("an error tool result was counted as delivered evidence")
	}
}

func TestP3MeasuresDoNotCreditFailureAsWithholdingOrHideCommittedError(t *testing.T) {
	spec := p3Case{WantComplete: false}
	report := p3CaseReport{Result: worker.Result{Status: "failed", FailureKind: "transport"}, RunError: "synthetic transport failure"}
	measures := p3Measure(spec, report)
	if measures.CompletionMatches != nil || measures.Assessment != "inconclusive_run_failure" {
		t.Fatal("a negative fixture with no successful decision was credited as correct withholding")
	}
	report.FinalState.Graph.Project.Status = "completed"
	measures = p3Measure(spec, report)
	if measures.CompletionMatches == nil || *measures.CompletionMatches || measures.Assessment != "unexpected_completion_requires_review" {
		t.Fatal("later runtime failure hid an authoritative incorrect completion")
	}
}
