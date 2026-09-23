package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
)

func TestLeaseUpdatesWriteOnlyLeaseColumns(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	intent := f.newIntent()
	before := f.state()
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		for _, table := range []string{"facts", "hints", "intents", "intent_sources"} {
			if _, err := tx.Exec("CREATE TRIGGER reject_lease_insert_" + table + " BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'lease rewrote graph'); END"); err != nil {
				return err
			}
		}
		_, err := tx.Exec("CREATE TRIGGER reject_lease_metadata BEFORE UPDATE OF data ON xloom_state BEGIN SELECT RAISE(ABORT,'lease rewrote metadata'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "heartbeat", "release"} {
		f.request("POST", f.base()+"/reason/"+op, map[string]string{"worker": "planner", "trigger": "initial"}, false, http.StatusOK, nil)
	}
	now := time.Date(2026, 9, 22, 10, 0, 1, 0, time.UTC)
	store.Now = func() time.Time { return now }
	for _, op := range []string{"heartbeat", "heartbeat", "release", "release"} {
		var saved board.Intent
		f.request("POST", f.base()+"/intents/"+intent.ID+"/"+op, map[string]string{"worker": "executor"}, false, http.StatusOK, &saved)
		if saved.Heartbeat == nil || *saved.Heartbeat != now.Format(time.RFC3339) || (op == "release") != (saved.Worker == nil) {
			t.Fatalf("%s returned incorrect lease: %+v", op, saved)
		}
	}
	after := f.state()
	if after.Revision != before.Revision || after.DecisionRevision != before.DecisionRevision || after.Graph.Project.Reason != nil || len(legacyEvents(f)) != 1 {
		t.Fatal("lease activity changed business state or left a reason claim")
	}
}

// The graph sizes match the S0 mutation benchmark. This measures the complete
// HTTP heartbeat and transaction on a fixed graph, including lease expiry.
func BenchmarkHeartbeat(b *testing.B) {
	for _, size := range []struct{ intents, description, sources int }{{16, 128, 1}, {128, 128, 1}, {512, 128, 1}, {128, 8192, 1}, {128, 128, 16}, {128, 8192, 16}} {
		for _, kind := range []string{"reason", "intent"} {
			b.Run(fmt.Sprintf("%s/intents=%d/description=%d/sources=%d", kind, size.intents, size.description, size.sources), func(b *testing.B) {
				store, err := board.Open(filepath.Join(b.TempDir(), "heartbeat.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				store.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
				if err = store.Do(context.Background(), func(tx *board.Tx) error {
					g := board.Graph{Project: board.Project{ID: "benchmark", Title: "Synthetic benchmark", Status: "active", CreatedAt: tx.Now,
						Reason: &board.Reason{Worker: "planner", Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}},
						Facts: []board.Fact{{ID: "origin", Description: "Inspect the synthetic fixture"}, {ID: "goal", Description: "Explain the synthetic fixture"}}}
					from := []string{}
					for n := 0; n < size.sources; n++ {
						id := fmt.Sprintf("source%03d", n)
						from = append(from, id)
						g.Facts = append(g.Facts, board.Fact{ID: id, Description: "Independent synthetic observation " + id})
					}
					for n := 1; n <= size.intents; n++ {
						id, result := fmt.Sprintf("i%03d", n), fmt.Sprintf("f%03d", n)
						g.Facts = append(g.Facts, board.Fact{ID: result, Description: "Synthetic result " + result})
						g.Intents = append(g.Intents, board.Intent{ID: id, From: from, To: &result, Description: strings.Repeat("x", size.description), Creator: "history", CreatedAt: tx.Now, ConcludedAt: board.Ptr(tx.Now)})
					}
					g.Intents = append(g.Intents, board.Intent{ID: "active", From: []string{"origin"}, Description: "Observe fixture", Creator: "history", Worker: board.Ptr("executor"), Heartbeat: board.Ptr(tx.Now), CreatedAt: tx.Now})
					return tx.Save(g)
				}); err != nil {
					b.Fatal(err)
				}
				path, worker := "/projects/benchmark/reason/heartbeat", "planner"
				if kind == "intent" {
					path, worker = "/projects/benchmark/intents/active/heartbeat", "executor"
				}
				body := []byte(`{"worker":"` + worker + `"}`)
				handler := New(store)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, httptest.NewRequest("POST", path, bytes.NewReader(body)))
					if response.Code != http.StatusOK {
						b.Fatalf("heartbeat: %d: %s", response.Code, response.Body.String())
					}
				}
			})
		}
	}
}
