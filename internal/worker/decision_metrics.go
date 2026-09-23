package worker

import (
	"encoding/json"
	"strings"
	"time"

	"xloom/internal/agent"
	"xloom/internal/contract"
)

// DecisionMetrics reports observed work, not a bill or proof that proposed
// changes were applied. The execution receipt and graph events remain the
// authority for committed business actions.
type DecisionMetrics struct {
	Version              int                `json:"version"`
	ModelCalls           int                `json:"model_calls"`
	SummaryCalls         int                `json:"summary_calls"`
	CompletedCalls       int                `json:"completed_calls"`
	FailedCalls          int                `json:"failed_calls"`
	UsageCalls           int                `json:"usage_calls"`
	Usage                agent.Usage        `json:"usage"`
	UsageStatus          string             `json:"usage_status"`
	CostStatus           string             `json:"cost_status"`
	InitialInputBytes    int                `json:"initial_input_bytes"`
	TotalInputBytes      int64              `json:"total_input_bytes"`
	InputBytesBasis      string             `json:"input_bytes_basis,omitempty"`
	ModelDurationMS      int64              `json:"model_duration_ms"`
	ElapsedWallMS        int64              `json:"elapsed_wall_ms"`
	GraphReads           int                `json:"graph_reads"`
	GraphReadFailures    int                `json:"graph_read_failures"`
	GraphActions         int                `json:"graph_actions"`
	GraphActionSuccesses int                `json:"graph_action_successes"`
	GraphActionFailures  int                `json:"graph_action_failures"`
	DraftCalls           int                `json:"draft_calls,omitempty"`
	DraftFailures        int                `json:"draft_failures,omitempty"`
	DraftActions         int                `json:"draft_actions,omitempty"`
	PreviewCalls         int                `json:"preview_calls,omitempty"`
	PreviewFailures      int                `json:"preview_failures,omitempty"`
	CommitCalls          int                `json:"commit_calls,omitempty"`
	CommitFailures       int                `json:"commit_failures,omitempty"`
	CommitDurationMS     int64              `json:"commit_duration_ms,omitempty"`
	ReceiptCalls         int                `json:"receipt_calls,omitempty"`
	ReceiptFailures      int                `json:"receipt_failures,omitempty"`
	StateChanged         int                `json:"state_changed,omitempty"`
	Committed            bool               `json:"committed,omitempty"`
	CommittedActions     int                `json:"committed_actions,omitempty"`
	Outcome              string             `json:"outcome"`
	Trigger              string             `json:"trigger,omitempty"`
	Repeated             bool               `json:"repeated"`
	ViewMode             string             `json:"view_mode,omitempty"`
	BaselineViewBytes    int                `json:"baseline_view_bytes,omitempty"`
	SelectedViewBytes    int                `json:"selected_view_bytes,omitempty"`
	Replan               *ReplanObservation `json:"replan,omitempty"`
}

