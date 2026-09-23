//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func draftTestAction(op, key, payload string) board.StateAction {
	return board.StateAction{Op: op, IdempotencyKey: key, Payload: json.RawMessage(payload)}
}

func TestDecisionDraftStagesAliasesWithoutPublishing(t *testing.T) {
	requests := 0
	draft := &decisionDraft{request: func(context.Context, GraphRequest) (string, error) {
		requests++
		return "", errors.New("draft staging must not publish anything")
	}}
	ctx := context.Background()
	goal := draftTestAction("goal", "auth", `{"action":"add","condition":"Check the authentication boundary"}`)
	step := draftTestAction("step", "probe", `{"action":"add","goal_id":"$auth","from":["origin"],"description":"Inspect one unauthenticated request"}`)
	if reply, err := draft.action(ctx, goal, "original"); err != nil || !strings.Contains(reply, `"id":"$auth"`) {
		t.Fatalf("goal was not staged with its temporary reference: %s %v", reply, err)
	}
	if _, err := draft.action(ctx, step, "newly-read-version"); err != nil {
		t.Fatal(err)
	}
	// Retrying equivalent JSON preserves the one private action and its alias.
	step.Payload = json.RawMessage(`{"description":"Inspect one unauthenticated request","from":["origin"],"goal_id":"$auth","action":"add"}`)
	if _, err := draft.action(ctx, step, "newly-read-version"); err != nil {
		t.Fatal(err)
	}
	if requests != 0 || len(draft.actions) != 2 || draft.version != "original" || draft.actions[1].Ref != "probe" {
		t.Fatalf("staging published, duplicated or rebound a plan: requests=%d draft=%+v", requests, draft)
	}
	step.Payload = json.RawMessage(`{"action":"abandon","id":"some-other-step","reason":"Replaced action"}`)
	if _, err := draft.action(ctx, step, "original"); err == nil || len(draft.actions) != 2 {
		t.Fatal("a reused draft key changed the already staged plan")
	}
}

func TestDecisionDraftRejectsRootGoalWithoutConsumingDraftOrKey(t *testing.T) {
	for _, transition := range []string{"achieve", "withdraw", "add"} {
		for _, seeded := range []bool{false, true} {
			name := transition + "/empty"
			if seeded {
				name = transition + "/existing_draft"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var operations []string
				var wantActions []board.DecisionAction
				wantVersion := "corrected-version"
				draft := &decisionDraft{request: func(_ context.Context, request GraphRequest) (string, error) {
					operations = append(operations, request.Op)
					if request.Batch == nil || request.Batch.ExpectedVersion != wantVersion || !reflect.DeepEqual(request.Batch.Actions, wantActions) {
						t.Fatalf("request changed the corrected draft or version: %+v", request)
					}
					if request.Op != "decision_preview" && request.Op != "decision_commit" {
						t.Fatalf("unexpected request %q", request.Op)
					}
					receipt := board.DecisionReceipt{Committed: request.Op == "decision_commit", StateVersion: wantVersion}
					if request.Op == "decision_preview" {
						receipt.ValidationScope = "protocol_only"
						receipt.CompletionReview = &board.CompletionReview{StateVersion: wantVersion, Acceptance: "not_checked"}
					}
					raw, err := json.Marshal(receipt)
					return string(raw), err
				}}
				if seeded {
					if _, err := draft.action(ctx, draftTestAction("step", "retained", `{"action":"abandon","id":"i-old","reason":"The completed investigation is no longer needed"}`), "original-version"); err != nil {
						t.Fatal(err)
					}
					wantVersion = "original-version"
				}
				beforeVersion := draft.version
				beforeKeys := append([]string(nil), draft.keys...)
				beforeActions, _ := json.Marshal(draft.actions)
				invalid := draftTestAction("goal", "finish", `{"action":"`+transition+`","id":"goal","reason":"Root requirement met","condition":"Root condition","sources":["f001"]}`)
				if _, err := draft.action(ctx, invalid, "invalid-version"); err == nil || !strings.Contains(err.Error(), "root goal") || !strings.Contains(err.Error(), "use complete") || !strings.Contains(err.Error(), "draft unchanged") {
					t.Fatalf("illegal root action was not rejected with recovery guidance: %v", err)
				}
				afterActions, _ := json.Marshal(draft.actions)
				if len(operations) != 0 || draft.version != beforeVersion || !reflect.DeepEqual(draft.keys, beforeKeys) || string(afterActions) != string(beforeActions) || draft.uncertain || draft.committed {
					t.Fatalf("illegal root action consumed or changed the existing draft: %+v operations=%v", draft, operations)
				}
				// The failed action must not reserve its key or bind an empty draft
				// to its version. A corrected completion needs no reset or restaging.
				if _, err := draft.action(ctx, draftTestAction("complete", "finish", `{"from":["f001"],"description":"The retained evidence satisfies the original requirement"}`), "corrected-version"); err != nil {
					t.Fatalf("the rejected key could not be reused for complete: %v", err)
				}
				if draft.version != wantVersion || len(draft.actions) != len(beforeKeys)+1 || draft.actions[len(draft.actions)-1].Op != "complete" || draft.keys[len(draft.keys)-1] != "finish" {
					t.Fatalf("corrected completion lost the prior draft or version: %+v", draft)
				}
				retained, _ := json.Marshal(draft.actions[:len(beforeKeys)])
				if len(beforeKeys) > 0 && string(retained) != string(beforeActions) {
					t.Fatal("correcting the root action rewrote an earlier legal action")
				}
				wantActions = append([]board.DecisionAction(nil), draft.actions...)
				for _, op := range []string{"preview", "commit"} {
					if op == "commit" {
						draft.beforeRequest(&agent.Loop{})
					}
					if _, err := draft.action(ctx, draftTestAction(op, op, `{}`), "later-version"); err != nil {
						t.Fatal(err)
					}
				}
				if strings.Join(operations, ",") != "decision_preview,decision_commit" || !draft.committed || draft.uncertain {
					t.Fatalf("corrected plan did not preview and commit once: %+v operations=%v", draft, operations)
				}
			})
		}
	}
}

