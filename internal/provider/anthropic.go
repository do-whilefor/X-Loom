// Package provider implements the configured Anthropic-compatible endpoint.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"xloom/internal/agent"
)

const System = "Work toward the assigned task using available tools. Distinguish confirmed facts from guesses. Follow the task's result contract."
const DefaultBaseURL = "https://opencode.ai/zen/go"
const DefaultModel = "deepseek-v4.1-flash"
const DefaultReasoningEffort = "max"
const DefaultMaxTokens = 32768

type Anthropic struct {
	BaseURL         string
	Token           string
	Model           string
	MaxTokens       int
	ReasoningEffort string
	Timeout         time.Duration
	Client          *http.Client
	SessionID       string
	sessionOnce     sync.Once
	sessionID       string
	sessionErr      error
}
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("model endpoint returned HTTP %d", e.Status) }
func (p *Anthropic) endpoint() string {
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if strings.HasSuffix(base, "/messages") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}
func (p *Anthropic) Generate(ctx context.Context, messages []agent.Message, tools []agent.Definition, emit agent.Emit) (agent.Message, error) {
	return p.generate(ctx, messages, tools, p.MaxTokens, emit)
}

func (p *Anthropic) GenerateSummary(ctx context.Context, messages []agent.Message, maxTokens int, emit agent.Emit) (agent.Message, error) {
	return p.generate(ctx, messages, nil, maxTokens, emit)
}

// InputBytes counts the complete serialized request, including system text,
// tools and output controls, without exposing local transcript metadata.
func (p *Anthropic) InputBytes(messages []agent.Message, tools []agent.Definition) (int, error) {
	raw, err := p.payload(messages, tools, p.MaxTokens)
	return len(raw), err
}

func (p *Anthropic) payload(messages []agent.Message, tools []agent.Definition, limit int) ([]byte, error) {
	model := p.Model
	if model == "" {
		model = DefaultModel
	}
	if limit <= 0 {
		limit = DefaultMaxTokens
	}
	effort := p.ReasoningEffort
	if effort == "" {
		effort = DefaultReasoningEffort
	}
	switch effort {
	case "low", "high", "max":
	default:
		return nil, errors.New("reasoning effort must be low, high, or max")
	}
	payload := struct {
		Model     string             `json:"model"`
		MaxTokens int                `json:"max_tokens"`
		System    string             `json:"system"`
		Messages  []agent.Message    `json:"messages"`
		Tools     []agent.Definition `json:"tools,omitempty"`
		Stream    bool               `json:"stream"`
		Thinking  struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}{Model: model, MaxTokens: limit, System: System, Messages: agent.WireHistory(messages), Tools: tools, Stream: true}
	payload.Thinking.Type = "enabled"
	payload.OutputConfig.Effort = effort
	return json.Marshal(payload)
}

func (p *Anthropic) generate(ctx context.Context, messages []agent.Message, tools []agent.Definition, maxTokens int, emit agent.Emit) (agent.Message, error) {
	if strings.TrimSpace(p.Token) == "" {
		return agent.Message{}, errors.New("missing model authentication token")
	}
	p.sessionOnce.Do(func() {
		p.sessionID = p.SessionID
		if p.sessionID == "" {
			value := make([]byte, 16)
			_, p.sessionErr = rand.Read(value)
			p.sessionID = hex.EncodeToString(value)
		}
	})
	if p.sessionErr != nil {
		return agent.Message{}, p.sessionErr
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// DeepSeek's Anthropic format uses output_config.effort for reasoning
	// strength; budget_tokens is ignored. A returned thinking:"" block is
	// transcript data and is unrelated to these request controls.
	data, err := p.payload(messages, tools, maxTokens)
	if err != nil {
		return agent.Message{}, err
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	var res *http.Response
	// Retries only precede consumption of a response; never replay tool actions.
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", p.endpoint(), bytes.NewReader(data))
		if err != nil {
			return agent.Message{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Authorization", "Bearer "+p.Token)
		req.Header.Set("x-api-key", p.Token)
		req.Header.Set("User-Agent", "xloom/0.1")
		req.Header.Set("x-opencode-session", p.sessionID)
		res, err = client.Do(req)
		if err != nil {
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: err}
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			break
		}
		status := res.StatusCode
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		res.Body.Close()
		classified := classifyEndpointError(status, body)
		if classified.Kind == agent.ErrorContextOverflow {
			return agent.Message{}, classified
		}
		if attempt >= 2 || (status != 429 && status < 500) {
			return agent.Message{}, classified
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return agent.Message{}, ctx.Err()
		case <-timer.C:
		}
	}
	defer res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		var m struct {
			Role    string        `json:"role"`
			Content []agent.Block `json:"content"`
			Stop    string        `json:"stop_reason"`
			Usage   *agent.Usage  `json:"usage"`
		}
		if err = json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&m); err != nil {
			kind := agent.ErrorProvider
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
				kind = agent.ErrorTransport
			}
			return agent.Message{}, &agent.ModelError{Kind: kind, Err: errors.New("invalid or incomplete model response body")}
		}
		if m.Role != "assistant" || len(m.Content) == 0 || m.Stop == "" {
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorProvider, Err: errors.New("invalid model response")}
		}
		return agent.Message{Role: m.Role, Content: m.Content, StopReason: m.Stop, Usage: m.Usage}, nil
	}
	message, streamErr := consumeSSE(ctx, io.LimitReader(res.Body, 32<<20), emit)
	if streamErr != nil {
		var modelErr *agent.ModelError
		if !errors.As(streamErr, &modelErr) && !errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, context.DeadlineExceeded) {
			streamErr = &agent.ModelError{Kind: agent.ErrorProvider, Err: streamErr}
		}
	}
	return message, streamErr
}

