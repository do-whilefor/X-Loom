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
				input := history[len(history)-1].Text()
				for _, rule := range []string{"high confidence", "Never use the submission API to brute force", "API response explicitly confirms acceptance", "${TSEC_SERVER_HOST}/api/submit", "${TSEC_AGENT_TOKEN}"} {
					if !strings.Contains(input, rule) {
						t.Fatalf("model input lost submission rule %q in %s", rule, phase)
					}
				}
				restricted := phase == "conclude" || (phase == "repair" && calls == 2)
				if restricted != (len(definitions) == 0) {
					t.Fatalf("wrong phase capabilities: restricted=%v definitions=%v", restricted, definitions)
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
			prompt := map[string]string{"execute": saved.TaskPrompt, "conclude": saved.ConclusionPrompt, "repair": saved.RepairPrompt}[phase]
			if !strings.Contains(prompt, ctfPolicy) {
				t.Fatal("durable phase prompt lost the CTF submission policy")
			}
		})
	}
}