func TestDecisionDraftAllowsSubgoalsWithRootParentAndGoalAlias(t *testing.T) {
	draft := &decisionDraft{request: func(context.Context, GraphRequest) (string, error) {
		t.Fatal("staging subgoals made a server request")
		return "", nil
	}}
	for _, action := range []board.StateAction{
		draftTestAction("goal", "goal", `{"action":"add","parent_id":"goal","condition":"Check one auxiliary condition"}`),
		draftTestAction("goal", "achieve-child", `{"action":"achieve","id":"$goal","reason":"Auxiliary condition verified","sources":["f001"]}`),
		draftTestAction("goal", "withdraw-child", `{"action":"withdraw","id":"g001","reason":"This auxiliary direction is no longer needed"}`),
	} {
		if _, err := draft.action(context.Background(), action, "original-version"); err != nil {
			t.Fatalf("legal child goal was rejected as a root change: %v", err)
		}
	}
	if len(draft.actions) != 3 || draft.actions[0].Ref != "goal" || draft.version != "original-version" {
		t.Fatalf("root-parent or alias handling changed the child draft: %+v", draft)
	}
}

func TestDecisionDraftRejectsRootGoalSourceWithoutConsumingDraftOrKey(t *testing.T) {
	for _, from := range []string{`["goal"]`, `["origin","goal"]`, `["goal","f001"]`} {
		for _, seeded := range []bool{false, true} {
			name := from + "/empty"
			if seeded {
				name = from + "/existing_draft"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var operations []string
				var wantActions []board.DecisionAction
				wantVersion := "corrected-version"
				draft := &decisionDraft{request: func(_ context.Context, request GraphRequest) (string, error) {
					operations = append(operations, request.Op)
					if request.Batch == nil || request.Batch.ExpectedVersion != wantVersion || !reflect.DeepEqual(request.Batch.Actions, wantActions) {
						t.Fatalf("request changed the corrected draft or version: %+v", request)
					}
					raw, err := json.Marshal(board.DecisionReceipt{Committed: request.Op == "decision_commit", StateVersion: wantVersion})
					return string(raw), err
				}}
				if seeded {
					if _, err := draft.action(ctx, draftTestAction("goal", "retained", `{"action":"add","parent_id":"goal","condition":"Check one auxiliary condition"}`), "original-version"); err != nil {
						t.Fatal(err)
					}
					wantVersion = "original-version"
				}
				beforeVersion := draft.version
				beforeKeys := append([]string(nil), draft.keys...)
				beforeActions, _ := json.Marshal(draft.actions)
				invalid := draftTestAction("step", "probe", `{"action":"add","goal_id":"goal","from":`+from+`,"description":"Inspect the service"}`)
				if _, err := draft.action(ctx, invalid, "invalid-version"); err == nil || !strings.Contains(err.Error(), "use goal_id") || !strings.Contains(err.Error(), "from accepts evidence") || !strings.Contains(err.Error(), "origin is allowed") || !strings.Contains(err.Error(), "draft unchanged") {
					t.Fatalf("root goal source was not rejected with recovery guidance: %v", err)
				}
				afterActions, _ := json.Marshal(draft.actions)
				if len(operations) != 0 || draft.version != beforeVersion || !reflect.DeepEqual(draft.keys, beforeKeys) || string(afterActions) != string(beforeActions) || draft.uncertain || draft.committed {
					t.Fatalf("invalid source consumed or changed the existing draft: %+v operations=%v", draft, operations)
				}
				// Root goal binding and origin evidence remain valid, and fixing
				// from must not require resetting the draft or choosing a new key.
				corrected := draftTestAction("step", "probe", `{"action":"add","goal_id":"goal","from":["origin","f001"],"description":"Inspect the service"}`)
				if _, err := draft.action(ctx, corrected, "corrected-version"); err != nil {
					t.Fatalf("the rejected key could not be reused for a corrected Step: %v", err)
				}
				if draft.version != wantVersion || len(draft.actions) != len(beforeKeys)+1 || draft.actions[len(draft.actions)-1].Ref != "probe" || draft.keys[len(draft.keys)-1] != "probe" {
					t.Fatalf("corrected Step lost the prior draft or version: %+v", draft)
				}
				retained, _ := json.Marshal(draft.actions[:len(beforeKeys)])
				if len(beforeKeys) > 0 && string(retained) != string(beforeActions) {
					t.Fatal("correcting the source rewrote an earlier legal action")
				}
				wantActions = append([]board.DecisionAction(nil), draft.actions...)
				for _, op := range []string{"preview", "commit"} {
					if _, err := draft.action(ctx, draftTestAction(op, op, `{}`), "later-version"); err != nil {
						t.Fatal(err)
					}
				}
				if strings.Join(operations, ",") != "decision_preview,decision_commit" || !draft.committed || draft.uncertain {
					t.Fatalf("corrected plan did not preview and commit once: %+v operations=%v", draft, operations)
				}
			})
		}
	}
}

