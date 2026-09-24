package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type stopResultProvider struct {
	calls   int
	message Message
}

func (p *stopResultProvider) Generate(context.Context, []Message, []Definition, Emit) (Message, error) {
	p.calls++
	if p.calls != 1 {
		return Message{}, errors.New("model called after the authoritative commit")
	}
	return p.message, nil
}

func TestStopResultSettlesToolGroupWithoutMoreSideEffectsOrModelCalls(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "normal"
		if cancelled {
			name = "lease_cancelled_after_commit"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const authoritative = `{"accepted":true,"data":{"decided":true}}`
			committed := false
			executed := []string{}
			provider := &stopResultProvider{message: Message{Role: "assistant", StopReason: "tool_use", Content: []Block{
				{Type: "tool_use", ID: "before", Name: "read", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "commit", Name: "commit", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "after", Name: "write", Input: json.RawMessage(`{}`)},
			}}}
			makeTool := func(name string, effect func()) Tool {
				return Tool{Definition: Definition{Name: name, Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, Parallel: true, Execute: func(context.Context, json.RawMessage) (string, error) {
					executed = append(executed, name)
					if effect != nil {
						effect()
					}
					return name + " finished", nil
				}}
			}
			followUp := make(chan string, 1)
			followUp <- "Continue with more work"
			var saved []Message
			var events []Event
			loop := &Loop{
				Provider: provider,
				Tools: []Tool{makeTool("read", nil), makeTool("commit", func() {
					committed = true
					if cancelled {
						cancel()
					}
				}), makeTool("write", nil)},
				FollowUp:   followUp,
				StopResult: func() (string, bool) { return authoritative, committed },
				SaveState: func(history []Message, _ *ContextCheckpoint) error {
					saved = append([]Message{}, history...)
					return nil
				},
				Emit: func(event Event) { events = append(events, event) },
				OnTurnEnd: func(context.Context, *Loop, Message) (context.Context, string, error) {
					return nil, "", errors.New("result repair/continuation ran after the authoritative commit")
				},
			}
			result, err := loop.Run(ctx, "Create and commit one bounded plan")
			if err != nil || result != authoritative || provider.calls != 1 || strings.Join(executed, ",") != "read,commit" {
				t.Fatalf("runtime failed to stop at commit: result=%q err=%v calls=%d effects=%v", result, err, provider.calls, executed)
			}
			if len(followUp) != 1 {
				t.Fatal("commit consumed queued follow-up work")
			}
			if len(saved) == 0 || saved[len(saved)-1].Role != "user" {
				t.Fatal("tool results were not durably settled before returning")
			}
			results := saved[len(saved)-1].Content
			if len(results) != 3 {
				t.Fatalf("pending tool group was left unsettled: %+v", results)
			}
			for i, id := range []string{"before", "commit", "after"} {
				if results[i].Type != "tool_result" || results[i].ToolUseID != id || results[i].IsError != (id == "after") {
					t.Fatalf("wrong result identity or execution status: %+v", results[i])
				}
			}
			if !strings.Contains(string(results[2].Content), "not executed") {
				t.Fatal("skipped side effect was not explicitly marked as unexecuted")
			}
			if err := (&Loop{History: saved}).RepairHistory(); err != nil {
				t.Fatalf("stopping after commit left an unrecoverable transcript: %v", err)
			}
			foundResult := false
			for _, event := range events {
				if event.Type == "runtime_result" && event.Text == authoritative {
					foundResult = true
				}
			}
			if !foundResult {
				t.Fatal("authoritative result was not recorded in the event stream")
			}
		})
	}
}
