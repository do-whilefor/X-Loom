package board

import (
	"bytes"
	"encoding/json"
)

// RecordExecutionObservation attaches runtime observations after authoritative
// success. It cannot change the outcome, input, receipt or lease. The completed
// execution identity authorizes this write even after its lease was revoked.
func (t *Tx) RecordExecutionObservation(project, run string, fence ExecutionFence, metrics json.RawMessage) error {
	e, err := t.Execution(project, run)
	if err != nil {
		return err
	}
	if e.Kind != "reason" || e.Fence() != fence {
		return Err(403, "observation requires the registered Decide identity")
	}
	if e.Status != "succeeded" {
		return Err(409, "observations require a successful decision receipt")
	}
	var observation map[string]json.RawMessage
	var version int
	if len(metrics) > 16<<10 || json.Unmarshal(metrics, &observation) != nil || observation == nil || json.Unmarshal(observation["version"], &version) != nil || version != 1 {
		return Err(422, "invalid decision observation")
	}
	canonical, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(e.Result, &result) != nil || result == nil {
		return Err(409, "execution has no valid result")
	}
	if old, ok := result["metrics"]; ok && !bytes.Equal(old, []byte("null")) {
		var previous map[string]json.RawMessage
		if json.Unmarshal(old, &previous) != nil {
			return Err(409, "existing observation is invalid")
		}
		encoded, _ := json.Marshal(previous)
		if !bytes.Equal(encoded, canonical) {
			return Err(409, "decision observation is immutable")
		}
		return nil
	}
	result["metrics"] = canonical
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = t.Exec("UPDATE xloom_executions SET result=? WHERE project_id=? AND id=?", raw, project, run)
	return err
}