// decisionOperation is emitted by the runtime, never inferred from a successful
// graph_action tool call: a version 2 action may only have staged a local draft.
// These events are journaled so recovery retains observed attempts and receipts.
type decisionOperation struct {
	Op           string `json:"op"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	Failed       bool   `json:"failed"`
	StateChanged bool   `json:"state_changed"`
	Committed    bool   `json:"committed"`
	Actions      int    `json:"actions"`
}

func (operation decisionOperation) event() agent.Event {
	raw, _ := json.Marshal(operation)
	return agent.Event{Type: "decision_operation", Text: string(raw)}
}

func newDecisionMetrics() DecisionMetrics {
	return DecisionMetrics{Version: 1, UsageStatus: "unknown", CostStatus: "unknown", Outcome: "unknown"}
}

func (m *DecisionMetrics) observe(event agent.Event) {
	// Shadow work is part of total cost. Its separate receipt retains the
	// breakdown; its transcript never becomes planning history.
	shadow := strings.HasPrefix(event.Type, "replan_")
	if shadow {
		event.Type = strings.TrimPrefix(event.Type, "replan_")
		// The read-only check cannot send graph mutations. Keep attempts at
		// unavailable write tools in its own metrics; they cannot make the
		// real planner's noop look like an uncertain server write.
		if event.ToolName == "graph_action" {
			return
		}
	}
	switch event.Type {
	case "decision_operation":
		if shadow {
			return
		}
		var operation decisionOperation
		if json.Unmarshal([]byte(event.Text), &operation) != nil {
			return
		}
		m.observeDecisionOperation(operation)
	case "model_call_start":
		if event.Request == nil {
			return
		}
		m.ModelCalls++
		if event.Request.Kind == "summary" {
			m.SummaryCalls++
		}
		if !shadow && m.InitialInputBytes == 0 && event.Request.Kind == "turn" {
			m.InitialInputBytes = event.Request.InputBytes
		}
		m.TotalInputBytes += int64(event.Request.InputBytes)
		if m.InputBytesBasis == "" {
			m.InputBytesBasis = event.Request.InputBasis
		} else if m.InputBytesBasis != event.Request.InputBasis {
			m.InputBytesBasis = "mixed"
		}
	case "model_call_end":
		if event.Request == nil {
			return
		}
		m.CompletedCalls++
		m.ModelDurationMS += event.Request.DurationMS
		if event.Request.Failed {
			m.FailedCalls++
		}
		if usage := event.Request.Usage; usage != nil {
			m.UsageCalls++
			m.Usage.InputTokens += usage.InputTokens
			m.Usage.OutputTokens += usage.OutputTokens
			m.Usage.CacheReadTokens += usage.CacheReadTokens
			m.Usage.CacheWriteTokens += usage.CacheWriteTokens
		}
	case "tool_start":
		if event.ToolName == "read_graph" {
			m.GraphReads++
		} else if event.ToolName == "graph_action" {
			m.GraphActions++
		}
	case "tool_end":
		if event.ToolName == "read_graph" && event.Error != "" {
			m.GraphReadFailures++
		} else if event.ToolName == "graph_action" {
			if event.Error == "" {
				m.GraphActionSuccesses++
			} else {
				m.GraphActionFailures++
			}
		}
	}
	// Reported usage can exclude provider-internal HTTP retries even when every
	// logical call supplied usage. Never describe these totals as a full bill.
	if m.UsageCalls == 0 {
		m.UsageStatus = "unknown"
	} else if m.UsageCalls < m.ModelCalls {
		m.UsageStatus = "partial"
	} else {
		m.UsageStatus = "reported_only"
	}
}

func (m *DecisionMetrics) observeDecisionOperation(operation decisionOperation) {
	failure := 0
	if operation.Failed {
		failure = 1
	}
	switch operation.Op {
	case "draft":
		m.DraftCalls++
		m.DraftFailures += failure
		if !operation.Failed {
			m.DraftActions += max(0, operation.Actions)
		}
	case "decision_preview":
		m.PreviewCalls++
		m.PreviewFailures += failure
	case "decision_commit":
		m.CommitCalls++
		m.CommitFailures += failure
		m.CommitDurationMS += max(0, operation.ElapsedMS)
	case "decision_receipt":
		m.ReceiptCalls++
		m.ReceiptFailures += failure
	case "read_graph":
	default:
		return
	}
	if operation.StateChanged {
		m.StateChanged++
	}
	if !operation.Failed && operation.Committed && (operation.Op == "decision_commit" || operation.Op == "decision_receipt") {
		m.Committed = true
		// A run has one authoritative batch. Receipt retries confirm that same
		// commit; counting their actions again would inflate published work.
		m.CommittedActions = max(m.CommittedActions, operation.Actions)
	}
}

func (m DecisionMetrics) finish(j Job, r Result, started, ended time.Time) DecisionMetrics {
	m.ElapsedWallMS = max(0, ended.Sub(started).Milliseconds())
	m.Trigger, m.Repeated = j.DecisionTrigger, j.DecisionRepeated
	if j.Decision != nil {
		m.ViewMode, m.BaselineViewBytes, m.SelectedViewBytes = j.Decision.Mode, j.Decision.BaselineBytes, len(j.Decision.View)
	}
	if j.Decision != nil && j.Decision.Version == 2 {
		if m.Committed {
			m.Outcome = "no_op_committed"
			if m.CommittedActions > 0 {
				m.Outcome = "actions_committed"
			}
		} else if m.DraftActions > 0 {
			m.Outcome = "draft_only_observed"
		}
		return m
	}
	if m.GraphActionSuccesses > 0 {
		m.Outcome = "actions_observed"
		return m
	}
	if r.Status != "success" || m.GraphActions > 0 {
		return m // An interrupted/failed action may have reached the server.
	}
	parsed, err := contract.Parse(r.Text, j.Kind, r.Conclude, j.Graph.OpenCount(), j.Budget.MaxIntents)
	if err != nil {
		return m
	}
	switch parsed.Kind {
	case "noop":
		m.Outcome = "no_op_observed"
	case "intents":
		m.Outcome = "plan_proposed"
	case "complete":
		m.Outcome = "completion_proposed"
	case "rejected":
		m.Outcome = "rejected"
	}
	return m
}
