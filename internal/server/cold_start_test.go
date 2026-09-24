package server

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"xloom/internal/board"
)

func TestLegacyBootstrapPendingResultResumesUnderItsRegisteredContract(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(map[int]string{0: "unversioned", 1: "outcome_v1"}[version], func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			f.registerWithFields("bootstrap", nil, version, map[string]any{"budget": map[string]int{"timeout": 123, "conclude_timeout": 17, "max_intents": 2}}, http.StatusCreated)
			output := `{"accepted":true,"data":{"fact":{"description":"Historical observation"},"complete":{"description":"Historical goal reached"}}}`
			if version == 1 {
				output = `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Historical observation"},"complete":{"description":"Historical goal reached"}}}`
			}
			f.pending(output)
			var before []board.Execution
			before = f.executionRecords()
			var resumed board.Execution
			f.request("POST", f.base()+"/executions/"+f.run+"/resume", map[string]any{}, true, http.StatusOK, &resumed)
			if len(before) != 1 || resumed.ID != before[0].ID || resumed.Kind != "bootstrap" || resumed.Lease != before[0].Lease || resumed.Status != "result_pending" || !bytes.Equal(resumed.Job, before[0].Job) || !bytes.Equal(resumed.Result, before[0].Result) {
				t.Fatal("bootstrap resume changed its identity, budget, result contract or pending result")
			}
			f.apply(http.StatusOK)
			f.apply(http.StatusOK)
			state := f.state()
			if len(state.Graph.Facts) != 3 || state.Graph.Project.Status != "completed" || len(state.Graph.Intents) != 2 {
				t.Fatal("historical pending bootstrap result was lost or applied twice")
			}
		})
	}
}

func TestNewProjectNormalizesDeprecatedBootstrapParameter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		present bool
	}{
		{"omitted", nil, false}, {"web_true", true, true}, {"explicit_false", false, true},
		{"legacy_string", "true", true}, {"legacy_numeric", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			body := map[string]any{"title": "Cold start", "origin": "Synthetic input", "goal": "Check one fixture"}
			if tc.present {
				body["bootstrap_enabled"] = tc.value
			}
			var graph board.Graph
			f.request("POST", "/projects", body, false, http.StatusCreated, &graph)
			if graph.Project.Bootstrap || len(graph.Intents) != 0 {
				t.Fatalf("new project retained bootstrap or started work: %+v", graph.Project)
			}
			var saved board.Graph
			f.request("GET", "/projects/"+graph.Project.ID, nil, false, http.StatusOK, &saved)
			if saved.Project.Bootstrap {
				t.Fatal("saved strategy differed from creation response")
			}
		})
	}
}

func TestDeprecatedBootstrapParameterStillRejectsInvalidInput(t *testing.T) {
	for _, value := range []any{nil, "sometimes", 2, []any{true}, map[string]any{"value": true}} {
		f := newExecutionProtocolFixture(t)
		f.request("POST", "/projects", map[string]any{"title": "Invalid strategy", "origin": "Input", "goal": "Goal", "bootstrap_enabled": value}, false, http.StatusUnprocessableEntity, nil)
		var projects []board.Summary
		f.request("GET", "/projects", nil, false, http.StatusOK, &projects)
		if len(projects) != 1 {
			t.Fatal("invalid creation persisted a project")
		}
	}
}

func TestHistoricalBootstrapStrategySurvivesLifecycleAndDatabaseReopen(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(map[bool]string{false: "decide", true: "bootstrap"}[bootstrap], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "historical.db")
			store, err := board.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			// Seed the saved historical value directly; POST /projects now normalizes it.
			err = store.Do(context.Background(), func(tx *board.Tx) error {
				return tx.Save(board.Graph{Project: board.Project{ID: "historical", Title: "Historical project", Status: "active", Bootstrap: bootstrap, CreatedAt: tx.Now}, Facts: []board.Fact{{ID: "origin", Description: "Original input"}, {ID: "goal", Description: "Original goal"}}})
			})
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = board.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			f := &executionProtocolFixture{t: t, handler: New(store), store: store, project: "historical"}
			assertStrategy := func() {
				var graph board.Graph
				f.request("GET", f.base(), nil, false, http.StatusOK, &graph)
				if graph.Project.Bootstrap != bootstrap {
					t.Fatal("historical bootstrap strategy was rewritten")
				}
			}
			assertStrategy()
			for _, status := range []string{"stopped", "active"} {
				f.request("PUT", f.base()+"/status", map[string]string{"status": status}, false, http.StatusOK, nil)
				assertStrategy()
			}
			f.request("POST", f.base()+"/complete", map[string]any{"from": []string{"origin"}, "description": "Historical completion", "worker": "fixture"}, false, http.StatusOK, nil)
			f.request("POST", f.base()+"/reopen", map[string]string{"description": "More checks requested", "creator": "user"}, false, http.StatusOK, nil)
			assertStrategy()
			f.request("POST", f.base()+"/restart", map[string]any{}, false, http.StatusOK, nil)
			assertStrategy()
		})
	}
}
