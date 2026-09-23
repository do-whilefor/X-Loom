package board

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// CheckLegacyConclusion prevents an immutable version-two job from bypassing
// its evidence contract through the compatibility description-only endpoint.
func (t *Tx) CheckLegacyConclusion(project, worker string) error {
	var raw []byte
	err := t.QueryRow("SELECT job FROM xloom_executions WHERE project_id=? AND lease=?", project, worker).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var job struct {
		Version int `json:"result_contract_version"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		return err
	}
	if job.Version >= 2 {
		return Err(409, "registered evidence-contract execution must conclude through its final result")
	}
	return nil
}

// ConcludeEvidenceStep shares the process-fact command and validations. Its
// caller commits this mutation and the execution receipt in one transaction.
// An existing observation remains immutable when reused as the result anchor.
func (t *Tx) ConcludeEvidenceStep(project string, fence ExecutionFence, factID string, factPayload json.RawMessage) (Conclusion, error) {
	if (factID == "") == (len(factPayload) == 0) {
		return Conclusion{}, Err(422, "conclusion requires exactly one fact_id or fact")
	}
	s, err := t.State(project)
	if err != nil {
		return Conclusion{}, err
	}
	if err = s.Graph.RequireActive(); err != nil {
		return Conclusion{}, err
	}
	if fence.Run == "" || fence.Intent == "" || (fence.Lease != "explore" && fence.Lease != "bootstrap") {
		return Conclusion{}, Err(403, "evidence conclusion requires an Execute lease")
	}
	if err = t.CheckExecution(s.Graph, fence); err != nil {
		return Conclusion{}, err
	}
	if len(factPayload) != 0 {
		created, err := t.StateAction(project, fence, StateAction{Op: "fact", IdempotencyKey: fence.Run + ":final-fact", Payload: factPayload})
		if err != nil {
			return Conclusion{}, err
		}
		factID = created.ID
		s, err = t.State(project)
		if err != nil {
			return Conclusion{}, err
		}
	}
	var fact *FactRecord
	for n := range s.FactRecords {
		if s.FactRecords[n].ID == factID {
			fact = &s.FactRecords[n]
			break
		}
	}
	if fact == nil {
		return Conclusion{}, Err(404, "conclusion fact does not exist")
	}
	if fact.Legacy || fact.Status != "valid" || fact.SourceStepID != fence.Intent || len(fact.Evidence) == 0 {
		return Conclusion{}, Err(409, "conclusion requires an effective evidence-backed observation from this Step")
	}
	for n := range s.Graph.Intents {
		i := &s.Graph.Intents[n]
		if i.ID != fence.Intent {
			continue
		}
		if i.To != nil || i.ConcludedAt != nil {
			return Conclusion{}, Err(409, "Step already concluded")
		}
		i.To, i.Worker, i.Heartbeat, i.ConcludedAt = Ptr(factID), Ptr(fence.Run), Ptr(t.Now), Ptr(t.Now)
		if err = t.Save(s.Graph); err != nil {
			return Conclusion{}, err
		}
		result := Conclusion{Fact: Fact{ID: fact.ID, Description: fact.Description}, Intent: *i}
		d, revision, decision, err := t.stateData(project)
		if err != nil {
			return Conclusion{}, err
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return Conclusion{}, err
		}
		revision++
		if _, err = t.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=excluded.revision,decision_revision=excluded.decision_revision", project, string(raw), revision, decision+1); err != nil {
			return Conclusion{}, err
		}
		payload, _ := json.Marshal(map[string]string{"fact_id": factID})
		encoded, _ := json.Marshal(result)
		event, _ := json.Marshal(StateEvent{Revision: revision, Op: "step_completed", ID: fence.Intent, RunID: fence.Run, CreatedAt: t.Now, Payload: payload, Result: encoded})
		_, err = t.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES(?,?,?)", project, revision, string(event))
		return result, err
	}
	return Conclusion{}, Err(404, "Step not found")
}
