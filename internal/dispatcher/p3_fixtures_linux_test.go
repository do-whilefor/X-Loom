//go:build linux

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/worker"
)

// These are synthetic, fixed tasks, not measurements of real deployment success.
// Expectations never enter the worker prompt. Presentation is the sole ablation
// for discovery: the underlying graph, root requirement and evidence stay fixed.
type p3Case struct {
	Name, Origin, Goal, Presentation string
	Facts                            []board.FactRecord
	WantComplete                     bool
	RequiredIDs                      []string
	ExpectedAnswer                   string
}

func p3Cases() []p3Case {
	var cases []p3Case
	for _, presentation := range []string{"visible", "id", "page"} {
		facts := []board.FactRecord{p3Fact("f001", "Route observation for local deployment build-42: route R17 selects backend slot beta.", `{"build":"build-42","route":"R17","slot":"beta"}`)}
		for i := 2; i <= 62; i++ {
			facts = append(facts, p3Fact(fmt.Sprintf("f%03d", i), fmt.Sprintf("Inventory observation for unrelated archive item %d; this item has no routing or backend configuration.", i), fmt.Sprintf(`{"archive_item":%d,"retained":true}`, i)))
		}
		// The short discovery excerpt cannot itself reveal the answer.
		facts = append(facts, p3Fact("f063", "Backend catalog observation for local deployment build-42. "+strings.Repeat("The catalog was read from the local retained inventory. ", 5)+`Catalog: alpha=worker-3:8100; beta=worker-7:4317; gamma=worker-9:6200.`, `{"build":"build-42","backends":{"alpha":"worker-3:8100","beta":"worker-7:4317","gamma":"worker-9:6200"}}`))
		cases = append(cases, p3Case{
			Name: fmt.Sprintf("P3-0%d_%s", len(cases)+1, presentation), Presentation: presentation,
			Origin: "A synthetic local routing inventory. Independent observation branches recorded the active route pointer and backend catalog for build-42. No external service is involved.",
			Goal:   "Determine the host:port serving route R17 in local deployment build-42 from the retained observations. Record the selected host:port and its supporting facts in the completion description. No deployment changes or separate report file are requested.",
			Facts:  facts, WantComplete: true, RequiredIDs: []string{"f001", "f063"}, ExpectedAnswer: "worker-7:4317",
		})
	}
	return append(cases, p3AcceptanceCases()...)
}

func p3Fact(id, description, evidence string) board.FactRecord {
	return board.FactRecord{ID: id, Description: description, Scope: "synthetic local fixture", ObservedAt: "2026-09-01T00:00:00Z", Status: "valid", Evidence: []board.EvidenceRef{{RunID: "fixture-" + id, Path: "retained/observation.json", Excerpt: evidence}}}
}

func p3InitialView(state board.State, presentation string) (*board.DecisionContext, error) {
	if presentation == "full" {
		view, err := board.BuildDecisionContext(state, nil, nil, board.DefaultContextViewBytes)
		if err == nil {
			view.Version = 2
		}
		return view, err
	}
	if presentation != "visible" && presentation != "id" && presentation != "page" {
		return nil, fmt.Errorf("unknown P3 presentation %q", presentation)
	}
	// Start with the production closure for the same changed fact in every arm.
	// Only the B body and overview entry are ablated below. This measures use of
	// presented information, not whether a production selector chose that arm.
	cursor := &board.DecisionCursor{ProjectID: state.Graph.Project.ID, Generation: state.Graph.Project.Generation, Revision: state.Revision - 1}
	view, err := board.BuildDecisionContextFromCursor(state, cursor, []board.StateEvent{{Revision: state.Revision, Op: "fact", ID: "f001"}}, board.DefaultContextViewBytes)
	if err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(view.View, &body); err != nil {
		return nil, err
	}
	var records []board.FactRecord
	if err = json.Unmarshal(body["fact_records"], &records); err != nil {
		return nil, err
	}
	filtered := records[:0]
	var second board.FactRecord
	for _, fact := range state.FactRecords {
		if fact.ID == "f063" {
			second = fact
		}
	}
	if second.ID == "" {
		return nil, fmt.Errorf("P3 discovery fixture has no second fact")
	}
	for _, fact := range records {
		if fact.ID != second.ID {
			filtered = append(filtered, fact)
		}
	}
	if presentation == "visible" {
		filtered = append(filtered, second)
	}
	body["fact_records"], _ = json.Marshal(filtered)
	var omitted map[string]int
	if err = json.Unmarshal(body["omitted"], &omitted); err != nil {
		return nil, err
	}
	// The production count excludes immutable origin/goal input records.
	visible := 0
	for _, fact := range filtered {
		if fact.ID != "origin" && fact.ID != "goal" {
			visible++
		}
	}
	total := 0
	for _, fact := range state.FactRecords {
		if fact.ID != "origin" && fact.ID != "goal" {
			total++
		}
	}
	omitted["fact_records"] = total - visible
	body["omitted"], _ = json.Marshal(omitted)
	var overview struct {
		Sections []map[string]json.RawMessage `json:"sections"`
		ReadMore string                       `json:"read_more"`
	}
	if err = json.Unmarshal(body["overview"], &overview); err != nil {
		return nil, err
	}
	for _, section := range overview.Sections {
		var name string
		_ = json.Unmarshal(section["section"], &name)
		if name != "facts" {
			continue
		}
		var items []map[string]json.RawMessage
		_ = json.Unmarshal(section["items"], &items)
		kept := items[:0]
		for _, item := range items {
			var id string
			_ = json.Unmarshal(item["id"], &id)
			if id != second.ID {
				kept = append(kept, item)
			}
		}
		if presentation != "page" {
			raw, _ := json.Marshal(map[string]any{"id": second.ID, "status": "valid", "excerpt": second.Description[:240], "text_truncated": true})
			var item map[string]json.RawMessage
			_ = json.Unmarshal(raw, &item)
			kept = append(kept, item)
		}
		var count int
		_ = json.Unmarshal(section["total"], &count)
		section["items"], _ = json.Marshal(kept)
		section["omitted"], _ = json.Marshal(count - len(kept))
	}
	body["overview"], _ = json.Marshal(overview)
	view.View, err = json.Marshal(body)
	view.Version = 2
	return view, err
}

