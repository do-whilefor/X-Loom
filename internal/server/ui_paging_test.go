package server

import (
	"net/http"
	"testing"

	"xloom/internal/board"
)

func TestProjectExecutionPagingRetainsLegacyArrayAndValidatesBoundaries(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("reason", nil, 0)
	var legacy []board.ExecutionView
	f.request("GET", f.base()+"/executions", nil, false, http.StatusOK, &legacy)
	var page board.ExecutionViewPage
	f.request("GET", f.base()+"/executions?limit=1&cursor=0&through=0", nil, false, http.StatusOK, &page)
	if len(legacy) != 1 || len(page.Items) != 1 || page.Items[0].ID != legacy[0].ID || page.Items[0].Generation != legacy[0].Generation || page.NextCursor != 0 || page.Through == 0 {
		t.Fatalf("paging differs from legacy array: %+v %+v", legacy, page)
	}
	for _, query := range []string{"limit=0", "limit=101", "cursor=-1", "cursor=bad", "through=-1", "through=", "limit=1&cursor=3&through=2"} {
		f.request("GET", f.base()+"/executions?"+query, nil, false, http.StatusUnprocessableEntity, nil)
	}
	f.request("GET", "/projects/missing/executions?limit=20", nil, false, http.StatusNotFound, nil)
}
