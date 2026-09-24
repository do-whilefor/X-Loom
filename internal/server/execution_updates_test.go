package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/worker"
)

func prepareUpdateFixture(t *testing.T, input string) (*stepInvalidationFixture, worker.Job) {
	t.Helper()
	f := newStepInvalidationFixture(t)
	f.claim(http.StatusOK)
	e := f.registration()
	var job worker.Job
	if err := json.Unmarshal(e.Job, &job); err != nil {
		t.Fatal(err)
	}
	switch input {
	case "snapshot":
		job.State = nil
		job.Graph = board.Graph{Project: job.Graph.Project}
		e.Job, _ = json.Marshal(job)
		_, job = prepareSnapshot(t, f.executionProtocolFixture, e)
	case "legacy":
		job.State = nil
		e.Job, _ = json.Marshal(job)
		f.registerLegacy(e, http.StatusCreated)
	default:
		f.registerLegacy(e, http.StatusCreated)
	}
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
	return f, job
}

func updateRead(cursor *board.ExecuteUpdateCursor) worker.GraphRequest {
	return worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_updates", Updates: cursor}
}

func TestExecutionUpdatesUseOriginalInputAndBoundCurrentRevision(t *testing.T) {
	for _, input := range []string{"snapshot", "inline"} {
		t.Run(input, func(t *testing.T) {
			f, job := prepareUpdateFixture(t, input)
			baseline := job.State
			var revision int64
			if baseline != nil {
				revision = baseline.Revision
			} else {
				revision = job.InputSnapshot.Revision
			}
			// The correction precedes the first update read. Starting at the
			// live revision here would lose exactly this change.
			f.refute()
			var update board.ExecuteUpdates
			path := f.base() + "/executions/" + f.run + "/updates"
			raw := f.request("POST", path, updateRead(nil), true, http.StatusOK, &update)
			current := f.state()
			if len(raw) > board.MaxExecuteUpdateBytes+1 || !update.Complete || update.FromRevision != revision || update.ToRevision != current.Revision || update.StateVersion != board.DecisionStateVersion(current) || update.RunID != f.run || update.StepID != f.intent || update.Generation != job.Graph.Project.Generation || !reflect.DeepEqual(update.InvalidSources, []string{f.source}) || len(update.Relations) != 1 || update.Relations[0].Source != f.correction || len(update.Facts) != 2 {
				t.Fatalf("read did not bind original/current boundaries: %+v", update)
			}
			cursor := update.ExecuteUpdateCursor
			cursor.Revision = update.ToRevision
			f.request("POST", path, updateRead(&cursor), true, http.StatusOK, &update)
			if !update.Complete || len(update.Facts)+len(update.Relations)+len(update.InvalidSources) != 0 {
				t.Fatalf("consumed update repeated: %+v", update)
			}
			if input == "snapshot" {
				var frozen struct {
					Items []board.FactRecord `json:"items"`
				}
				f.request("POST", f.base()+"/executions/"+f.run+"/input/read", worker.GraphRequest{RequestID: strings.Repeat("b", 32), Op: "read_snapshot", Section: "facts", IDs: []string{f.source}}, true, http.StatusOK, &frozen)
				if len(frozen.Items) != 1 || frozen.Items[0].Status != "valid" {
					t.Fatalf("updates rewrote the immutable snapshot: %+v", frozen)
				}
			}
		})
	}
}

func TestExecutionUpdatesRejectChangedIdentityScopeAndLease(t *testing.T) {
	f, _ := prepareUpdateFixture(t, "snapshot")
	var update board.ExecuteUpdates
	path := f.base() + "/executions/" + f.run + "/updates"
	f.request("POST", path, updateRead(nil), true, http.StatusOK, &update)
	for _, change := range []func(*board.ExecuteUpdateCursor){
		func(c *board.ExecuteUpdateCursor) { c.ProjectID = "other" },
		func(c *board.ExecuteUpdateCursor) { c.Generation++ },
		func(c *board.ExecuteUpdateCursor) { c.StepID = "other" },
		func(c *board.ExecuteUpdateCursor) { c.RunID = "other" },
		func(c *board.ExecuteUpdateCursor) { c.Revision-- },
		func(c *board.ExecuteUpdateCursor) { c.Revision = update.ToRevision + 1 },
	} {
		cursor := update.ExecuteUpdateCursor
		change(&cursor)
		f.request("POST", path, updateRead(&cursor), true, http.StatusUnprocessableEntity, nil)
	}
	f.request("POST", path, updateRead(nil), false, http.StatusForbidden, nil)
	other := *f.executionProtocolFixture
	other.lease = "another@run"
	other.request("POST", path, updateRead(nil), true, http.StatusForbidden, nil)
	f.request("POST", path, map[string]any{"op": "read_updates", "request_id": strings.Repeat("a", 32), "sources": []string{f.correction}}, true, http.StatusUnprocessableEntity, nil)
	read := updateRead(nil)
	read.IDs = []string{f.correction}
	f.request("POST", path, read, true, http.StatusUnprocessableEntity, nil)
	// A real project restart archives the old execution. Old readers cannot
	// obtain data from the next generation under their former run identity.
	f.request("POST", f.base()+"/restart", map[string]any{"expected_generation": 0}, false, http.StatusOK, nil)
	f.request("POST", path, updateRead(nil), true, http.StatusNotFound, nil)
}

func TestExecutionUpdatesUnknownLegacyRevisionStaysPending(t *testing.T) {
	f, _ := prepareUpdateFixture(t, "legacy")
	f.refute()
	path := f.base() + "/executions/" + f.run + "/updates"
	var update board.ExecuteUpdates
	f.request("POST", path, updateRead(nil), true, http.StatusOK, &update)
	if update.Complete || update.PendingReason != "legacy_revision_unknown" || update.Revision != 0 || len(update.Facts) != 2 || update.ReadMore == "" {
		t.Fatalf("legacy read invented an original revision: %+v", update)
	}
	cursor := update.ExecuteUpdateCursor
	f.request("POST", path, updateRead(&cursor), true, http.StatusOK, &update)
	if update.Complete || update.PendingReason != "legacy_revision_unknown" {
		t.Fatalf("supplied cursor cleared unknown legacy boundary: %+v", update)
	}
	cursor.Revision = update.ToRevision
	f.request("POST", path, updateRead(&cursor), true, http.StatusUnprocessableEntity, nil)
}

func TestExecutionUpdatesHonorAbandonmentAndTerminalFence(t *testing.T) {
	for _, stop := range []string{"abandon", "terminal"} {
		t.Run(stop, func(t *testing.T) {
			f, _ := prepareUpdateFixture(t, "snapshot")
			if stop == "abandon" {
				f.decider.action("step", "abandon", map[string]string{"action": "abandon", "id": f.intent, "reason": "Direction no longer needed"})
			} else {
				f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "failed"}, true, http.StatusOK, nil)
			}
			f.request("POST", f.base()+"/executions/"+f.run+"/updates", updateRead(nil), true, http.StatusConflict, nil)
		})
	}
}
