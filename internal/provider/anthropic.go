// Package provider implements the configured Anthropic-compatible endpoint.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"xloom/internal/agent"
)

const System = "Work toward the assigned task using available tools. Distinguish confirmed facts from guesses. Follow the task's result contract."

type Anthropic struct {
	BaseURL   string
	Token     string
	Model     string
	MaxTokens int
	Timeout   time.Duration
	Client    *http.Client
}
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("model endpoint returned HTTP %d", e.Status) }
func (p *Anthropic) endpoint() string {
	base := strings.TrimRight(p.BaseURL, "/")
	if strings.HasSuffix(base, "/messages") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}
func (p *Anthropic) Generate(ctx context.Context, messages []agent.Message, tools []agent.Definition, emit agent.Emit) (agent.Message, error) {
	limit := p.MaxTokens
	if limit <= 0 {
		limit = 8192
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload := struct {
		Model     string             `json:"model"`
		MaxTokens int                `json:"max_tokens"`
		System    string             `json:"system"`
		Messages  []agent.Message    `json:"messages"`
		Tools     []agent.Definition `json:"tools,omitempty"`
		Stream    bool               `json:"stream"`
	}{p.Model, limit, System, messages, tools, true}
	data, err := json.Marshal(payload)
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
		res, err = client.Do(req)
		if err != nil {
			return agent.Message{}, err
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			break
		}
		status := res.StatusCode
		io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
		res.Body.Close()
		if attempt >= 2 || (status != 429 && status < 500) {
			return agent.Message{}, &HTTPError{Status: status}
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
		}
		if err = json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&m); err != nil {
			return agent.Message{}, err
		}
		if m.Role != "assistant" || len(m.Content) == 0 {
			return agent.Message{}, errors.New("invalid model response")
		}
		return agent.Message{Role: m.Role, Content: m.Content, StopReason: m.Stop}, nil
	}
	return consumeSSE(ctx, res.Body, emit)
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
			Type  string      `json:"type"`
			Index int         `json:"index"`
			Block agent.Block `json:"content_block"`
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
			started = true
		case "content_block_start":
			if e.Index < 0 || e.Index > 4096 {
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
				b.Text += e.Delta.Text
				if emit != nil {
					emit(agent.Event{Type: "text_delta", Text: e.Delta.Text})
				}
			case "input_json_delta":
				if parts[e.Index] == nil {
					return errors.New("tool delta without tool block")
				}
				parts[e.Index].WriteString(e.Delta.Partial)
			case "thinking_delta":
				b.Thinking += e.Delta.Thinking
			case "signature_delta":
				b.Signature += e.Delta.Signature
			}
		case "message_delta":
			if e.Delta.Stop != "" {
				m.StopReason = e.Delta.Stop
			}
		case "message_stop":
			stopped = true
		case "error":
			return errors.New("model stream reported an error")
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
		return m, err
	}
	if err := consume(); err != nil {
		return m, err
	}
	if !started || !stopped {
		return m, errors.New("model stream ended before message_stop")
	}
	for i, part := range parts {
		if part.Len() > 0 {
			raw := json.RawMessage(part.String())
			if !json.Valid(raw) {
				if m.StopReason != "max_tokens" {
					return m, errors.New("invalid streamed tool arguments")
				}
				raw = json.RawMessage(`{}`)
			}
			m.Content[i].Input = raw
		}
	}
	for _, b := range m.Content {
		if b.Type == "tool_use" && (b.ID == "" || b.Name == "") {
			return m, errors.New("tool call missing id or name")
		}
	}
	return m, nil
}
