package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func TestGraphActionRejectsMalformedCollectionsBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name, kind, op, payload, path string
	}{
		{"from object", "reason", "step", `{"action":"add","from":{"item":"origin"},"description":"Inspect"}`, "from"},
		{"from null", "reason", "step", `{"action":"add","from":null}`, "from"},
		{"from element", "reason", "step", `{"action":"add","from":[{"item":"origin"}]}`, "from[0]"},
		{"sources object", "reason", "goal", `{"action":"achieve","sources":{"item":"fact001"}}`, "sources"},
		{"sources element", "explore", "finding", `{"sources":[7]}`, "sources[0]"},
		{"evidence object", "explore", "fact", `{"evidence":{"path":"result.txt"}}`, "evidence"},
		{"evidence null", "explore", "finding", `{"evidence":null}`, "evidence"},
		{"evidence element", "explore", "fact", `{"evidence":["result.txt"]}`, "evidence[0]"},
		{"evidence path", "explore", "fact", `{"evidence":[{"path":7}]}`, "evidence[0].path"},
		{"evidence run", "explore", "finding", `{"evidence":[{"run_id":false}]}`, "evidence[0].run_id"},
		{"evidence excerpt", "explore", "finding", `{"evidence":[{"excerpt":[]}]}`, "evidence[0].excerpt"},
		{"evidence start", "explore", "fact", `{"evidence":[{"start_line":"1"}]}`, "evidence[0].start_line"},
		{"evidence end", "explore", "fact", `{"evidence":[{"end_line":1.5}]}`, "evidence[0].end_line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{RunDir: t.TempDir(), Tools: []agent.Tool{}}
			requests := 0
			opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(GraphRequest) (any, error) {
				requests++
				return board.DecisionReceipt{}, nil
			}}
			job := Job{Kind: tc.kind, RunID: "schema-test", GraphRPC: true}
			if tc.kind == "reason" {
				job.Decision = &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}
			}
			if err := ConfigureRuntimeTools(job, &opts); err != nil {
				t.Fatal(err)
			}
			executed := 0
			for n := range opts.Tools {
				if opts.Tools[n].Name == "graph_action" {
					execute := opts.Tools[n].Execute
					opts.Tools[n].Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
						executed++
						return execute(ctx, raw)
					}
				}
			}
			calls := 0
			loop := agent.Loop{Tools: opts.Tools, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls == 1 {
					return draftModelCall("malformed", "graph_action", `{"op":"`+tc.op+`","idempotency_key":"probe","payload":`+tc.payload+`}`), nil
				}
				results := history[len(history)-1].Content
				if calls != 2 || len(results) != 1 || !results[0].IsError || !strings.Contains(string(results[0].Content), "arguments.payload."+tc.path+" must be") {
					t.Fatalf("missing typed rejection: %+v", results)
				}
				return agent.Text("assistant", "done"), nil
			})}
			if _, err := loop.Run(context.Background(), "Inspect"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || executed != 0 || requests != 0 || (opts.decision != nil && len(opts.decision.actions) != 0) {
				t.Fatalf("malformed input reached execution: calls=%d executed=%d requests=%d draft=%+v", calls, executed, requests, opts.decision)
			}
		})
	}
}

func TestGraphActionCorrectedArrayReusesKeyAndPreviews(t *testing.T) {
	requests := 0
	opts, _, action := draftTestTools(t, func(request GraphRequest) (any, error) {
		requests++
		if request.Op != "decision_preview" || request.Batch == nil || len(request.Batch.Actions) != 1 || request.Batch.Actions[0].Ref != "probe" {
			t.Fatalf("unexpected preview: %+v", request)
		}
		var payload struct {
			From []string `json:"from"`
		}
		if err := json.Unmarshal(request.Batch.Actions[0].Payload, &payload); err != nil || len(payload.From) != 1 || payload.From[0] != "origin" {
			t.Fatalf("array changed before preview: %+v %v", payload, err)
		}
		return board.DecisionReceipt{}, nil
	})
	calls := 0
	loop := agent.Loop{Tools: []agent.Tool{action}, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls > 1 {
			results := history[len(history)-1].Content
			if len(results) != 1 || results[0].IsError != (calls == 2) {
				t.Fatalf("unexpected tool result at turn %d: %+v", calls, results)
			}
		}
		switch calls {
		case 1:
			return draftModelCall("bad", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":{"item":"origin"},"description":"Inspect"}}`), nil
		case 2:
			return draftModelCall("corrected", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":["origin"],"description":"Inspect"}}`), nil
		case 3:
			return draftModelCall("preview", "graph_action", `{"op":"preview","idempotency_key":"preview","payload":{}}`), nil
		default:
			return agent.Text("assistant", "done"), nil
		}
	})}
	if _, err := loop.Run(context.Background(), "Inspect"); err != nil || calls != 4 || requests != 1 || len(opts.decision.actions) != 1 {
		t.Fatalf("corrected draft could not preview: calls=%d requests=%d draft=%+v err=%v", calls, requests, opts.decision, err)
	}
}

func TestGraphActionCollectionSchemaPreservesSupportedPayloads(t *testing.T) {
	opts := Options{Tools: []agent.Tool{}}
	if err := ConfigureRuntimeTools(Job{Kind: "explore"}, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	for _, payload := range []string{
		`{}`,
		`{"from":[],"sources":[],"evidence":[]}`,
		`{"from":["origin"],"sources":["fact001","fact002"]}`,
		`{"evidence":[{"path":"result.txt"}]}`,
		`{"evidence":[{"path":"retained.txt","run_id":"previous-run","excerpt":"exact text","start_line":0,"end_line":0}]}`,
		`{"evidence":[{"path":"result.txt","start_line":1,"end_line":3}]}`,
		`{"claim":"Observed","scope":"fixture","status":"verified","reason":"Correction","replace_support":true,"sources":["fact001"]}`,
	} {
		raw := json.RawMessage(`{"op":"finding","idempotency_key":"probe","payload":` + payload + `}`)
		if err := agent.ValidateArguments(action.Schema, raw); err != nil {
			t.Errorf("supported shape rejected: %s: %v", payload, err)
		}
	}
}
