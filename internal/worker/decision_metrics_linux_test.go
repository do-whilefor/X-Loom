//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func TestDecisionMetricsSeparateDraftsFromCommittedReceipts(t *testing.T) {
	m := newDecisionMetrics()
	m.observe(agent.Event{Type: "tool_start", ToolName: "graph_action"})
	m.observe(agent.Event{Type: "tool_end", ToolName: "graph_action"})
	m.observe((decisionOperation{Op: "draft", Actions: 2}).event())
	m.observe((decisionOperation{Op: "draft", Failed: true}).event())
	m.observe((decisionOperation{Op: "decision_preview", Committed: true, Actions: 2}).event())
	j := Job{Kind: "reason", Decision: &board.DecisionContext{Version: 2}}
	finish := func() DecisionMetrics { return m.finish(j, Result{Status: "success"}, time.Time{}, time.Time{}) }
	if got := finish(); got.Outcome != "draft_only_observed" || got.Committed || got.DraftCalls != 2 || got.DraftActions != 2 || got.DraftFailures != 1 || got.PreviewCalls != 1 {
		t.Fatalf("draft or preview misreported as publication: %+v", got)
	}
	m.observe((decisionOperation{Op: "read_graph", Failed: true, StateChanged: true}).event())
	m.observe((decisionOperation{Op: "decision_preview", Failed: true, StateChanged: true}).event())
	m.observe((decisionOperation{Op: "decision_commit", Failed: true, ElapsedMS: 37}).event())
	if got := finish(); got.Committed || got.StateChanged != 2 || got.CommitCalls != 1 || got.CommitFailures != 1 || got.CommitDurationMS != 37 || got.PreviewFailures != 1 {
		t.Fatalf("uncertain commit metrics: %+v", got)
	}
	m.observe((decisionOperation{Op: "decision_receipt", Failed: true}).event())
	m.observe((decisionOperation{Op: "decision_receipt", Committed: true, Actions: 2}).event())
	m.observe((decisionOperation{Op: "decision_receipt", Committed: true, Actions: 2}).event())
	if got := finish(); got.Outcome != "actions_committed" || !got.Committed || got.CommittedActions != 2 || got.ReceiptCalls != 3 || got.ReceiptFailures != 1 {
		t.Fatalf("receipt recovery must confirm one batch: %+v", got)
	}
}

func TestDecisionMetricsEmptyCommitAndLegacyOutcomes(t *testing.T) {
	m := newDecisionMetrics()
	m.observe((decisionOperation{Op: "decision_commit", Committed: true, ElapsedMS: 4}).event())
	batch := Job{Kind: "reason", Decision: &board.DecisionContext{Version: 2}}
	if got := m.finish(batch, Result{Status: "success"}, time.Time{}, time.Time{}); got.Outcome != "no_op_committed" || !got.Committed || got.CommittedActions != 0 {
		t.Fatalf("empty batch receipt: %+v", got)
	}
	legacy := newDecisionMetrics()
	legacy.observe(agent.Event{Type: "tool_end", ToolName: "graph_action"})
	if got := legacy.finish(Job{Kind: "reason", Decision: &board.DecisionContext{Version: 1}}, Result{}, time.Time{}, time.Time{}); got.Outcome != "actions_observed" || got.Version != 1 {
		t.Fatalf("legacy meaning changed: %+v", got)
	}
	before := m
	event := (decisionOperation{Op: "decision_commit", Committed: true, Actions: 9}).event()
	event.Type = "replan_" + event.Type
	m.observe(event)
	m.observe(agent.Event{Type: "decision_operation", Text: "bad JSON"})
	if !reflect.DeepEqual(m, before) {
		t.Fatalf("shadow or malformed operation changed decision metrics: %+v", m)
	}
}

