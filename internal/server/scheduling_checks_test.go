package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestSchedulingChecksAreOptionalAndPaged(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(f.project)
		if err != nil {
			return err
		}
		for n := 0; n < 105; n++ {
			g.Intents = append(g.Intents, board.Intent{ID: fmt.Sprintf("s%03d", n), From: []string{"origin"}, Description: "Observe fixture", Creator: "fixture", CreatedAt: tx.Now})
		}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	var legacy, first, next board.SchedulePage
	f.request("GET", f.base()+"/scheduling", nil, false, http.StatusOK, &legacy)
	if legacy.ExecutionChecks != nil {
		t.Fatal("legacy caller unexpectedly requested registry checks")
	}
	f.request("GET", f.base()+"/scheduling?namespace=test", nil, false, http.StatusOK, &first)
	if len(first.Intents) != 100 || len(first.ExecutionChecks) != 100 || first.NextOffset != 100 {
		t.Fatalf("first page: intents=%d checks=%d next=%d", len(first.Intents), len(first.ExecutionChecks), first.NextOffset)
	}
	f.request("GET", f.base()+"/scheduling?namespace=test&offset=100&expected_version="+first.StateVersion, nil, false, http.StatusOK, &next)
	if len(next.Intents) != 5 || len(next.ExecutionChecks) != 5 || next.NextOffset != 0 {
		t.Fatalf("next page: intents=%d checks=%d next=%d", len(next.Intents), len(next.ExecutionChecks), next.NextOffset)
	}
	for _, page := range []board.SchedulePage{first, next} {
		for _, intent := range page.Intents {
			check, ok := page.ExecutionChecks["explore:"+intent.ID]
			if !ok || check.Blocked || check.Pending {
				t.Fatalf("missing or blocked new candidate %s: %+v", intent.ID, check)
			}
		}
	}
	for _, namespace := range []string{"", strings.Repeat("n", 129)} {
		f.request("GET", f.base()+"/scheduling?namespace="+namespace, nil, false, http.StatusUnprocessableEntity, nil)
	}
	f.request("GET", f.base()+"/scheduling?namespace=test&expected_version=outdated", nil, false, http.StatusConflict, nil)
}