// Reply through the real file bridge so version tracking and tool boundaries
// are exercised, rather than substituting a second implementation of them.
type draftTestBridge struct {
	dir    string
	handle func(GraphRequest) (any, error)
}

func (b *draftTestBridge) Write(raw []byte) (int, error) {
	var event GraphRequestEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return 0, err
	}
	if event.Type != "graph_request" {
		return len(raw), nil
	}
	value, err := b.handle(event.Request)
	response := GraphResponse{RequestID: event.Request.RequestID}
	if err != nil {
		response.Error = err.Error()
	} else {
		response.Result, err = json.Marshal(value)
		if err != nil {
			return 0, err
		}
	}
	data, err := json.Marshal(response)
	if err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(b.dir, "graph-response-"+event.Request.RequestID+".json"), data, 0600); err != nil {
		return 0, err
	}
	return len(raw), nil
}

func draftTestTools(t *testing.T, handle func(GraphRequest) (any, error)) (*Options, agent.Tool, agent.Tool) {
	t.Helper()
	opts := &Options{RunDir: t.TempDir()}
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: handle}
	job := Job{Kind: "reason", RunID: "draft-test", GraphRPC: true, Decision: &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}}
	if err := ConfigureRuntimeTools(job, opts); err != nil {
		t.Fatal(err)
	}
	return opts, opts.Tools[0], opts.Tools[1]
}

