package board

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

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

var stateBenchmarkReset = errors.New("rollback benchmark iteration")

// Setup and payload encoding are excluded from measurements. Every measured
// iteration starts a transaction on this fixed database and rolls it back,
// including successful CommitDecision calls. This measures the real write and
// validation paths without growing the graph or hitting idempotent receipts;
// it does not measure durable COMMIT/fsync latency.
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
