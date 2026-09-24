package server

import (
	"fmt"
	"net/http"
	"testing"

	"xloom/internal/board"
)

func TestDecisionBatchMakesProgressAfterFiniteConcurrentObservations(t *testing.T) {
	f := newDecisionBatchFixture(t)
	execute := *f
	execute.run, execute.lease = "observer-run", "planner@observer-run"
	live := true
	execute.register("explore", &live, 2)
	var latest string
	conflicts := 0
	for n := 0; n < 3; n++ {
		// A planner reads a stable snapshot while an already running Execute
		// obtains another independent observation before the plan commits.
		batch := f.batch(batchAction("step", "next", `{"action":"add","from":["origin"],"description":"Plan from an older view"}`))
		fact := evidenceFixtureFact(execute.run)
		fact["description"] = fmt.Sprintf("Independent fixture observation %d", n)
		latest = execute.action("fact", fmt.Sprintf("observation-%d", n), fact).ID
		before := f.state()
		f.decision("commit", batch, http.StatusConflict)
		conflicts++
		after := f.state()
		if after.Revision != before.Revision || len(after.Steps) != 1 || f.decisionReceipt(http.StatusOK).Committed {
			t.Fatal("stale plan escaped after a concurrent observation")
		}
	}
	// With the finite publisher settled, a newly read, revised plan commits.
	batch := f.batch(batchAction("step", "next", fmt.Sprintf(`{"action":"add","from":[%q],"description":"Review the latest independent observation"}`, latest)))
	receipt := f.decision("commit", batch, http.StatusOK)
	if !receipt.Committed || conflicts != 3 || len(f.state().Steps) != 2 {
		t.Fatalf("no progress after bounded conflicts: %+v", receipt)
	}
	var executions []board.Execution
	executions = f.executionRecords()
	for _, e := range executions {
		if e.ID == f.run && e.Status != "succeeded" {
			t.Fatal("committed planner has no durable success")
		}
	}
}

func TestDecisionBatchRefusalDoesNotPublishDraftOrRequireCommit(t *testing.T) {
	f := newDecisionBatchFixture(t)
	before := f.state()
	f.pending(`{"accepted":false,"reason":"The supplied input is outside this task scope"}`)
	f.apply(http.StatusOK)
	f.apply(http.StatusOK)
	after := f.state()
	if len(after.Steps) != len(before.Steps) || len(after.Graph.Facts) != len(before.Graph.Facts) || after.Graph.Project.Status != "active" {
		t.Fatal("refusal published a plan, observation or project completion")
	}
	var executions []board.Execution
	executions = f.executionRecords()
	if len(executions) != 1 || executions[0].Status != "rejected" {
		t.Fatalf("refusal was not delivered idempotently: %+v", executions)
	}
}
