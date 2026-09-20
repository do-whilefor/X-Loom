package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestGraphPagesRetainCompleteObjectsAndExposeNextOffset(t *testing.T) {
	s := board.State{FactRecords: []board.FactRecord{{ID: "f1", Description: "first", Status: "valid"}, {ID: "f2", Description: "second", Status: "refuted"}, {ID: "f3", Description: "third", Status: "valid"}}}
	value, err := GraphPage(s, GraphRequest{Section: "facts", Offset: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var page graphPage
	json.Unmarshal(raw, &page)
	if page.Total != 3 || page.Offset != 1 || page.NextOffset == nil || *page.NextOffset != 2 || len(page.Items) != 1 || !strings.Contains(string(page.Items[0]), "refuted") {
		t.Fatal(string(raw))
	}
	s.FactRecords[0].Description = strings.Repeat("x", MaxGraphRPCBytes)
	if _, err := GraphPage(s, GraphRequest{Section: "facts", Limit: 1}); err == nil {
		t.Fatal("silently truncated oversized fact")
	}
	for _, r := range []GraphRequest{{Section: "unknown"}, {Section: "facts", Offset: -1}, {Section: "facts", Limit: 51}} {
		if _, err := GraphPage(s, r); err == nil {
			t.Fatal("accepted invalid page", r)
		}
	}
}

func TestBootstrapContextDoesNotRequireSyntheticIntentInGraph(t *testing.T) {
	j := Job{Kind: "bootstrap", Graph: board.Graph{Project: board.Project{ID: "p"}, Facts: []board.Fact{{ID: "origin", Description: "start"}, {ID: "goal", Description: "finish"}}}, Intent: &board.Intent{ID: "bootstrap", Description: "bootstrap task"}}
	if _, err := jobContextView(j); err != nil {
		t.Fatal(err)
	}
}
