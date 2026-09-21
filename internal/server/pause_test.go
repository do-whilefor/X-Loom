package server

import (
	"testing"

	b "xloom/internal/board"
)

func TestContinueDoesNotRetryPreexistingFailures(t *testing.T) {
	for _, kind := range []string{"reason", "bootstrap", "explore"} {
		for _, status := range []string{"failed", "rejected", "cancelled"} {
			t.Run(kind+"/"+status, func(t *testing.T) {
				h, e := executionFixture(t, kind)
				executionOp(t, h, e, "status", `{"status":"`+status+`","result":{"status":"failed","error":"preexisting failure"}}`, 200)
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
				var entries []b.Execution
				getUIJSON(t, h, "/executions?namespace=test", &entries)
				if len(entries) != 1 || entries[0].Status != status {
					t.Fatalf("continue authorized unrelated failure: %+v", entries)
				}
			})
		}
	}
}

func TestContinueDoesNotReviveAbandonedDirection(t *testing.T) {
	h, e := executionFixture(t, "explore")
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"planner@decision","trigger":"change plan"}`, 200)
	stateActionCall(t, h, true, "step", "abandon", `{"action":"abandon","id":"i001","reason":"no longer needed"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
	executionOp(t, h, e, "status", `{"status":"cancelled","result":{"status":"failed","error":"abandoned"}}`, 200)
	var entries []b.Execution
	getUIJSON(t, h, "/executions?namespace=test", &entries)
	if len(entries) != 1 || entries[0].Status != "cancelled" || readState(t, h).Steps[0].Status != "abandoned" {
		t.Fatal("continue revived an abandoned direction")
	}
}
