package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"xloom/internal/board"
)

func legacyEvents(f *executionProtocolFixture) []board.StateEvent {
	f.t.Helper()
	var events []board.StateEvent
	f.request("GET", f.base()+"/state/events", nil, false, http.StatusOK, &events)
	return events
}

func TestLegacyHTTPMutationsPublishOrderedBusinessEvents(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	intent := f.newIntent()
	initial := f.state()
	if initial.Revision != 1 || initial.DecisionRevision != 0 {
		t.Fatalf("creating a Step did not emit its event, or retriggered Decide: %+v", initial)
	}
	for _, op := range []string{"heartbeat", "heartbeat", "release", "release", "heartbeat"} {
		f.request("POST", f.base()+"/intents/"+intent.ID+"/"+op, map[string]string{"worker": "legacy-executor"}, false, http.StatusOK, nil)
	}
	f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": "legacy-planner", "trigger": "initial"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/reason/heartbeat", map[string]string{"worker": "legacy-planner"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/reason/release", map[string]string{"worker": "legacy-planner"}, false, http.StatusOK, nil)
	if state := f.state(); state.Revision != initial.Revision || state.DecisionRevision != initial.DecisionRevision {
		t.Fatal("claims, heartbeats or releases emitted domain changes")
	}
	var conclusion board.Conclusion
	f.request("POST", f.base()+"/intents/"+intent.ID+"/conclude", map[string]string{"worker": "legacy-executor", "description": "Observed an authenticated fixture response"}, false, http.StatusOK, &conclusion)
	f.request("POST", f.base()+"/complete", map[string]any{"from": []string{conclusion.Fact.ID}, "description": "Required fixture checked", "worker": "legacy-planner"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/reopen", map[string]string{"description": "The fixture changed", "creator": "user"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Check the changed response", "creator": "user"}, false, http.StatusCreated, nil)
	state, events := f.state(), legacyEvents(f)
	ops := []string{"step", "step_completed", "complete", "reopen", "hint"}
	if len(events) != len(ops) || state.Revision != 5 || state.DecisionRevision != 3 {
		t.Fatalf("legacy changes were not represented exactly once: revision=%d decision=%d events=%+v", state.Revision, state.DecisionRevision, events)
	}
	for n, event := range events {
		if event.Op != ops[n] || event.Revision != int64(n+1) || event.ID == "" || !json.Valid(event.Payload) || !json.Valid(event.Result) {
			t.Fatalf("invalid event %d: %+v", n, event)
		}
	}
	var recorded board.Conclusion
	if err := json.Unmarshal(events[1].Result, &recorded); err != nil || recorded.Fact.ID != conclusion.Fact.ID || recorded.Intent.ID != intent.ID || events[1].RunID != "legacy-executor" {
		t.Fatalf("completion event lost its observation, Step or run: %+v", events[1])
	}
}

func TestLegacyEventDoesNotEnableExtendedCompletionRules(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Legacy input", "creator": "user"}, false, http.StatusCreated, nil)
	// Pure compatibility callers historically may complete using original input.
	// Event counters alone must not opt them into the extended fact contract.
	f.request("POST", f.base()+"/complete", map[string]any{"from": []string{"origin"}, "description": "Legacy completion", "worker": "legacy"}, false, http.StatusOK, nil)
	if f.state().Graph.Project.Status != "completed" || len(legacyEvents(f)) != 2 {
		t.Fatal("event adaptation changed the legacy completion contract")
	}
}

func TestLegacyFinalResultReplayAndModernActionsDoNotDuplicateEvents(t *testing.T) {
	t.Run("persisted final result", func(t *testing.T) {
		f := newExecutionProtocolFixture(t)
		f.register("explore", nil, 0)
		before := f.state()
		f.pending(`{"accepted":true,"data":{"description":"Legacy verified observation"}}`)
		f.apply(http.StatusOK)
		first := f.state()
		f.apply(http.StatusOK)
		after := f.state()
		if first.Revision != before.Revision+1 || first.DecisionRevision != before.DecisionRevision+1 || after.Revision != first.Revision || len(legacyEvents(f)) != int(after.Revision) {
			t.Fatal("result replay duplicated or omitted the legacy completion event")
		}
	})
	t.Run("modern action and duplicate compatibility plan", func(t *testing.T) {
		f := newExecutionProtocolFixture(t)
		f.register("reason", nil, 0)
		input := map[string]any{"action": "add", "from": []string{"origin"}, "description": "Observe one fixture"}
		f.action("step", "one-step", input)
		f.action("step", "one-step", input)
		f.request("POST", f.base()+"/intents", map[string]any{"from": []string{"origin"}, "description": "Observe one fixture", "creator": f.lease}, true, http.StatusOK, nil)
		if state := f.state(); state.Revision != 1 || state.DecisionRevision != 0 || len(legacyEvents(f)) != 1 {
			t.Fatal("modern command or duplicate compatibility plan emitted extra events")
		}
	})
}
