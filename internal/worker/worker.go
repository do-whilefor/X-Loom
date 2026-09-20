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
	"xloom/internal/config"
	"xloom/internal/contract"
	"xloom/internal/process"
	"xloom/internal/provider"
	"xloom/internal/tools"
)

type Job struct {
	RunID      string        `json:"run_id"`
	Kind       string        `json:"kind"`
	WorkerType string        `json:"worker_type"`
	Graph      board.Graph   `json:"graph"`
	Intent     *board.Intent `json:"intent,omitempty"`
	Budget     config.Task   `json:"budget"`
	Workspace  string        `json:"workspace"`
}
type Result struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Conclude bool   `json:"conclude"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

func Execute(ctx context.Context, jobPath string, output io.Writer) error {
	data, err := os.ReadFile(jobPath)
	if err != nil {
		return err
	}
	var j Job
	if err = json.Unmarshal(data, &j); err != nil {
		return err
	}
	if j.Kind != "reason" && j.Kind != "bootstrap" && j.Kind != "explore" {
		return errors.New("invalid job kind")
	}
	if j.Kind != "reason" && j.Intent == nil {
		return errors.New("job requires an intent")
	}
	runDir := filepath.Dir(jobPath)
	if err = os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(runDir, "worker.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(runDir, "worker.pid"))
	defer func() {
		if ctx.Err() != nil {
			process.KillGroups(runDir)
		}
	}()
	log, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	enc := json.NewEncoder(io.MultiWriter(log, output))
	var logErr error
	if j.WorkerType == "mock" {
		result := mock(j)
		return enc.Encode(result)
	}
	p := &provider.Anthropic{BaseURL: os.Getenv("ANTHROPIC_BASE_URL"), Token: os.Getenv("ANTHROPIC_AUTH_TOKEN"), Model: os.Getenv("ANTHROPIC_MODEL"), MaxTokens: envInt("XLOOM_MAX_OUTPUT_TOKENS", 8192), Timeout: time.Duration(envInt("XLOOM_REQUEST_TIMEOUT", 180)) * time.Second}
	if p.Model == "" {
		p.Model = os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	if p.BaseURL == "" || p.Token == "" || p.Model == "" {
		return errors.New("missing model configuration")
	}
	set := tools.Set{Dir: j.Workspace, RunDir: runDir}
	session := filepath.Join(runDir, "session.json")
	l := &agent.Loop{Provider: p, Tools: set.All(), ContextBytes: envInt("XLOOM_CONTEXT_BYTES", 240000), Emit: func(e agent.Event) {
		if err := enc.Encode(e); err != nil && logErr == nil {
			logErr = err
		}
	}, Save: func(messages []agent.Message) error {
		if logErr != nil {
			return logErr
		}
		data, err := json.Marshal(messages)
		if err != nil {
			return err
		}
		tmp := session + ".tmp"
		if err = os.WriteFile(tmp, data, 0600); err != nil {
			return err
		}
		return os.Rename(tmp, session)
	}}
	// Explicit restart resumes the transcript of this execution, never another run.
	if previous, err := os.ReadFile(session); err == nil {
		if err = json.Unmarshal(previous, &l.History); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	prompt, err := Prompt(j, false, runDir)
	if err != nil {
		return err
	}
	runCtx, cancel := deadline(ctx, j.Budget.Timeout)
	text, runErr := l.Run(runCtx, prompt)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	result := Result{Type: "result", Text: text, Status: "success"}
	_, parseErr := contract.Parse(text, j.Kind, false, j.Graph.OpenCount(), j.Budget.MaxIntents)
	if j.Kind != "reason" && (timedOut || (runErr == nil && parseErr != nil)) {
		process.KillGroups(runDir)
		l.Concluding = true
		prompt, err = Prompt(j, true, runDir)
		if err != nil {
			return err
		}
		endCtx, endCancel := deadline(ctx, j.Budget.ConcludeTimeout)
		text, runErr = l.Run(endCtx, prompt)
		endCancel()
		result.Text = text
		result.Conclude = true
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runErr != nil {
		result.Status = "failed"
		result.Error = runErr.Error()
	}
	if logErr != nil {
		return logErr
	}
	return enc.Encode(result)
}
func deadline(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}
func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
func Prompt(j Job, conclude bool, runDir string) (string, error) {
	graph, err := board.Export(j.Graph, "yaml")
	if err != nil {
		return "", err
	}
	path := filepath.Join(runDir, "graph.yaml")
	if err = os.WriteFile(path, []byte(graph), 0600); err != nil {
		return "", err
	}
	context := "Read the complete task graph from " + path + ". Long evidence belongs in files; cite its path in the result. Distinguish confirmed findings from hypotheses.\n"
	if conclude {
		if j.Kind == "bootstrap" {
			return context + `Stop exploration and waiting. Summarize only confirmed findings so far. Return a JSON object {"accepted":true,"data":{"fact":{"description":"..."}}}. Do not declare completion in this phase. If no factual conclusion can be submitted, return {"accepted":false,"reason":"..."}.`, nil
		}
		return context + `Stop exploration and waiting. Summarize confirmed incremental findings for the current intent. Return {"accepted":true,"data":{"description":"..."}}, or {"accepted":false,"reason":"..."} if there is no factual conclusion.` + intentContext(j), nil
	}
	switch j.Kind {
	case "bootstrap":
		return context + `Work directly from origin toward goal. Continue until the goal is confirmed or a conclude instruction arrives. On success return {"accepted":true,"data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why goal is met"}}}. If unable to accept the task return {"accepted":false,"reason":"..."}.`, nil
	case "explore":
		return context + `Explore only the assigned intent. Report confirmed incremental findings, including a substantiated negative result when appropriate. Return {"accepted":true,"data":{"description":"..."}}. If unable to accept the task return {"accepted":false,"reason":"..."}.` + intentContext(j), nil
	case "reason":
		return context + fmt.Sprintf(`Determine whether confirmed facts satisfy goal. If so return {"accepted":true,"data":{"complete":{"from":["fact id"],"description":"proof of completion"}}}. Otherwise propose at most %d independent valuable directions with {"accepted":true,"data":{"intents":[{"from":["fact id"],"description":"direction"}]}}. Sources must exist and cannot be goal. If open intents exist and cover the useful directions, {"accepted":true,"data":{}} is allowed. If there are no open intents, propose an intent. If unable to accept the task return {"accepted":false,"reason":"..."}.`, j.Budget.MaxIntents), nil
	}
	return "", errors.New("unknown task")
}
func intentContext(j Job) string {
	if j.Intent == nil {
		return ""
	}
	return "\nCurrent intent " + j.Intent.ID + ": " + j.Intent.Description
}
func mock(j Job) Result {
	if configured := os.Getenv("XLOOM_MOCK_" + strings.ToUpper(j.Kind)); configured != "" {
		return Result{Type: "result", Status: "success", Text: configured}
	}
	result := map[string]any{"accepted": true}
	data := map[string]any{}
	result["data"] = data
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
	raw, _ := json.Marshal(result)
	return Result{Type: "result", Status: "success", Text: string(raw)}
}
