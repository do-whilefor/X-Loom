//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
)

const continueOutput = `{"accepted":true,"outcome":"continue","reason":"More work remains"}`
const incompleteOutput = `{"accepted":true,"outcome":"incomplete","reason":"Only 19 of 30 chunks have been verified"}`

func completedOutput(kind string) string {
	if kind == "bootstrap" {
		return `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"All 30 chunks verified"},"complete":{"description":"Goal reached"}}}`
	}
	return `{"accepted":true,"outcome":"completed","data":{"description":"All 30 chunks verified"}}`
}

func outcomeJob(t *testing.T, kind string) Job {
	j := scenarioJob(t, "", kind)
	j.ResultContractVersion = 1
	return j
}

func outcomeSession(t *testing.T, runDir string) session {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func progressTool(calls *int, fail bool) agent.Tool {
	return agent.Tool{Definition: agent.Definition{Name: "progress", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}, Execute: func(context.Context, json.RawMessage) (string, error) {
		*calls++
		if fail {
			return "", errors.New("blocked")
		}
		return "Chunk verified", nil
	}}
}

func progressCall(id int) agent.Message {
	return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: fmt.Sprintf("progress-%d", id), Name: "progress", Input: json.RawMessage(`{}`)}}}
}

func TestExecutionContinuesInSameRunUntilExplicitCompletion(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		t.Run(kind, func(t *testing.T) {
			j, runDir := outcomeJob(t, kind), t.TempDir()
			j.Budget.Timeout = 60
			start := time.Now()
			turns, toolCalls := 0, 0
			provider := scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
				turns++
				if len(defs) == 0 {
					t.Fatal("continuation disabled execution tools")
				}
				switch turns {
				case 1:
					return agent.Text("assistant", continueOutput), nil
				case 2:
					return progressCall(1), nil
				case 3:
					return agent.Text("assistant", completedOutput(kind)), nil
				default:
					t.Fatal("unexpected extra model turn")
					return agent.Message{}, nil
				}
			})
			r, err := Run(context.Background(), j, Options{Provider: provider, Tools: []agent.Tool{progressTool(&toolCalls, false)}, RunDir: runDir, Now: func() time.Time { return start }})
			if err != nil || r.Status != "success" || r.Conclude || turns != 3 || toolCalls != 1 {
				t.Fatalf("result=%+v err=%v turns=%d tool calls=%d", r, err, turns, toolCalls)
			}
			s := outcomeSession(t, runDir)
			if s.RunID != j.RunID || !s.ExecutionDeadline.Equal(start.Add(time.Minute)) || s.ContinuationCount != 0 || s.Concluding {
				t.Fatalf("continuation changed identity, deadline, or progress: %+v", s)
			}
		})
	}
}

func TestIncompleteNeverBecomesWorkerSuccess(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		for _, conclude := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conclude=%t", kind, conclude), func(t *testing.T) {
				stop := make(chan struct{})
				if conclude {
					close(stop)
				}
				r, err := Run(context.Background(), outcomeJob(t, kind), Options{RunDir: t.TempDir(), SoftStop: stop, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
					return agent.Text("assistant", incompleteOutput), nil
				})})
				if err != nil || r.Status != "failed" || r.FailureKind != "incomplete" || r.Retryable || r.Text != incompleteOutput || r.Conclude != conclude || !strings.Contains(r.Error, "19 of 30") {
					t.Fatalf("incomplete result=%+v err=%v", r, err)
				}
			})
		}
	}
}

func TestFormatRepairCanReturnToExecutionWithoutRefreshingBudget(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	j.Budget.Timeout = 60
	turns, toolCalls := 0, 0
	start := time.Now()
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		switch turns {
		case 1:
			return agent.Text("assistant", `{"accepted":true,"data":{"description":"Continuing..."}}`), nil
		case 2:
			if len(defs) != 0 {
				t.Fatal("repair exposed tools")
			}
			return agent.Text("assistant", continueOutput), nil
		case 3:
			if len(defs) == 0 {
				t.Fatal("repair exit did not restore execution tools")
			}
			return progressCall(1), nil
		case 4:
			return agent.Text("assistant", completedOutput("explore")), nil
		default:
			t.Fatal("unexpected model call")
			return agent.Message{}, nil
		}
	})
	r, err := Run(context.Background(), j, Options{Provider: provider, Tools: []agent.Tool{progressTool(&toolCalls, false)}, RunDir: runDir, Now: func() time.Time { return start }})
	s := outcomeSession(t, runDir)
	if err != nil || r.Status != "success" || r.Conclude || s.RepairCount != 1 || s.Repairing || s.RepairPending || s.RepairPrompt != "" || toolCalls != 1 || !s.ExecutionDeadline.Equal(start.Add(time.Minute)) {
		t.Fatalf("result=%+v err=%v repair=%d/%t calls=%d", r, err, s.RepairCount, s.Repairing, toolCalls)
	}
	if err := s.validate(s.Identity); err != nil {
		t.Fatalf("completed repair state cannot resume: %v", err)
	}
}