func TestP3DiscoveryPresentationAndPaging(t *testing.T) {
	cases := p3Cases()[:3]
	var initial string
	for i, spec := range cases {
		if !reflect.DeepEqual(spec.Facts, cases[0].Facts) || spec.Origin != cases[0].Origin || spec.Goal != cases[0].Goal {
			t.Fatal("discovery arms changed the task or evidence")
		}
		t.Run(spec.Name, func(t *testing.T) {
			fixture := newP3EvaluationFixture(t, spec, context.Background())
			normalized := fixture.Initial
			normalized.Graph.Project.ID = "fixture-project"
			rawState, err := json.Marshal(normalized)
			if err != nil {
				t.Fatal(err)
			}
			if initial == "" {
				initial = string(rawState)
			} else if initial != string(rawState) {
				t.Fatal("discovery arms changed the persisted graph beyond project identity")
			}
			prompt, err := worker.Prompt(fixture.Task.Job, false, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, spec.Facts[0].Description) || strings.Contains(prompt, spec.ExpectedAnswer) != (i == 0) || strings.Contains(prompt, `"f063"`) != (i < 2) {
				t.Fatalf("wrong initial visibility in %s", spec.Name)
			}
			if len(fixture.Task.Job.Decision.View) > board.DefaultContextViewBytes {
				t.Fatal("fixture exceeded production view budget")
			}
			firstPage, err := worker.GraphPage(fixture.Initial, worker.GraphRequest{Op: "read_graph", Section: "facts", Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			firstRaw, _ := json.Marshal(firstPage)
			if strings.Contains(string(firstRaw), `"id":"f063"`) || !strings.Contains(string(firstRaw), `"next_offset":50`) {
				t.Fatal("the second fact must require discovery beyond even the maximum first page")
			}
			// Default section paging must reach B losslessly and at a pinned version.
			offset, pages, found := 0, 0, false
			for {
				page, err := worker.GraphPage(fixture.Initial, worker.GraphRequest{Op: "read_graph", Section: "facts", Offset: offset, Limit: 20, ExpectedVersion: fixture.Task.Job.Decision.StateVersion})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(page)
				var decoded struct {
					Items []board.FactRecord `json:"items"`
					Next  *int               `json:"next_offset"`
				}
				if err = json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				pages++
				for _, fact := range decoded.Items {
					if fact.ID == "f063" {
						found = fact.Description == spec.Facts[len(spec.Facts)-1].Description
					}
				}
				if decoded.Next == nil {
					break
				}
				if *decoded.Next <= offset || pages > 10 {
					t.Fatal("non-terminating graph paging")
				}
				offset = *decoded.Next
			}
			if !found || pages != 4 {
				t.Fatalf("B was not discovered on the fourth default page: found=%v pages=%d", found, pages)
			}
		})
	}
}

func TestP3AcceptanceStartsWithEveryObservation(t *testing.T) {
	for _, spec := range p3AcceptanceCases() {
		t.Run(spec.Name, func(t *testing.T) {
			fixture := newP3EvaluationFixture(t, spec, context.Background())
			prompt, err := worker.Prompt(fixture.Task.Job, false, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prompt, spec.Name) || strings.Contains(prompt, "WantComplete") || strings.Contains(prompt, "RequiredIDs") {
				t.Fatal("evaluation oracle leaked into the model input")
			}
			for _, fact := range spec.Facts {
				if !strings.Contains(prompt, fact.Description) {
					t.Fatalf("initial input omitted fact %s", fact.ID)
				}
				for _, evidence := range fact.Evidence {
					raw, _ := json.Marshal(evidence.Excerpt)
					if !strings.Contains(prompt, string(raw)) {
						t.Fatalf("initial input omitted evidence for %s", fact.ID)
					}
				}
			}
		})
	}
}
