package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestGraphHandlerRoutesUpdatesWithRegisteredIdentity(t *testing.T) {
	job := worker.Job{RunID: "run", Kind: "explore", GraphRPC: true, Graph: board.Graph{Project: board.Project{ID: "project", Generation: 2}}, Intent: &board.Intent{ID: "step"}}
	cursor := &board.ExecuteUpdateCursor{ProjectID: "project", Generation: 2, StepID: "step", RunID: "run", Revision: 4}
	request := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_updates", Updates: cursor}
	reads := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/project/executions/run/identity":
			_ = json.NewEncoder(w).Encode(board.ExecutionSummary{ProjectID: "project", Generation: 2, ID: "run", Namespace: "xloom", Kind: "explore", Intent: "step", Lease: "backend@run"})
		case "/projects/project/executions/run/updates":
			reads++
			if r.Method != "POST" || r.Header.Get("X-Xloom-Run") != "backend@run" || r.Header.Get("X-Xloom-Lease") != "explore" || r.Header.Get("X-Xloom-Intent") != "step" {
				t.Error("update route lost registered lease")
			}
			var got worker.GraphRequest
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.Op != request.Op || got.RequestID != request.RequestID || !reflect.DeepEqual(got.Updates, cursor) {
				t.Errorf("cursor changed across dispatcher: %+v", got)
			}
			_ = json.NewEncoder(w).Encode(board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: *cursor, FromRevision: 4, ToRevision: 5, Complete: true})
		default:
			t.Errorf("unexpected updates route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	runner := &batchProtocolRunner{}
	New(config.Config{Server: api.URL}, runner)
	result, err := runner.handler(context.Background(), job, request)
	if err != nil || reads != 1 {
		t.Fatalf("updates forwarding failed: %v, reads=%d", err, reads)
	}
	raw, _ := json.Marshal(result)
	var update board.ExecuteUpdates
	if err := json.Unmarshal(raw, &update); err != nil || update.ToRevision != 5 || update.RunID != job.RunID {
		t.Fatalf("updates forwarding changed response: %s, %v", raw, err)
	}
}