func draftToolAction(t *testing.T, tool agent.Tool, op, key, payload string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(draftTestAction(op, key, payload))
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw)
}

func TestDecisionDraftPreviewDoesNotAdvanceVersion(t *testing.T) {
	original, hypothetical, current := strings.Repeat("a", 64), strings.Repeat("c", 64), strings.Repeat("b", 64)
	previews := 0
	opts, read, action := draftTestTools(t, func(request GraphRequest) (any, error) {
		switch request.Op {
		case "decision_preview":
			previews++
			if request.Batch.ExpectedVersion != original || len(request.Batch.Actions) != 1 {
				t.Fatalf("preview rebound or altered the draft: %+v", request.Batch)
			}
			return board.DecisionReceipt{StateVersion: hypothetical}, nil
		case "read_graph":
			return map[string]any{"state_version": current, "counts": map[string]int{"facts": 2}}, nil
		case "decision_commit":
			if request.Batch.ExpectedVersion != original {
				t.Fatalf("commit blindly rebound an existing draft: %+v", request.Batch)
			}
			return nil, errors.New("state_changed: read corrected evidence")
		default:
			t.Fatalf("unexpected bridge operation %s", request.Op)
			return nil, nil
		}
	})
	if _, err := draftToolAction(t, action, "step", "probe", `{"action":"add","from":["origin"],"description":"Check one boundary"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := draftToolAction(t, action, "preview", "preview", `{}`); err != nil {
		t.Fatal(err)
	}
	if previews != 1 || *opts.GraphVersion != original || opts.decision.version != original || opts.decision.committed {
		t.Fatal("hypothetical preview state became the actual decision version or a committed result")
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`)); err != nil {
		t.Fatal(err)
	}
	if *opts.GraphVersion != current || opts.decision.version != original {
		t.Fatal("reading current state retroactively changed a staged plan's base")
	}
	if _, err := draftToolAction(t, action, "commit", "commit", `{}`); err == nil || !strings.Contains(err.Error(), "state_changed") {
		t.Fatalf("stale draft should conflict: %v", err)
	}
	if len(opts.decision.actions) != 0 || len(opts.decision.keys) != 0 || opts.decision.version != "" || opts.decision.uncertain {
		t.Fatal("a definitive conflict retained a partial or uncertain old draft")
	}
}

