//go:build linux

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/server"
	"xloom/internal/worker"
)

// This verifies delivery and publication, not a model's semantic judgment.
// The scripted provider chooses its final action from the response it receives;
// Worker, graph-response files, Dispatcher, HTTP and SQLite remain real.
func TestCompletionReviewOmissionsReachProviderThroughEvidencePages(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("accepted=%t", accepted), func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "completion.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			api := httptest.NewServer(server.New(store))
			t.Cleanup(api.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			runner := &updateLocalRunner{}
			scheduler := New(config.Config{Server: api.URL, Runtime: config.Runtime{MaxWorkers: 1, Interval: 1, HealthMode: "disabled"}}, runner)
			f := &updateRequestFixture{t: t, ctx: ctx, scheduler: scheduler, runner: runner, workspace: t.TempDir()}
			const goal = "Confirm that the verifier explicitly accepted request R17."
			var graph board.Graph
			f.do("POST", "/projects", map[string]any{"title": "Retained response", "origin": "Inspect the retained response for request R17.", "goal": goal, "bootstrap_enabled": false}, &graph, nil)
			f.project = graph.Project
			fact := board.FactRecord{ID: "f001", Description: "The request client retained diagnostics and its verifier response.", Scope: "request R17", Status: "valid"}
			for n := 0; n < 17; n++ {
				fact.Evidence = append(fact.Evidence, board.EvidenceRef{RunID: "retained-run", Path: fmt.Sprintf("retained/diagnostic-%02d.txt", n), Excerpt: strings.Repeat("x", 8192)})
			}
			response := fmt.Sprintf(`{"request":"R17","accepted":%t}`, accepted)
			fact.Evidence = append(fact.Evidence, board.EvidenceRef{RunID: "retained-run", Path: "retained/response.json", Excerpt: response})
			// Seed earlier retained evidence, as in the frame-budget regression.
			// Evidence size forces both graph support paging and review omission.
			if err := store.Do(ctx, func(tx *board.Tx) error {
				current, err := tx.Load(graph.Project.ID)
				if err != nil {
					return err
				}
				current.Facts = append(current.Facts, board.Fact{ID: fact.ID, Description: fact.Description})
				if err := tx.Save(current); err != nil {
					return err
				}
				data, err := json.Marshal(map[string]any{"facts": []board.FactRecord{fact}})
				if err != nil {
					return err
				}
				_, err = tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,1,1) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data", graph.Project.ID, string(data))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			lease := Lease{Run: "fixture@completion", Kind: "reason"}
			f.do("POST", projectPath(f.project.ID)+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil)
			job := worker.Job{RunID: "completion", Kind: "reason", WorkerType: "go", Workspace: f.workspace, GraphRPC: true, ResultContractVersion: 2, Graph: board.Graph{Project: f.project}, Budget: config.Task{Timeout: 60, ConcludeTimeout: 10, MaxIntents: 2}}
			raw, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			execution := board.Execution{ProjectID: f.project.ID, ID: job.RunID, Namespace: "xloom", Backend: "fixture", Kind: "reason", Lease: lease.Run, Job: raw}
			f.do("POST", projectPath(f.project.ID)+"/executions/prepare", execution, &execution, &lease)
			if err := json.Unmarshal(execution.Job, &job); err != nil {
				t.Fatal(err)
			}
			run := &task{Job: job, Worker: config.Worker{Name: "fixture", Type: "go"}, Lease: lease, Execution: execution, LeaseTimeout: 30 * time.Second}
			f.start(run)
			var initial board.State
			f.do("GET", projectPath(f.project.ID)+"/state", nil, &initial, nil)
			version := run.Job.Decision.StateVersion
			calls, previews, commits, pages, next := 0, 0, 0, 0, 0
			forward := runner.handler
			runner.handler = func(ctx context.Context, job worker.Job, request worker.GraphRequest) (any, error) {
				if request.Op == "decision_preview" {
					previews++
				}
				if request.Op == "decision_commit" {
					commits++
					if calls != 5 {
						t.Fatal("commit reached the Dispatcher before evidence delivery")
					}
				}
				return forward(ctx, job, request)
			}
			var delivered []board.EvidenceRef
			provider := updateProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if len(definitions) == 0 {
					t.Fatal("bounded fixture unexpectedly required compaction")
				}
				switch calls {
				case 1:
					encodedResponse, _ := json.Marshal(response)
					for _, message := range history {
						// Inspect the actual prompt; marshaling history again would
						// double-escape the excerpt and hide a real input leak.
						if strings.Contains(message.Text(), string(encodedResponse[1:len(encodedResponse)-1])) {
							t.Fatal("decisive response leaked into the initial view")
						}
					}
					message := updateToolCall("draft", "graph_action", `{"op":"complete","idempotency_key":"finish","payload":{"from":["f001"],"description":"The retained response confirms acceptance of R17."}}`)
					message.Content = append(message.Content,
						updateToolCall("preview", "graph_action", `{"op":"preview","idempotency_key":"preview","payload":{}}`).Content[0],
						updateToolCall("premature", "graph_action", `{"op":"commit","idempotency_key":"premature","payload":{}}`).Content[0])
					return message, nil
				case 2:
					var receipt board.DecisionReceipt
					if err := json.Unmarshal(completionEvidenceToolResult(t, history, "preview", false), &receipt); err != nil {
						t.Fatal(err)
					}
					review := receipt.CompletionReview
					if receipt.Committed || receipt.Completed || receipt.ValidationScope != "protocol_only" || review == nil || review.Acceptance != "not_checked" || review.StateVersion != version || len(review.FactRecords) != 0 || !reflect.DeepEqual(review.OmittedFactIDs, []string{fact.ID}) || review.ReadMore == "" || !reflect.DeepEqual(review.UserInputs, initial.Graph.Facts[:2]) {
						t.Fatal("provider did not receive the authoritative omission review")
					}
					if len(completionEvidenceToolResult(t, history, "premature", true)) == 0 || commits != 0 {
						t.Fatal("same-response completion commit was not rejected")
					}
					var afterPreview board.State
					f.do("GET", projectPath(f.project.ID)+"/state", nil, &afterPreview, nil)
					if !reflect.DeepEqual(afterPreview, initial) {
						t.Fatal("preview published the proposed completion")
					}
					return updateToolCall("fact", "read_graph", `{"section":"facts","ids":["f001"],"limit":1}`), nil
				case 3:
					var page struct {
						StateVersion string `json:"state_version"`
						Items        []struct {
							ID              string              `json:"id"`
							EvidenceOmitted bool                `json:"evidence_omitted"`
							EvidenceCount   int                 `json:"evidence_count"`
							Evidence        []board.EvidenceRef `json:"evidence"`
						} `json:"items"`
					}
					if err := json.Unmarshal(completionEvidenceToolResult(t, history, "fact", false), &page); err != nil {
						t.Fatal(err)
					}
					if page.StateVersion != version || len(page.Items) != 1 || page.Items[0].ID != fact.ID || !page.Items[0].EvidenceOmitted || page.Items[0].EvidenceCount != len(fact.Evidence) || len(page.Items[0].Evidence) != 0 {
						t.Fatal("fact page did not expose its omitted evidence")
					}
					return updateToolCall("evidence-0", "read_graph", `{"section":"evidence","ids":["f001"],"limit":50}`), nil
				case 4, 5:
					var page struct {
						StateVersion string              `json:"state_version"`
						Items        []board.EvidenceRef `json:"items"`
						Offset       int                 `json:"offset"`
						Next         *int                `json:"next_offset"`
					}
					if err := json.Unmarshal(completionEvidenceToolResult(t, history, fmt.Sprintf("evidence-%d", next), false), &page); err != nil {
						t.Fatal(err)
					}
					if page.StateVersion != version || page.Offset != len(delivered) || len(page.Items) == 0 {
						t.Fatal("evidence paging lost its pinned version or progress")
					}
					delivered = append(delivered, page.Items...)
					pages++
					if page.Next != nil {
						if calls != 4 || *page.Next <= next || len(delivered) >= len(fact.Evidence) {
							t.Fatal("decisive response did not require a second evidence page")
						}
						next = *page.Next
						return updateToolCall(fmt.Sprintf("evidence-%d", next), "read_graph", fmt.Sprintf(`{"section":"evidence","ids":["f001"],"offset":%d,"limit":50}`, next)), nil
					}
					if calls != 5 || !reflect.DeepEqual(delivered, fact.Evidence) {
						t.Fatal("complete retained evidence did not reach the provider losslessly")
					}
					var response struct {
						Accepted bool `json:"accepted"`
					}
					if err := json.Unmarshal([]byte(delivered[len(delivered)-1].Excerpt), &response); err != nil {
						t.Fatal(err)
					}
					if response.Accepted {
						return updateToolCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`), nil
					}
					message := updateToolCall("reset", "graph_action", `{"op":"reset","idempotency_key":"reset","payload":{}}`)
					message.Content = append(message.Content,
						updateToolCall("followup", "graph_action", `{"op":"step","idempotency_key":"followup","payload":{"action":"add","from":["f001"],"description":"Investigate why R17 was not accepted and obtain the required verifier confirmation.","goal_id":"goal"}}`).Content[0],
						updateToolCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`).Content[0])
					return message, nil
				default:
					t.Fatalf("unexpected provider request %d: %s", calls, updateHistoryText(history[len(history)-1:]))
					return agent.Message{}, nil
				}
			})
			runner.options = worker.Options{Provider: provider, RunDir: t.TempDir(), ContextBytes: worker.DefaultContextBytes, ContextTokens: worker.DefaultContextTokens, ContextTargetTokens: worker.DefaultContextTargetTokens}
			result, err := runner.Run(ctx, run.Worker, run.Job)
			if err != nil || result.Status != "success" || calls != 5 || previews != 1 || commits != 1 || pages != 2 {
				t.Fatalf("evidence completion flow failed: result=%+v err=%v calls=%d previews=%d commits=%d pages=%d", result, err, calls, previews, commits, pages)
			}
			var final board.State
			f.do("GET", projectPath(f.project.ID)+"/state", nil, &final, nil)
			wantStatus := "active"
			if accepted {
				wantStatus = "completed"
			}
			if final.Graph.Project.Status != wantStatus {
				t.Fatal("published state did not match the provider's evidence-based action")
			}
			if !accepted && (len(final.Steps) != 1 || final.Steps[0].Status != "open" || !reflect.DeepEqual(final.Steps[0].From, []string{fact.ID})) {
				t.Fatal("rejected completion did not publish its required follow-up Step")
			}
		})
	}
}

func completionEvidenceToolResult(t *testing.T, history []agent.Message, id string, wantError bool) json.RawMessage {
	t.Helper()
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type != "tool_result" || block.ToolUseID != id {
				continue
			}
			if block.IsError != wantError {
				t.Fatalf("tool %s error=%t, want %t", id, block.IsError, wantError)
			}
			var content string
			if err := json.Unmarshal(block.Content, &content); err != nil {
				t.Fatal(err)
			}
			return json.RawMessage(content)
		}
	}
	t.Fatalf("provider did not receive tool result %s", id)
	return nil
}
