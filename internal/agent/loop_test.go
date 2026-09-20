package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type scripted struct {
	replies []Message
	calls   int
}

func (p *scripted) Generate(_ context.Context, _ []Message, _ []Definition, _ Emit) (Message, error) {
	if p.calls >= len(p.replies) {
		return Message{}, errors.New("unexpected model call")
	}
	m := p.replies[p.calls]
	p.calls++
	return m, nil
}
func TestToolsAndFollowUp(t *testing.T) {
	q := make(chan string, 1)
	q <- "Continue in the same session"
	p := &scripted{replies: []Message{{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)}}}, Text("assistant", "first"), Text("assistant", "second")}}
	l := Loop{Provider: p, FollowUp: q, Tools: []Tool{{Definition: Definition{Name: "read"}, Execute: func(context.Context, json.RawMessage) (string, error) { return "evidence", nil }}}}
	got, err := l.Run(context.Background(), "task")
	if err != nil || got != "second" || p.calls != 3 {
		t.Fatalf("%q %v calls=%d", got, err, p.calls)
	}
	if l.History[2].Content[0].ToolUseID != "t1" {
		t.Fatal("lost tool pairing")
	}
}
func TestTruncatedToolDoesNotExecute(t *testing.T) {
	p := &scripted{replies: []Message{{Role: "assistant", StopReason: "max_tokens", Content: []Block{{Type: "tool_use", ID: "t", Name: "write", Input: json.RawMessage(`{}`)}}}, Text("assistant", "done")}}
	called := false
	l := Loop{Provider: p, Tools: []Tool{{Definition: Definition{Name: "write"}, Execute: func(context.Context, json.RawMessage) (string, error) { called = true; return "", nil }}}}
	_, err := l.Run(context.Background(), "task")
	if err != nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	if !l.History[2].Content[0].IsError {
		t.Fatal("missing truncation result")
	}
}
func TestConcludeBlocksExploration(t *testing.T) {
	p := &scripted{replies: []Message{{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "t", Name: "bash", Input: json.RawMessage(`{}`)}}}, Text("assistant", "summary")}}
	l := Loop{Provider: p, Concluding: true, Tools: []Tool{{Definition: Definition{Name: "bash"}, Execute: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("executed exploration during conclude")
		return "", nil
	}}}}
	if _, err := l.Run(context.Background(), "summarize"); err != nil {
		t.Fatal(err)
	}
}