func TestDecisionDraftConflictRequiresFreshEvidenceRead(t *testing.T) {
	oldVersion, newVersion := strings.Repeat("a", 64), strings.Repeat("b", 64)
	commits := 0
	opts, read, action := draftTestTools(t, func(request GraphRequest) (any, error) {
		if request.Op == "read_graph" {
			return map[string]any{"state_version": newVersion, "items": []map[string]string{{"id": "corrected", "description": "The initial premise was refuted"}}}, nil
		}
		if request.Op == "decision_commit" {
			commits++
			if commits == 1 {
				if request.Batch.ExpectedVersion != oldVersion {
					t.Fatal("first commit did not retain the original base")
				}
				return nil, errors.New("state_changed: a premise was refuted")
			}
			if request.Batch.ExpectedVersion != newVersion || len(request.Batch.Actions) != 1 || !strings.Contains(string(request.Batch.Actions[0].Payload), "corrected") {
				t.Fatalf("restaged plan retained old work: %+v", request.Batch)
			}
			return board.DecisionReceipt{Committed: true, StateVersion: newVersion}, nil
		}
		return nil, errors.New("unexpected request")
	})
	oldPlan := `{"action":"add","from":["origin"],"description":"Follow the old premise"}`
	if _, err := draftToolAction(t, action, "step", "old", oldPlan); err != nil {
		t.Fatal(err)
	}
	// This cached version predates the conflict and cannot satisfy a reread gate.
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := draftToolAction(t, action, "commit", "commit", `{}`); err == nil {
		t.Fatal("fixture did not return the expected conflict")
	}
	if _, err := draftToolAction(t, action, "step", "old", oldPlan); err == nil {
		t.Fatal("conflicted draft could be blindly restaged using the previously cached newer hash")
	}
	if _, err := draftToolAction(t, action, "reset", "reset", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := draftToolAction(t, action, "step", "old", oldPlan); err == nil {
		t.Fatal("reset bypassed the evidence reread requirement")
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := draftToolAction(t, action, "step", "old", oldPlan); err == nil {
		t.Fatal("refreshing only the version/counts allowed restaging without affected evidence")
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"section":"facts","ids":["corrected"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := draftToolAction(t, action, "step", "new", `{"action":"add","from":["corrected"],"description":"Inspect the corrected boundary"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := draftToolAction(t, action, "commit", "commit", `{}`); err != nil || !opts.decision.committed || commits != 2 {
		t.Fatalf("freshly read and rebuilt plan could not commit once: %v commits=%d", err, commits)
	}
}

func TestDecisionDraftLostCommitReplyRecoversReceiptWithoutRepublishing(t *testing.T) {
	commits, receipts := 0, 0
	draft := &decisionDraft{request: func(_ context.Context, request GraphRequest) (string, error) {
		switch request.Op {
		case "decision_commit":
			commits++
			return "", errors.New("connection closed after the transaction committed")
		case "decision_receipt":
			receipts++
			return `{"committed":true,"state_version":"committed","ids":{"probe":"i001"}}`, nil
		default:
			t.Fatalf("unexpected request after an uncertain commit: %s", request.Op)
			return "", nil
		}
	}}
	ctx := context.Background()
	if _, err := draft.action(ctx, draftTestAction("step", "probe", `{"action":"add","from":["origin"],"description":"Check once"}`), "initial"); err != nil {
		t.Fatal(err)
	}
	commit := draftTestAction("commit", "commit", `{}`)
	if _, err := draft.action(ctx, commit, "initial"); err == nil || !draft.uncertain {
		t.Fatal("lost response was incorrectly treated as a definite rejection")
	}
	if _, err := draft.action(ctx, commit, "some-new-version"); err != nil {
		t.Fatal(err)
	}
	if text, done := draft.result(); !done || text != committedDecisionText || commits != 1 || receipts != 1 {
		t.Fatalf("receipt recovery republished or lost the authoritative result: committed=%v commits=%d receipts=%d", done, commits, receipts)
	}
	if _, err := draft.action(ctx, draftTestAction("reset", "reset", `{}`), "new"); err == nil {
		t.Fatal("a committed run accepted additional draft changes")
	}
}

func TestDecisionDraftUncertainReceiptFailureKeepsOriginalAttempt(t *testing.T) {
	requests := []string{}
	draft := &decisionDraft{request: func(_ context.Context, request GraphRequest) (string, error) {
		requests = append(requests, request.Op)
		return "", errors.New("temporary transport loss")
	}}
	ctx := context.Background()
	if _, err := draft.action(ctx, draftTestAction("step", "probe", `{"action":"add","from":["origin"],"description":"Check once"}`), "initial"); err != nil {
		t.Fatal(err)
	}
	_, _ = draft.action(ctx, draftTestAction("commit", "commit", `{}`), "initial")
	if _, err := draft.action(ctx, draftTestAction("reset", "reset", `{}`), "new"); err == nil {
		t.Fatal("failed receipt lookup allowed an uncertain commit to be discarded")
	}
	if !draft.uncertain || draft.version != "initial" || len(draft.actions) != 1 || strings.Join(requests, ",") != "decision_commit,decision_receipt" {
		t.Fatalf("receipt failure replaced or retransmitted the original attempt: %+v requests=%v", draft, requests)
	}
}

func draftRunJob(t *testing.T) Job {
	t.Helper()
	job := scenarioJob(t, "", "reason")
	job.Intent, job.Graph.Intents = nil, nil
	job.GraphRPC, job.ResultContractVersion, job.Budget.Timeout = true, 2, 60
	job.State = &board.State{Graph: job.Graph}
	decision, err := board.BuildDecisionContextFromCursor(*job.State, nil, nil, board.DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	decision.Version = 2
	job.Decision = decision
	return job
}

func draftModelCall(id, tool, input string) agent.Message {
	return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: id, Name: tool, Input: json.RawMessage(input)}}}
}

func TestDecisionDraftRunRecoversCommittedReceiptWithoutModel(t *testing.T) {
	job, runDir, start := draftRunJob(t), t.TempDir(), time.Now()
	committed := false
	commits, receipts, calls := 0, 0, 0
	finalVersion := strings.Repeat("e", 64)
	bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		switch request.Op {
		case "decision_receipt":
			receipts++
			return board.DecisionReceipt{Committed: committed, StateVersion: finalVersion}, nil
		case "decision_commit":
			commits++
			committed = true
			return nil, errors.New("response lost after the durable commit")
		default:
			t.Fatalf("unexpected request %s", request.Op)
			return nil, nil
		}
	}}
	provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		switch calls {
		case 1:
			return draftModelCall("stage", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":["origin"],"description":"Check once"}}`), nil
		case 2:
			return draftModelCall("publish", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`), nil
		default:
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("synthetic worker interruption")}
		}
	})
	first, err := Run(context.Background(), job, Options{Provider: provider, RunDir: runDir, Output: bridge, Now: func() time.Time { return start }})
	if err != nil || !first.Retryable || first.Status != "failed" || commits != 1 || calls != 3 {
		t.Fatalf("did not reach the lost-commit-reply recovery boundary: %+v err=%v commits=%d calls=%d", first, err, commits, calls)
	}
	before := outcomeSession(t, runDir)
	resumedCalls := 0
	result, err := Run(context.Background(), job, Options{RunDir: runDir, Output: bridge, Now: func() time.Time { return start.Add(3 * time.Second) }, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		resumedCalls++
		return agent.Message{}, errors.New("recovery called the model despite a committed receipt")
	})})
	if err != nil || result.Status != "success" || result.Text != committedDecisionText || result.StateVersion != finalVersion || resumedCalls != 0 || commits != 1 || receipts != 2 {
		t.Fatalf("committed recovery did not finish from the same receipt: %+v err=%v calls=%d commits=%d receipts=%d", result, err, resumedCalls, commits, receipts)
	}
	after := outcomeSession(t, runDir)
	if after.RunID != before.RunID || !after.ExecutionDeadline.Equal(before.ExecutionDeadline) || after.RecoveryCount != before.RecoveryCount+1 {
		t.Fatal("receipt recovery refreshed the execution identity, deadline or allowance")
	}
}

func TestDecisionDraftRunDiscardsUncommittedPlanOnResume(t *testing.T) {
	job, runDir, start := draftRunJob(t), t.TempDir(), time.Now()
	reads, commits, calls := []string{}, 0, 0
	version := strings.Repeat("d", 64)
	bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		switch request.Op {
		case "decision_receipt":
			return board.DecisionReceipt{}, nil
		case "read_graph":
			reads = append(reads, request.Section)
			return map[string]any{"state_version": version, "items": []board.Fact{{ID: "origin", Description: "Current user scope"}}}, nil
		case "decision_commit":
			commits++
			if len(request.Batch.Actions) != 1 || request.Batch.Actions[0].Ref != "fresh" || request.Batch.ExpectedVersion != version || strings.Contains(string(request.Batch.Actions[0].Payload), "$old") {
				t.Fatalf("recovery republished an old private draft or reused its alias: %+v", request.Batch)
			}
			return board.DecisionReceipt{Committed: true, StateVersion: version}, nil
		default:
			t.Fatalf("unexpected request %s", request.Op)
			return nil, nil
		}
	}}
	first, err := Run(context.Background(), job, Options{RunDir: runDir, Output: bridge, Now: func() time.Time { return start }, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			return draftModelCall("private", "graph_action", `{"op":"goal","idempotency_key":"old","payload":{"action":"add","condition":"An old private direction"}}`), nil
		}
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("synthetic interruption with private draft")}
	})})
	if err != nil || !first.Retryable || commits != 0 {
		t.Fatalf("failed to preserve a private uncommitted boundary: %+v err=%v", first, err)
	}
	before := outcomeSession(t, runDir)
	resumeCalls := 0
	result, err := Run(context.Background(), job, Options{RunDir: runDir, Output: bridge, Now: func() time.Time { return start.Add(5 * time.Second) }, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		resumeCalls++
		switch resumeCalls {
		case 1:
			if !strings.Contains(history[len(history)-1].Text(), "draft was discarded") {
				t.Fatal("resumed model was not told its old private aliases are invalid")
			}
			return draftModelCall("stale", "graph_action", `{"op":"step","idempotency_key":"stale","payload":{"action":"add","goal_id":"$old","from":["origin"],"description":"Blindly reuse the old draft"}}`), nil
		case 2:
			last := history[len(history)-1]
			if len(last.Content) != 1 || !last.Content[0].IsError || !strings.Contains(string(last.Content[0].Content), "read") {
				t.Fatal("uncommitted recovery accepted an old plan before reading current state")
			}
			return draftModelCall("overview", "read_graph", `{"section":"overview"}`), nil
		case 3:
			return draftModelCall("evidence", "read_graph", `{"section":"facts","ids":["origin"]}`), nil
		case 4:
			return draftModelCall("fresh", "graph_action", `{"op":"step","idempotency_key":"fresh","payload":{"action":"add","from":["origin"],"description":"Use the freshly read scope"}}`), nil
		case 5:
			return draftModelCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`), nil
		default:
			return agent.Message{}, errors.New("extra model call after committed recovery")
		}
	})})
	if err != nil || result.Status != "success" || result.Text != committedDecisionText || resumeCalls != 5 || commits != 1 || strings.Join(reads, ",") != "overview,facts" {
		t.Fatalf("resumed draft failed to replan and commit once: %+v err=%v calls=%d commits=%d reads=%v", result, err, resumeCalls, commits, reads)
	}
	after := outcomeSession(t, runDir)
	if !after.ExecutionDeadline.Equal(before.ExecutionDeadline) || !after.ReasonDeadline.Equal(before.ReasonDeadline) || !after.StartedAt.Equal(before.StartedAt) || after.RecoveryCount != before.RecoveryCount+1 || after.Identity != before.Identity {
		t.Fatal("uncommitted recovery refreshed its identity, budget or recovery allowance")
	}
}

