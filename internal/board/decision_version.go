package board

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// DecisionStateVersion binds a plan to shared content rather than lease
// heartbeats or event counters. Copy slices before removing runtime metadata.
func DecisionStateVersion(state State) string {
	state.Revision, state.DecisionRevision = 0, 0
	state.Graph.Project.Reason = nil
	state.Graph.Intents = append([]Intent{}, state.Graph.Intents...)
	for n := range state.Graph.Intents {
		state.Graph.Intents[n].Heartbeat = nil
		state.Graph.Intents[n].Worker = nil
	}
	state.Steps = append([]Step{}, state.Steps...)
	for n := range state.Steps {
		state.Steps[n].Worker = nil
		// Claiming work changes neither its plan nor its evidence. Keep actual
		// running status for operation checks, but do not stale a decision when
		// the dispatcher starts a step that decision just proposed.
		if state.Steps[n].Status == "running" {
			state.Steps[n].Status = "open"
		}
	}
	raw, _ := json.Marshal(state)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// DecisionJobVersion returns the initial version for the new decision protocol.
// A malformed non-null marker must never silently opt a job into legacy writes.
func DecisionJobVersion(raw json.RawMessage) (string, error) {
	var job struct {
		Kind     string `json:"kind"`
		Graph    Graph  `json:"graph"`
		State    *State `json:"state"`
		Decision *struct {
			Version      int    `json:"version"`
			StateVersion string `json:"state_version"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return "", Err(422, "invalid decision job")
	}
	if job.Decision == nil {
		return "", nil
	}
	if job.Kind != "reason" || (job.Decision.Version != 1 && job.Decision.Version != 2) || job.Decision.StateVersion == "" || job.State == nil {
		return "", Err(422, "decision requires version 1 or 2 and a bound state snapshot")
	}
	version := DecisionStateVersion(*job.State)
	if job.Decision.StateVersion != version {
		return "", Err(422, "decision state_version does not match its saved state")
	}
	state := *job.State
	state.Graph = job.Graph
	if DecisionStateVersion(state) != version {
		return "", Err(422, "decision state and job graph do not match")
	}
	return version, nil
}

// CheckDecisionStateVersion runs inside the same transaction as the write.
// Old clients may supply a version voluntarily; new registered Decide jobs
// must supply one. Existing lease and source checks remain independent.
func (t *Tx) CheckDecisionStateVersion(state State, fence ExecutionFence, expected string) error {
	if fence.Lease == "reason" {
		var raw []byte
		err := t.QueryRow("SELECT job FROM xloom_executions WHERE project_id=? AND lease=? AND kind='reason'", state.Graph.Project.ID, fence.Run).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			version, err := DecisionJobVersion(raw)
			if err != nil {
				return err
			}
			if version != "" && expected == "" {
				return Err(422, "decision write requires expected_version")
			}
		}
	}
	if expected != "" && expected != DecisionStateVersion(state) {
		return Err(409, "state_changed: decision input is no longer current; read the graph before deciding again")
	}
	return nil
}
