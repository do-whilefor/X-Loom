package worker

import (
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
	Outcome              string             `json:"outcome"`
	Trigger              string             `json:"trigger,omitempty"`
	Repeated             bool               `json:"repeated"`
	ViewMode             string             `json:"view_mode,omitempty"`
	BaselineViewBytes    int                `json:"baseline_view_bytes,omitempty"`
	SelectedViewBytes    int                `json:"selected_view_bytes,omitempty"`
	Replan               *ReplanObservation `json:"replan,omitempty"`
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

func (m DecisionMetrics) finish(j Job, r Result, started, ended time.Time) DecisionMetrics {
	m.ElapsedWallMS = max(0, ended.Sub(started).Milliseconds())
	m.Trigger, m.Repeated = j.DecisionTrigger, j.DecisionRepeated
	if j.Decision != nil {
		m.ViewMode, m.BaselineViewBytes, m.SelectedViewBytes = j.Decision.Mode, j.Decision.BaselineBytes, len(j.Decision.View)
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