func TestDecisionDraftRunBoundsUncommittedTextAndTruncatedTurns(t *testing.T) {
	for _, output := range []string{"uncommitted_final", "malformed_final", "truncated_commit"} {
		t.Run(output, func(t *testing.T) {
			job, runDir, start := draftRunJob(t), t.TempDir(), time.Now()
			calls := 0
			bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
				if request.Op != "decision_receipt" {
					t.Fatalf("uncommitted or truncated text published a plan: %s", request.Op)
				}
				return board.DecisionReceipt{}, nil
			}}
			result, err := Run(context.Background(), job, Options{RunDir: runDir, Output: bridge, Now: func() time.Time { return start }, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls > maxContinuations+1 {
					t.Fatal("uncommitted decision continued without a bound")
				}
				read, action := false, false
				for _, definition := range definitions {
					read = read || definition.Name == "read_graph"
					action = action || definition.Name == "graph_action"
				}
				if !read || !action {
					t.Fatal("uncommitted Decide entered JSON repair with required planning tools disabled")
				}
				switch output {
				case "uncommitted_final":
					return agent.Text("assistant", committedDecisionText), nil
				case "malformed_final":
					return agent.Text("assistant", "The plan is ready."), nil
				default:
					message := draftModelCall("truncated-commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`)
					message.StopReason = "max_tokens"
					return message, nil
				}
			})})
			if err != nil || result.Status != "failed" || result.Retryable || !strings.Contains(result.Error, "continuation_exhausted") || calls != maxContinuations+1 {
				t.Fatalf("uncommitted planning did not exhaust its fixed continuation allowance: %+v err=%v calls=%d", result, err, calls)
			}
			saved := outcomeSession(t, runDir)
			if saved.RepairCount != 0 || saved.Repairing || saved.ContinuationCount != maxContinuations || !saved.ExecutionDeadline.Equal(start.Add(time.Minute)) {
				t.Fatal("bounded planning used JSON repair or refreshed its original task budget")
			}
		})
	}
}