func classifyEndpointError(status int, body []byte) *agent.ModelError {
	var detail struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &detail)
	kind := agent.ErrorProvider
	if status == 429 {
		kind = agent.ErrorRateLimit
	} else if status >= 500 {
		kind = agent.ErrorUnavailable
	}
	message := strings.ToLower(detail.Error.Message)
	if detail.Error.Type == "context_length_exceeded" || detail.Error.Type == "context_window_exceeded" ||
		((status == 400 || status == 413 || status == 422) && (strings.Contains(message, "prompt is too long") || strings.Contains(message, "context length") || strings.Contains(message, "maximum context") || strings.Contains(message, "context window"))) {
		kind = agent.ErrorContextOverflow
	}
	return &agent.ModelError{Kind: kind, Err: &HTTPError{Status: status}}
}

func mergeUsage(dst **agent.Usage, src *agent.Usage) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = &agent.Usage{}
	}
	if src.InputTokens > 0 {
		(*dst).InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		(*dst).OutputTokens = src.OutputTokens
	}
	if src.CacheReadTokens > 0 {
		(*dst).CacheReadTokens = src.CacheReadTokens
	}
	if src.CacheWriteTokens > 0 {
		(*dst).CacheWriteTokens = src.CacheWriteTokens
	}
}
func consumeSSE(ctx context.Context, reader io.Reader, emit agent.Emit) (agent.Message, error) {
	m := agent.Message{Role: "assistant", Content: []agent.Block{}}
	parts := map[int]*strings.Builder{}
	started := false
	stopped := false
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 64<<10), 8<<20)
	var lines []string
	consume := func() error {
		if len(lines) == 0 {
			return nil
		}
		data := strings.Join(lines, "\n")
		lines = nil
		if data == "[DONE]" {
			return nil
		}
		var e struct {
			Type    string      `json:"type"`
			Index   int         `json:"index"`
			Block   agent.Block `json:"content_block"`
			Message struct {
				Usage *agent.Usage `json:"usage"`
			} `json:"message"`
			Usage *agent.Usage `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Delta struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Partial   string `json:"partial_json"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
				Stop      string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return fmt.Errorf("invalid model stream event: %w", err)
		}
		switch e.Type {
		case "message_start":
			if started {
				return errors.New("duplicate message_start")
			}
			started = true
			mergeUsage(&m.Usage, e.Message.Usage)
		case "content_block_start":
			if !started || e.Index != len(m.Content) || e.Index > 4096 {
				return errors.New("invalid content block index")
			}
			for len(m.Content) <= e.Index {
				m.Content = append(m.Content, agent.Block{})
			}
			m.Content[e.Index] = e.Block
			if e.Block.Type == "tool_use" {
				parts[e.Index] = &strings.Builder{}
			}
		case "content_block_delta":
			if e.Index < 0 || e.Index >= len(m.Content) {
				return errors.New("delta without content block")
			}
			b := &m.Content[e.Index]
			switch e.Delta.Type {
			case "text_delta":
				if b.Type != "text" {
					return errors.New("text delta for a non-text block")
				}
				b.Text += e.Delta.Text
				if emit != nil {
					emit(agent.Event{Type: "text_delta", Text: e.Delta.Text})
				}
			case "input_json_delta":
				if parts[e.Index] == nil {
					return errors.New("tool delta without tool block")
				}
				parts[e.Index].WriteString(e.Delta.Partial)
				if emit != nil {
					emit(agent.Event{Type: "tool_delta", ToolID: b.ID, ToolName: b.Name, Text: e.Delta.Partial})
				}
			case "thinking_delta":
				b.Thinking += e.Delta.Thinking
			case "signature_delta":
				b.Signature += e.Delta.Signature
			}
		case "message_delta":
			mergeUsage(&m.Usage, e.Usage)
			if e.Delta.Stop != "" {
				m.StopReason = e.Delta.Stop
			}
		case "message_stop":
			stopped = true
		case "error":
			status := 400
			if e.Error.Type == "overloaded_error" || e.Error.Type == "api_error" {
				status = 503
			}
			if e.Error.Type == "rate_limit_error" {
				status = 429
			}
			return classifyEndpointError(status, []byte(data))
		}
		return nil
	}
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		line := strings.TrimSuffix(scan.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return m, err
			}
			if stopped {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scan.Err(); err != nil {
		return m, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("model stream read failed")}
	}
	if err := consume(); err != nil {
		return m, err
	}
	if !started || !stopped || len(m.Content) == 0 || m.StopReason == "" {
		return m, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("model stream ended before message_stop")}
	}
	for i, part := range parts {
		if part.Len() > 0 {
			raw := json.RawMessage(part.String())
			if !json.Valid(raw) {
				if m.StopReason != "max_tokens" && m.StopReason != "length" {
					return m, errors.New("invalid streamed tool arguments")
				}
				raw = json.RawMessage(`{}`)
			}
			m.Content[i].Input = raw
		}
	}
	seen := map[string]bool{}
	for _, b := range m.Content {
		if b.Type == "tool_use" && (b.ID == "" || b.Name == "") {
			return m, errors.New("tool call missing id or name")
		}
		if b.Type == "tool_use" {
			if seen[b.ID] {
				return m, errors.New("duplicate streamed tool call id")
			}
			seen[b.ID] = true
		}
	}
	return m, nil
}
