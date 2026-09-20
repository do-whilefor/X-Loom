//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
)

func lastToolResults(history []agent.Message) []agent.Block {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "user" && len(m.Content) > 0 && m.Content[0].Type == "tool_result" {
			return m.Content
		}
	}
	return nil
}
func loadSession(t *testing.T, runDir string) session {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s session
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func noExecutionTool(t *testing.T, name string) agent.Tool {
	t.Helper()
	return agent.Tool{Definition: agent.Definition{Name: name}, Conclude: true, Execute: func(context.Context, json.RawMessage) (string, error) {
		t.Error("pure-output repair executed a tool")
		return "unexpected", nil
	}}
}

func TestReasonInvalidFinalOutputIsRepairedInSameSession(t *testing.T) {
	j := job(t, "reason")
	runDir := t.TempDir()
	calls := 0
	repaired := `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"Check the documented direction"}]}}`
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Tools: []agent.Tool{noExecutionTool(t, "read")}, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			return agent.Text("assistant", "I have a direction but no JSON yet"), nil
		}
		if d != nil || len(m) != 3 || m[1].Text() != "I have a direction but no JSON yet" {
			t.Fatal("repair did not preserve the same session or close tools")
		}
		if !strings.Contains(m[2].Text(), "invalid_contract") || !strings.Contains(m[2].Text(), "<task_graph>") {
			t.Fatal("missing diagnosis or original reasoning input")
		}
		return agent.Text("assistant", repaired), nil
	})})
	if err != nil || r.Status != "success" || r.Conclude || r.Text != repaired || calls != 2 {
		t.Fatal(r, calls, err)
	}
	saved := loadSession(t, runDir)
	if saved.RepairCount != 1 || saved.RepairPending || !saved.Repairing || saved.RepairReason != "invalid_contract" {
		t.Fatal(saved)
	}
}

func TestThinkingOnlyTruncationRepairsWithoutRepeatingActionsOrChangingSnapshot(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	stop := make(chan struct{})
	calls, actions := 0, 0
	var frozen string
	var deadline time.Time
	p := modelFunc(func(ctx context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		switch calls {
		case 1:
			return toolCall("write"), nil
		case 2:
			if d != nil {
				t.Fatal("conclusion left tools open")
			}
			frozen = m[len(m)-1].Text()
			deadline, _ = ctx.Deadline()
			os.WriteFile(filepath.Join(runDir, "output-proof.txt"), []byte("changed after conclusion"), 0600)
			return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "thinking", Thinking: "unfinished reasoning", Signature: "test"}}, StopReason: "max_tokens"}, nil
		default:
			if d != nil {
				t.Fatal("repair left tools open")
			}
			next, _ := ctx.Deadline()
			if !deadline.Equal(next) {
				t.Fatal("repair refreshed conclusion deadline")
			}
			if !strings.Contains(m[len(m)-1].Text(), "output_truncated") || !containsInstruction(m, frozen) {
				t.Fatal("repair lost truncation diagnosis or frozen input")
			}
			return agent.Text("assistant", conclusion), nil
		}
	})
	r, err := Run(context.Background(), j, Options{RunDir: runDir, SoftStop: stop, Provider: p, Tools: []agent.Tool{{Definition: agent.Definition{Name: "write"}, Execute: func(context.Context, json.RawMessage) (string, error) {
		actions++
		os.WriteFile(filepath.Join(runDir, "output-proof.txt"), []byte("confirmed before conclusion"), 0600)
		close(stop)
		return "confirmed before conclusion", nil
	}}}})
	if err != nil || r.Status != "success" || !r.Conclude || calls != 3 || actions != 1 {
		t.Fatal(r, calls, actions, err)
	}
	saved := loadSession(t, runDir)
	if saved.ConclusionPrompt != frozen || strings.Contains(frozen, "changed after conclusion") || saved.RepairCount != 1 {
		t.Fatal("repair rewrote frozen evidence or count")
	}
}

func TestTruncatedParseableJSONNeverSubmitsAndRepairIsBounded(t *testing.T) {
	for _, stop := range []string{"max_tokens", "length"} {
		for _, text := range []string{declined, `{"accepted":true,"data":{"complete":{"from":["origin"],"description":"untrusted truncated completion"}}}`} {
			t.Run(stop+text, func(t *testing.T) {
				calls := 0
				runDir := t.TempDir()
				r, err := Run(context.Background(), job(t, "reason"), Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
					calls++
					if calls > 1 && d != nil {
						t.Fatal("repair exposed tools")
					}
					return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "text", Text: text}}, StopReason: stop}, nil
				})})
				if err != nil || r.Status != "failed" || calls != 1+maxOutputRepairs || !strings.Contains(r.Error, "output_truncated") || !strings.Contains(r.Error, "exhausted") {
					t.Fatal(r, calls, err)
				}
				if saved := loadSession(t, runDir); saved.RepairCount != maxOutputRepairs {
					t.Fatal("repair allowance not persisted")
				}
			})
		}
	}
}

