//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xloom/internal/agent"
)

func TestCTFSubmissionRulesReachModelAndPersistAcrossPhases(t *testing.T) {
	for _, phase := range []string{"execute", "conclude", "repair"} {
		t.Run(phase, func(t *testing.T) {
			job := scenarioJob(t, "ctf", "explore")
			job.ResultContractVersion = 1
			runDir := t.TempDir()
			var stop chan struct{}
			if phase == "conclude" {
				stop = make(chan struct{})
				close(stop)
			}
			calls := 0
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				var input string
				for _, message := range history {
					input += message.Text() + "\n"
				}
				for _, rule := range []string{"high confidence", "Never use the submission API to brute force", "API response explicitly confirms acceptance"} {
					if !strings.Contains(input, rule) {
						t.Fatalf("model input lost submission rule %q in %s", rule, phase)
					}
				}
				restricted := phase == "conclude" || (phase == "repair" && calls == 2)
				if restricted != (len(definitions) == 0) {
					t.Fatalf("wrong phase capabilities: restricted=%v definitions=%v", restricted, definitions)
				}
				if strings.Count(input, ctfPolicy) != 1 || strings.Count(input, "<task_graph>") != 1 || strings.Contains(input, ctfExecution) {
					t.Fatal("task policy was duplicated or execution instructions leaked into phase history")
				}
				submission := false
				for _, definition := range definitions {
					if definition.Name == "bash" {
						submission = strings.Contains(definition.Description, "${TSEC_SERVER_HOST}/api/submit") && strings.Contains(definition.Description, "${TSEC_AGENT_TOKEN}")
					}
				}
				if submission == restricted {
					t.Fatal("CTF submission recipe must exist only in executable tool definitions")
				}
				if phase == "repair" && calls == 1 {
					return agent.Text("assistant", "A candidate flag is not enough to claim submission success."), nil
				}
				return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic fixture: no high-confidence flag or confirmed submission response."}`), nil
			})
			result, err := Run(context.Background(), job, Options{RunDir: runDir, Provider: provider, SoftStop: stop})
			wantCalls := 1
			if phase == "repair" {
				wantCalls = 2
			}
			if err != nil || result.Status != "success" || calls != wantCalls {
				t.Fatalf("run: %+v, %v; model calls=%d", result, err, calls)
			}
			raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved session
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(saved.TaskPrompt, ctfPolicy) || strings.Contains(saved.ConclusionPrompt+saved.RepairPrompt, ctfPolicy) {
				t.Fatal("durable phase prompts did not retain exactly one shared CTF policy")
			}
		})
	}
}
