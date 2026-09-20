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
	"xloom/internal/process"
	"xloom/internal/provider"
	"xloom/internal/tools"
)

type Options struct {
	Provider     agent.Provider
	Tools        []agent.Tool
	RunDir       string
	Output       io.Writer
	Now          func() time.Time
	SoftStop     <-chan struct{}
	ContextBytes int
}
type session struct {
	RunID                  string          `json:"run_id"`
	Kind                   string          `json:"kind"`
	StartedAt              time.Time       `json:"started_at"`
	ReasonDeadline         time.Time       `json:"reason_deadline,omitempty"`
	ConcludeStartedAt      time.Time       `json:"conclude_started_at,omitempty"`
	ConcludeDeadline       time.Time       `json:"conclude_deadline,omitempty"`
	Concluding             bool            `json:"concluding"`
	History                []agent.Message `json:"history"`
	Result                 *Result         `json:"result,omitempty"`
	TaskPrompt             string          `json:"task_prompt,omitempty"`
	ConclusionPrompt       string          `json:"conclusion_prompt,omitempty"`
	ConclusionInputVersion int             `json:"conclusion_input_version,omitempty"`
	Repairing              bool            `json:"repairing,omitempty"`
	RepairCount            int             `json:"repair_count,omitempty"`
	RepairReason           string          `json:"repair_reason,omitempty"`
	RepairPrompt           string          `json:"repair_prompt,omitempty"`
	RepairPending          bool            `json:"repair_pending,omitempty"`
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
		if ctx.Err() != nil {
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
	log, err := os.OpenFile(filepath.Join(o.RunDir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, err
	}
	defer log.Close()
	enc := json.NewEncoder(io.MultiWriter(log, o.Output))
	var logErr error
	state := session{RunID: j.RunID, Kind: j.Kind, StartedAt: o.Now()}
	emit := func(e agent.Event) {
		// The following Save persists the response and its repair phase
		// together. A crash before that save conservatively consumes a retry.
		if state.Repairing && e.Type == "message_end" && e.Message != nil && e.Message.Role == "assistant" {
			state.RepairPending = false
		}
		if logErr == nil {
			logErr = enc.Encode(e)
		}
	}
	statePath := filepath.Join(o.RunDir, "session.json")
	if previous, readErr := os.ReadFile(statePath); readErr == nil {
		if err = json.Unmarshal(previous, &state); err != nil {
			return Result{}, fmt.Errorf("invalid saved session: %w", err)
		}
		if state.RunID != j.RunID || state.Kind != j.Kind || state.StartedAt.IsZero() {
			return Result{}, errors.New("session belongs to another execution or is invalid")
		}
		if state.RepairCount < 0 || state.RepairCount > maxOutputRepairs || (state.RepairCount > 0 && !state.Repairing) || (state.Repairing && (state.RepairCount == 0 || state.RepairPrompt == "")) {
			return Result{}, errors.New("invalid saved result-repair state")
		}
	} else if !os.IsNotExist(readErr) {
		return Result{}, readErr
	}
	var l *agent.Loop
	save := func(history []agent.Message) error {
		if logErr != nil {
			return logErr
		}
		state.History = history
		if l != nil {
			state.TaskPrompt = l.TaskPrompt
			state.ConclusionPrompt = l.ConclusionPrompt
			state.RepairPrompt = l.RepairPrompt
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(o.RunDir, "session-*.tmp")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err = tmp.Write(raw); err == nil {
			err = tmp.Sync()
		}
		err = errors.Join(err, tmp.Close())
		if err != nil {
			return err
		}
		return os.Rename(name, statePath)
	}
	finish := func(r Result) (Result, error) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if process.Cancelled(o.RunDir) {
			return Result{}, context.Canceled
		}
		state.Result = &r
		if err := save(state.History); err != nil {
			return r, err
		}
		return r, enc.Encode(r)
	}
	if state.Result != nil {
		// Older versions could cache a parseable but truncated prefix. Never
		// submit it as success; restore its transcript into bounded repair.
		last, ok := lastAssistant(state.History)
		if state.Result.Status != "success" || !ok || !truncated(last) {
			return finish(*state.Result)
		}
		state.Result = nil
	}
	if j.WorkerType == "mock" {
		return finish(mock(j))
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
	if o.Tools == nil {
		set := tools.Set{Dir: j.Workspace, RunDir: o.RunDir}
		o.Tools = set.All()
	}
	if o.ContextBytes <= 0 {
		o.ContextBytes = envInt("XLOOM_CONTEXT_BYTES", 240000)
	}
	l = &agent.Loop{Provider: o.Provider, Tools: o.Tools, History: state.History, Concluding: state.Concluding, Repairing: state.Repairing, RepairPrompt: state.RepairPrompt, Emit: emit, Save: save, ContextBytes: o.ContextBytes, TaskPrompt: state.TaskPrompt, ConclusionPrompt: state.ConclusionPrompt}
	var endCancel context.CancelFunc = func() {}
	defer func() { endCancel() }()
	runCtx := ctx
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
			// Older sessions only persisted the start time; use their original
			// job budget once. New sessions retain an absolute deadline.
			state.ConcludeDeadline = state.ConcludeStartedAt.Add(time.Duration(j.Budget.ConcludeTimeout) * time.Second)
		}
		remaining := state.ConcludeDeadline.Sub(o.Now())
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		next, cancel := context.WithTimeout(ctx, remaining)
		endCancel = cancel
		if !wasConcluding {
			// Freeze the boundary before reading any artifact. A crash during
			// preparation resumes conservatively without reading new outputs.
			if err := save(l.History); err != nil {
				return nil, err
			}
		}
		if state.ConclusionInputVersion != conclusionInputVersion || l.ConclusionPrompt == "" {
			input, err := conclusionInput(next, j, o.RunDir, !wasConcluding)
			if err != nil {
				return nil, err
			}
			l.ConclusionPrompt = input
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
			state.ReasonDeadline = state.StartedAt.Add(time.Duration(j.Budget.Timeout) * time.Second)
		}
		remaining := state.ReasonDeadline.Sub(o.Now())
		reasonCtx, reasonCancel := context.WithTimeout(ctx, remaining)
		defer reasonCancel()
		runCtx = reasonCtx
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
		return j.Budget.Timeout > 0 && o.Now().Sub(state.StartedAt) >= time.Duration(j.Budget.Timeout)*time.Second
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
	l.OnTurnEnd = func(turnCtx context.Context, l *agent.Loop, m agent.Message) (context.Context, string, error) {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		hasCalls := hasToolCalls(m)
		problem := outputProblem(j, l.Concluding, m)
		needsResult := !hasCalls || truncated(m) || l.Concluding || l.Repairing
		if !l.Concluding && j.Kind != "reason" && (shouldConclude() || (needsResult && problem != nil)) {
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
		return turnCtx, "", nil
	}
	if state.Repairing && state.RepairPending {
		if containsInstruction(l.History, l.RepairPrompt) {
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
			if problem := outputProblem(j, l.Concluding, last); problem == nil {
				return finish(Result{Type: "result", Status: "success", Text: last.Text(), Conclude: l.Concluding})
			}
			runCtx, prompt, err = l.OnTurnEnd(runCtx, l, last)
			if err != nil {
				return finish(Result{Type: "result", Status: "failed", Text: last.Text(), Conclude: l.Concluding, Error: err.Error()})
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
	text, runErr := l.Run(runCtx, prompt)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r := Result{Type: "result", Status: "success", Text: text, Conclude: l.Concluding}
	if runErr == nil {
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
func mock(j Job) Result {
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
		if len(j.Graph.Facts) > 2 {
			data["complete"] = map[string]any{"from": []string{j.Graph.Facts[len(j.Graph.Facts)-1].ID}, "description": "Mock goal reached"}
		} else {
			data["intents"] = []any{map[string]any{"from": []string{"origin"}, "description": "Mock exploration"}}
		}
	}
	raw, _ := json.Marshal(map[string]any{"accepted": true, "data": data})
	return Result{Type: "result", Status: "success", Text: string(raw)}
}
