// Package worker executes one isolated task session.
package worker

import (
	"xloom/internal/board"
	"xloom/internal/config"
)

type Job struct {
	RunID      string        `json:"run_id"`
	Kind       string        `json:"kind"`
	WorkerType string        `json:"worker_type"`
	Graph      board.Graph   `json:"graph"`
	Intent     *board.Intent `json:"intent,omitempty"`
	Budget     config.Task   `json:"budget"`
	Workspace  string        `json:"workspace"`
}
type Result struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Conclude bool   `json:"conclude"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}
