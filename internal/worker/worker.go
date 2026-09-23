//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/process"
	"xloom/internal/provider"
)

type Options struct {
	Provider            agent.Provider
	Tools               []agent.Tool
	RunDir              string
	Output              io.Writer
	Now                 func() time.Time
	SoftStop            <-chan struct{}
	ContextBytes        int
	ContextTokens       int
	ContextTargetTokens int
	GraphVersion        *string
	ReplanShadow        bool
	decision            *decisionDraft
	decisionEmit        agent.Emit
}

func Execute(ctx context.Context, jobPath string, output io.Writer) error {
	raw, err := os.ReadFile(jobPath)
	if err != nil {
		return err
	}
	var j Job
	if err = json.Unmarshal(raw, &j); err != nil {
		return err
	}
	_, err = Run(ctx, j, Options{RunDir: filepath.Dir(jobPath), Output: output})
	return err
}

// Run persists tool calls before executing their side effects. A task budget
// requests conclusion only at a settled turn; cancellation never restarts it.
func Run(parent context.Context, j Job, o Options) (Result, error) {
	if err := parent.Err(); err != nil {
		return Result{}, err
	}
	if j.RunID == "" {
		return Result{}, errors.New("job requires run_id")
	}
	if j.PreviousRunID == j.RunID || j.Graph.Project.ID == "" {
		return Result{}, errors.New("job requires a project and a distinct previous_run_id")
	}
	if j.Kind != "bootstrap" && j.Kind != "reason" && j.Kind != "explore" {
		return Result{}, errors.New("invalid job kind")
	}
	if j.Kind != "reason" && j.Intent == nil {
		return Result{}, errors.New("job requires an intent")
	}
	if j.Budget.Timeout < 0 || j.Budget.ConcludeTimeout < 0 {
		return Result{}, errors.New("task budgets must not be negative")
	}
	if j.Kind != "reason" && j.Budget.ConcludeTimeout <= 0 {
		return Result{}, errors.New("conclude timeout must be positive")
	}
	if o.RunDir == "" || j.Workspace == "" {
		return Result{}, errors.New("job requires workspace and execution directory")
	}
	var err error
	o.RunDir, err = filepath.Abs(o.RunDir)
	if err != nil {
		return Result{}, err
	}
	j.Workspace, err = filepath.Abs(j.Workspace)
	if err != nil {
		return Result{}, err
	}
	if err = os.MkdirAll(o.RunDir, 0700); err != nil {
		return Result{}, err
	}
	unlock, err := process.Lock(o.RunDir)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	if err = process.CheckLaunch(o.RunDir); err != nil {
		return Result{}, err
	}
	if process.Cancelled(o.RunDir) {
		return Result{}, context.Canceled
	}
	if err = process.RegisterWorker(o.RunDir); err != nil {
		return Result{}, err
	}
	defer os.Remove(filepath.Join(o.RunDir, "worker.pid"))
	defer process.KillGroups(o.RunDir)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer func() {
		if ctx.Err() != nil && !errors.Is(context.Cause(parent), ErrInterrupted) {
			_ = os.WriteFile(filepath.Join(o.RunDir, "cancelled"), []byte("hard stop\n"), 0600)
		}
	}()
	// The marker also covers cancel arriving before a container exec starts.
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if process.Cancelled(o.RunDir) {
					cancel()
					return
				}
			}
		}
	}()
	if process.Cancelled(o.RunDir) {
		return Result{}, context.Canceled
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Output == nil {
		o.Output = io.Discard
	}
	identity, err := identityFor(j, o.RunDir)
	if err != nil {
		return Result{}, err
	}
	state := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: j.RunID, Kind: j.Kind, StartedAt: o.Now()}
	if j.Kind == "reason" && j.Decision != nil {
		state.GraphVersion = j.Decision.StateVersion
	}
	if j.Budget.Timeout > 0 {
		state.ExecutionDeadline = state.StartedAt.Add(time.Duration(j.Budget.Timeout) * time.Second)
	}
	statePath := filepath.Join(o.RunDir, "session.json")
	resuming := false
	if previous, readErr := os.ReadFile(statePath); readErr == nil {
		if err = json.Unmarshal(previous, &state); err != nil {
			return Result{}, fmt.Errorf("invalid saved session: %w", err)
		}
		if err = state.validate(identity); err != nil {
			return Result{}, err
		}
		resuming = true
	} else if !os.IsNotExist(readErr) {
		return Result{}, readErr
	}
	var checkpoint *journalCheckpoint
	if resuming {
		checkpoint = &state.Log
	}
	journal, err := openJournal(o.RunDir, checkpoint)
	if err != nil {
		return Result{}, err
	}
	defer journal.file.Close()
	enc := json.NewEncoder(o.Output)
	var logErr error
	emit := func(e agent.Event) {
		if e.Type == "tool_end" && e.Error == "" {
			// The next settled tool-result save makes this progress durable.
			state.ContinuationCount = 0
		}
		if state.Repairing && e.Type == "message_end" && e.Message != nil && e.Message.Role == "assistant" {
			state.RepairPending = false
		}
		if logErr == nil {
			logErr = journal.append(e)
		}
		if logErr == nil {
			logErr = enc.Encode(e)
		}
	}
	o.decisionEmit = emit
	var l *agent.Loop
	save := func(history []agent.Message) error {
		if logErr != nil {
			return logErr
		}
		before := state
		state.History = history
		if l != nil {
			state.TaskPrompt = l.TaskPrompt
			state.ConclusionPrompt = l.ConclusionPrompt
			state.RepairPrompt = l.RepairPrompt
			state.ContextCheckpoint = l.Checkpoint
		}
		if err := state.save(o.RunDir, journal); err != nil {
			state = before
			return err
		}
		return nil
	}
	finish := func(r Result) (Result, error) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if process.Cancelled(o.RunDir) {
			return Result{}, context.Canceled
		}
		r = checkedResult(j, r)
		if r.Status == "success" {
			prepared, err := prepareFinalEvidence(ctx, j, o.RunDir, r, state.ConclusionEvidence)
			if err != nil {
				r.Status, r.FailureKind, r.Error = "failed", "result_evidence", err.Error()
			} else {
				r = prepared
			}
		}
		if r.Status == "failed" && r.FailureKind == "" {
			r.FailureKind = "execution"
		}
		r.StateVersion = state.GraphVersion
		if j.Kind == "reason" {
			metrics := journal.metrics.finish(j, r, state.StartedAt, o.Now())
			metrics.Replan = state.Replan
			r.Metrics = &metrics
		}
		if err := journal.append(r); err != nil {
			return r, err
		}
		state.Result = &r
		if err := save(state.History); err != nil {
			return r, err
		}
		return r, enc.Encode(r)
	}
	infrastructureResume := false
	if state.Result != nil {
		last, ok := lastAssistant(state.History)
		parsed, parseErr := parseOutput(j, state.Result.Conclude, state.Result.Text)
		if state.Result.Retryable {
			infrastructureResume = true
			state.Result = nil
		} else if state.Result.Status == "success" && (parseErr != nil || parsed.Outcome == "continue" || (ok && truncated(last))) {
			if !ok {
				return finish(checkedResult(j, *state.Result))
			}
			state.Result = nil
		} else if state.Result.Status == "success" && parsed.Outcome == "incomplete" {
			return finish(checkedResult(j, *state.Result))
		} else {
			return *state.Result, enc.Encode(state.Result)
		}
	}
	if resuming {
		if state.RecoveryCount >= maxRunRecoveries {
			return finish(Result{Type: "result", Status: "failed", Conclude: state.Concluding, FailureKind: "recovery_exhausted", Error: "same-run infrastructure recovery exhausted after 2 attempts"})
		}
		state.RecoveryCount++
		if state.ContextCheckpoint == nil && (journal.lastSequence > 0 || journal.lastCompaction > 0) {
			state.ContextCheckpoint = &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion}
		}
		if state.ContextCheckpoint != nil && state.ContextCheckpoint.LastSequence < journal.lastSequence {
			state.ContextCheckpoint.LastSequence = journal.lastSequence
		}
		if state.ContextCheckpoint != nil && state.ContextCheckpoint.CompactionCount < journal.lastCompaction {
			state.ContextCheckpoint.CompactionCount = journal.lastCompaction
		}
		emit(agent.Event{Type: "recovery", Text: fmt.Sprintf("same run recovery %d/%d; retained %d uncommitted log bytes; incomplete tail archive: %s", state.RecoveryCount, maxRunRecoveries, journal.uncommitted, journal.partialArchive)})
	}
	// The identity, budget and consumed recovery allowance precede any request.
	if err := save(state.History); err != nil {
		return Result{}, err
	}
	if j.WorkerType == "mock" {
		return finish(mock(j, o.RunDir))
	}
	if o.Provider == nil {
		p := &provider.Anthropic{BaseURL: os.Getenv("ANTHROPIC_BASE_URL"), Token: os.Getenv("ANTHROPIC_AUTH_TOKEN"), Model: os.Getenv("ANTHROPIC_MODEL"), MaxTokens: envInt("XLOOM_MAX_OUTPUT_TOKENS", provider.DefaultMaxTokens), ReasoningEffort: os.Getenv("XLOOM_REASONING_EFFORT"), Timeout: time.Duration(envInt("XLOOM_REQUEST_TIMEOUT", 180)) * time.Second}
		p.SessionID = j.RunID
		if p.Model == "" {
			p.Model = os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
		}
		if strings.TrimSpace(p.Token) == "" {
			return Result{}, errors.New("ANTHROPIC_AUTH_TOKEN is required")
		}
		o.Provider = p
	}
	o.GraphVersion = &state.GraphVersion
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		return Result{}, err
	}
	if o.decision != nil {
		if _, err := o.decision.recover(ctx); err != nil {
			return Result{}, err
		}
		if o.decision.committed {
			return finish(Result{Type: "result", Status: "success", Text: committedDecisionText})
		}
		if resuming {
			o.decision.invalidate()
		}
		// A batch planner succeeds through a tool receipt, never repaired final
		// JSON. Resume its uncommitted planning phase without restoring consumed
		// repair/continuation allowances or deadlines. Execute repair is unchanged.
		state.Repairing, state.RepairPending = false, false
		state.RepairReason, state.RepairPrompt = "", ""
	}
	if o.ContextBytes <= 0 {
		o.ContextBytes = envInt("XLOOM_CONTEXT_BYTES", DefaultContextBytes)
	}
	if o.ContextTokens <= 0 {
		o.ContextTokens = envInt("XLOOM_CONTEXT_TOKENS", DefaultContextTokens)
	}
	if o.ContextTargetTokens <= 0 {
		o.ContextTargetTokens = envInt("XLOOM_CONTEXT_TARGET_TOKENS", DefaultContextTargetTokens)
	}
	l = &agent.Loop{Provider: o.Provider, Tools: o.Tools, History: state.History, Concluding: state.Concluding, Repairing: state.Repairing, RepairPrompt: state.RepairPrompt, Emit: emit, Checkpoint: state.ContextCheckpoint, SaveState: func(history []agent.Message, _ *agent.ContextCheckpoint) error {
		return save(history)
	}, ContextBytes: o.ContextBytes, ContextTokens: o.ContextTokens, ContextTargetTokens: o.ContextTargetTokens, ObserveRequests: j.Kind == "reason", TaskPrompt: state.TaskPrompt, ConclusionPrompt: state.ConclusionPrompt}
	if o.decision != nil {
		l.StopResult = o.decision.result
	}
	var endCancel context.CancelFunc = func() {}
	defer func() { endCancel() }()
	runCtx := ctx
	phaseCtx := ctx
	prompt := ""
	startConclusion := func() (context.Context, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wasConcluding := state.Concluding
		state.Concluding = true
		l.Concluding = true
		if state.ConcludeStartedAt.IsZero() {
			if wasConcluding {
				return nil, errors.New("saved conclusion has no start time; cannot refresh its deadline")
			}
			state.ConcludeStartedAt = o.Now()
		}
		if state.ConcludeDeadline.IsZero() {
			// Only a newly entered conclusion can establish its deadline.
			state.ConcludeDeadline = state.ConcludeStartedAt.Add(time.Duration(j.Budget.ConcludeTimeout) * time.Second)
		}
		remaining := state.ConcludeDeadline.Sub(o.Now())
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		next, cancel := context.WithTimeout(ctx, remaining)
		endCancel = cancel
		phaseCtx = next
		if !wasConcluding {
			// Freeze the boundary before reading any artifact. A crash during
			// preparation resumes conservatively without reading new outputs.
			if err := save(l.History); err != nil {
				return nil, err
			}
		}
		if state.ConclusionInputVersion != conclusionInputVersion || l.ConclusionPrompt == "" {
			input, refs, err := conclusionInputWithEvidence(next, j, o.RunDir, !wasConcluding)
			if err != nil {
				return nil, err
			}
			l.ConclusionPrompt = input
			state.ConclusionEvidence = refs
			state.ConclusionInputVersion = conclusionInputVersion
			// Persist the frozen input before another model request. Recovery
			// reuses it even if files have changed or disappeared since then.
			if err = save(l.History); err != nil {
				return nil, err
			}
		}
		return next, nil
	}
	if state.Concluding {
		runCtx, err = startConclusion()
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: true, Error: err.Error()})
		}
		if !containsInstruction(l.History, l.ConclusionPrompt) {
			prompt = l.ConclusionPrompt
		}
	}
	if j.Kind == "reason" && (j.Budget.Timeout > 0 || !state.ReasonDeadline.IsZero()) {
		if state.ReasonDeadline.IsZero() {
			state.ReasonDeadline = state.ExecutionDeadline
		}
		remaining := state.ReasonDeadline.Sub(o.Now())
		reasonCtx, reasonCancel := context.WithTimeout(ctx, remaining)
		defer reasonCancel()
		runCtx = reasonCtx
		phaseCtx = reasonCtx
	}
	shouldConclude := func() bool {
		if j.Kind == "reason" || l.Concluding {
			return false
		}
		select {
		case <-o.SoftStop:
			return true
		default:
		}
		return !state.ExecutionDeadline.IsZero() && !o.Now().Before(state.ExecutionDeadline)
	}
	prepareRepairHistory := func() error {
		if err := l.RepairHistory(); err != nil {
			return err
		}
		if l.Concluding && !containsInstruction(l.History, l.ConclusionPrompt) {
			return l.AppendInstruction(l.ConclusionPrompt)
		}
		return nil
	}
	startRepair := func(turnCtx context.Context, problem *outputFailure) (string, error) {
		if err := turnCtx.Err(); err != nil {
			return "", err
		}
		if state.RepairCount >= maxOutputRepairs {
			return "", fmt.Errorf("result-format repair exhausted after %d attempts: %w", maxOutputRepairs, problem)
		}
		state.RepairCount++
		state.Repairing = true
		l.Repairing = true
		state.RepairReason = problem.Reason
		instruction, err := repairInstruction(j, l.Concluding, state.RepairCount, problem)
		if err != nil {
			return "", err
		}
		l.RepairPrompt = instruction
		state.RepairPending = true
		// Consume the attempt before adding its prompt or making a request.
		if err = save(l.History); err != nil {
			return "", err
		}
		if err = prepareRepairHistory(); err != nil {
			return "", err
		}
		return instruction, nil
	}
	continueExecution := func() (string, error) {
		last, ok := lastAssistant(l.History)
		if !ok || last.Sequence == 0 {
			return "", errors.New("continuation requires a durable assistant message")
		}
		if state.ContinuationSequence != last.Sequence {
			if state.ContinuationCount >= maxContinuations {
				return "", &outputFailure{Reason: "continuation_exhausted", Detail: "repeated continue responses made no successful tool progress"}
			}
			state.ContinuationCount++
			state.ContinuationSequence = last.Sequence
		}
		state.Repairing, state.RepairPending, l.Repairing = false, false, false
		state.RepairReason, state.RepairPrompt, l.RepairPrompt = "", "", ""
		// Persist consumption before the follow-up instruction. Recovery can
		// then reissue that instruction without replaying tools or buying turns.
		if err := save(l.History); err != nil {
			return "", err
		}
		if o.decision != nil {
			return "This Decide has no committed receipt. Continue planning in this same run with read_graph and graph_action, then commit the complete draft (including an empty plan). Final JSON cannot publish a plan. For truncated tool calls, reissue complete arguments. Tools remain available and the original deadline still applies. If the task must be declined, return accepted:false with its reason.", nil
		}
		return "Continue the unfinished work in this same execution. Tools are enabled. The original task deadline still applies. Use completed only when the assigned task is finished; otherwise continue working or report incomplete with the remaining work and blocker.", nil
	}
	l.OnTurnEnd = func(turnCtx context.Context, l *agent.Loop, m agent.Message) (context.Context, string, error) {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		hasCalls := hasToolCalls(m)
		if o.decision != nil {
			if !truncated(m) {
				if hasCalls {
					return turnCtx, "", nil
				}
				if parsed, err := parseOutput(j, false, m.Text()); err == nil && parsed.Kind == "rejected" {
					return turnCtx, "", nil
				}
			}
			instruction, err := continueExecution()
			return turnCtx, instruction, err
		}
		problem := outputProblem(j, l.Concluding, m)
		needsResult := !hasCalls || truncated(m) || l.Concluding || l.Repairing
		if !l.Concluding && j.Kind != "reason" && (shouldConclude() || (j.ResultContractVersion == 0 && needsResult && problem != nil)) {
			next, err := startConclusion()
			if err != nil {
				return nil, "", err
			}
			if needsResult && problem != nil {
				instruction, err := startRepair(next, problem)
				return next, instruction, err
			}
			return next, l.ConclusionPrompt, nil
		}
		if needsResult && problem != nil {
			instruction, err := startRepair(turnCtx, problem)
			return turnCtx, instruction, err
		}
		if needsResult {
			parsed, err := parseOutput(j, l.Concluding, m.Text())
			if err != nil {
				return nil, "", err
			}
			if parsed.Outcome == "continue" {
				instruction, err := continueExecution()
				return turnCtx, instruction, err
			}
		}
		return turnCtx, "", nil
	}
	if state.Repairing && state.RepairPending {
		if containsInstruction(l.History, l.RepairPrompt) && infrastructureResume {
			// Transport recovery continues the unanswered request without buying
			// or consuming another JSON-format repair attempt.
			prompt = ""
		} else if containsInstruction(l.History, l.RepairPrompt) {
			// The request may have been sent before a crash. Do not reset or
			// replay that attempt for free; use only a remaining repair slot.
			prompt, err = startRepair(runCtx, &outputFailure{Reason: "interrupted_repair", Detail: "the previous repair request has no durable response"})
		} else {
			err = prepareRepairHistory()
			prompt = l.RepairPrompt
		}
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: l.Concluding, Error: err.Error()})
		}
	}
	// The experiment runs once, before planning, with the same absolute task
	// deadline. A crash cannot buy another check or carry its private transcript
	// into Decide. Even a valid keep does not skip the normal planner.
	if j.Kind == "reason" {
		if state.Replan != nil && state.Replan.Status == "running" {
			state.Replan.Status, state.Replan.Fallback = "interrupted", "decide"
		} else if !resuming && (o.ReplanShadow || os.Getenv("XLOOM_REPLAN_SHADOW") == "1") {
			state.Replan = &ReplanObservation{Mode: "shadow", Status: "skipped", Fallback: "decide"}
			if j.Decision != nil {
				state.Replan.StateVersion, state.Replan.Generation = j.Decision.StateVersion, j.Decision.Generation
				state.Replan.FromRevision, state.Replan.ToRevision = j.Decision.FromRevision, j.Decision.ToRevision
			}
			if j.Decision != nil && j.Decision.Mode == "changes" && (j.State != nil || j.InputSnapshot != nil) && j.openCount() > 0 {
				state.Replan.Status = "running"
				if err = save(l.History); err != nil {
					return Result{}, err
				}
				if err = runReplanCheck(runCtx, j, o, state.Replan, emit, func() error { return save(l.History) }); err != nil {
					return Result{}, err
				}
			}
		}
		if state.Replan != nil {
			raw, _ := json.Marshal(state.Replan)
			emit(agent.Event{Type: "replan_observation", Text: string(raw)})
			if err = save(l.History); err != nil {
				return Result{}, err
			}
		}
	}
	if len(l.History) == 0 {
		if shouldConclude() {
			runCtx, err = startConclusion()
			if err != nil {
				return Result{}, err
			}
		}
		if l.Concluding {
			prompt = l.ConclusionPrompt
		} else {
			prompt, err = Prompt(j, false, o.RunDir)
		}
		if err != nil {
			return Result{}, err
		}
	} else if last, ok := lastAssistant(l.History); ok && prompt == "" && !awaitingInstructionResponse(l.History) {
		if !hasToolCalls(last) || truncated(last) || state.Repairing {
			// A model turn may have been saved immediately before the worker crashed.
			runCtx, prompt, err = l.OnTurnEnd(runCtx, l, last)
			if err != nil {
				return finish(Result{Type: "result", Status: "failed", Text: last.Text(), Conclude: l.Concluding, Error: err.Error()})
			}
			if prompt == "" {
				return finish(Result{Type: "result", Status: "success", Text: last.Text(), Conclude: l.Concluding})
			}
		}
	}
	// Resuming a transcript is itself a boundary. An expired exploration
	// budget must not buy another unrestricted model/tool turn after restart.
	if len(l.History) > 0 && shouldConclude() {
		runCtx, err = startConclusion()
		if err == nil {
			prompt = l.ConclusionPrompt
		}
		if err != nil {
			return Result{}, err
		}
	}
	if resuming && o.decision != nil {
		// Saved tool results describe a previous private draft, not a plan that
		// survived the restart. Keep the immutable job and its original budget.
		if err := l.RepairHistory(); err != nil {
			return Result{}, err
		}
		if err := l.AppendInstruction("The uncommitted decision draft was discarded on recovery. Read the current overview and affected facts, restage the whole plan and commit. Old $aliases and draft receipts are no longer valid."); err != nil {
			return Result{}, err
		}
	}
	text, runErr := l.Run(runCtx, prompt)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r := Result{Type: "result", Status: "success", Text: text, Conclude: l.Concluding}
	if runErr == nil && !(o.decision != nil && o.decision.committed) {
		if last, ok := lastAssistant(l.History); ok {
			if problem := outputProblem(j, l.Concluding, last); problem != nil {
				runErr = problem
			}
		} else {
			runErr = errors.New("model returned no final message")
		}
	}
	if runErr != nil {
		r.Status = "failed"
		r.Error = runErr.Error()
		r.FailureKind, r.Retryable = classifyFailure(runErr, phaseCtx)
		if r.Retryable && state.RecoveryCount >= maxRunRecoveries {
			r.Retryable = false
			r.FailureKind = "recovery_exhausted"
		}
	}
	if logErr != nil {
		return r, logErr
	}
	return finish(r)
}