func TestContinuationLimitRequiresSuccessfulToolProgress(t *testing.T) {
	for _, toolFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tool_failure=%t", toolFailure), func(t *testing.T) {
			turns, toolCalls, controls := 0, 0, 0
			provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				turns++
				if turns > 12 {
					t.Fatal("unbounded continuation")
				}
				if turns%2 == 0 {
					return progressCall(turns), nil
				}
				controls++
				if controls == 5 {
					return agent.Text("assistant", completedOutput("explore")), nil
				}
				return agent.Text("assistant", continueOutput), nil
			})
			r, err := Run(context.Background(), outcomeJob(t, "explore"), Options{RunDir: t.TempDir(), Provider: provider, Tools: []agent.Tool{progressTool(&toolCalls, toolFailure)}})
			if err != nil {
				t.Fatal(err)
			}
			if toolFailure && (r.Status != "failed" || r.Retryable || controls != maxContinuations+1 || !strings.Contains(r.Error, "continuation_exhausted")) {
				t.Fatalf("failed tools renewed continuation allowance: %+v controls=%d", r, controls)
			}
			if !toolFailure && (r.Status != "success" || controls != 5 || toolCalls != 4) {
				t.Fatalf("successful progress did not renew allowance: %+v controls=%d calls=%d", r, controls, toolCalls)
			}
		})
	}
}

// Seed the exact durable boundary before/after continuation consumption; no
// previous tool is replayed when the follow-up instruction was not yet saved.
func seedOutcomeSession(t *testing.T, j Job, runDir, text string, consumed int, savedResult bool) {
	t.Helper()
	id, err := identityFor(j, runDir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(runDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	m := agent.Text("assistant", text)
	m.Sequence = 1
	s := session{SchemaVersion: sessionSchemaVersion, Identity: id, RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), History: []agent.Message{m}, ContextCheckpoint: &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 1}, ContinuationCount: consumed}
	if j.Budget.Timeout > 0 {
		s.ExecutionDeadline = s.StartedAt.Add(time.Duration(j.Budget.Timeout) * time.Second)
	}
	if consumed > 0 {
		s.ContinuationSequence = 1
	}
	if savedResult {
		s.Result = &Result{Type: "result", Status: "success", Text: text}
	}
	if err := journal.append(agent.Event{Type: "message_end", Message: &m}); err != nil {
		t.Fatal(err)
	}
	if err := s.save(runDir, journal); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRevalidatesControlAndIncompleteResults(t *testing.T) {
	for _, text := range []string{continueOutput, incompleteOutput} {
		for _, savedResult := range []bool{false, true} {
			for _, consumed := range []int{0, maxContinuations} {
				t.Run(fmt.Sprintf("%s/saved=%t/count=%d", text, savedResult, consumed), func(t *testing.T) {
					j, runDir := outcomeJob(t, "explore"), t.TempDir()
					seedOutcomeSession(t, j, runDir, text, consumed, savedResult)
					turns := 0
					r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
						turns++
						if len(defs) == 0 || turns > 1 {
							t.Fatal("resumed continuation did not enter execution exactly once")
						}
						return agent.Text("assistant", completedOutput("explore")), nil
					})})
					if err != nil {
						t.Fatal(err)
					}
					if text == incompleteOutput {
						if r.Status != "failed" || r.FailureKind != "incomplete" || turns != 0 {
							t.Fatalf("incomplete replayed as success: %+v turns=%d", r, turns)
						}
					} else if r.Status != "success" || turns != 1 {
						t.Fatalf("saved continuation was terminal or consumed twice: %+v turns=%d", r, turns)
					}
				})
			}
		}
	}
}

func TestSoftStopWinsOverContinueAndDisablesTools(t *testing.T) {
	stop := make(chan struct{})
	turns := 0
	r, err := Run(context.Background(), outcomeJob(t, "explore"), Options{RunDir: t.TempDir(), SoftStop: stop, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns == 1 {
			close(stop)
			return agent.Text("assistant", continueOutput), nil
		}
		if turns > 2 || len(defs) != 0 {
			t.Fatal("continue escaped soft stop")
		}
		return agent.Text("assistant", incompleteOutput), nil
	})})
	if err != nil || !r.Conclude || r.Status != "failed" || r.FailureKind != "incomplete" || turns != 2 {
		t.Fatalf("result=%+v err=%v turns=%d", r, err, turns)
	}
}

func TestRecoveredContinuationCannotResetItsAllowance(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	seedOutcomeSession(t, j, runDir, continueOutput, maxContinuations, false)
	turns := 0
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		if turns > 1 {
			t.Fatal("restart renewed the continuation allowance")
		}
		return agent.Text("assistant", continueOutput), nil
	})})
	if err != nil || r.Status != "failed" || r.Retryable || turns != 1 || !strings.Contains(r.Error, "continuation_exhausted") {
		t.Fatalf("result=%+v err=%v turns=%d", r, err, turns)
	}
}

