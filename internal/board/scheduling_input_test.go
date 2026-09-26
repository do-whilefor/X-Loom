package board

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestScheduleInputLargePageMatchesLegacyPages(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{205, 8192, 4}, 2)
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		full, err := tx.ScheduleInputPage(stateBenchmarkProject, 0, MaxSchedulePageSize, "")
		if err != nil {
			return err
		}
		var combined SchedulePage
		for offset := 0; ; {
			page, err := tx.ScheduleInput(stateBenchmarkProject, offset, full.StateVersion)
			if err != nil {
				return err
			}
			if len(page.Intents) > 100 {
				t.Fatal("legacy page exceeded its bound")
			}
			if offset == 0 {
				combined = page
			} else {
				combined.Intents = append(combined.Intents, page.Intents...)
				combined.Steps = append(combined.Steps, page.Steps...)
			}
			combined.NextOffset = page.NextOffset
			if page.NextOffset == 0 {
				break
			}
			offset = page.NextOffset
		}
		if full.NextOffset != 0 || len(full.Intents) != 206 || !reflect.DeepEqual(full, combined) {
			t.Fatal("single-transaction scheduling projection differs from its pages")
		}
		for _, intent := range full.Intents {
			if intent.Description != "" || intent.Creator != "" || intent.From != nil {
				t.Fatal("scheduling included business content")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleInputLargePageReadsCurrentLeasesAndRejectsChangedContent(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{105, 128, 1}, 2)
	var original SchedulePage
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		var err error
		original, err = tx.ScheduleInputPage(stateBenchmarkProject, 0, MaxSchedulePageSize, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE intents SET worker='replacement@run',last_heartbeat_at='later' WHERE project_id=? AND id='active'", stateBenchmarkProject); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE projects SET reason_worker='replacement@plan',reason_last_heartbeat_at='later' WHERE id=?", stateBenchmarkProject); err != nil {
			return err
		}
		current, err := tx.ScheduleInputPage(stateBenchmarkProject, 0, MaxSchedulePageSize, original.StateVersion)
		if err != nil {
			return err
		}
		active := current.Intents[len(current.Intents)-1]
		if current.StateVersion != original.StateVersion || current.RetryKey != original.RetryKey || Value(active.Worker) != "replacement@run" || Value(active.Heartbeat) != "later" || current.Project.Reason.Worker != "replacement@plan" {
			t.Fatal("lease refresh changed the content version or returned stale runtime metadata")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback changed graph")
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		// Save is also used by legacy callers; it need not advance State.Revision.
		// The content version must still detect a write within this transaction.
		if _, err := tx.Exec("INSERT INTO hints(id,project_id,content,creator,created_at) VALUES('new',?,'new content','fixture',?)", stateBenchmarkProject, tx.Now); err != nil {
			return err
		}
		_, err := tx.ScheduleInputPage(stateBenchmarkProject, 0, MaxSchedulePageSize, original.StateVersion)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 409 {
			t.Fatalf("changed content accepted old version: %v", err)
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		current, err := tx.ScheduleInputPage(stateBenchmarkProject, 0, MaxSchedulePageSize, original.StateVersion)
		if err == nil && (current.HintCount != original.HintCount || current.NextOffset != 0) {
			t.Fatal("rolled back input leaked into a later schedule")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSchedulingInput(b *testing.B) {
	for _, limit := range []int{100, MaxSchedulePageSize} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			f := newStateBenchmarkFixture(b, stateBenchmarkSize{512, 8192, 4}, 2)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				version := ""
				for offset := 0; ; {
					var page SchedulePage
					if err := f.store.Do(context.Background(), func(tx *Tx) error {
						var err error
						page, err = tx.ScheduleInputPage(stateBenchmarkProject, offset, limit, version)
						return err
					}); err != nil {
						b.Fatal(err)
					}
					if page.NextOffset == 0 {
						break
					}
					offset, version = page.NextOffset, page.StateVersion
				}
			}
		})
	}
}

func TestScheduleInputLargePageBoundsAndContinuation(t *testing.T) {
	store := executionQueryStore(t)
	executionQueryTx(t, store, func(tx *Tx) {
		g := Graph{Project: Project{ID: "p", Title: "Many compact steps", Status: "active", CreatedAt: tx.Now}, Facts: []Fact{{ID: "origin"}, {ID: "goal"}}}
		for n := 0; n < 2051; n++ {
			g.Intents = append(g.Intents, Intent{ID: fmt.Sprintf("i%04d", n), From: []string{"origin"}, Description: "observe", Creator: "fixture", CreatedAt: tx.Now})
		}
		if err := tx.Save(g); err != nil {
			t.Fatal(err)
		}
		for _, limit := range []int{-1, 0, MaxSchedulePageSize + 1} {
			if _, err := tx.ScheduleInputPage("p", 0, limit, ""); err == nil {
				t.Fatalf("invalid page limit accepted: %d", limit)
			}
		}
		version := ""
		for n, want := range []int{1000, 1000, 51} {
			page, err := tx.ScheduleInputPage("p", n*1000, MaxSchedulePageSize, version)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Intents) != want || len(page.Steps) != want || page.Intents[0].ID != fmt.Sprintf("i%04d", n*1000) || (n < 2 && page.NextOffset != (n+1)*1000) || (n == 2 && page.NextOffset != 0) {
				t.Fatalf("large page boundary lost steps: page=%d intents=%d next=%d", n, len(page.Intents), page.NextOffset)
			}
			version = page.StateVersion
		}
	})
}
