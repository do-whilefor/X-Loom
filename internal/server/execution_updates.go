package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	b "xloom/internal/board"
	"xloom/internal/worker"
)

// Reads and filters under one transaction: event boundaries and node versions
// cannot come from different revisions. The HTTP body supplies no source IDs.
func (s *Server) executionUpdates(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return 0, nil, err
	}
	var read worker.GraphRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&read); err != nil || read.Op != "read_updates" {
		return 0, nil, b.Err(422, "expected a read_updates request")
	}
	e, err := t.Execution(r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	if r.Header.Get("X-Xloom-Run") != e.Lease || r.Header.Get("X-Xloom-Lease") != e.Kind || r.Header.Get("X-Xloom-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Updates read requires its execution lease")
	}
	if e.Kind == "reason" || e.Intent == "" || !e.Pending() || e.Status == "result_pending" {
		return 0, nil, b.Err(409, "Updates require an active Execute run")
	}
	var job worker.Job
	if err = json.Unmarshal(e.Job, &job); err != nil {
		return 0, nil, err
	}
	if err = worker.ValidateGraphRequest(job, read); err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	current, err := t.State(e.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	if err = guard(t, current.Graph, r); err != nil {
		return 0, nil, err
	}
	if job.Graph.Project.ID != e.ProjectID || job.Graph.Project.Generation != current.Graph.Project.Generation || job.RunID != e.ID || job.Kind != e.Kind {
		return 0, nil, b.Err(409, "Updates input belongs to another execution or project round")
	}
	original := job.State
	if job.InputSnapshot != nil {
		state, err := t.ReadInputSnapshot(e.ProjectID, job.InputSnapshot.ID)
		if err != nil {
			return 0, nil, err
		}
		original = &state
	}
	base := b.ExecuteUpdateCursor{ProjectID: e.ProjectID, Generation: job.Graph.Project.Generation, StepID: e.Intent, RunID: e.ID}
	known := original != nil
	var sources []string
	if original != nil {
		if original.Graph.Project.ID != base.ProjectID || original.Graph.Project.Generation != base.Generation {
			return 0, nil, b.Err(409, "Updates original input identity mismatch")
		}
		base.Revision = original.Revision
		for _, step := range original.Steps {
			if step.ID == e.Intent {
				sources = step.From
				break
			}
		}
		if sources == nil {
			for _, intent := range original.Graph.Intents {
				if intent.ID == e.Intent {
					sources = intent.From
					break
				}
			}
		}
	}
	if sources == nil && job.Intent != nil && job.Intent.ID == e.Intent {
		sources = job.Intent.From
	}
	if len(sources) == 0 {
		return 0, nil, b.Err(409, "Updates original input has no Step sources")
	}
	cursor := base
	if read.Updates != nil {
		cursor = *read.Updates
		if cursor.ProjectID != base.ProjectID || cursor.Generation != base.Generation || cursor.StepID != base.StepID || cursor.RunID != base.RunID || cursor.Revision < base.Revision || cursor.Revision > current.Revision || (!known && cursor.Revision != 0) {
			return 0, nil, b.Err(422, "Updates cursor does not match the registered input")
		}
	}
	changes, err := t.StateChanges(e.ProjectID, cursor.Revision, current.Revision)
	if err != nil {
		return 0, nil, err
	}
	updates, err := b.BuildExecuteUpdates(current, cursor, sources, changes, known, b.MaxExecuteUpdateBytes)
	if err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	return http.StatusOK, updates, nil
}
