package server

import (
	"net/http"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestHintAdmissionPreservesRunnableContextAndRollsBack(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for n := 0; n < 3; n++ {
		f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("x", 8192), "creator": "user"}, false, http.StatusCreated, nil)
	}
	before := f.state()
	response := f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("y", 8192), "creator": "user"}, false, http.StatusUnprocessableEntity, nil)
	if !strings.Contains(response, "input_context_limit") {
		t.Fatalf("missing actionable admission error: %s", response)
	}
	after := f.state()
	if len(after.Graph.Hints) != 3 || after.Revision != before.Revision || len(legacyEvents(f)) != 3 {
		t.Fatal("rejected Hint changed the graph or event cursor")
	}
	if _, err := board.BuildDecisionContextFromCursor(after, nil, nil, board.DefaultContextViewBytes); err != nil {
		t.Fatalf("accepted requirements cannot start Decide: %v", err)
	}
	var hint board.Hint
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Keep every earlier requirement", "creator": "user"}, false, http.StatusCreated, &hint)
	if hint.ID != "h004" || len(f.state().Graph.Hints) != 4 {
		t.Fatal("failed admission consumed IDs or discarded an earlier Hint")
	}
}

func TestHintAdmissionIncludesExistingExecuteContext(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	var step board.Intent
	f.request("POST", f.base()+"/intents", map[string]any{"from": []string{"origin"}, "description": strings.Repeat("s", 16000), "creator": "user"}, false, http.StatusCreated, &step)
	f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("h", 16000), "creator": "user"}, false, http.StatusUnprocessableEntity, nil)
	state := f.state()
	if len(state.Graph.Hints) != 0 {
		t.Fatal("Hint fits Decide alone but prevents the pending Step from starting")
	}
	if _, err := board.ContextView(state, step.ID, board.DefaultContextViewBytes); err != nil {
		t.Fatal(err)
	}
}

func TestInitialInputAndTitleAdmissionCountsEncodedBytes(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	// Each control character expands to six JSON bytes. Checking only raw
	// character count would admit a project whose initial prompt cannot fit.
	f.request("POST", "/projects", map[string]string{"title": "Escaped input", "origin": strings.Repeat("\x01", 6000), "goal": "Observe fixture"}, false, http.StatusUnprocessableEntity, nil)
	var projects []board.Summary
	f.request("GET", "/projects", nil, false, http.StatusOK, &projects)
	if len(projects) != 1 {
		t.Fatal("rejected project left partial data")
	}
	before := f.state().Graph.Project.Title
	f.request("PUT", f.base()+"/title", map[string]string{"title": strings.Repeat("t", 32768)}, false, http.StatusUnprocessableEntity, nil)
	if f.state().Graph.Project.Title != before {
		t.Fatal("oversized title was persisted")
	}
}
