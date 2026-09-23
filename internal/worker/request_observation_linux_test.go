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

func TestExecutePersistsRequestObservationsWithoutDoubleCountingUsage(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	turns, toolCalls := 0, 0
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		m := agent.Text("assistant", completedOutput("explore"))
		if turns == 1 {
			m = progressCall(1)
		}
		m.Usage = &agent.Usage{InputTokens: 19, OutputTokens: 7, CacheReadTokens: 3}
		return m, nil
	})
	result, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: provider, Tools: []agent.Tool{progressTool(&toolCalls, false)}})
	if err != nil || result.Status != "success" || turns != 2 || toolCalls != 1 {
		t.Fatalf("execute failed: result=%+v err=%v turns=%d tools=%d", result, err, turns, toolCalls)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	metrics := newDecisionMetrics()
	requestStarts, requestEnds, messages := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e agent.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		metrics.observe(e)
		switch e.Type {
		case "model_call_start", "model_call_end":
			if e.At == "" || e.Request == nil || e.Request.Kind != "turn" {
				t.Fatalf("missing Execute request observation: %+v", e)
			}
			if e.Type == "model_call_start" {
				requestStarts++
			} else {
				requestEnds++
			}
		case "message_end":
			if e.Message != nil && e.Message.Role == "assistant" && e.Message.Usage != nil {
				messages++
			}
		}
	}
	wantUsage := agent.Usage{InputTokens: 38, OutputTokens: 14, CacheReadTokens: 6}
	if requestStarts != 2 || requestEnds != 2 || messages != 2 || metrics.UsageCalls != 2 || metrics.Usage != wantUsage || metrics.UsageStatus != "reported_only" {
		t.Fatalf("Execute usage duplicated or missing: starts=%d ends=%d messages=%d metrics=%+v", requestStarts, requestEnds, messages, metrics)
	}
}
