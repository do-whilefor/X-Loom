//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func reviewReceipt(version string) board.DecisionReceipt {
	return board.DecisionReceipt{ValidationScope: "protocol_only", CompletionReview: &board.CompletionReview{
		StateVersion: version, Acceptance: "not_checked",
		UserInputs: []board.Fact{{ID: "goal", Description: "Obtain an actual response for each path"}},
		From:       []string{"f001"}, Description: "Proposed proof",
	}}
}

func TestCompletionDraftRejectsMalformedPayloadWithoutPoisoningKey(t *testing.T) {
	for _, payload := range []string{
		`{"action":"complete","from":["f001"],"description":"proof"}`,
		`{"from":"f001","description":"proof"}`,
		`{"from":[],"description":"proof"}`,
		`{"from":["f001"],"description":"  "}`,
	} {
		t.Run(payload, func(t *testing.T) {
			d := &decisionDraft{}
			if _, err := d.action(context.Background(), draftTestAction("complete", "finish", payload), "bad"); err == nil {
				t.Fatal("invalid completion became a private draft")
			}
			if len(d.keys) != 0 || len(d.actions) != 0 || d.version != "" {
				t.Fatal("rejected completion consumed the key or version")
			}
			if _, err := d.action(context.Background(), draftTestAction("complete", "finish", `{"from":["f001"],"description":"proof"}`), "good"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletionReviewIsInvalidatedWithDraft(t *testing.T) {
	for _, mutation := range []string{"reset", "conflict", "repreview_failure"} {
		t.Run(mutation, func(t *testing.T) {
			failPreview := false
			d := &decisionDraft{request: func(_ context.Context, r GraphRequest) (string, error) {
				if r.Op != "decision_preview" {
					t.Fatalf("unreviewed completion reached %s", r.Op)
				}
				if failPreview {
					return "", errors.New("preview unavailable")
				}
				raw, _ := json.Marshal(reviewReceipt("v1"))
				return string(raw), nil
			}}
			ctx := context.Background()
			complete := draftTestAction("complete", "finish", `{"from":["f001"],"description":"proof"}`)
			if _, err := d.action(ctx, complete, "v1"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.action(ctx, draftTestAction("preview", "preview", `{}`), "v1"); err != nil {
				t.Fatal(err)
			}
			loop := &agent.Loop{}
			d.beforeRequest(loop)
			if !d.reviewReady || len(loop.ContextData) != 1 {
				t.Fatal("review was not protected for the next request")
			}
			switch mutation {
			case "reset":
				if _, err := d.action(ctx, draftTestAction("reset", "reset", `{}`), "v1"); err != nil {
					t.Fatal(err)
				}
				if _, err := d.action(ctx, complete, "v1"); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				d.invalidate()
				d.observeRead("overview")
				d.observeRead("facts")
				if _, err := d.action(ctx, complete, "v2"); err != nil {
					t.Fatal(err)
				}
			case "repreview_failure":
				failPreview = true
				if _, err := d.action(ctx, draftTestAction("preview", "preview", `{}`), "v1"); err == nil {
					t.Fatal("preview did not fail")
				}
			}
			d.beforeRequest(loop)
			if d.reviewReady || len(loop.ContextData) != 0 {
				t.Fatal("stale review survived a draft invalidation")
			}
			if _, err := d.action(ctx, draftTestAction("commit", "commit", `{}`), d.version); err == nil || d.uncertain {
				t.Fatal("unreviewed commit was attempted")
			}
		})
	}
}

func TestCompletionReviewReachesModelBeforeCommit(t *testing.T) {
	job, runDir := draftRunJob(t), t.TempDir()
	version := job.Decision.StateVersion
	commits, previews, calls := 0, 0, 0
	bridge := &draftTestBridge{dir: runDir, handle: func(r GraphRequest) (any, error) {
		switch r.Op {
		case "decision_receipt":
			return board.DecisionReceipt{}, nil
		case "decision_preview":
			previews++
			return reviewReceipt(version), nil
		case "decision_commit":
			commits++
			if calls < 2 {
				t.Fatal("completion committed before the model received its review")
			}
			return board.DecisionReceipt{Committed: true, Completed: true, StateVersion: version}, nil
		default:
			return nil, errors.New("unexpected operation " + r.Op)
		}
	}}
	p := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			m := draftModelCall("stage", "graph_action", `{"op":"complete","idempotency_key":"finish","payload":{"from":["f001"],"description":"Proposed proof"}}`)
			for _, op := range []string{"commit", "preview", "commit"} {
				id := op
				if len(m.Content) == 3 {
					id = "premature"
				}
				m.Content = append(m.Content, agent.Block{Type: "tool_use", ID: id, Name: "graph_action", Input: json.RawMessage(`{"op":"` + op + `","idempotency_key":"` + id + `","payload":{}}`)})
			}
			return m, nil
		}
		if calls != 2 {
			return agent.Message{}, errors.New("unexpected extra request")
		}
		results := history[len(history)-1].Content
		if len(results) != 4 || !results[1].IsError || !results[3].IsError || results[2].IsError {
			t.Fatalf("premature commits were not paired with errors: %+v", results)
		}
		if !strings.Contains(string(results[2].Content), `not_checked`) || !strings.Contains(string(results[2].Content), `Obtain an actual response`) {
			t.Fatal("next request lost the authoritative completion review")
		}
		return draftModelCall("final", "graph_action", `{"op":"commit","idempotency_key":"final","payload":{}}`), nil
	})
	result, err := Run(context.Background(), job, Options{RunDir: runDir, Provider: p, Output: bridge})
	if err != nil || result.Status != "success" || calls != 2 || previews != 1 || commits != 1 {
		t.Fatalf("review flow failed: %+v %v calls=%d previews=%d commits=%d", result, err, calls, previews, commits)
	}
}

func TestCompletionReviewSurvivesCompactionThatOmitsIt(t *testing.T) {
	ctx := context.Background()
	commits := 0
	d := &decisionDraft{request: func(_ context.Context, r GraphRequest) (string, error) {
		var receipt board.DecisionReceipt
		switch r.Op {
		case "decision_preview":
			receipt = reviewReceipt("original")
		case "decision_commit":
			commits++
			receipt.Committed = true
		default:
			t.Fatalf("unexpected request %s", r.Op)
		}
		raw, err := json.Marshal(receipt)
		return string(raw), err
	}}
	if _, err := d.action(ctx, draftTestAction("complete", "finish", `{"from":["f001"],"description":"Proposed proof"}`), "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.action(ctx, draftTestAction("preview", "preview", `{}`), "original"); err != nil {
		t.Fatal(err)
	}
	summaries, requests := 0, 0
	loop := &agent.Loop{
		TaskPrompt: "Original task", History: []agent.Message{agent.Text("user", "Original task"), agent.Text("assistant", strings.Repeat("Old investigation. ", 4000))},
		ContextBytes: 12000, RecentBytes: 1000, SummaryBytes: 300,
		StopResult: d.result,
		Tools: []agent.Tool{{Definition: agent.Definition{Name: "commit", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			return d.action(ctx, draftTestAction("commit", "commit", `{}`), "original")
		}}},
		BeforeRequest: func(ctx context.Context, loop *agent.Loop) (context.Context, error) {
			d.beforeRequest(loop)
			return ctx, nil
		},
		Provider: scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
			if len(defs) == 0 {
				summaries++
				return agent.Text("assistant", `{"notes":"Earlier work summarized without its review.","quotes":[]}`), nil
			}
			requests++
			raw, _ := json.Marshal(history)
			if !strings.Contains(string(raw), "Obtain an actual response for each path") || !strings.Contains(string(raw), "not_checked") {
				t.Fatal("summary erased the completion review before commit")
			}
			return draftModelCall("finish", "commit", `{}`), nil
		}),
	}
	if _, err := loop.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || requests != 1 || commits != 1 {
		t.Fatalf("summaries=%d requests=%d commits=%d", summaries, requests, commits)
	}
}

func TestCompletionPreviewMustCarryReviewForItsDraftVersion(t *testing.T) {
	for _, kind := range []string{"missing", "wrong_version", "semantic_claim"} {
		t.Run(kind, func(t *testing.T) {
			d := &decisionDraft{request: func(context.Context, GraphRequest) (string, error) {
				receipt := reviewReceipt("v1")
				switch kind {
				case "missing":
					receipt.CompletionReview = nil
				case "wrong_version":
					receipt.CompletionReview.StateVersion = "v2"
				case "semantic_claim":
					receipt.CompletionReview.Acceptance = "accepted"
				}
				raw, err := json.Marshal(receipt)
				return string(raw), err
			}}
			ctx := context.Background()
			if _, err := d.action(ctx, draftTestAction("complete", "finish", `{"from":["f001"],"description":"proof"}`), "v1"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.action(ctx, draftTestAction("preview", "preview", `{}`), "v1"); err == nil {
				t.Fatal("invalid review armed completion")
			}
			d.beforeRequest(&agent.Loop{})
			if d.reviewReady || d.reviewData != "" {
				t.Fatal("invalid preview retained review authority")
			}
		})
	}
}
