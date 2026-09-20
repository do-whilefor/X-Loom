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
	"xloom/internal/contract"
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
	RunID             string          `json:"run_id"`
	Kind              string          `json:"kind"`
	StartedAt         time.Time       `json:"started_at"`
	ConcludeStartedAt time.Time       `json:"conclude_started_at,omitempty"`
	Concluding        bool            `json:"concluding"`
	History           []agent.Message `json:"history"`
	Result            *Result         `json:"result,omitempty"`
	TaskPrompt        string          `json:"task_prompt,omitempty"`
	ConclusionPrompt  string          `json:"conclusion_prompt,omitempty"`
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
	emit := func(e agent.Event) {
		if logErr == nil {
			logErr = enc.Encode(e)
		}
	}
	state := session{RunID: j.RunID, Kind: j.Kind, StartedAt: o.Now()}
	statePath := filepath.Join(o.RunDir, "session.json")
	if previous, readErr := os.ReadFile(statePath); readErr == nil {
		if err = json.Unmarshal(previous, &state); err != nil {
			return Result{}, fmt.Errorf("invalid saved session: %w", err)
		}
		if state.RunID != j.RunID || state.Kind != j.Kind || state.StartedAt.IsZero() {
			return Result{}, errors.New("session belongs to another execution or is invalid")
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
		return finish(*state.Result)
	}
	if j.WorkerType == "mock" {
		return finish(mock(j))
	}
	if o.Provider == nil {
		p := &provider.Anthropic{BaseURL: os.Getenv("ANTHROPIC_BASE_URL"), Token: os.Getenv("ANTHROPIC_AUTH_TOKEN"), Model: os.Getenv("ANTHROPIC_MODEL"), MaxTokens: envInt("XLOOM_MAX_OUTPUT_TOKENS", 8192), Timeout: time.Duration(envInt("XLOOM_REQUEST_TIMEOUT", 180)) * time.Second}
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
	l = &agent.Loop{Provider: o.Provider, Tools: o.Tools, History: state.History, Concluding: state.Concluding, Emit: emit, Save: save, ContextBytes: o.ContextBytes, TaskPrompt: state.TaskPrompt, ConclusionPrompt: state.ConclusionPrompt}
	var endCancel context.CancelFunc = func() {}
	defer func() { endCancel() }()
	runCtx := ctx
	startConclusion := func() (context.Context, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state.Concluding = true
		l.Concluding = true
		if state.ConcludeStartedAt.IsZero() {
			state.ConcludeStartedAt = o.Now()
		}
		remaining := time.Duration(j.Budget.ConcludeTimeout)*time.Second - o.Now().Sub(state.ConcludeStartedAt)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		next, cancel := context.WithTimeout(ctx, remaining)
		endCancel = cancel
		return next, nil
	}
	if state.Concluding {
		runCtx, err = startConclusion()
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: true, Error: err.Error()})
		}
	}
	if j.Kind == "reason" && j.Budget.Timeout > 0 {
		remaining := time.Duration(j.Budget.Timeout)*time.Second - o.Now().Sub(state.StartedAt)
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
	l.OnTurnEnd = func(turnCtx context.Context, l *agent.Loop, m agent.Message) (context.Context, string, error) {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		hasCalls := false
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				hasCalls = true
			}
		}
		_, parseErr := contract.Parse(m.Text(), j.Kind, l.Concluding, j.Graph.OpenCount(), j.Budget.MaxIntents)
		if !l.Concluding && j.Kind != "reason" && (shouldConclude() || (!hasCalls && parseErr != nil)) {
			next, err := startConclusion()
			if err != nil {
				return nil, "", err
			}
			prompt, err := Prompt(j, true, o.RunDir)
			return next, prompt, err
		}
		return turnCtx, "", nil
	}
	prompt := ""
	if len(l.History) == 0 {
		if shouldConclude() {
			runCtx, err = startConclusion()
			if err != nil {
				return Result{}, err
			}
		}
		prompt, err = Prompt(j, l.Concluding, o.RunDir)
		if err != nil {
			return Result{}, err
		}
	} else if last := l.History[len(l.History)-1]; last.Role == "assistant" {
		calls := false
		for _, b := range last.Content {
			if b.Type == "tool_use" {
				calls = true
			}
		}
		if !calls {
			// A model turn may have been saved immediately before the worker crashed.
			if _, parseErr := contract.Parse(last.Text(), j.Kind, l.Concluding, j.Graph.OpenCount(), j.Budget.MaxIntents); parseErr == nil {
				return finish(Result{Type: "result", Status: "success", Text: last.Text(), Conclude: l.Concluding})
			} else if j.Kind == "reason" || l.Concluding {
				return finish(Result{Type: "result", Status: "failed", Text: last.Text(), Conclude: l.Concluding, Error: parseErr.Error()})
			}
			if j.Kind != "reason" && !l.Concluding {
				runCtx, err = startConclusion()
				if err == nil {
					prompt, err = Prompt(j, true, o.RunDir)
				}
				if err != nil {
					return Result{}, err
				}
			}
		}
	}
	// Resuming a transcript is itself a boundary. An expired exploration
	// budget must not buy another unrestricted model/tool turn after restart.
	if len(l.History) > 0 && shouldConclude() {
		runCtx, err = startConclusion()
		if err == nil {
			prompt, err = Prompt(j, true, o.RunDir)
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
		_, runErr = contract.Parse(text, j.Kind, l.Concluding, j.Graph.OpenCount(), j.Budget.MaxIntents)
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
