package server

import (
	"net/http"
	"testing"

	"xloom/internal/board"
)

func TestProjectExecutionPagingHasOneResponseShapeAndValidatesBoundaries(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("reason", nil, 0)
	var defaults board.ExecutionViewPage
	f.request("GET", f.base()+"/executions", nil, false, http.StatusOK, &defaults)
	var page board.ExecutionViewPage
	f.request("GET", f.base()+"/executions?limit=1&cursor=0&through=0", nil, false, http.StatusOK, &page)
	if len(defaults.Items) != 1 || len(page.Items) != 1 || page.Items[0].ID != defaults.Items[0].ID || page.Items[0].Generation != defaults.Items[0].Generation || page.NextCursor != 0 || page.Through == 0 || page.Through != defaults.Through {
		t.Fatalf("default paging differs from explicit page: %+v %+v", defaults, page)
	}
	for _, query := range []string{"limit=0", "limit=101", "cursor=-1", "cursor=bad", "through=-1", "through=", "limit=1&cursor=3&through=2"} {
		f.request("GET", f.base()+"/executions?"+query, nil, false, http.StatusUnprocessableEntity, nil)
	}
	f.request("GET", "/projects/missing/executions?limit=20", nil, false, http.StatusNotFound, nil)
}

func TestProjectIdentityTracksRestartsWithoutGraphPayload(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	var identity map[string]any
	f.request("GET", f.base()+"/identity", nil, false, http.StatusOK, &identity)
	if len(identity) != 2 || identity["id"] != f.project || identity["generation"] != float64(0) {
		t.Fatalf("unexpected initial identity: %+v", identity)
	}
	f.request("POST", f.base()+"/restart", map[string]int{"expected_generation": 0}, false, http.StatusOK, nil)
	f.request("GET", f.base()+"/identity", nil, false, http.StatusOK, &identity)
	if len(identity) != 2 || identity["id"] != f.project || identity["generation"] != float64(1) {
		t.Fatalf("identity missed restart: %+v", identity)
	}
	f.request("GET", "/projects/missing/identity", nil, false, http.StatusNotFound, nil)
}

func TestRetiredHTTPRoutesAreUnavailable(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for _, path := range []string{"/ui/overview", "/executions?namespace=protocol-test", f.base() + "/state/changes?after=0&through=1"} {
		f.request("GET", path, nil, false, http.StatusNotFound, nil)
	}
	f.request("POST", f.base()+"/executions", map[string]any{}, false, http.StatusMethodNotAllowed, nil)
}
