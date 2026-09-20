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

type Job struct {
	RunID            string        `json:"run_id"`
	PreviousRunID    string        `json:"previous_run_id,omitempty"`
	GraphRPC         bool          `json:"graph_rpc,omitempty"`
	EnvironmentID    string        `json:"environment_id,omitempty"`
	DecisionRevision int64         `json:"decision_revision,omitempty"`
	Kind             string        `json:"kind"`
	WorkerType       string        `json:"worker_type"`
	Graph            board.Graph   `json:"graph"`
	State            *board.State  `json:"state,omitempty"`
	Intent           *board.Intent `json:"intent,omitempty"`
	Budget           config.Task   `json:"budget"`
	Workspace        string        `json:"workspace"`
}
type Result struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Conclude    bool   `json:"conclude"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Retryable   bool   `json:"retryable,omitempty"`
	FailureKind string `json:"failure_kind,omitempty"`
}
