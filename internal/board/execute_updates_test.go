package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func executeUpdateFixture() (State, ExecuteUpdateCursor, []StateChange) {
	state := State{Graph: Graph{Project: Project{ID: "p", Generation: 3}}, Revision: 2,
		FactRecords: []FactRecord{
			{ID: "F1", Description: "An old authenticated-session observation", Status: "refuted"},
			{ID: "F2", Description: "A fresh session is denied", Status: "valid", Evidence: []EvidenceRef{{RunID: "producer", Path: "evidence.txt", Excerpt: "HTTP 403"}}},
			{ID: "unrelated", Description: "Independent observation from the same producer", Status: "valid"},
		}, FactRelations: []FactRelation{{Kind: "refutes", Target: "F1", Source: "F2", Reason: "Different session identity"}}}
	return state, ExecuteUpdateCursor{ProjectID: "p", Generation: 3, StepID: "S1", RunID: "run", Revision: 1}, []StateChange{{Revision: 2, Op: "fact_relation", ID: "F1"}}
}

func TestExecuteUpdatesCarryKnownCorrectionAndSupport(t *testing.T) {
	state, cursor, events := executeUpdateFixture()
	before, _ := json.Marshal(state)
	update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !update.Complete || update.Version != 1 || update.FromRevision != 1 || update.ToRevision != 2 || update.Revision != 1 || update.RunID != "run" || !reflect.DeepEqual(update.InvalidSources, []string{"F1"}) {
		t.Fatalf("correction has wrong boundary/identity: %+v", update)
	}
	if len(update.Facts) != 2 || update.Facts[1].ID != "F2" || len(update.Facts[1].Evidence) != 1 || len(update.Relations) != 1 || update.Relations[0].Reason == "" {
		t.Fatalf("correction support missing or unrelated fact included: %+v", update)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("update projection mutated authoritative state")
	}
	cursor.Revision = update.ToRevision
	repeated, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, nil, true, 0)
	if err != nil || !repeated.Complete || len(repeated.Facts)+len(repeated.Relations)+len(repeated.InvalidSources) != 0 {
		t.Fatalf("acknowledged correction was repeated: %+v, %v", repeated, err)
	}
}

func TestExecuteUpdatesDoNotBroadcastUnrelatedChanges(t *testing.T) {
	state, cursor, _ := executeUpdateFixture()
	update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, []StateChange{{Revision: 2, Op: "fact", ID: "unrelated"}}, true, 0)
	if err != nil || !update.Complete || update.ToRevision != 2 || len(update.Facts)+len(update.Relations)+len(update.InvalidSources) != 0 || update.ReadMore != "" {
		t.Fatalf("unrelated update was injected: %+v, %v", update, err)
	}
}

func TestExecuteUpdatesBoundSilentAcknowledgementEnvelope(t *testing.T) {
	state, cursor, _ := executeUpdateFixture()
	events := []StateChange{{Revision: 2, Op: "fact", ID: "unrelated"}}
	if response, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, 64); err == nil || response != nil {
		t.Fatalf("unrelated event bypassed the response budget: %+v, %v", response, err)
	}
	response, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(response)
	exact, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, len(raw))
	if err != nil || !exact.Complete || len(exact.Facts) != 0 {
		t.Fatalf("exact envelope budget rejected a silent acknowledgement: %+v, %v", exact, err)
	}
}

func TestExecuteUpdatesFollowCorrectionChainAndSourceStatus(t *testing.T) {
	state, cursor, _ := executeUpdateFixture()
	state.FactRecords[1].Status = "narrowed"
	state.FactRecords = append(state.FactRecords, FactRecord{ID: "F3", Description: "Only one environment was denied", Status: "valid"})
	state.FactRelations = append(state.FactRelations, FactRelation{Kind: "narrows", Target: "F2", Source: "F3", Reason: "Environment scope"})
	for _, op := range []string{"fact_relation", "fact", "reopen"} {
		update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, []StateChange{{Revision: 2, Op: op, ID: "F2"}}, true, 0)
		if err != nil || !update.Complete || len(update.Relations) != 2 || len(update.Facts) != 3 {
			t.Fatalf("%s lost corrective chain: %+v, %v", op, update, err)
		}
	}
}

func TestExecuteUpdatesRetainPendingRanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		known  bool
		events []StateChange
		want   string
	}{
		{"gap", true, nil, "event_gap"},
		{"wrong_revision", true, []StateChange{{Revision: 1, Op: "fact_relation", ID: "F1"}}, "event_gap"},
		{"unknown_event", true, []StateChange{{Revision: 2, Op: "future_mutation", ID: "F1"}}, "unknown_event"},
		{"legacy", false, []StateChange{{Revision: 2, Op: "fact_relation", ID: "F1"}}, "legacy_revision_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, cursor, _ := executeUpdateFixture()
			update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, tc.events, tc.known, 0)
			if err != nil || update.Complete || update.PendingReason != tc.want || update.Revision != cursor.Revision || len(update.Facts) != 2 || update.ReadMore == "" {
				t.Fatalf("incomplete scan lost its pending range/review: %+v, %v", update, err)
			}
		})
	}
	state, cursor, events := executeUpdateFixture()
	state.Revision = 2002
	update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, 0)
	if err != nil || update.Complete || update.PendingReason != "event_limit" || update.FromRevision != 1 || update.ToRevision != 2002 {
		t.Fatalf("event limit acknowledged unscanned changes: %+v, %v", update, err)
	}
}

func TestExecuteUpdatesAreBoundedWithoutTruncatingEvidence(t *testing.T) {
	state, cursor, events := executeUpdateFixture()
	state.FactRecords[1].Evidence[0].Excerpt = strings.Repeat("原始证据", MaxExecuteUpdateBytes)
	update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(update)
	if len(raw) > MaxExecuteUpdateBytes || update.Complete || update.PendingReason != "context_budget" || update.Omitted["facts"] != 1 || len(update.Relations) != 1 || update.Revision != 1 || update.ReadMore == "" {
		t.Fatalf("oversized evidence hid its omission/boundary: bytes=%d, %+v", len(raw), update)
	}
	if len(state.FactRecords[1].Evidence[0].Excerpt) != len("原始证据")*MaxExecuteUpdateBytes {
		t.Fatal("evidence was changed")
	}
}

func TestExecuteUpdatesValidateIdentityAndTerminateCycles(t *testing.T) {
	state, cursor, events := executeUpdateFixture()
	for _, mutate := range []func(*ExecuteUpdateCursor){
		func(c *ExecuteUpdateCursor) { c.ProjectID = "other" },
		func(c *ExecuteUpdateCursor) { c.Generation++ },
		func(c *ExecuteUpdateCursor) { c.StepID = "" },
		func(c *ExecuteUpdateCursor) { c.RunID = "" },
		func(c *ExecuteUpdateCursor) { c.Revision = -1 },
		func(c *ExecuteUpdateCursor) { c.Revision = 3 },
	} {
		bad := cursor
		mutate(&bad)
		if _, err := BuildExecuteUpdates(state, bad, []string{"F1"}, events, true, 0); err == nil {
			t.Fatalf("accepted invalid cursor %+v", bad)
		}
	}
	state.FactRelations = append(state.FactRelations, FactRelation{Kind: "refutes", Target: "F2", Source: "F1"})
	update, err := BuildExecuteUpdates(state, cursor, []string{"F1", "F1"}, events, true, 0)
	if err != nil || len(update.Facts) != 2 || len(update.Relations) != 2 {
		t.Fatalf("cyclic legacy relation did not terminate: %+v, %v", update, err)
	}
}

func TestExecuteUpdatesKeepAuthoritativeVersionAndOnlyCorrectiveRelations(t *testing.T) {
	state, cursor, events := executeUpdateFixture()
	state.FactRelations = append(state.FactRelations, FactRelation{Kind: "supports", Source: "unrelated", Target: "F1"})
	// Graph-only compatibility projection must not change the version used by
	// read_graph for the exact retained evidence that did not fit this response.
	for _, fact := range state.FactRecords {
		state.Graph.Facts = append(state.Graph.Facts, Fact{ID: fact.ID, Description: fact.Description})
	}
	state.FactRecords = nil
	version := DecisionStateVersion(state)
	update, err := BuildExecuteUpdates(state, cursor, []string{"F1"}, events, true, 0)
	if err != nil || update.StateVersion != version || len(update.Relations) != 1 || len(update.Facts) != 2 {
		t.Fatalf("legacy projection changed version or expanded non-correction: %+v, %v", update, err)
	}
}

func TestExecuteUpdatesCapCorrectionClosure(t *testing.T) {
	state, cursor, events := executeUpdateFixture()
	state.FactRecords = nil
	state.FactRelations = nil
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("F%d", i)
		state.FactRecords = append(state.FactRecords, FactRecord{ID: id, Status: "valid"})
		state.FactRelations = append(state.FactRelations, FactRelation{Kind: "refutes", Target: id, Source: fmt.Sprintf("F%d", i+1)})
	}
	update, err := BuildExecuteUpdates(state, cursor, []string{"F0"}, events, true, 0)
	if err != nil || update.Complete || update.PendingReason != "closure_limit" || update.Omitted["closure_frontier"] == 0 || update.Revision != cursor.Revision {
		t.Fatalf("large closure was silently acknowledged: %+v, %v", update, err)
	}
	raw, _ := json.Marshal(update)
	if len(raw) > MaxExecuteUpdateBytes {
		t.Fatalf("unbounded closure: %d bytes", len(raw))
	}
}
