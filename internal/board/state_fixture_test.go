package board

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stateBenchmarkSize struct {
	intents, descriptionBytes, sources int
}

type stateBenchmarkFixture struct {
	store   *Store
	size    stateBenchmarkSize
	version string
	from    []string
}

const stateBenchmarkProject = "benchmark"

var (
	stateBenchmarkPlanner = ExecutionFence{Run: "planner@benchmark", Lease: "reason"}
	stateBenchmarkWorker  = ExecutionFence{Run: "worker@benchmark", Lease: "intent", Intent: "active"}
)

// Share the same persisted graph and registered executions between regression
// tests and benchmarks so both exercise the production persistence paths.
func newStateBenchmarkFixture(tb testing.TB, size stateBenchmarkSize, decisionVersion int) *stateBenchmarkFixture {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "state.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	store.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	f := &stateBenchmarkFixture{store: store, size: size}
	err = store.Do(context.Background(), func(tx *Tx) error {
		g := Graph{
			Project: Project{ID: stateBenchmarkProject, Title: "Synthetic benchmark", Status: "active", CreatedAt: tx.Now,
				Reason: &Reason{Worker: stateBenchmarkPlanner.Run, Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}},
			Facts:   []Fact{{ID: "origin", Description: "Inspect the synthetic fixture"}, {ID: "goal", Description: "Explain the synthetic fixture"}},
			Intents: []Intent{}, Hints: []Hint{},
		}
		for n := 0; n < size.sources; n++ {
			id := fmt.Sprintf("source%03d", n)
			f.from = append(f.from, id)
			g.Facts = append(g.Facts, Fact{ID: id, Description: "Independent synthetic observation " + id})
		}
		// Full legacy Step JSON deliberately provides the same persisted input
		// before and after the metadata simplification, without using stateData.
		steps := []Step{}
		for n := 1; n <= size.intents; n++ {
			id, result := fmt.Sprintf("i%03d", n), fmt.Sprintf("f%03d", n)
			description := fmt.Sprintf("Historical task %04d: ", n) + strings.Repeat("x", size.descriptionBytes-22)
			g.Facts = append(g.Facts, Fact{ID: result, Description: "Synthetic result " + result})
			g.Intents = append(g.Intents, Intent{ID: id, From: f.from, To: Ptr(result), Description: description,
				Creator: "history", Worker: Ptr("worker@history"), CreatedAt: tx.Now, ConcludedAt: Ptr(tx.Now)})
			steps = append(steps, Step{ID: id, From: f.from, GoalID: "goal", Description: description,
				Status: "completed", Result: Ptr(result), Worker: Ptr("worker@history"), CreatedAt: tx.Now})
		}
		g.Intents = append(g.Intents, Intent{ID: "active", From: []string{"origin"}, Description: "Observe fixture",
			Creator: "history", Worker: Ptr(stateBenchmarkWorker.Run), Heartbeat: Ptr(tx.Now), CreatedAt: tx.Now})
		if err := tx.Save(g); err != nil {
			return err
		}
		for _, kind := range []string{"fact", "intent"} {
			if _, err := tx.Exec("INSERT INTO scoped_counters(project_id,kind,value) VALUES(?,?,?)", stateBenchmarkProject, kind, size.intents); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(map[string]any{"goals": []Goal{}, "steps": steps, "facts": []FactRecord{}, "findings": []Finding{}, "fact_relations": []FactRelation{}})
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,0,0)", stateBenchmarkProject, string(raw)); err != nil {
			return err
		}
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		f.version = DecisionStateVersion(state)
		job, err := json.Marshal(map[string]any{"kind": "reason", "run_id": "benchmark", "graph": state.Graph, "state": state,
			"decision": map[string]any{"version": decisionVersion, "state_version": f.version}, "budget": map[string]int{"max_intents": 64}})
		if err != nil {
			return err
		}
		return tx.RegisterExecution(Execution{ProjectID: stateBenchmarkProject, ID: "benchmark", Namespace: "benchmark", Backend: "planner",
			Kind: "reason", Lease: stateBenchmarkPlanner.Run, Job: job, RetryKey: "reason:benchmark"})
	})
	if err != nil {
		tb.Fatal(err)
	}
	return f
}

func (f *stateBenchmarkFixture) action(op string, index int) StateAction {
	payload := map[string]any{"action": "add", "from": f.from,
		"description": fmt.Sprintf("New task %02d: ", index) + strings.Repeat("y", f.size.descriptionBytes-13)}
	if op == "fact" {
		payload = map[string]any{"description": "New synthetic observation", "scope": "fixture", "observed_at": "2026-01-02T03:04:05Z",
			"evidence": []EvidenceRef{{RunID: stateBenchmarkWorker.Run, Path: "evidence/fixture.txt", Excerpt: "Synthetic observation"}}}
	}
	raw, _ := json.Marshal(payload)
	return StateAction{Op: op, IdempotencyKey: fmt.Sprintf("benchmark:%s:%d", op, index), Payload: raw, ExpectedVersion: f.version}
}

func (f *stateBenchmarkFixture) batch(count int) DecisionBatch {
	batch := DecisionBatch{ExpectedVersion: f.version}
	for n := 0; n < count; n++ {
		batch.Actions = append(batch.Actions, DecisionAction{Op: "step", Ref: fmt.Sprintf("step%d", n), Payload: f.action("step", n).Payload})
	}
	return batch
}
