//go:build linux

package worker

import (
	"context"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

// This verifies prompt delivery and retained requirements, not whether a model
// correctly applies the distinction between a finished Step and its root goal.
func TestDecideFirstRequestDistinguishesStepAndRootCompletion(t *testing.T) {
	const origin = "Observe synthetic samples A, B and C under the original calibration."
	const goal = "Obtain an actual measurement for each of samples A, B and C."
	const distinction = "A supported negative observation can finish that Step. Accounting for a missing result does not satisfy a root requirement to obtain that result; execution failure is not an observation."
	const rootRoute = "goal actions cannot achieve or withdraw the root id:goal; use the project completion contract."
	for _, mode := range []string{"legacy", "v2"} {
		t.Run(mode, func(t *testing.T) {
			job := scenarioJob(t, "", "reason")
			job.Intent, job.Graph.Intents = nil, nil
			job.Graph.Facts = []board.Fact{{ID: "origin", Description: origin}, {ID: "goal", Description: goal}}
			job.Budget.Timeout = 60
			if mode == "v2" {
				job.GraphRPC, job.ResultContractVersion = true, 2
				job.State = &board.State{Graph: job.Graph}
				decision, err := board.BuildDecisionContext(*job.State, nil, nil, board.DefaultContextViewBytes)
				if err != nil {
					t.Fatal(err)
				}
				decision.Version = 2
				job.Decision = decision
			}
			runDir := t.TempDir()
			receipts, calls := 0, 0
			bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
				if mode != "v2" || request.Op != "decision_receipt" {
					t.Fatalf("prompt delivery check performed an unexpected graph operation: %s", request.Op)
				}
				receipts++
				return board.DecisionReceipt{StateVersion: job.Decision.StateVersion}, nil
			}}
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls != 1 || len(history) != 1 || history[0].Role != "user" {
					t.Fatalf("unexpected initial request: calls=%d messages=%d", calls, len(history))
				}
				prompt := history[0].Text()
				if strings.Count(prompt, distinction) != 1 || !strings.Contains(prompt, origin) || !strings.Contains(prompt, goal) {
					t.Fatal("first request lost the Step/root distinction or original user requirements")
				}
				foundRootRoute := false
				for _, definition := range definitions {
					if definition.Name == "graph_action" {
						foundRootRoute = strings.Contains(definition.Description, rootRoute)
					}
				}
				if !foundRootRoute {
					t.Fatal("first request's graph_action definition lost the root completion route")
				}
				return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic prompt delivery check only."}`), nil
			})
			result, err := Run(context.Background(), job, Options{Provider: provider, RunDir: runDir, Output: bridge})
			wantReceipts := 0
			if mode == "v2" {
				wantReceipts = 1
			}
			if err != nil || result.Status != "success" || calls != 1 || receipts != wantReceipts {
				t.Fatalf("prompt delivery run failed: result=%+v err=%v calls=%d receipts=%d", result, err, calls, receipts)
			}
		})
	}
}