func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
func mock(j Job, runDir string) Result {
	if configured := os.Getenv("XLOOM_MOCK_" + strings.ToUpper(j.Kind)); configured != "" {
		return Result{Type: "result", Status: "success", Text: configured}
	}
	data := map[string]any{}
	switch j.Kind {
	case "bootstrap":
		data["fact"] = map[string]string{"description": "Mock confirmed result"}
		data["complete"] = map[string]string{"description": "Mock goal reached"}
	case "explore":
		data["description"] = "Mock exploration confirmed"
	case "reason":
		facts := j.Graph.Facts
		if j.InputSnapshot != nil && j.Decision != nil {
			var view struct {
				Facts []board.Fact `json:"fact_records"`
			}
			_ = json.Unmarshal(j.Decision.View, &view)
			facts = []board.Fact{{ID: "origin"}, {ID: "goal"}}
			for _, fact := range view.Facts {
				if fact.ID != "origin" && fact.ID != "goal" {
					facts = append(facts, fact)
				}
			}
		}
		if len(facts) > 2 {
			data["complete"] = map[string]any{"from": []string{facts[len(facts)-1].ID}, "description": "Mock goal reached"}
		} else {
			data["intents"] = []any{map[string]any{"from": []string{"origin"}, "description": "Mock exploration"}}
		}
	}
	response := map[string]any{"accepted": true, "data": data}
	if j.ResultContractVersion >= 1 && j.Kind != "reason" {
		response["outcome"] = "completed"
	}
	if j.ResultContractVersion >= 2 && j.Kind != "reason" {
		name := filepath.Join(runDir, "output-mock.txt")
		if err := retainEvidenceFile(context.Background(), name, []byte("Synthetic Mock fixture observation: assigned check completed.\n")); err != nil {
			return Result{Type: "result", Status: "failed", FailureKind: "fixture", Error: err.Error()}
		}
		delete(data, "description")
		info, err := os.Stat(name)
		if err != nil {
			return Result{Type: "result", Status: "failed", FailureKind: "fixture", Error: err.Error()}
		}
		data["fact"] = map[string]any{"description": "Synthetic Mock fixture check completed", "scope": "local synthetic fixture", "observed_at": info.ModTime().UTC().Format(time.RFC3339), "evidence": []map[string]string{{"path": name}}}
	}
	raw, _ := json.Marshal(response)
	return Result{Type: "result", Status: "success", Text: string(raw)}
}
