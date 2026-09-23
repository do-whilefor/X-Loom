package board

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func overviewState() State {
	return State{
		Graph: Graph{Project: Project{ID: "project", Status: "active", Generation: 1}, Facts: []Fact{
			{ID: "origin", Description: "Inspect the specified service only; preserve other users' data."},
			{ID: "goal", Description: "Check authentication and explain any untested scope."},
		}},
		Goals: []Goal{{ID: "goal", Condition: "Check authentication and explain any untested scope.", Status: "open"}},
		FactRecords: []FactRecord{
			{ID: "first", Description: "The service accepts session tokens in query parameters.", Status: "valid"},
			{ID: "second", Description: "The reverse proxy retains complete request URLs in access logs.", Status: "valid"},
		},
		Findings: []Finding{{ID: "candidate", Claim: "A possible credential disclosure path needs verification.", Sources: []string{"first"}, Status: "candidate", SupportValid: true}},
		Revision: 1,
	}
}

func decodeOverview(t *testing.T, raw json.RawMessage) *contextOverview {
	t.Helper()
	var view struct {
		Overview *contextOverview `json:"overview"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Overview == nil {
		t.Fatal("clean context has no discovery overview")
	}
	return view.Overview
}

func overviewSection(t *testing.T, overview *contextOverview, name string) contextOverviewSection {
	t.Helper()
	for _, section := range overview.Sections {
		if section.Section == name {
			return section
		}
	}
	t.Fatalf("missing %s section", name)
	return contextOverviewSection{}
}

func TestDecisionOverviewRevealsUnchangedFactsAndFindings(t *testing.T) {
	previous := overviewState()
	current := previous
	current.FactRecords = append(append([]FactRecord{}, previous.FactRecords...), FactRecord{ID: "new", Description: "The status endpoint is reachable.", Status: "valid"})
	current.Revision++
	cursor := &DecisionCursor{ProjectID: current.Graph.Project.ID, Generation: current.Graph.Project.Generation, Revision: previous.Revision}
	decision, err := BuildDecisionContextFromCursor(current, cursor, []StateEvent{{Revision: 2, Op: "fact", ID: "new"}}, DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != "changes" {
		t.Fatalf("expected incremental evidence, got %s (%s)", decision.Mode, decision.Fallback)
	}
	var body decisionView
	if err := json.Unmarshal(decision.View, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Facts) != 1 || body.Facts[0].ID != "new" {
		t.Fatalf("unrelated history should not expand evidence bodies: %+v", body.Facts)
	}
	overview := decodeOverview(t, decision.View)
	for _, section := range overview.Sections {
		if section.Omitted != 0 || len(section.Items) != section.Total {
			t.Fatalf("small graph overview is incomplete: %+v", section)
		}
	}
	facts := overviewSection(t, overview, "facts")
	for _, expected := range previous.FactRecords {
		found := false
		for _, fact := range facts.Items {
			if fact.ID == expected.ID && fact.Excerpt == expected.Description && fact.Status == expected.Status {
				found = true
			}
		}
		if !found {
			t.Fatalf("old fact %s is undiscoverable; its combination with another old fact could be missed", expected.ID)
		}
	}
	findings := overviewSection(t, overview, "findings")
	if len(findings.Items) != 1 || findings.Items[0].ID != "candidate" || findings.Items[0].Sources[0] != "first" {
		t.Fatalf("unchanged finding lost its support: %+v", findings)
	}
	if len(body.UserInputs) != 2 || body.UserInputs[0].Description != current.Graph.Facts[0].Description {
		t.Fatal("the original scope and constraints were not retained")
	}
}

func TestDecisionOverviewPreservesCorrectionsAndAffectedPlans(t *testing.T) {
	state := overviewState()
	state.FactRecords[0].Status = "superseded"
	state.FactRecords = append(state.FactRecords,
		FactRecord{ID: "correction", Description: "Only test credentials were accepted.", Status: "superseded"},
		FactRecord{ID: "latest", Description: "Production credentials in the query were rejected.", Status: "valid"},
	)
	state.FactRelations = []FactRelation{
		{Kind: "supersedes", Source: "latest", Target: "correction", Reason: "Production scope was checked."},
		{Kind: "supersedes", Source: "correction", Target: "first", Reason: "The original observation concerned test credentials."},
	}
	state.Steps = []Step{
		{ID: "waiting", GoalID: "goal", From: []string{"first"}, Status: "needs_review", Description: "Check the credential disclosure path.", InvalidSources: []string{"first"}},
		{ID: "running", GoalID: "goal", From: []string{"first"}, Status: "running", Description: "Inspect the proxy log policy.", InvalidSources: []string{"first"}},
	}
	state.Findings[0].SupportValid = false
	decision, err := BuildDecisionContextFromCursor(state, nil, nil, 12<<10)
	if err != nil {
		t.Fatal(err)
	}
	overview := decodeOverview(t, decision.View)
	steps := overviewSection(t, overview, "steps")
	if len(steps.Items) != 2 {
		t.Fatalf("affected plans disappeared: %+v", steps)
	}
	for _, step := range steps.Items {
		if len(step.InvalidSources) != 1 || step.InvalidSources[0] != "first" {
			t.Fatalf("correction impact missing from plan: %+v", step)
		}
	}
	relations := overviewSection(t, overview, "relations")
	if relations.Omitted != 0 || len(relations.Items) != 2 {
		t.Fatalf("correction chain missing: %+v", relations)
	}
	var body struct {
		Facts []contextFact `json:"fact_records"`
	}
	if err := json.Unmarshal(decision.View, &body); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "correction", "latest"} {
		found := false
		for _, fact := range body.Facts {
			if fact.ID == id && fact.Description != "" && !fact.DetailsOmitted {
				found = true
			}
		}
		if !found {
			t.Fatalf("correction chain lost full evidence for %s", id)
		}
	}
}

func TestDecisionOverviewBoundsLargePlansAndChangeIndexes(t *testing.T) {
	current := overviewState()
	// The first 1000 pairs predate the cursor. The latest 500 pairs fill the
	// production event page with 1000 continuous fact and step revisions.
	cursor := &DecisionCursor{ProjectID: current.Graph.Project.ID, Generation: current.Graph.Project.Generation, Revision: 2001}
	var events []StateEvent
	for i := 0; i < 1500; i++ {
		stepID, factID := fmt.Sprintf("step-%04d", i), fmt.Sprintf("fact-%04d", i)
		current.Steps = append(current.Steps, Step{ID: stepID, GoalID: "goal", Status: "open", From: []string{"first"}, Description: "Inspect one bounded part of the agreed scope."})
		current.FactRecords = append(current.FactRecords, FactRecord{ID: factID, Description: strings.Repeat("Evidence detail. ", 100), Status: "valid"})
		current.Revision += 2
		if i >= 1000 {
			events = append(events, StateEvent{Revision: current.Revision - 1, Op: "step", ID: stepID}, StateEvent{Revision: current.Revision, Op: "fact", ID: factID})
		}
	}
	const budget = 16 << 10
	decision, err := BuildDecisionContextFromCursor(current, cursor, events, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.View) > budget || decision.Fallback != "related_context_over_budget" {
		t.Fatalf("large context did not fall back within budget: bytes=%d fallback=%s", len(decision.View), decision.Fallback)
	}
	overview := decodeOverview(t, decision.View)
	for _, name := range []string{"steps", "facts"} {
		section := overviewSection(t, overview, name)
		if section.Omitted == 0 || section.Total != section.Omitted+len(section.Items) || section.Offset != 0 || section.Limit != 20 {
			t.Fatalf("large section cannot be discovered by paging: %+v", section)
		}
	}
	var body struct {
		UserInputs     []Fact         `json:"user_inputs"`
		ChangedOmitted map[string]int `json:"changed_omitted"`
	}
	if err := json.Unmarshal(decision.View, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.UserInputs) != 2 || body.UserInputs[1].Description != current.Graph.Facts[1].Description {
		t.Fatal("large history displaced the immutable user conditions")
	}
	if body.ChangedOmitted["fact_records"]+body.ChangedOmitted["steps"] == 0 {
		t.Fatal("large change index was truncated without an omission count")
	}
}

func TestDecisionOverviewMarksTruncatedTextAndReferences(t *testing.T) {
	state := overviewState()
	state.FactRecords[0].Description = strings.Repeat("权限范围核对。", 80)
	for i := 0; i < 30; i++ {
		state.Findings[0].Sources = append(state.Findings[0].Sources, fmt.Sprintf("ref-%d", i))
	}
	view, err := ContextView(state, "", DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	overview := decodeOverview(t, view)
	for _, fact := range overviewSection(t, overview, "facts").Items {
		if fact.ID == "first" && (!fact.TextTruncated || !utf8.ValidString(fact.Excerpt) || len(fact.Excerpt) > 240 || !strings.HasPrefix(state.FactRecords[0].Description, fact.Excerpt)) {
			t.Fatalf("excerpt changed the original observation or hid truncation: %+v", fact)
		}
	}
	finding := overviewSection(t, overview, "findings").Items[0]
	if len(finding.Sources)+finding.SourcesOmitted != len(state.Findings[0].Sources) || finding.SourcesOmitted == 0 {
		t.Fatalf("support references were truncated silently: %+v", finding)
	}
}

func TestDecisionContextNeverTruncatesUserRequirementsToFit(t *testing.T) {
	state := overviewState()
	state.Graph.Facts[0].Description = strings.Repeat("Required scope and constraints. ", 2000)
	if _, err := BuildDecisionContextFromCursor(state, nil, nil, 4096); err == nil {
		t.Fatal("oversized original requirements should fail explicitly instead of being silently shortened")
	}
}
