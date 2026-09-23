package board

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLoadPreservesInterleavedSourceOrderAndEmptyCollections(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		for _, id := range []string{"empty", "first", "second"} {
			if _, err := tx.Exec("INSERT INTO intents(id,project_id,description,creator,created_at) VALUES(?,'proj_001',?,'fixture',?)", id, id, tx.Now); err != nil {
				return err
			}
		}
		for _, pair := range [][2]string{{"second", "f002"}, {"first", "f001"}, {"second", "origin"}, {"first", "f002"}} {
			if _, err := tx.Exec("INSERT INTO intent_sources(intent_id,project_id,fact_id) VALUES(?,'proj_001',?)", pair[0], pair[1]); err != nil {
				return err
			}
		}
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		want := [][]string{{}, {"f001", "f002"}, {"f002", "origin"}}
		for n, intent := range g.Intents {
			if !reflect.DeepEqual(intent.From, want[n]) {
				t.Fatalf("intent %s sources=%#v want=%#v", intent.ID, intent.From, want[n])
			}
		}
		return nil
	})
}

func TestEventReadsPreserveExistenceBoundariesAndLimit(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		for revision := int64(1); revision <= 1003; revision++ {
			raw, _ := json.Marshal(StateEvent{Revision: revision, Op: "step", ID: "i001"})
			if _, err := tx.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES('proj_001',?,?)", revision, string(raw)); err != nil {
				return err
			}
		}
		// Event discovery depends only on the project and event index. Unrelated
		// graph data must never be read just to establish project existence.
		if _, err := tx.Exec("DROP TABLE facts"); err != nil {
			return err
		}
		events, err := tx.StateEvents("proj_001", 1)
		if err != nil {
			return err
		}
		if len(events) != 1000 || events[0].Revision != 2 || events[999].Revision != 1001 {
			t.Fatalf("event bounds: %d", len(events))
		}
		changes, err := tx.StateChanges("proj_001", 1000, 1002)
		if err != nil {
			return err
		}
		if len(changes) != 2 || changes[0].Revision != 1001 || changes[1].Revision != 1002 {
			t.Fatalf("change bounds: %+v", changes)
		}
		events, err = tx.StateEvents("proj_001", 1003)
		if err != nil {
			return err
		}
		if events == nil || len(events) != 0 {
			t.Fatalf("empty events: %#v", events)
		}
		changes, err = tx.StateChanges("proj_001", 1003, 1003)
		if err != nil {
			return err
		}
		if changes == nil || len(changes) != 0 {
			t.Fatalf("empty changes: %#v", changes)
		}
		_, err = tx.StateChanges("proj_001", 2, 1)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 422 {
			t.Fatalf("invalid interval: %v", err)
		}
		_, err = tx.StateEvents("missing", 0)
		if !errors.As(err, &api) || api.Status != 404 {
			t.Fatalf("missing event project: %v", err)
		}
		_, err = tx.StateChanges("missing", 2, 1)
		if !errors.As(err, &api) || api.Status != 404 {
			t.Fatalf("missing change project must precede interval validation: %v", err)
		}
		return nil
	})
}

func BenchmarkEmptyEventRead(b *testing.B) {
	for _, size := range stateBenchmarkSizes {
		for _, op := range []string{"events", "changes"} {
			b.Run(op+"/"+size.name(), func(b *testing.B) {
				f := newStateBenchmarkFixture(b, size, 1)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.store.Do(context.Background(), func(tx *Tx) error {
						if op == "events" {
							_, err := tx.StateEvents(stateBenchmarkProject, 0)
							return err
						}
						_, err := tx.StateChanges(stateBenchmarkProject, 0, 0)
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
