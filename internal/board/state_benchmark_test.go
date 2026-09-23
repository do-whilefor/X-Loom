package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stateBenchmarkSize struct {
	intents, descriptionBytes, sources int
}

var stateBenchmarkSizes = []stateBenchmarkSize{
	{16, 128, 1},
	{128, 128, 1},
	{512, 128, 1},
	{128, 8192, 1},
	{128, 128, 16},
	{128, 8192, 16},
}

func (s stateBenchmarkSize) name() string {
	return fmt.Sprintf("intents=%d/description=%d/sources=%d", s.intents, s.descriptionBytes, s.sources)
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
	stateBenchmarkReset   = errors.New("rollback benchmark iteration")
)

// Setup and payload encoding are excluded from measurements. Every measured
// iteration starts a transaction on this fixed database and rolls it back,
// including successful CommitDecision calls. This measures the real write and
// validation paths without growing the graph or hitting idempotent receipts;
// it does not measure durable COMMIT/fsync latency.
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

func (f *stateBenchmarkFixture) run(fn func(*Tx) error) error {
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return stateBenchmarkReset
	})
	if errors.Is(err, stateBenchmarkReset) {
		return nil
	}
	return err
}

func BenchmarkStateAction(b *testing.B) {
	for _, op := range []string{"fact", "step"} {
		for _, size := range stateBenchmarkSizes {
			b.Run(op+"/"+size.name(), func(b *testing.B) {
				f := newStateBenchmarkFixture(b, size, 1)
				action, fence := f.action(op, 0), stateBenchmarkPlanner
				if op == "fact" {
					fence = stateBenchmarkWorker
				}
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.run(func(tx *Tx) error {
						result, err := tx.StateAction(stateBenchmarkProject, fence, action)
						if err == nil && (result.Unchanged || result.Revision != 1) {
							return fmt.Errorf("iteration did not add a node: %+v", result)
						}
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkDecisionBatch(b *testing.B) {
	for _, size := range stateBenchmarkSizes {
		for _, count := range []int{1, 8} {
			b.Run(fmt.Sprintf("%s/actions=%d", size.name(), count), func(b *testing.B) {
				f := newStateBenchmarkFixture(b, size, 2)
				batch := f.batch(count)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.run(func(tx *Tx) error {
						result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
						if err == nil && (!result.Committed || result.ChangedActions != count || len(result.Results) != count) {
							return fmt.Errorf("iteration did not apply %d actions: %+v", count, result)
						}
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func TestStateMutationBenchmarkFixture(t *testing.T) {
	for _, op := range []string{"fact", "step", "batch"} {
		t.Run(op, func(t *testing.T) {
			version := 1
			if op == "batch" {
				version = 2
			}
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 4}, version)
			for iteration := 0; iteration < 2; iteration++ {
				err := f.run(func(tx *Tx) error {
					addedFacts, addedIntents, changed := 0, 1, 1
					if op == "batch" {
						result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, f.batch(8))
						if err != nil {
							return err
						}
						if !result.Committed || result.ChangedActions != 8 || result.IDs["step0"] != "i017" || result.IDs["step7"] != "i024" {
							t.Fatalf("batch did not start from the fixture: %+v", result)
						}
						addedIntents, changed = 8, 8
					} else {
						fence, expectedID := stateBenchmarkPlanner, "i017"
						if op == "fact" {
							fence, expectedID, addedFacts, addedIntents = stateBenchmarkWorker, "f017", 1, 0
						}
						result, err := tx.StateAction(stateBenchmarkProject, fence, f.action(op, 0))
						if err != nil {
							return err
						}
						if result.Unchanged || result.ID != expectedID {
							t.Fatalf("action did not start from the fixture: %+v", result)
						}
					}
					state, err := tx.State(stateBenchmarkProject)
					if err != nil {
						return err
					}
					if len(state.Graph.Facts) != 22+addedFacts || len(state.Graph.Intents) != 17+addedIntents || state.Revision != int64(changed) {
						t.Fatalf("unexpected mutated fixture counts: facts=%d intents=%d revision=%d", len(state.Graph.Facts), len(state.Graph.Intents), state.Revision)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.store.Do(context.Background(), func(tx *Tx) error {
					state, err := tx.State(stateBenchmarkProject)
					if err != nil {
						return err
					}
					if state.Revision != 0 || DecisionStateVersion(state) != f.version {
						t.Fatal("iteration did not restore the original state")
					}
					events, err := tx.StateEvents(stateBenchmarkProject, 0)
					if err == nil && len(events) != 0 {
						t.Fatal("iteration left events behind")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
