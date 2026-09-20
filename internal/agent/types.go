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
type Message struct {
	Role       string  `json:"role"`
	Content    []Block `json:"content"`
	StopReason string  `json:"-"`
}
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
}
type Tool struct {
	Definition
	Parallel bool
	Conclude bool
	Execute  func(context.Context, json.RawMessage) (string, error)
}
type Event struct {
	Type     string   `json:"type"`
	Text     string   `json:"text,omitempty"`
	ToolID   string   `json:"tool_id,omitempty"`
	ToolName string   `json:"tool_name,omitempty"`
	Error    string   `json:"error,omitempty"`
	Message  *Message `json:"message,omitempty"`
}
type Emit func(Event)
type Provider interface {
	Generate(context.Context, []Message, []Definition, Emit) (Message, error)
}

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
