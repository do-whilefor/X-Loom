package worker

import "xloom/internal/contract"

// ReplanObservation is an experiment receipt, never an executable decision.
// Metrics on the enclosing result include this work as well as normal Decide.
type ReplanObservation struct {
	Mode         string                   `json:"mode"`
	Status       string                   `json:"status"`
	StateVersion string                   `json:"state_version"`
	Generation   int64                    `json:"generation"`
	FromRevision int64                    `json:"from_revision"`
	ToRevision   int64                    `json:"to_revision"`
	Judgment     *contract.ReplanJudgment `json:"judgment,omitempty"`
	Fallback     string                   `json:"fallback,omitempty"`
	Error        string                   `json:"error,omitempty"`
	Metrics      *DecisionMetrics         `json:"metrics,omitempty"`
}
