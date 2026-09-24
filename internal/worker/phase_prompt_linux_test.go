//go:build linux

package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xloom/internal/agent"
)

func phaseHistoryText(history []agent.Message) string {
	var text strings.Builder
	for _, message := range history {
		text.WriteString(message.Text())
		text.WriteByte('\n')
	}
	return text.String()
}

func checkSharedPhaseInput(t *testing.T, history []agent.Message, policy string) {
	t.Helper()
	input := phaseHistoryText(history)
	if strings.Count(input, "<task_graph>") != 1 || strings.Count(input, policy) != 1 {
		t.Fatal("request lost or duplicated its original task graph or scenario policy")
	}
	if strings.Count(input, "Environment:") != 1 {
		t.Fatal("request lost or duplicated its initial environment")
	}
	if strings.Contains(input, ctfExecution) {
		t.Fatal("execution-only recipe leaked into pinned phase history")
	}
}

func TestPhaseDeltasSurviveConclusionRepairsAndRecovery(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "after execution", true: "direct conclusion"}[direct], func(t *testing.T) {
			job, dir := scenarioJob(t, "ctf", "explore"), t.TempDir()
			job.ResultContractVersion = 2
			stop := make(chan struct{})
			if direct {
				close(stop)
			}
			calls, concludingCalls := 0, 0
			opts := Options{RunDir: dir, SoftStop: stop, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				checkSharedPhaseInput(t, history, ctfPolicy)
				if !direct && calls == 1 {
					if len(definitions) == 0 {
						t.Fatal("normal execution lost its tools")
					}
					close(stop)
					return agent.Text("assistant", continueOutput), nil
				}
				concludingCalls++
				if len(definitions) != 0 || !strings.Contains(phaseHistoryText(history), "do not return continue") {
					t.Fatal("conclusion or repair lost the conclusion boundary")
				}
				if concludingCalls <= 2 {
					return agent.Text("assistant", "invalid result"), nil
				}
				return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("synthetic interrupted second repair")}
			})}
			first, err := Run(context.Background(), job, opts)
			if err != nil || !first.Retryable || !first.Conclude || concludingCalls != 3 {
				t.Fatalf("expected recoverable repair interruption: result=%+v err=%v calls=%d", first, err, concludingCalls)
			}
			before := outcomeSession(t, dir)
			if before.RepairCount != 2 || strings.Contains(before.ConclusionPrompt+before.RepairPrompt, ctfPolicy) {
				t.Fatal("phase state copied the common policy or lost repair accounting")
			}
			resumedCalls := 0
			opts.Provider = scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				resumedCalls++
				checkSharedPhaseInput(t, history, ctfPolicy)
				if len(definitions) != 0 || !strings.Contains(phaseHistoryText(history), before.ConclusionPrompt) || history[len(history)-1].Text() != before.RepairPrompt {
					t.Fatal("recovery changed the frozen conclusion or pending repair")
				}
				return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic fixture has no verified result"}`), nil
			})
			last, err := Run(context.Background(), job, opts)
			after := outcomeSession(t, dir)
			if err != nil || last.Status != "success" || resumedCalls != 1 || after.RepairCount != before.RepairCount || !after.ConcludeDeadline.Equal(before.ConcludeDeadline) || after.TaskPrompt != before.TaskPrompt || after.ConclusionPrompt != before.ConclusionPrompt {
				t.Fatalf("recovery changed task, phase or budget: result=%+v err=%v calls=%d", last, err, resumedCalls)
			}
		})
	}
}

func TestPhaseDeltasRetainTaskThroughCompaction(t *testing.T) {
	job, dir := scenarioJob(t, "pentest", "explore"), t.TempDir()
	job.ResultContractVersion = 2
	task, err := Prompt(job, false, dir)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, _, err := conclusionInputWithEvidence(context.Background(), job, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := repairInstruction(job, true, 2, &outputFailure{Reason: "invalid_contract", Detail: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	requests, summaries := 0, 0
	loop := agent.Loop{
		TaskPrompt: task, ConclusionPrompt: conclusion, RepairPrompt: repair, Concluding: true, Repairing: true,
		ContextBytes: 16000, SummaryBytes: 2048, RecentBytes: 128,
		History: []agent.Message{agent.Text("user", task), agent.Text("assistant", strings.Repeat("Old unverified observation. ", 3000)), agent.Text("user", conclusion), agent.Text("assistant", "invalid result"), agent.Text("user", repair)},
		Provider: scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
			checkSharedPhaseInput(t, history, pentestPolicy)
			if len(definitions) != 0 {
				t.Fatal("pure-output compaction exposed tools")
			}
			if strings.HasPrefix(history[0].Text(), "Summarize for continuation") {
				summaries++
				return agent.Text("assistant", `{"notes":"No confirmed result; original task remains incomplete.","quotes":[]}`), nil
			}
			requests++
			input := phaseHistoryText(history)
			if !strings.Contains(input, conclusion) || !strings.Contains(input, repair) || !strings.Contains(input, "Synthetic local fixture") {
				t.Fatal("compaction lost original requirements or active phase restrictions")
			}
			return agent.Text("assistant", incompleteOutput), nil
		}),
	}
	if _, err := loop.Run(context.Background(), ""); err != nil || requests != 1 || summaries != 1 || loop.Checkpoint.CompactionCount != 1 {
		t.Fatalf("compaction failed: err=%v requests=%d summaries=%d", err, requests, summaries)
	}
}
