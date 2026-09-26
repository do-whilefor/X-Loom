package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func TestGraphActionSchemaDescribesModeFields(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		fields string
	}{
		{"reason", "action condition description final_report from goal_id id kind parent_id priority reason source sources target"},
		{"explore", "claim description evidence observed_at reason replace_support scope sources status"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			opts := Options{Tools: []agent.Tool{}}
			if err := ConfigureRuntimeTools(Job{Kind: tc.kind}, &opts); err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties struct {
					Payload struct {
						Properties map[string]struct {
							Type, Description string
							Enum              []string
						} `json:"properties"`
					} `json:"payload"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(opts.Tools[1].Schema, &schema); err != nil {
				t.Fatal(err)
			}
			props := schema.Properties.Payload.Properties
			var fields []string
			for name, field := range props {
				fields = append(fields, name)
				want := "string"
				switch name {
				case "from", "sources", "evidence":
					want = "array"
				case "priority":
					want = "integer"
				case "replace_support", "final_report":
					want = "boolean"
				}
				if field.Type != want {
					t.Errorf("%s type = %q, want %q", name, field.Type, want)
				}
			}
			slices.Sort(fields)
			if strings.Join(fields, " ") != tc.fields {
				t.Fatalf("mode fields = %v, want %s", fields, tc.fields)
			}
			if tc.kind == "reason" {
				if !reflect.DeepEqual(props["action"].Enum, []string{"add", "achieve", "withdraw", "abandon", "priority"}) || !strings.Contains(props["action"].Description, "Required for goal") {
					t.Fatalf("missing transition discriminator guidance: %+v", props["action"])
				}
				if !reflect.DeepEqual(props["kind"].Enum, []string{"supersedes", "refutes", "narrows"}) {
					t.Fatalf("relation kinds: %+v", props["kind"])
				}
			} else if !reflect.DeepEqual(props["status"].Enum, []string{"candidate", "verified", "refuted"}) {
				t.Fatalf("finding statuses: %+v", props["status"])
			}
		})
	}
}

func TestGraphActionSchemaPreservesOperationPayloads(t *testing.T) {
	for _, tc := range []struct{ kind, op, payload string }{
		{"reason", "goal", `{"action":"add","condition":"Check authorization","parent_id":"goal"}`},
		{"reason", "goal", `{"action":"achieve","id":"g001","reason":"Verified","sources":["fact001"]}`},
		{"reason", "goal", `{"action":"withdraw","id":"g001","reason":"No longer needed"}`},
		{"reason", "step", `{"action":"add","from":["origin"],"description":"Inspect","goal_id":"g001","priority":1000000}`},
		{"reason", "step", `{"action":"add","from":["origin"],"description":"Report","final_report":true}`},
		{"reason", "step", `{"action":"abandon","id":"i001","reason":"Covered"}`},
		{"reason", "step", `{"action":"priority","id":"i001","reason":"First","priority":0}`},
		{"reason", "fact_relation", `{"kind":"supersedes","source":"fact002","target":"fact001","reason":"Corrected"}`},
		{"reason", "complete", `{"from":["fact002"],"description":"Verified proof"}`},
		{"explore", "fact", `{"description":"Observed","scope":"fixture","observed_at":"2026-09-25T01:02:03Z","evidence":[{"path":"result.txt","start_line":1,"end_line":3}]}`},
		{"explore", "finding", `{"claim":"Observed","scope":"fixture","status":"candidate"}`},
		{"explore", "finding", `{"claim":"Observed","scope":"fixture","status":"verified","sources":["fact001"]}`},
		{"explore", "finding", `{"claim":"Observed","scope":"fixture","status":"refuted","sources":["fact002"],"reason":"Corrected","replace_support":true,"evidence":[{"path":"retained.txt","run_id":"previous-run","excerpt":"exact text","start_line":0,"end_line":0}]}`},
	} {
		t.Run(tc.kind+"/"+tc.op+"/"+tc.payload, func(t *testing.T) {
			job := Job{Kind: tc.kind}
			if tc.kind == "reason" {
				job.Decision = &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}
			}
			opts := Options{Tools: []agent.Tool{}}
			if err := ConfigureRuntimeTools(job, &opts); err != nil {
				t.Fatal(err)
			}
			raw := json.RawMessage(`{"op":"` + tc.op + `","idempotency_key":"probe","payload":` + tc.payload + `}`)
			if err := agent.ValidateArguments(opts.Tools[1].Schema, raw); err != nil {
				t.Fatalf("valid operation payload rejected: %v", err)
			}
		})
	}
}

func TestGraphActionSchemaRejectsMalformedScalarFields(t *testing.T) {
	for _, tc := range []struct{ kind, field, value string }{
		{"reason", "action", `[]`},
		{"reason", "description", `7`},
		{"reason", "condition", `null`},
		{"reason", "goal_id", `{}`},
		{"reason", "final_report", `"true"`},
		{"explore", "scope", `false`},
		{"explore", "observed_at", `7`},
		{"explore", "claim", `[]`},
		{"explore", "status", `true`},
		{"explore", "replace_support", `"true"`},
	} {
		t.Run(tc.kind+"/"+tc.field, func(t *testing.T) {
			schema, err := json.Marshal(graphActionPayloadSchema(tc.kind))
			if err != nil {
				t.Fatal(err)
			}
			raw := json.RawMessage(`{"` + tc.field + `":` + tc.value + `}`)
			if err := agent.ValidateArguments(schema, raw); err == nil || !strings.Contains(err.Error(), "arguments."+tc.field+" must be") {
				t.Fatalf("missing typed rejection: %v", err)
			}
		})
	}
}

func TestGraphActionSchemaPreservesFrozenAndHistoricalEvidence(t *testing.T) {
	job := Job{Kind: "explore", RunID: "schema-evidence", GraphRPC: true, Workspace: t.TempDir()}
	opts := Options{Tools: []agent.Tool{}, RunDir: t.TempDir()}
	original := "first\nselected\nlast\n"
	source := filepath.Join(job.Workspace, "result.txt")
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var submissions []json.RawMessage
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(request GraphRequest) (any, error) {
		submissions = append(submissions, request.Action.Payload)
		return board.StateActionResult{ID: "finding001"}, nil
	}}
	if err := ConfigureRuntimeTools(job, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	raw := json.RawMessage(`{"op":"finding","idempotency_key":"probe","payload":{"claim":"Observed","scope":"fixture","status":"verified","sources":["fact001"],"evidence":[{"path":"result.txt","start_line":2,"end_line":2},{"path":"retained.txt","run_id":"previous-run","excerpt":"exact text","start_line":0,"end_line":0}]}}`)
	for range 2 {
		if err := agent.ValidateArguments(action.Schema, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := action.Execute(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("changed after submission\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if len(submissions) != 2 || string(submissions[0]) != string(submissions[1]) {
		t.Fatalf("retry changed frozen support: %s", submissions)
	}
	var payload struct {
		Sources  []string            `json:"sources"`
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if err := json.Unmarshal(submissions[0], &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Sources, []string{"fact001"}) || len(payload.Evidence) != 2 {
		t.Fatalf("support changed before server validation: %+v", payload)
	}
	fresh, historical := payload.Evidence[0], payload.Evidence[1]
	if fresh.RunID != job.RunID || fresh.Excerpt != "selected\n" || fresh.StartLine != 2 || fresh.EndLine != 2 || fresh.Path == source {
		t.Fatalf("fresh evidence was not frozen: %+v", fresh)
	}
	if retained, err := os.ReadFile(fresh.Path); err != nil || string(retained) != original {
		t.Fatalf("original evidence changed: %q %v", retained, err)
	}
	if historical != (board.EvidenceRef{Path: "retained.txt", RunID: "previous-run", Excerpt: "exact text"}) {
		t.Fatalf("historical reference changed: %+v", historical)
	}
}
