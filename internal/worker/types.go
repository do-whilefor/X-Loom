// Package worker executes one isolated task session.
package worker

import (
	"errors"
	"xloom/internal/board"
	"xloom/internal/config"
)

// ErrInterrupted marks a dispatcher shutdown/infrastructure interruption that
// may resume the same run. Ordinary cancellation remains a permanent stop.
var ErrInterrupted = errors.New("execution interrupted; same-run recovery permitted")

// DefaultContextTokens is the input allowance for a 1M-token model, leaving
// room for the normal output budget and estimation error. The byte limit is
// a separate request-size guard, not a token conversion.
const (
	DefaultContextTokens = 920000
	// DefaultContextTargetTokens is the estimated input size after compaction.
	DefaultContextTargetTokens = 250000
	DefaultContextBytes        = 8 << 20
)

type Job struct {
	RunID                 string                 `json:"run_id"`
	PreviousRunID         string                 `json:"previous_run_id,omitempty"`
	GraphRPC              bool                   `json:"graph_rpc,omitempty"`
	ResultContractVersion int                    `json:"result_contract_version,omitempty"`
	EnvironmentID         string                 `json:"environment_id,omitempty"`
	DecisionRevision      int64                  `json:"decision_revision,omitempty"`
	Decision              *board.DecisionContext `json:"decision,omitempty"`
	DecisionTrigger       string                 `json:"decision_trigger,omitempty"`
	DecisionTriggers      []string               `json:"decision_triggers,omitempty"`
	DecisionRepeated      bool                   `json:"decision_repeated,omitempty"`
	Kind                  string                 `json:"kind"`
	WorkerType            string                 `json:"worker_type"`
	Graph                 board.Graph            `json:"graph"`
	State                 *board.State           `json:"state,omitempty"`
	Intent                *board.Intent          `json:"intent,omitempty"`
	Budget                config.Task            `json:"budget"`
	Workspace             string                 `json:"workspace"`
}
type Result struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	Conclude     bool             `json:"conclude"`
	Status       string           `json:"status"`
	Error        string           `json:"error,omitempty"`
	Retryable    bool             `json:"retryable,omitempty"`
	FailureKind  string           `json:"failure_kind,omitempty"`
	StateVersion string           `json:"state_version,omitempty"`
	Metrics      *DecisionMetrics `json:"metrics,omitempty"`
}