func TestExpiredOriginalDeadlineCannotResumeExecution(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	j.Budget.Timeout = 1
	seedOutcomeSession(t, j, runDir, continueOutput, 1, false)
	original := outcomeSession(t, runDir).ExecutionDeadline
	now := original.Add(time.Second)
	turns := 0
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Now: func() time.Time { return now }, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns > 1 || len(defs) != 0 {
			t.Fatal("expired continuation regained execution tools")
		}
		return agent.Text("assistant", incompleteOutput), nil
	})})
	s := outcomeSession(t, runDir)
	if err != nil || !r.Conclude || r.FailureKind != "incomplete" || !s.ExecutionDeadline.Equal(original) || !s.ConcludeDeadline.Equal(now.Add(time.Duration(j.Budget.ConcludeTimeout)*time.Second)) {
		t.Fatalf("expired result=%+v err=%v deadlines=%v/%v", r, err, s.ExecutionDeadline, s.ConcludeDeadline)
	}
}

func TestRepairToolCallsStayDisabledUntilContinue(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	turns, toolCalls := 0, 0
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Tools: []agent.Tool{progressTool(&toolCalls, false)}, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		switch turns {
		case 1:
			return agent.Text("assistant", "not a result"), nil
		case 2:
			if len(defs) != 0 {
				t.Fatal("repair advertised tools")
			}
			return progressCall(1), nil
		case 3:
			if toolCalls != 0 || len(defs) != 0 {
				t.Fatal("repair executed an unadvertised tool")
			}
			return agent.Text("assistant", continueOutput), nil
		case 4:
			return progressCall(2), nil
		case 5:
			return agent.Text("assistant", completedOutput("explore")), nil
		default:
			t.Fatal("unexpected model request")
			return agent.Message{}, nil
		}
	})})
	s := outcomeSession(t, runDir)
	if err != nil || r.Status != "success" || toolCalls != 1 || s.RepairCount != 2 || s.Repairing || r.Conclude {
		t.Fatalf("result=%+v err=%v calls=%d repairs=%d", r, err, toolCalls, s.RepairCount)
	}
}

func TestRecoveryAfterRepairExitRetainsSpentRepairAttempts(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	turns := 0
	first, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		switch turns {
		case 1:
			return agent.Text("assistant", "invalid first response"), nil
		case 2:
			return agent.Text("assistant", continueOutput), nil
		case 3:
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorUnavailable, Err: errors.New("temporary outage")}
		default:
			t.Fatal("unexpected request before recovery")
			return agent.Message{}, nil
		}
	})})
	if err != nil || !first.Retryable || outcomeSession(t, runDir).RepairCount != 1 {
		t.Fatalf("first result=%+v err=%v", first, err)
	}
	turns = 0
	last, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		if turns > 2 {
			t.Fatal("recovery refunded consumed repairs")
		}
		return agent.Text("assistant", "still invalid"), nil
	})})
	if err != nil || last.Status != "failed" || last.Retryable || turns != 2 || !strings.Contains(last.Error, "repair exhausted") || outcomeSession(t, runDir).RepairCount != 2 {
		t.Fatalf("last result=%+v err=%v turns=%d", last, err, turns)
	}
}

func TestMockHonorsVersionedResultContract(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		j := outcomeJob(t, kind)
		j.WorkerType = "mock"
		r, err := Run(context.Background(), j, Options{RunDir: t.TempDir()})
		if err != nil || r.Status != "success" || !strings.Contains(r.Text, `"outcome":"completed"`) {
			t.Fatalf("versioned mock result=%+v err=%v", r, err)
		}
		t.Setenv("XLOOM_MOCK_"+strings.ToUpper(kind), `{"accepted":true,"data":{"description":"Continuing..."}}`)
		r, err = Run(context.Background(), j, Options{RunDir: t.TempDir()})
		if err != nil || r.Status != "failed" || r.FailureKind != "result_contract" {
			t.Fatalf("mock bypassed contract: %+v err=%v", r, err)
		}
	}
}

func TestBootstrapConclusionCannotTurnPartialEvidenceIntoSuccess(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	turns := 0
	r, err := Run(context.Background(), outcomeJob(t, "bootstrap"), Options{RunDir: t.TempDir(), SoftStop: stop, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if len(defs) != 0 || turns > 2 {
			t.Fatal("partial result escaped the bounded conclusion")
		}
		if turns == 1 {
			return agent.Text("assistant", `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Only 19 of 30 chunks verified"}}}`), nil
		}
		return agent.Text("assistant", incompleteOutput), nil
	})})
	if err != nil || r.Status != "failed" || r.FailureKind != "incomplete" || !r.Conclude || turns != 2 {
		t.Fatalf("partial evidence reported success: %+v err=%v turns=%d", r, err, turns)
	}
}