func TestInvalidContractRetriesOnlyTwiceAndDoesNotForceAcceptance(t *testing.T) {
	calls := 0
	r, err := Run(context.Background(), job(t, "reason"), Options{RunDir: t.TempDir(), Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Text("assistant", `{"accepted":true,"data":{"intents":"not an array"}}`), nil
	})})
	if err != nil || r.Status != "failed" || calls != 3 || !strings.Contains(r.Error, "invalid_contract") {
		t.Fatal(r, calls, err)
	}
}

func TestRepairToolCallsAreRefusedAndConsumeRemainingAttempt(t *testing.T) {
	calls := 0
	r, err := Run(context.Background(), job(t, "reason"), Options{RunDir: t.TempDir(), Tools: []agent.Tool{noExecutionTool(t, "write")}, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			return agent.Text("assistant", "bad output"), nil
		}
		if d != nil {
			t.Fatal("repair definitions not nil")
		}
		if calls == 2 {
			return toolCall("write"), nil
		}
		results := lastToolResults(m)
		if len(results) != 1 || !results[0].IsError || results[0].ToolUseID != "t1" {
			t.Fatal("tool refusal lost pairing")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || r.Text != declined || r.Conclude || calls != 3 {
		t.Fatal(r, calls, err)
	}
}

func TestRepairRetainsReasonDeadlineAndHardCancellation(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "hard stop"}[hard], func(t *testing.T) {
			j := job(t, "reason")
			j.Budget.Timeout = 1
			calls := 0
			var original time.Time
			var out bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, err := Run(ctx, j, Options{RunDir: t.TempDir(), Output: &out, Provider: modelFunc(func(ctx context.Context, _ []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls == 1 {
					original, _ = ctx.Deadline()
					return agent.Text("assistant", "invalid"), nil
				}
				deadline, _ := ctx.Deadline()
				if !deadline.Equal(original) || d != nil {
					t.Fatal("repair reset deadline or reopened tools")
				}
				if hard {
					cancel()
				}
				<-ctx.Done()
				return agent.Message{}, ctx.Err()
			})})
			if calls != 2 {
				t.Fatal(calls)
			}
			if hard {
				if !errors.Is(err, context.Canceled) || r.Type != "" || strings.Contains(out.String(), `"type":"result"`) {
					t.Fatal(r, err)
				}
			} else if err != nil || r.Status != "failed" || !strings.Contains(r.Error, "deadline") {
				t.Fatal(r, err)
			}
		})
	}
}

func TestRecoveryDoesNotSubmitAssistantBeforeUnansweredConclusion(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	frozen, err := conclusionInput(context.Background(), j, runDir, true)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: started, Concluding: true, ConcludeStartedAt: started, ConcludeDeadline: started.Add(time.Second), ConclusionInputVersion: conclusionInputVersion, ConclusionPrompt: frozen, History: []agent.Message{agent.Text("user", "task"), agent.Text("assistant", conclusion), agent.Text("user", frozen)}})
	called := false
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		called = true
		if d != nil || m[len(m)-1].Text() != frozen {
			t.Fatal("conclusion not resumed")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || !called || r.Text != declined {
		t.Fatal(r, called, err)
	}
}