func TestDecisionMetricsReplayJournalReceipts(t *testing.T) {
	dir := t.TempDir()
	journal, err := openJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []decisionOperation{
		{Op: "draft", Actions: 3},
		{Op: "decision_preview"},
		{Op: "read_graph", Failed: true, StateChanged: true},
		{Op: "decision_commit", Failed: true, ElapsedMS: 53},
		{Op: "decision_receipt", Committed: true, Actions: 2},
		{Op: "decision_receipt", Committed: true, Actions: 2},
	} {
		if err := journal.append(operation.event()); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, want := journal.checkpoint(), journal.metrics
	if err := journal.file.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openJournal(dir, &checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.file.Close()
	if !reflect.DeepEqual(recovered.metrics, want) || !recovered.metrics.Committed || recovered.metrics.CommittedActions != 2 {
		t.Fatalf("replay changed observations: got %+v; want %+v", recovered.metrics, want)
	}
}

type decisionMetricsOutput func([]byte) (int, error)

func (write decisionMetricsOutput) Write(raw []byte) (int, error) { return write(raw) }

func TestDecisionMetricsRuntimeCompactCommit(t *testing.T) {
	for _, changed := range []int{0, 2} {
		dir, version := t.TempDir(), strings.Repeat("a", 64)
		m := newDecisionMetrics()
		o := Options{RunDir: dir, decisionEmit: m.observe}
		o.Output = decisionMetricsOutput(func(raw []byte) (int, error) {
			var event GraphRequestEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return 0, err
			}
			// The production dispatcher removes Results from successful commit
			// and recovery responses to keep the graph bridge payload bounded.
			receipt := board.DecisionReceipt{Committed: true, StateVersion: version, ChangedActions: changed}
			result, _ := json.Marshal(receipt)
			response, _ := json.Marshal(GraphResponse{RequestID: event.Request.RequestID, Result: result})
			err := os.WriteFile(filepath.Join(dir, "graph-response-"+event.Request.RequestID+".json"), response, 0600)
			return len(raw), err
		})
		j := Job{Kind: "reason", GraphRPC: true, Decision: &board.DecisionContext{Version: 2, StateVersion: version}}
		if err := ConfigureRuntimeTools(j, &o); err != nil {
			t.Fatal(err)
		}
		batch := board.DecisionBatch{ExpectedVersion: version, Actions: []board.DecisionAction{}}
		for n := 0; n < changed; n++ {
			payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": strings.Repeat("Fixture", n+1)})
			batch.Actions = append(batch.Actions, board.DecisionAction{Op: "step", Payload: payload})
		}
		if _, err := o.decision.request(context.Background(), GraphRequest{Op: "decision_commit", Batch: &batch}); err != nil {
			t.Fatal(err)
		}
		got := m.finish(j, Result{Status: "success"}, time.Time{}, time.Time{})
		wantOutcome := "no_op_committed"
		if changed > 0 {
			wantOutcome = "actions_committed"
		}
		if !got.Committed || got.CommittedActions != changed || got.CommitCalls != 1 || got.Outcome != wantOutcome {
			t.Fatalf("compact receipt lost action count %d: %+v", changed, got)
		}
	}
}

func TestDecisionMetricsRuntimeOperationsAndLostCommit(t *testing.T) {
	dir, version := t.TempDir(), strings.Repeat("a", 64)
	m := newDecisionMetrics()
	conflict, committed := true, false
	o := Options{RunDir: dir, decisionEmit: m.observe}
	o.Output = decisionMetricsOutput(func(raw []byte) (int, error) {
		var event GraphRequestEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		response := GraphResponse{RequestID: event.Request.RequestID}
		switch event.Request.Op {
		case "read_graph":
			if conflict && event.Request.Section == "facts" {
				response.Error, conflict = "state_changed: updated evidence", false
			} else {
				response.Result, _ = json.Marshal(map[string]string{"state_version": version})
			}
		case "decision_preview":
			response.Result, _ = json.Marshal(board.DecisionReceipt{StateVersion: version})
		case "decision_commit":
			committed = true
			response.Error = "response lost after publish"
		case "decision_receipt":
			response.Result, _ = json.Marshal(board.DecisionReceipt{Committed: committed, StateVersion: version, ChangedActions: 1})
		default:
			t.Fatalf("unexpected RPC: %s", event.Request.Op)
		}
		body, err := json.Marshal(response)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "graph-response-"+event.Request.RequestID+".json"), body, 0600)
		}
		return len(raw), err
	})
	j := Job{Kind: "reason", GraphRPC: true, Decision: &board.DecisionContext{Version: 2, StateVersion: version}}
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	call := func(name, input string, wantError bool) {
		t.Helper()
		for _, tool := range o.Tools {
			if tool.Name != name {
				continue
			}
			_, err := tool.Execute(context.Background(), json.RawMessage(input))
			if (err != nil) != wantError {
				t.Fatalf("%s %s: error = %v", name, input, err)
			}
			return
		}
		t.Fatalf("tool %s missing", name)
	}
	stage := `{"op":"step","idempotency_key":"first","payload":{"action":"add","from":["origin"],"description":"Inspect evidence"}}`
	call("graph_action", stage, false)
	call("graph_action", stage, false) // An idempotent draft is not a second action.
	call("graph_action", `{"op":"preview","idempotency_key":"preview","payload":{}}`, false)
	if m.DraftCalls != 2 || m.DraftActions != 1 || m.PreviewCalls != 1 || m.CommitCalls != 0 || m.Committed {
		t.Fatalf("staged work was confused with published work: %+v", m)
	}
	call("read_graph", `{"section":"facts"}`, true)
	call("read_graph", `{"section":"overview"}`, false)
	call("read_graph", `{"section":"facts"}`, false)
	call("graph_action", stage, false)
	call("graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`, true)
	if m.Committed || m.StateChanged != 1 || m.CommitCalls != 1 || m.CommitFailures != 1 {
		t.Fatalf("lost commit response cannot prove publication: %+v", m)
	}
	if _, err := o.decision.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Committed || m.CommittedActions != 1 || m.ReceiptCalls != 1 || m.DraftActions != 2 {
		t.Fatalf("receipt recovery or unchanged-action counting: %+v", m)
	}
}
