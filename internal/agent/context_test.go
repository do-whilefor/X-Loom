package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBudgetIncludesDefinitionsAndRejectsUnshrinkablePins(t *testing.T) {
	for _, test := range []struct {
		name, task, description string
		limit                   int
	}{
		{"task", strings.Repeat("fixed", 1000), "", 1000},
		{"tools", "task", strings.Repeat("schema", 1000), 1000},
		{"tiny", "task", "", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := Loop{ContextBytes: test.limit, TaskPrompt: test.task, History: []Message{Text("user", test.task)}, Tools: []Tool{{Definition: Definition{Name: "read", Description: test.description}}}, Provider: providerFunc(func(context.Context, []Message, []Definition, Emit) (Message, error) {
				t.Fatal("unshrinkable input reached provider")
				return Message{}, nil
			})}
			_, err := l.Run(context.Background(), "")
			var budget *ModelError
			if !errors.As(err, &budget) || budget.Kind != ErrorBudget {
				t.Fatal(err)
			}
		})
	}
}

func TestLargeSingleToolGroupCompactsAndPersistsReplayableRecord(t *testing.T) {
	evidence := strings.Repeat("log ", 20000) + "FINAL_PROOF"
	raw, _ := json.Marshal(evidence)
	history := []Message{Text("user", "task"), {Role: "assistant", Content: []Block{call("t", "read", `{}`)}}, {Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "t", Content: raw}}}}
	var saved *ContextCheckpoint
	var event *CompactionRecord
	l := Loop{History: history, ContextBytes: 6000, SummaryBytes: 1000, Provider: providerFunc(func(_ context.Context, m []Message, d []Definition, _ Emit) (Message, error) {
		if len(d) != 0 || !strings.Contains(m[0].Text(), "FINAL_PROOF") || !strings.Contains(m[0].Text(), "Middle omitted") {
			t.Fatal("summary lost tail evidence or invoked tools")
		}
		return Message{Role: "assistant", Content: []Block{{Type: "text", Text: "Tool t reported FINAL_PROOF; original evidence is sequence 3."}}, Usage: &Usage{InputTokens: 900, OutputTokens: 20}}, nil
	}), SaveState: func(view []Message, checkpoint *ContextCheckpoint) error { saved = checkpoint; return nil }, Emit: func(e Event) {
		if e.Compaction != nil {
			event = e.Compaction
		}
	}}
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if event == nil || saved.LastCompaction != event || event.SourceStart != 2 || event.SourceEnd != 3 || event.AfterBytes > 6000 || event.AfterBytes >= event.BeforeBytes || event.Usage.InputTokens != 900 {
		t.Fatalf("incomplete checkpoint: %#v", event)
	}
	if len(history) != 3 || !strings.Contains(string(history[2].Content[0].Content), "FINAL_PROOF") {
		t.Fatal("original transcript modified")
	}
	if err := l.RepairHistory(); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(saved)
	var restored ContextCheckpoint
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	recovered := Loop{History: restored.LastCompaction.View, Checkpoint: &restored}
	if err := recovered.initCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if err := recovered.append(Text("assistant", "after recovery")); err != nil {
		t.Fatal(err)
	}
	if recovered.History[len(recovered.History)-1].Sequence != 4 {
		t.Fatal("sequence restarted")
	}
}

func TestCompactionFailureKeepsPriorRequestView(t *testing.T) {
	for _, kind := range []string{"too large", "empty", "tool", "truncated", "persist"} {
		t.Run(kind, func(t *testing.T) {
			history := []Message{Text("user", "task"), Text("assistant", strings.Repeat("old ", 5000))}
			l := Loop{History: history, ContextBytes: 6000, SummaryBytes: 400, Provider: providerFunc(func(context.Context, []Message, []Definition, Emit) (Message, error) {
				switch kind {
				case "too large":
					return Text("assistant", strings.Repeat("big", 500)), nil
				case "empty":
					return Text("assistant", " "), nil
				case "tool":
					return Message{Role: "assistant", Content: []Block{call("x", "read", `{}`)}}, nil
				case "truncated":
					return Message{Role: "assistant", Content: []Block{{Type: "text", Text: "cut"}}, StopReason: "max_tokens"}, nil
				}
				return Text("assistant", "short summary"), nil
			}), SaveState: func([]Message, *ContextCheckpoint) error {
				if kind == "persist" {
					return errors.New("disk failed")
				}
				return nil
			}}
			if err := l.compact(context.Background()); err == nil {
				t.Fatal("unsafe checkpoint accepted")
			}
			if len(l.History) != 2 || l.History[1].Text() != history[1].Text() || l.Checkpoint.CompactionCount != 0 {
				t.Fatal("failed summary replaced history")
			}
		})
	}
}

