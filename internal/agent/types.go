// Package agent implements the small, two-level Pi-style execution loop.
package agent

import (
	"context"
	"encoding/json"
)

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

// Anthropic block schemas require these fields even when the provider emits
// an empty value. In particular, a signature-only thinking block must replay
// with thinking:""; omitting it causes a validation error on the next turn.
func (b Block) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	case "thinking":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{b.Type, b.Thinking, b.Signature})
	case "redacted_thinking":
		return json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{b.Type, b.Data})
	default:
		type plain Block
		return json.Marshal(plain(b))
	}
}

type Message struct {
	Role       string  `json:"role"`
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason,omitempty"`
	// Sequence and Usage are local transcript metadata, never request fields.
	Sequence uint64 `json:"sequence,omitempty"`
	Usage    *Usage `json:"usage,omitempty"`
}
type Usage struct {
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_creation_input_tokens,omitempty"`
}

func (u Usage) Input() int { return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens }

type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
}
type Tool struct {
	Definition
	Parallel bool
	Conclude bool // Legacy tool hint; Loop.Concluding disables every tool.
	Execute  func(context.Context, json.RawMessage) (string, error)
}
type Event struct {
	Type       string            `json:"type"`
	Text       string            `json:"text,omitempty"`
	ToolID     string            `json:"tool_id,omitempty"`
	ToolName   string            `json:"tool_name,omitempty"`
	Error      string            `json:"error,omitempty"`
	Message    *Message          `json:"message,omitempty"`
	Compaction *CompactionRecord `json:"compaction,omitempty"`
}
type Emit func(Event)
type Provider interface {
	Generate(context.Context, []Message, []Definition, Emit) (Message, error)
}

// SummaryProvider allows a separate output allowance without changing the
// provider's normal response limit or reasoning strength.
type SummaryProvider interface {
	GenerateSummary(context.Context, []Message, int, Emit) (Message, error)
}

// RequestSizer includes the provider's system prompt and tool declarations.
type RequestSizer interface {
	InputBytes([]Message, []Definition) (int, error)
}

type ErrorKind string

const (
	ErrorContextOverflow ErrorKind = "context_overflow"
	ErrorTransport       ErrorKind = "transport"
	ErrorRateLimit       ErrorKind = "rate_limit"
	ErrorUnavailable     ErrorKind = "unavailable"
	ErrorProvider        ErrorKind = "provider"
	ErrorBudget          ErrorKind = "context_budget"
)

type ModelError struct {
	Kind ErrorKind
	Err  error
}

func (e *ModelError) Error() string { return string(e.Kind) + ": " + e.Err.Error() }
func (e *ModelError) Unwrap() error { return e.Err }

func Text(role, text string) Message {
	return Message{Role: role, Content: []Block{{Type: "text", Text: text}}}
}
func (m Message) Text() string {
	var s string
	for _, b := range m.Content {
		if b.Type == "text" {
			if s != "" {
				s += "\n"
			}
			s += b.Text
		}
	}
	return s
}
