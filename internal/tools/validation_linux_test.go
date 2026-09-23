//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"xloom/internal/agent"
)

type validationProvider func([]agent.Message) (agent.Message, error)

func (p validationProvider) Generate(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
	return p(history)
}

func TestLoopRejectsBuiltInToolSchemasBeforeSideEffects(t *testing.T) {
	s := fidelitySet(t)
	putFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	invalid := []struct{ tool, input string }{
		{"read", `{"path":"target","offset":0}`},
		{"bash", `{"command":"printf changed > target","timeout":0}`},
		{"edit", `{"path":"target","oldText":"original","newText":"changed","extra":true}`},
		{"write", `{"path":"target","content":"changed","extra":true}`},
		{"grep", `{"pattern":"original","ignoreCase":"yes"}`},
		{"find", `{"pattern":"*","path":null}`},
		{"ls", `{"path":[]}`},
		{"write", `{}`},
		{"write", `null`},
		{"write", `{"path":`},
	}
	calls := make([]agent.Block, len(invalid))
	for i, item := range invalid {
		calls[i] = agent.Block{Type: "tool_use", ID: fmt.Sprintf("invalid-%d", i), Name: item.tool, Input: json.RawMessage(item.input)}
	}
	executed, turns := 0, 0
	all := s.All()
	for i := range all {
		execute := all[i].Execute
		all[i].Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
			executed++
			return execute(ctx, raw)
		}
	}
	loop := agent.Loop{Tools: all, Provider: validationProvider(func(history []agent.Message) (agent.Message, error) {
		turns++
		if turns == 1 {
			return agent.Message{Role: "assistant", Content: calls}, nil
		}
		if turns != 2 {
			t.Fatal("invalid calls did not settle")
		}
		results := history[len(history)-1].Content
		if len(results) != len(calls) {
			t.Fatalf("got %d results for %d calls", len(results), len(calls))
		}
		for i, result := range results {
			if result.Type != "tool_result" || result.ToolUseID != calls[i].ID || !result.IsError {
				t.Fatalf("invalid %s call lost its paired rejection: %+v", calls[i].Name, result)
			}
		}
		return agent.Text("assistant", "done"), nil
	})}
	if _, err := loop.Run(context.Background(), "Check invalid calls."); err != nil || turns != 2 || executed != 0 {
		t.Fatalf("schema dispatch: turns=%d executions=%d err=%v", turns, executed, err)
	}
	assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	if _, err := os.Stat(s.RunDir); !os.IsNotExist(err) {
		t.Fatalf("invalid calls created output artifacts: %v", err)
	}
}
