package worker

import (
	"fmt"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/contract"
)

// Exercise the examples actually sent to the model against the same contract
// as the Worker. Phase deltas retain this contract without copying its examples.
func TestPromptExamplesMatchRegisteredResultProtocol(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		for _, kind := range []string{"bootstrap", "explore", "reason"} {
			for _, rpc := range []bool{false, true} {
				t.Run(fmt.Sprintf("v%d/%s/rpc=%t", version, kind, rpc), func(t *testing.T) {
					j := Job{Kind: kind, ResultContractVersion: version, GraphRPC: rpc, Budget: config.Task{MaxIntents: 3}}
					body, err := taskTemplate(j, false)
					if err != nil {
						t.Fatal(err)
					}
					seen := map[string]bool{}
					for _, line := range strings.Split(body, "\n") {
						if !strings.Contains(line, `{"accepted"`) {
							continue
						}
						r, err := contract.ParseWithPolicy(line, kind, false, 1, j.Budget.MaxIntents, contract.Policy{Version: version, GraphRPC: rpc})
						if err != nil {
							t.Fatalf("model-facing example violates registered protocol: %v\n%s", err, line)
						}
						seen[r.Outcome] = true
					}
					if version >= 1 && kind != "reason" {
						if !seen["completed"] || !seen["incomplete"] || !seen["continue"] {
							t.Fatalf("execution prompt lacks the correct terminal and continuation options: %v", seen)
						}
					} else if len(seen) != 1 || !seen[""] {
						t.Fatalf("legacy/planning prompt changed protocol: %v", seen)
					}
					if kind == "reason" && (strings.Contains(body, `"intents":[`) == rpc || strings.Contains(body, `"decided":true`) != rpc) {
						t.Fatalf("planning prompt advertises the wrong submission path: %s", body)
					}
				})
			}
		}
	}
}

// A project-level output requirement must remain visible for coverage without
// becoming an instruction for every independent Step to overwrite that output.
func TestExploreKeepsRootCoverageAndAssignedDeliverableBoundary(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		origin := "Verify the controls and deliver a consolidated final report."
		job := Job{
			Kind: "explore", ResultContractVersion: version,
			Graph:  board.Graph{Facts: []board.Fact{{ID: "origin", Description: origin}}},
			Intent: &board.Intent{ID: "i001", Description: "Verify the assigned control and publish supporting evidence."},
		}
		job.Graph.Intents = []board.Intent{*job.Intent}
		prompt, err := Prompt(job, false, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{origin, job.Intent.Description,
			"Project-wide deliverables in the original request do not expand this Step",
			"only when the current intent explicitly assigns them"} {
			if !strings.Contains(prompt, required) {
				t.Fatalf("v%d lost root coverage or the Step's output boundary: %q", version, required)
			}
		}
	}
}

func TestPlannerKeepsSingleWriterAndEvidenceReview(t *testing.T) {
	for _, rpc := range []bool{false, true} {
		prompt, err := taskTemplate(Job{Kind: "reason", GraphRPC: rpc, Budget: config.Task{MaxIntents: 3}}, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"Give each shared deliverable one writer", "after the relevant exploration Steps finish",
			"unless the user requests it earlier", "evidence review and targeted corrections", "producing Step's completion"} {
			if !strings.Contains(prompt, required) {
				t.Fatalf("rpc=%t lost deliverable ordering or verification: %q", rpc, required)
			}
		}
	}
}

func TestPhaseInstructionsDoNotCopyTaskInputOrScenario(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		for _, kind := range []string{"bootstrap", "explore"} {
			for _, scenario := range []string{"pentest", "ctf"} {
				t.Run(fmt.Sprintf("v%d/%s/%s", version, kind, scenario), func(t *testing.T) {
					j := Job{Kind: kind, ResultContractVersion: version, Graph: board.Graph{Project: board.Project{Scenario: scenario}}}
					original, err := Prompt(j, false, t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					conclusion, err := Prompt(j, true, t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					repair, err := repairInstruction(j, true, 2, &outputFailure{Reason: "invalid_contract", Detail: "fixture"})
					if err != nil {
						t.Fatal(err)
					}
					combined := original + conclusion + repair
					if strings.Count(combined, "<task_graph>") != 1 || strings.Count(combined, scenarioPrompt(j)) != 1 {
						t.Fatal("phase changes duplicated immutable task input or scenario policy")
					}
					if !strings.Contains(conclusion, "All tools are disabled") || (version >= 1 && !strings.Contains(conclusion, "do not return continue")) {
						t.Fatal("conclusion lost its phase restrictions")
					}
					if strings.Contains(combined, ctfExecution) || strings.Contains(repair, "<result_contract>") {
						t.Fatal("phase instructions copied execution guidance or the original contract")
					}
					if version == 0 && kind == "bootstrap" {
						if _, err := contract.ParseWithPolicy(conclusion, kind, true, 1, 3, contract.Policy{}); err != nil {
							t.Fatalf("legacy bootstrap conclusion changed its result format: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestBootstrapConclusionKeepsOriginalProofWithoutCompletingProject(t *testing.T) {
	for _, version := range []int{1, 2} {
		body, err := taskTemplate(Job{Kind: "bootstrap", ResultContractVersion: version}, false)
		if err != nil {
			t.Fatal(err)
		}
		completed := 0
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, `"outcome":"completed"`) {
				continue
			}
			result, err := contract.ParseWithPolicy(line, "bootstrap", true, 0, 3, contract.Policy{Version: version})
			if err != nil || result.Kind != "fact" || result.Outcome != "completed" {
				t.Fatalf("v%d original proof cannot conclude as evidence only: result=%+v err=%v", version, result, err)
			}
			completed++
		}
		if completed == 0 {
			t.Fatalf("v%d bootstrap has no completion proof example", version)
		}
	}
}

func TestTerminalWorkerResultRequiresCompletedOutcome(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		for _, conclude := range []bool{false, true} {
			for _, outcome := range []string{"completed", "continue", "incomplete", ""} {
				t.Run(fmt.Sprintf("%s/conclude=%t/%s", kind, conclude, outcome), func(t *testing.T) {
					data := `{"description":"Entire assigned task verified"}`
					if kind == "bootstrap" {
						data = `{"fact":{"description":"Goal evidence verified"},"complete":{"description":"All goal requirements verified"}}`
					}
					output := `{"accepted":true,"data":` + data + `}`
					if outcome == "completed" {
						output = `{"accepted":true,"outcome":"completed","data":` + data + `}`
					} else if outcome != "" {
						output = `{"accepted":true,"outcome":"` + outcome + `","reason":"Only 19 of 30 chunks verified"}`
					}
					r := checkedResult(Job{Kind: kind, ResultContractVersion: 1}, Result{Status: "success", Text: output, Conclude: conclude})
					if (r.Status == "success") != (outcome == "completed") || r.Text != output || r.Retryable {
						t.Fatalf("terminal result accepted unfinished work or lost its evidence: %+v", r)
					}
					if outcome == "incomplete" && (r.FailureKind != "incomplete" || !strings.Contains(r.Error, "19 of 30")) {
						t.Fatalf("incomplete result lost the failure reason: %+v", r)
					}
				})
			}
		}
	}
}