func TestDecisionDraftRunRecoversSavedRepairIntoPlanning(t *testing.T) {
	job, runDir, start := draftRunJob(t), t.TempDir(), time.Now()
	identity, err := identityFor(job, runDir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(runDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	const oldRepair = "Result-format repair: tools are disabled; rewrite the final JSON."
	history := []agent.Message{agent.Text("user", "Check the original bounded scope"), agent.Text("assistant", "Unfinished JSON"), agent.Text("user", oldRepair)}
	for n := range history {
		history[n].Sequence = uint64(n + 1)
		if err := journal.append(agent.Event{Type: "message_end", Message: &history[n]}); err != nil {
			t.Fatal(err)
		}
	}
	saved := session{
		SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: job.RunID, Kind: job.Kind,
		StartedAt: start, ExecutionDeadline: start.Add(time.Minute), ReasonDeadline: start.Add(time.Minute),
		GraphVersion: job.Decision.StateVersion, History: history, TaskPrompt: history[0].Text(),
		ContextCheckpoint: &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 3},
		Repairing:         true, RepairCount: 1, RepairPrompt: oldRepair, RepairPending: true,
	}
	if err := saved.save(runDir, journal); err != nil {
		t.Fatal(err)
	}
	if err := journal.file.Close(); err != nil {
		t.Fatal(err)
	}
	version, calls, commits := strings.Repeat("f", 64), 0, 0
	bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		switch request.Op {
		case "decision_receipt":
			return board.DecisionReceipt{}, nil
		case "read_graph":
			return map[string]any{"state_version": version, "items": []board.Fact{{ID: "origin", Description: "Original bounded scope"}}}, nil
		case "decision_commit":
			commits++
			return board.DecisionReceipt{Committed: true, StateVersion: version}, nil
		default:
			t.Fatalf("unexpected recovery request %s", request.Op)
			return nil, nil
		}
	}}
	result, err := Run(context.Background(), job, Options{RunDir: runDir, Output: bridge, Now: func() time.Time { return start.Add(7 * time.Second) }, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		read, action := false, false
		for _, definition := range definitions {
			read = read || definition.Name == "read_graph"
			action = action || definition.Name == "graph_action"
		}
		if !read || !action {
			t.Fatal("saved JSON repair still disabled the tools needed to rebuild an uncommitted decision")
		}
		switch calls {
		case 1:
			return draftModelCall("overview", "read_graph", `{"section":"overview"}`), nil
		case 2:
			return draftModelCall("evidence", "read_graph", `{"section":"facts","ids":["origin"]}`), nil
		case 3:
			return draftModelCall("stage", "graph_action", `{"op":"step","idempotency_key":"fresh","payload":{"action":"add","from":["origin"],"description":"Inspect current scope"}}`), nil
		case 4:
			return draftModelCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`), nil
		default:
			return agent.Message{}, errors.New("extra model call after commit")
		}
	})})
	if err != nil || result.Status != "success" || result.Text != committedDecisionText || commits != 1 || calls != 4 {
		t.Fatalf("saved repair did not return to committed planning: %+v err=%v calls=%d commits=%d", result, err, calls, commits)
	}
	after := outcomeSession(t, runDir)
	if after.RepairCount != saved.RepairCount || after.Repairing || after.RepairPending || after.RepairPrompt != "" || after.RecoveryCount != saved.RecoveryCount+1 || !after.ExecutionDeadline.Equal(saved.ExecutionDeadline) || !after.ReasonDeadline.Equal(saved.ReasonDeadline) {
		t.Fatal("leaving old JSON repair reset an already consumed allowance or the original deadline")
	}
}
