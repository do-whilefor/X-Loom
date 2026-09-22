package worker

import (
	"fmt"
	"strings"
	"testing"

	"xloom/internal/config"
	"xloom/internal/contract"
)

// Exercise the examples actually sent to the model against the same contract
// as the Worker. A version change must not leave the default or repair prompt
// teaching a response that will be rejected on every turn.
func TestPromptExamplesMatchRegisteredResultProtocol(t *testing.T) {
	for _, version := range []int{0, 1} {
		for _, kind := range []string{"bootstrap", "explore", "reason"} {
			for _, conclude := range []bool{false, true} {
				if kind == "reason" && conclude {
					continue
				}
				for _, rpc := range []bool{false, true} {
					t.Run(fmt.Sprintf("v%d/%s/conclude=%t/rpc=%t", version, kind, conclude, rpc), func(t *testing.T) {
						j := Job{Kind: kind, ResultContractVersion: version, GraphRPC: rpc, Budget: config.Task{MaxIntents: 3}}
						body, err := taskTemplate(j, conclude)
						if err != nil {
							t.Fatal(err)
						}
						seen := map[string]bool{}
						for _, line := range strings.Split(body, "\n") {
							if !strings.Contains(line, `{"accepted"`) {
								continue
							}
							r, err := contract.ParseWithPolicy(line, kind, conclude, 1, j.Budget.MaxIntents, contract.Policy{Version: version, GraphRPC: rpc})
							if err != nil {
								t.Fatalf("model-facing example violates registered protocol: %v\n%s", err, line)
							}
							seen[r.Outcome] = true
						}
						if version == 1 && kind != "reason" {
							if !seen["completed"] || !seen["incomplete"] || seen["continue"] == conclude {
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
