package board

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProjectExecutionsUseMetadataAndBoundedPublicResult(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		result, _ := json.Marshal(map[string]any{"text": strings.Repeat("观", 65537), "error": strings.Repeat("错", 4097), "thinking": strings.Repeat("PRIVATE", 1<<18)})
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "archived", Kind: "reason", Status: "succeeded", Job: json.RawMessage("archived, not JSON"), Result: result}, 7, "")
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "malformed", Kind: "reason", Status: "failed", Job: json.RawMessage("archived"), Result: json.RawMessage("not JSON")}, 8, "")
		nul, _ := json.Marshal(map[string]string{"text": "before\x00after"})
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "nul", Kind: "reason", Status: "succeeded", Result: nul}, 8, "")
		views, err := tx.ProjectExecutions("proj_001")
		if err != nil {
			return err
		}
		if len(views) != 3 || views[0].Generation != 7 || views[1].Generation != 8 || views[1].Result != nil {
			t.Fatalf("wrong metadata projection: %+v", views)
		}
		if result := views[0].Result; result == nil || len([]rune(result.Text)) != 65536 || len([]rune(result.Error)) != 4096 || !result.Truncated {
			t.Fatal("public result lost its character boundaries")
		}
		if result := views[2].Result; result == nil || result.Text != "before\x00after" || result.Truncated {
			t.Fatalf("embedded NUL changed result: %+v", result)
		}
		raw, _ := json.Marshal(views)
		if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "archived, not JSON") || strings.Contains(string(raw), `"job"`) {
			t.Fatal("private input/result fields leaked into the view")
		}
		return nil
	})
}

func TestProjectExecutionPagesBoundBytesAndRetainAllHistory(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		result, _ := json.Marshal(map[string]string{"text": strings.Repeat("\n", 65536)})
		for n := 0; n < 12; n++ {
			putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: fmt.Sprintf("run-%02d", n), Kind: "reason", Status: "succeeded", Result: result}, 0, "")
		}
		page, err := tx.ProjectExecutionPage("proj_001", 0, 0, 100)
		if err != nil {
			return err
		}
		if page.NextCursor == 0 || len(page.Items) == 0 || len(page.Items) >= 12 {
			t.Fatalf("byte limit did not return a continuation: %+v", page)
		}
		through := page.Through
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "new-run", Kind: "reason", Status: "running"}, 0, "")
		seen := map[string]bool{}
		for {
			raw, _ := json.Marshal(page)
			if len(raw) > MaxExecutionViewPageBytes || page.Through != through {
				t.Fatalf("page lost its boundary: %d bytes", len(raw))
			}
			for _, entry := range page.Items {
				if seen[entry.ID] || entry.ID == "new-run" {
					t.Fatalf("duplicate or post-boundary entry: %s", entry.ID)
				}
				seen[entry.ID] = true
			}
			if page.NextCursor == 0 {
				break
			}
			page, err = tx.ProjectExecutionPage("proj_001", page.NextCursor, through, 100)
			if err != nil {
				return err
			}
		}
		if len(seen) != 12 {
			t.Fatalf("lost execution history: got %d entries", len(seen))
		}
		legacy, err := tx.ProjectExecutions("proj_001")
		if err == nil && len(legacy) != 13 {
			t.Fatalf("legacy array lost entries: %d", len(legacy))
		}
		return err
	})
}