func TestOverflowRecoveryOnceWithoutReplayingTools(t *testing.T) {
	for _, again := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "bounded"}[again], func(t *testing.T) {
			calls, tools, summaries := 0, 0, 0
			p := providerFunc(func(_ context.Context, m []Message, d []Definition, _ Emit) (Message, error) {
				if strings.HasPrefix(m[0].Text(), "Summarize this execution") {
					summaries++
					return Text("assistant", "The tool already ran; use its recorded proof."), nil
				}
				calls++
				if calls == 1 {
					return Message{Role: "assistant", Content: []Block{call("t", "read", `{}`)}}, nil
				}
				if calls == 2 || again {
					return Message{}, &ModelError{Kind: ErrorContextOverflow, Err: errors.New("too long")}
				}
				return Text("assistant", "done"), nil
			})
			l := Loop{Provider: p, ContextBytes: 30000, Tools: []Tool{{Definition: Definition{Name: "read"}, Execute: func(context.Context, json.RawMessage) (string, error) {
				tools++
				return strings.Repeat("evidence ", 1000), nil
			}}}}
			_, err := l.Run(context.Background(), "task")
			if (!again && err != nil) || (again && err == nil) || tools != 1 || summaries != 1 || l.Checkpoint.OverflowRetries != 1 {
				t.Fatal(err, tools, summaries, l.Checkpoint)
			}
			if again {
				restored := Loop{Provider: p, History: l.History, Checkpoint: l.Checkpoint, ContextBytes: 30000}
				if _, err := restored.Run(context.Background(), ""); err == nil || summaries != 1 {
					t.Fatal("resume renewed overflow budget", err, summaries)
				}
			}
		})
	}
}

func TestTransportDoesNotTriggerCompactionRecovery(t *testing.T) {
	n := 0
	l := Loop{Provider: providerFunc(func(context.Context, []Message, []Definition, Emit) (Message, error) {
		n++
		return Message{}, &ModelError{Kind: ErrorTransport, Err: errors.New("connection lost")}
	})}
	if _, err := l.Run(context.Background(), "task"); err == nil || n != 1 || l.Checkpoint.OverflowRetries != 0 {
		t.Fatal(err, n)
	}
}

func TestUsageBaselineDiscardedAfterCompaction(t *testing.T) {
	l := Loop{}
	m := Text("assistant", "kept")
	m.Sequence = 5
	m.Usage = &Usage{InputTokens: 90000, OutputTokens: 500}
	if count, basis := l.tokenEstimate([]Message{m}, 300, 0); count < 90000 || basis != "provider_usage_plus_estimated_tail" {
		t.Fatal(count, basis)
	}
	if count, basis := l.tokenEstimate([]Message{m}, 300, 5); count != 100 || basis != "request_bytes_div_3_estimate" {
		t.Fatal(count, basis)
	}
}

func TestCompactionCandidateIsLoggedBeforeCheckpointCommit(t *testing.T) {
	var events []Event
	l := Loop{TaskPrompt: "task", History: []Message{Text("user", "task"), Text("assistant", strings.Repeat("historical observation ", 1000))}, ContextBytes: 6000,
		Provider: providerFunc(func(context.Context, []Message, []Definition, Emit) (Message, error) {
			return Text("assistant", "observed evidence retained"), nil
		}),
		Emit: func(e Event) { events = append(events, e) },
		SaveState: func(_ []Message, checkpoint *ContextCheckpoint) error {
			if len(events) != 1 || events[0].Type != "context_compaction_prepared" || events[0].Compaction.Status != "prepared" || events[0].Compaction.Summary != checkpoint.LastCompaction.Summary || len(events[0].Compaction.View) == 0 {
				t.Fatal("checkpoint advanced without its complete source record")
			}
			return nil
		},
	}
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != "context_compacted" || events[0].Compaction.Status != "prepared" || events[1].Compaction.Status != "committed" {
		t.Fatal(events)
	}
}

type summaryAllowanceProvider struct{ limit int }

func (p *summaryAllowanceProvider) Generate(context.Context, []Message, []Definition, Emit) (Message, error) {
	return Message{}, errors.New("normal output allowance used for compaction")
}
func (p *summaryAllowanceProvider) GenerateSummary(_ context.Context, _ []Message, limit int, _ Emit) (Message, error) {
	p.limit = limit
	return Text("assistant", "concise evidence checkpoint"), nil
}

func TestSummaryAllowsReasoningWithoutInflatingVisibleContext(t *testing.T) {
	p := &summaryAllowanceProvider{}
	l := Loop{Provider: p, TaskPrompt: "task", History: []Message{Text("user", "task"), Text("assistant", strings.Repeat("observed history ", 1000))}, ContextBytes: 6000, SummaryBytes: 400}
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.limit != 16384 || len(l.Checkpoint.LastCompaction.Summary) > 400 || l.Checkpoint.LastCompaction.AfterBytes > 6000 {
		t.Fatal("reasoning and admitted text budgets were conflated", p.limit)
	}
}