func TestRepairRecoveryConsumesPersistedAllowance(t *testing.T) {
	valid := agent.Text("assistant", declined)
	invalid := agent.Text("assistant", "still invalid")
	partial := agent.Text("assistant", `{"accepted":true,"data":{}}`)
	partial.StopReason = "max_tokens"
	for _, tc := range []struct {
		name                 string
		count                int
		pending, appended    bool
		response             *agent.Message
		wantCalls, wantCount int
		wantStatus           string
	}{
		{"queued before prompt", 1, true, false, nil, 1, 1, "success"},
		{"request possibly sent", 1, true, true, nil, 1, 2, "success"},
		{"last request possibly sent", 2, true, true, nil, 0, 2, "failed"},
		{"invalid response saved", 1, false, true, &invalid, 1, 2, "success"},
		{"truncated valid response saved", 1, false, true, &partial, 1, 2, "success"},
		{"valid response saved", 1, false, true, &valid, 0, 1, "success"},
		{"exhausted invalid response saved", 2, false, true, &invalid, 0, 2, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := job(t, "reason")
			runDir := t.TempDir()
			now := time.Now()
			deadline := now.Add(5 * time.Second)
			instruction, err := repairInstruction(j, false, tc.count, &outputFailure{Reason: "invalid_contract", Detail: "invalid saved answer"})
			if err != nil {
				t.Fatal(err)
			}
			history := []agent.Message{agent.Text("user", "original task"), agent.Text("assistant", "invalid original response")}
			if tc.appended {
				history = append(history, agent.Text("user", instruction))
			}
			if tc.response != nil {
				history = append(history, *tc.response)
			}
			writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: now, ReasonDeadline: deadline, History: history, Repairing: true, RepairCount: tc.count, RepairPending: tc.pending, RepairReason: "invalid_contract", RepairPrompt: instruction})
			calls := 0
			r, err := Run(context.Background(), j, Options{RunDir: runDir, Tools: []agent.Tool{noExecutionTool(t, "write")}, Provider: modelFunc(func(ctx context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if d != nil || m[len(m)-1].Role != "user" || !strings.Contains(m[len(m)-1].Text(), "Result-format repair") {
					t.Fatal("repair recovery reopened tools or lost instruction")
				}
				end, _ := ctx.Deadline()
				if time.Until(end) > 6*time.Second {
					t.Fatal("repair recovery refreshed reason deadline")
				}
				return agent.Text("assistant", declined), nil
			})})
			if err != nil || r.Status != tc.wantStatus || calls != tc.wantCalls {
				t.Fatal(r, calls, err)
			}
			saved := loadSession(t, runDir)
			if saved.RepairCount != tc.wantCount || !saved.ReasonDeadline.Equal(deadline) {
				t.Fatal("recovery reset allowance or deadline")
			}
			if r.Status == "success" && r.Text != declined {
				t.Fatal("submitted truncated or invalid cached prefix", r)
			}
		})
	}
}

func TestCachedTruncatedSuccessIsNotReemitted(t *testing.T) {
	j := job(t, "reason")
	runDir := t.TempDir()
	m := agent.Text("assistant", `{"accepted":true,"data":{"complete":{"from":["origin"],"description":"partial completion"}}}`)
	m.StopReason = "length"
	writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), History: []agent.Message{agent.Text("user", "task"), m}, Result: &Result{Type: "result", Status: "success", Text: m.Text()}})
	calls := 0
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, _ []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if d != nil {
			t.Fatal("cached repair reopened tools")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || r.Text != declined || calls != 1 {
		t.Fatal(r, calls, err)
	}
}

func TestRepairRecoverySettlesUncertainCallsWithoutExecutingThem(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	frozen, err := conclusionInput(context.Background(), j, runDir, true)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now()
	m := toolCall("write")
	m.StopReason = "max_tokens"
	instruction, err := repairInstruction(j, true, 1, &outputFailure{Reason: "output_truncated", Detail: "interrupted truncated response"})
	if err != nil {
		t.Fatal(err)
	}
	writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: when, Concluding: true, ConcludeStartedAt: when, ConcludeDeadline: when.Add(time.Second), ConclusionInputVersion: conclusionInputVersion, ConclusionPrompt: frozen, Repairing: true, RepairCount: 1, RepairReason: "output_truncated", RepairPrompt: instruction, RepairPending: true, History: []agent.Message{agent.Text("user", "task"), m}})
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Tools: []agent.Tool{noExecutionTool(t, "write")}, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if d != nil || !containsInstruction(m, frozen) {
			t.Fatal("repair recovery lost frozen evidence")
		}
		results := lastToolResults(m)
		if len(results) != 1 || results[0].ToolUseID != "t1" || !results[0].IsError {
			t.Fatal("uncertain action was not settled")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || !r.Conclude {
		t.Fatal(r, err)
	}
}

func TestTransportFailureDoesNotTriggerResultRepair(t *testing.T) {
	runDir := t.TempDir()
	calls := 0
	r, err := Run(context.Background(), job(t, "reason"), Options{RunDir: runDir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Message{}, errors.New("simulated transport failure")
	})})
	if err != nil || r.Status != "failed" || calls != 1 {
		t.Fatal(r, calls, err)
	}
	if saved := loadSession(t, runDir); saved.RepairCount != 0 || saved.Repairing {
		t.Fatal("retried a transport error as output format")
	}
}
