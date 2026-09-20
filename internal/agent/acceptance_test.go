package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type providerFunc func(context.Context, []Message, []Definition, Emit) (Message, error)

func (f providerFunc) Generate(c context.Context, m []Message, d []Definition, e Emit) (Message, error) {
	return f(c, m, d, e)
}
func call(id, name, raw string) Block {
	return Block{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(raw)}
}
func TestArgumentSchemaPreventsExecution(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`)
	for _, raw := range []string{`null`, `[]`, `{}`, `{"path":null}`, `{"path":12}`, `{"path":"x","limit":0}`, `{"path":"x","limit":1.5}`, `{"path":"x","extra":true}`} {
		t.Run(raw, func(t *testing.T) {
			p := &scripted{replies: []Message{{Role: "assistant", Content: []Block{call("id", "read", raw)}}, Text("assistant", "done")}}
			l := Loop{Provider: p, Tools: []Tool{{Definition: Definition{Name: "read", Schema: schema}, Execute: func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("invalid arguments reached tool")
				return "", nil
			}}}}
			if _, err := l.Run(context.Background(), "task"); err != nil {
				t.Fatal(err)
			}
			if !l.History[2].Content[0].IsError {
				t.Fatal("invalid input was not reported")
			}
		})
	}
}
func TestDuplicateAndMissingIDsFailBeforeActions(t *testing.T) {
	for _, calls := range [][]Block{{call("same", "x", `{}`), call("same", "x", `{}`)}, {call("", "x", `{}`)}} {
		p := &scripted{replies: []Message{{Role: "assistant", Content: calls}}}
		l := Loop{Provider: p, Tools: []Tool{{Definition: Definition{Name: "x"}, Execute: func(context.Context, json.RawMessage) (string, error) {
			t.Fatal("executed invalid call IDs")
			return "", nil
		}}}}
		if _, err := l.Run(context.Background(), "task"); err == nil {
			t.Fatal("accepted ambiguous call IDs")
		}
	}
}
func TestParallelResultsKeepModelOrder(t *testing.T) {
	ready := make(chan struct{})
	finish := make(chan struct{})
	p := &scripted{replies: []Message{{Role: "assistant", Content: []Block{call("1", "first", `{}`), call("2", "second", `{}`)}}, Text("assistant", "done")}}
	l := Loop{Provider: p, Tools: []Tool{
		{Definition: Definition{Name: "first"}, Parallel: true, Execute: func(context.Context, json.RawMessage) (string, error) { close(ready); <-finish; return "one", nil }},
		{Definition: Definition{Name: "second"}, Parallel: true, Execute: func(context.Context, json.RawMessage) (string, error) { <-ready; close(finish); return "two", nil }},
	}}
	if _, err := l.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	for i, b := range l.History[2].Content {
		if b.ToolUseID != []string{"1", "2"}[i] {
			t.Fatal("completion order replaced model order")
		}
	}
}
func TestSequentialCancellationSettlesRemainingIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	p := &scripted{replies: []Message{{Role: "assistant", Content: []Block{call("1", "write", `{}`), call("2", "write", `{}`)}}}}
	l := Loop{Provider: p, Tools: []Tool{{Definition: Definition{Name: "write"}, Execute: func(context.Context, json.RawMessage) (string, error) { n++; cancel(); return "written", nil }}}}
	if _, err := l.Run(ctx, "task"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if n != 1 || len(l.History[2].Content) != 2 || !l.History[2].Content[1].IsError {
		t.Fatal("cancel left unpaired calls or executed a later action")
	}
}
func TestToolPanicBecomesErrorResult(t *testing.T) {
	p := &scripted{replies: []Message{{Role: "assistant", Content: []Block{call("1", "x", `{}`)}}, Text("assistant", "done")}}
	l := Loop{Provider: p, Tools: []Tool{{Definition: Definition{Name: "x"}, Execute: func(context.Context, json.RawMessage) (string, error) { panic("test") }}}}
	if _, err := l.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	if !l.History[2].Content[0].IsError {
		t.Fatal("panic was not surfaced")
	}
}
func TestBoundarySteeringPrecedesFollowUp(t *testing.T) {
	steering := make(chan string, 1)
	follow := make(chan string, 1)
	follow <- "follow"
	prompts := []string{}
	n := 0
	p := providerFunc(func(_ context.Context, m []Message, _ []Definition, _ Emit) (Message, error) {
		prompts = append(prompts, m[len(m)-1].Text())
		n++
		if n == 1 {
			return Message{Role: "assistant", Content: []Block{call("1", "x", `{}`)}}, nil
		}
		return Text("assistant", "done"), nil
	})
	l := Loop{Provider: p, Steering: steering, FollowUp: follow, Tools: []Tool{{Definition: Definition{Name: "x"}, Execute: func(context.Context, json.RawMessage) (string, error) { steering <- "steer"; return "ok", nil }}}}
	if _, err := l.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prompts, []string{"task", "steer", "follow"}) {
		t.Fatal(prompts)
	}
}
func TestInterruptedSessionSettlesWithoutReplay(t *testing.T) {
	p := providerFunc(func(_ context.Context, m []Message, _ []Definition, _ Emit) (Message, error) {
		if len(m) != 3 || m[2].Content[0].ToolUseID != "pending" || !m[2].Content[0].IsError {
			t.Fatalf("invalid repair: %#v", m)
		}
		return Text("assistant", "recovered"), nil
	})
	l := Loop{Provider: p, History: []Message{Text("user", "task"), {Role: "assistant", Content: []Block{call("pending", "write", `{}`)}}}, Tools: []Tool{{Definition: Definition{Name: "write"}, Execute: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("replayed uncertain write")
		return "", nil
	}}}}
	if got, err := l.Run(context.Background(), ""); err != nil || got != "recovered" {
		t.Fatal(got, err)
	}
}
func TestOrphanedResultsRejected(t *testing.T) {
	l := Loop{Provider: &scripted{}, History: []Message{{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "missing", Content: json.RawMessage(`"x"`)}}}}}
	if _, err := l.Run(context.Background(), ""); err == nil {
		t.Fatal("accepted orphaned result")
	}
}
func TestCompactionRetainsPairedTailAndRejectsTruncation(t *testing.T) {
	history := []Message{Text("user", "task"), Text("assistant", "a"), Text("user", "b"), {Role: "assistant", Content: []Block{call("old", "read", `{}`)}}, {Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "old", Content: json.RawMessage(`"evidence"`)}}}, Text("assistant", "c"), Text("user", "d"), Text("assistant", "e"), Text("user", "f"), Text("assistant", "g")}
	l := Loop{History: history, ContextBytes: 1, Provider: providerFunc(func(_ context.Context, m []Message, d []Definition, _ Emit) (Message, error) {
		if len(d) != 0 || !strings.Contains(m[0].Text(), "task") {
			t.Fatal("bad summary request")
		}
		return Text("assistant", "goal and evidence"), nil
	})}
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.RepairHistory(); err != nil {
		t.Fatal(err)
	}
	l.History = history
	l.Provider = &scripted{replies: []Message{{Role: "assistant", Content: []Block{{Type: "text", Text: "partial"}}, StopReason: "max_tokens"}}}
	if err := l.compact(context.Background()); err == nil {
		t.Fatal("accepted truncated summary")
	}
}

func TestRepeatedCompactionPinsTaskAndConclusionVerbatim(t *testing.T) {
	const task = "Unique contract: JSON fact must contain proof and origin."
	const conclude = "Unique boundary: stop all exploration and summarize confirmed evidence."
	l := Loop{TaskPrompt: task, ConclusionPrompt: conclude, Concluding: true, ContextBytes: 1, Provider: providerFunc(func(context.Context, []Message, []Definition, Emit) (Message, error) {
		return Text("assistant", "generic summary"), nil
	})}
	l.History = []Message{Text("user", task), Text("assistant", "old"), Text("user", conclude)}
	for range 6 {
		l.History = append(l.History, Text("assistant", "evidence"), Text("user", "read more evidence"))
	}
	for range 2 {
		if err := l.compact(context.Background()); err != nil {
			t.Fatal(err)
		}
		all := ""
		for _, m := range l.History {
			all += m.Text() + "\n"
		}
		if !strings.Contains(all, task) || !strings.Contains(all, conclude) || l.History[0].Text() != task {
			t.Fatal("compaction lost pinned instructions", all)
		}
		if err := l.RepairHistory(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactionKeepsCurrentRuntimeInstructionAfterOldAssistant(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "conclusion", true: "repair"}[repair], func(t *testing.T) {
			instruction := "Stop exploration and summarize existing evidence"
			l := Loop{TaskPrompt: "original task", ConclusionPrompt: instruction, Concluding: true, ContextBytes: 1}
			if repair {
				instruction = "Rewrite the invalid result as complete JSON"
				l.Repairing = true
				l.RepairPrompt = instruction
			}
			l.History = []Message{Text("user", "original task")}
			for range 5 {
				l.History = append(l.History, Text("assistant", "old evidence"), Text("user", "next"))
			}
			l.History = append(l.History, Text("assistant", `{"invalid":`), Text("user", instruction))
			calls := 0
			l.Provider = providerFunc(func(_ context.Context, m []Message, d []Definition, _ Emit) (Message, error) {
				calls++
				if calls == 1 {
					return Text("assistant", "summary"), nil
				}
				if d != nil || m[len(m)-1].Role != "user" || m[len(m)-1].Text() != instruction {
					t.Fatal("compaction moved runtime instruction before old assistant response")
				}
				return Text("assistant", "complete rewritten result"), nil
			})
			if _, err := l.Run(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatal("unexpected calls", calls)
			}
		})
	}
}
