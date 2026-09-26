package board

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const inputSnapshotSchema = `CREATE TABLE IF NOT EXISTS xloom_input_snapshots(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,metadata BLOB NOT NULL,state BLOB NOT NULL,
 PRIMARY KEY(project_id,id));`

// InputSnapshot identifies an immutable input, not a promise to reconstruct a
// historical revision from the current graph. Its state remains server-side.
type InputSnapshot struct {
	Version          int    `json:"version"`
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	Generation       int64  `json:"generation"`
	Revision         int64  `json:"revision"`
	DecisionRevision int64  `json:"decision_revision"`
	StateVersion     string `json:"state_version"`
	FactCount        int    `json:"fact_count"`
	HintCount        int    `json:"hint_count"`
	OpenCount        int    `json:"open_count"`
	StepCount        int    `json:"step_count"`
}

func (t *Tx) FreezeInput(state State) (*InputSnapshot, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	ref := &InputSnapshot{Version: 1, ID: hex.EncodeToString(sum[:]), ProjectID: state.Graph.Project.ID,
		Generation: state.Graph.Project.Generation, Revision: state.Revision, DecisionRevision: state.DecisionRevision,
		StateVersion: DecisionStateVersion(state), FactCount: len(state.Graph.Facts), HintCount: len(state.Graph.Hints), OpenCount: state.Graph.OpenCount(), StepCount: len(state.Steps)}
	metadata, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	_, err = t.Exec("INSERT OR IGNORE INTO xloom_input_snapshots(project_id,id,metadata,state) VALUES(?,?,?,?)", ref.ProjectID, ref.ID, metadata, raw)
	return ref, err
}

func (t *Tx) InputSnapshotMetadata(project, id string) (*InputSnapshot, error) {
	var raw []byte
	err := t.QueryRow("SELECT metadata FROM xloom_input_snapshots WHERE project_id=? AND id=?", project, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Err(404, "Input snapshot not found")
	}
	if err != nil {
		return nil, err
	}
	var ref InputSnapshot
	err = json.Unmarshal(raw, &ref)
	return &ref, err
}

func (t *Tx) ReadInputSnapshot(project, id string) (State, error) {
	var raw []byte
	err := t.QueryRow("SELECT state FROM xloom_input_snapshots WHERE project_id=? AND id=?", project, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, Err(404, "Input snapshot not found")
	}
	if err != nil {
		return State{}, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != id {
		return State{}, errors.New("input snapshot digest mismatch")
	}
	var state State
	err = json.Unmarshal(raw, &state)
	return state, err
}

// DecisionRetryKey preserves existing retry identity while its computation
// moves next to the authoritative graph, away from the dispatcher.
func DecisionRetryKey(g Graph, revision int64) string {
	ended := []string{}
	for _, i := range g.Intents {
		if i.To != nil || i.ConcludedAt != nil {
			ended = append(ended, i.ID)
		}
	}
	raw, _ := json.Marshal(struct {
		Facts    []Fact
		Hints    []Hint
		Ended    []string
		Revision int64
	}{g.Facts, g.Hints, ended, revision})
	sum := sha256.Sum256(raw)
	return "reason:" + hex.EncodeToString(sum[:])
}

// SchedulePage contains runtime metadata only. Node descriptions, evidence,
// hints and historical Jobs never cross the daily scheduling boundary.
type SchedulePage struct {
	Project          Project                   `json:"project"`
	FactCount        int                       `json:"fact_count"`
	HintCount        int                       `json:"hint_count"`
	OpenCount        int                       `json:"open_count"`
	Initial          bool                      `json:"initial"`
	Revision         int64                     `json:"revision"`
	DecisionRevision int64                     `json:"decision_revision"`
	StateVersion     string                    `json:"state_version"`
	RetryKey         string                    `json:"retry_key"`
	Intents          []Intent                  `json:"intents"`
	Steps            []Step                    `json:"steps"`
	NextOffset       int                       `json:"next_offset,omitempty"`
	ExecutionChecks  map[string]ExecutionCheck `json:"execution_checks,omitempty"`
}

const MaxSchedulePageSize = 1000

func (t *Tx) ScheduleInput(project string, offset int, expected string) (SchedulePage, error) {
	return t.ScheduleInputPage(project, offset, 100, expected)
}

// ScheduleInputPage amortizes state reconstruction across a larger, bounded
// compact projection. Each page still reads current runtime fields and validates
// its content version inside this transaction, without retaining a graph cache.
func (t *Tx) ScheduleInputPage(project string, offset, limit int, expected string) (SchedulePage, error) {
	if limit < 1 || limit > MaxSchedulePageSize {
		return SchedulePage{}, Err(422, "scheduling limit must be between 1 and 1000")
	}
	s, err := t.State(project)
	if err != nil {
		return SchedulePage{}, err
	}
	version := DecisionStateVersion(s)
	if expected != "" && expected != version {
		return SchedulePage{}, Err(409, "state_changed: scheduling input changed")
	}
	if offset < 0 || offset > len(s.Graph.Intents) {
		return SchedulePage{}, Err(422, "invalid scheduling offset")
	}
	p := SchedulePage{Project: s.Graph.Project, FactCount: len(s.Graph.Facts), HintCount: len(s.Graph.Hints), OpenCount: s.Graph.OpenCount(), Revision: s.Revision, DecisionRevision: s.DecisionRevision, StateVersion: version, RetryKey: DecisionRetryKey(s.Graph, s.DecisionRevision), Intents: []Intent{}, Steps: []Step{}}
	p.Initial = len(s.Graph.Facts) == 2
	origin, goal := false, false
	for _, f := range s.Graph.Facts {
		origin = origin || f.ID == "origin"
		goal = goal || f.ID == "goal"
	}
	p.Initial = p.Initial && origin && goal
	steps := map[string]Step{}
	for _, step := range s.Steps {
		steps[step.ID] = step
	}
	for _, i := range s.Graph.Intents {
		boot := i.To == nil && i.ConcludedAt == nil && i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin"
		p.Initial = p.Initial && boot
	}
	end := min(offset+limit, len(s.Graph.Intents))
	for _, i := range s.Graph.Intents[offset:end] {
		boot := i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin"
		if !boot {
			i.Description = ""
			i.Creator = ""
			i.From = nil
		}
		p.Intents = append(p.Intents, i)
		step := steps[i.ID]
		p.Steps = append(p.Steps, Step{ID: step.ID, Status: step.Status, Priority: step.Priority, InvalidSources: step.InvalidSources[:min(1, len(step.InvalidSources))]})
	}
	if end < len(s.Graph.Intents) {
		p.NextOffset = end
	}
	return p, nil
}
