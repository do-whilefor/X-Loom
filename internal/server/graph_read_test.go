package server

import (
	"net/http"
	"testing"

	"xloom/internal/worker"
)

func TestGraphReadValidatesRequestsAndFencesContinuations(t *testing.T) {
	f := newDecisionBatchFixture(t)
	read := worker.GraphRequest{RequestID: "00000000000000000000000000000001", Op: "read_graph", Section: "facts", IDs: []string{"origin"}, Limit: 1}
	var page struct {
		StateVersion string                `json:"state_version"`
		Items        []struct{ ID string } `json:"items"`
	}
	f.request("POST", f.base()+"/state/read", read, true, http.StatusOK, &page)
	if page.StateVersion == "" || len(page.Items) != 1 || page.Items[0].ID != "origin" {
		t.Fatalf("invalid graph page: %+v", page)
	}
	for _, body := range []any{
		map[string]any{"request_id": read.RequestID, "op": read.Op, "unknown": true},
		worker.GraphRequest{RequestID: read.RequestID, Op: "graph_action"},
		worker.GraphRequest{RequestID: read.RequestID, Op: "read_graph", Section: "unknown"},
		worker.GraphRequest{RequestID: read.RequestID, Op: "read_graph", Offset: -1},
		worker.GraphRequest{RequestID: read.RequestID, Op: "read_graph", Section: "evidence"},
		worker.GraphRequest{Op: "read_graph"},
	} {
		f.request("POST", f.base()+"/state/read", body, true, http.StatusUnprocessableEntity, nil)
	}
	read.ExpectedVersion = page.StateVersion
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "A newer observation"}, false, http.StatusCreated, nil)
	f.request("POST", f.base()+"/state/read", read, true, http.StatusConflict, nil)
	read.ExpectedVersion = ""
	f.request("PUT", f.base()+"/status", map[string]string{"status": "stopped"}, false, http.StatusOK, nil)
	f.request("PUT", f.base()+"/status", map[string]string{"status": "active"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/state/read", read, true, http.StatusConflict, nil)
}
