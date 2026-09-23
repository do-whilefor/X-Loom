package board

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
)

// DecisionContext is bound to one immutable State. A successful decision may
// acknowledge only this generation and revision, never a later live snapshot.
type DecisionContext struct {
	Version       int             `json:"version"`
	StateVersion  string          `json:"state_version"`
	View          json.RawMessage `json:"view"`
	Mode          string          `json:"mode"`
	Fallback      string          `json:"fallback,omitempty"`
	FromRevision  int64           `json:"from_revision"`
	ToRevision    int64           `json:"to_revision"`
	Generation    int64           `json:"generation"`
	BaselineBytes int             `json:"baseline_bytes"`
}

type decisionChanges map[string][]string

type decisionView struct {
	Version          int              `json:"version"`
	Revision         int64            `json:"revision"`
	DecisionRevision int64            `json:"decision_revision"`
	Generation       int64            `json:"generation"`
	FromRevision     int64            `json:"from_revision"`
	Project          Project          `json:"project"`
	UserInputs       []Fact           `json:"user_inputs"`
	Hints            []Hint           `json:"hints"`
	Goals            []Goal           `json:"goals"`
	Steps            []Step           `json:"steps"`
	Facts            []FactRecord     `json:"fact_records"`
	Findings         []Finding        `json:"findings"`
	Relations        []FactRelation   `json:"fact_relations"`
	Changed          decisionChanges  `json:"changed"`
	Removed          decisionChanges  `json:"removed"`
	Omitted          map[string]int   `json:"omitted"`
	ReadMore         string           `json:"read_more"`
	Overview         *contextOverview `json:"overview"`
}

// BuildDecisionContext narrows a Decide input around changes and their support.
// Events validate the revision interval and locate transient changes; snapshot
// comparison also catches legacy writes that do not emit graph events. Missing
// history and an oversized dependency closure use the existing bounded view.
func BuildDecisionContext(current State, previous *State, events []StateEvent, maxBytes int) (*DecisionContext, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultContextViewBytes
	}
	baseline, err := ContextView(current, "", maxBytes)
	if err != nil {
		return nil, err
	}
	result := &DecisionContext{Version: 1, StateVersion: DecisionStateVersion(current), View: baseline, Mode: "full", ToRevision: current.Revision, Generation: current.Graph.Project.Generation, BaselineBytes: len(baseline)}
	var changed, removed decisionChanges
	fallback := func(reason string) (*DecisionContext, error) {
		view, err := decisionFallbackView(current, baseline, changed, removed, maxBytes)
		if err != nil {
			return nil, err
		}
		result.View = view
		result.Fallback = reason
		return result, nil
	}
	if previous == nil {
		return fallback("no_baseline")
	}
	result.FromRevision = previous.Revision
	if previous.Graph.Project.ID != current.Graph.Project.ID {
		return fallback("project_changed")
	}
	if previous.Graph.Project.Generation != current.Graph.Project.Generation {
		return fallback("generation_changed")
	}
	if previous.Revision > current.Revision || previous.Revision < 0 {
		return fallback("invalid_cursor")
	}
	span := current.Revision - previous.Revision
	if span > 1000 && int64(len(events)) < span {
		return fallback("event_limit")
	}
	if int64(len(events)) != span {
		return fallback("event_gap")
	}
	ordered := append([]StateEvent{}, events...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Revision < ordered[j].Revision })
	for i, event := range ordered {
		if event.Revision != previous.Revision+int64(i)+1 {
			return fallback("event_gap")
		}
	}

	current = contextState(current)
	before := contextState(*previous)
	changed, removed = decisionChanges{}, decisionChanges{}
	facts, oldFacts := decisionIndex(current.FactRecords, func(v FactRecord) string { return v.ID }), decisionIndex(before.FactRecords, func(v FactRecord) string { return v.ID })
	goals := decisionIndex(current.Goals, func(v Goal) string { return v.ID })
	steps := decisionIndex(current.Steps, func(v Step) string { return v.ID })
	findings := decisionIndex(current.Findings, func(v Finding) string { return v.ID })
	relations := decisionIndex(current.FactRelations, decisionRelationKey)
	decisionDiff("fact_records", facts, oldFacts, changed, removed)
	decisionDiff("goals", goals, decisionIndex(before.Goals, func(v Goal) string { return v.ID }), changed, removed)
	decisionDiff("steps", steps, decisionIndex(before.Steps, func(v Step) string { return v.ID }), changed, removed)
	decisionDiff("findings", findings, decisionIndex(before.Findings, func(v Finding) string { return v.ID }), changed, removed)
	decisionDiff("fact_relations", relations, decisionIndex(before.FactRelations, decisionRelationKey), changed, removed)
	decisionDiff("hints", decisionIndex(current.Graph.Hints, func(v Hint) string { return v.ID }), decisionIndex(before.Graph.Hints, func(v Hint) string { return v.ID }), changed, removed)
	decisionDiff("facts", decisionIndex(current.Graph.Facts, func(v Fact) string { return v.ID }), decisionIndex(before.Graph.Facts, func(v Fact) string { return v.ID }), changed, removed)
	project, oldProject := current.Graph.Project, before.Graph.Project
	project.Reason, oldProject.Reason = nil, nil // Lease/heartbeat activity is not evidence.
	if !reflect.DeepEqual(project, oldProject) {
		changed["project"] = []string{project.ID}
	}

	return buildDecisionChanges(current, baseline, result, changed, removed, ordered, before.FactRelations, maxBytes)
}

// The view is built from the current FGS. Previous relations are used only by
// the legacy snapshot-comparison entry point for removed edges.
func buildDecisionChanges(current State, baseline json.RawMessage, result *DecisionContext, changed, removed decisionChanges, ordered []StateEvent, previousRelations []FactRelation, maxBytes int) (*DecisionContext, error) {
	current = contextState(current)
	project := current.Graph.Project
	project.Reason = nil
	facts := decisionIndex(current.FactRecords, func(v FactRecord) string { return v.ID })
	goals := decisionIndex(current.Goals, func(v Goal) string { return v.ID })
	steps := decisionIndex(current.Steps, func(v Step) string { return v.ID })
	findings := decisionIndex(current.Findings, func(v Finding) string { return v.ID })
	relations := decisionIndex(current.FactRelations, decisionRelationKey)
	fallback := func(reason string) (*DecisionContext, error) {
		view, err := decisionFallbackView(current, baseline, changed, removed, maxBytes)
		if err != nil {
			return nil, err
		}
		result.View, result.Fallback = view, reason
		return result, nil
	}
	selectedFacts, selectedGoals, selectedSteps, selectedFindings := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	seed := func(changes decisionChanges) {
		for _, id := range append(append([]string{}, changes["facts"]...), changes["fact_records"]...) {
			selectedFacts[id] = true
		}
		for _, id := range changes["goals"] {
			selectedGoals[id] = true
		}
		for _, id := range changes["steps"] {
			selectedSteps[id] = true
		}
		for _, id := range changes["findings"] {
			selectedFindings[id] = true
		}
	}
	seed(changed)
	seed(removed)
	for _, key := range changed["fact_relations"] {
		relation := relations[key]
		selectedFacts[relation.Source], selectedFacts[relation.Target] = true, true
	}
	oldRelations := decisionIndex(previousRelations, decisionRelationKey)
	for _, key := range removed["fact_relations"] {
		relation := oldRelations[key]
		selectedFacts[relation.Source], selectedFacts[relation.Target] = true, true
	}
	for _, event := range ordered {
		if event.ID != "" {
			changed["event_nodes"] = append(changed["event_nodes"], event.ID)
		}
		if _, ok := facts[event.ID]; ok {
			selectedFacts[event.ID] = true
		}
		if _, ok := goals[event.ID]; ok {
			selectedGoals[event.ID] = true
		}
		if _, ok := steps[event.ID]; ok {
			selectedSteps[event.ID] = true
		}
		if _, ok := findings[event.ID]; ok {
			selectedFindings[event.ID] = true
		}
	}
	if ids := changed["event_nodes"]; len(ids) > 0 {
		sort.Strings(ids)
		unique := ids[:0]
		for _, id := range ids {
			if len(unique) == 0 || unique[len(unique)-1] != id {
				unique = append(unique, id)
			}
		}
		changed["event_nodes"] = unique
	}
	// First find consumers affected by changed evidence. Do this before adding
	// support sources: a shared origin must not pull in every historical plan.
	decisionRelationClosure(current.FactRelations, selectedFacts)
	references := func(ids []string) bool {
		for _, id := range ids {
			if selectedFacts[id] && id != "origin" && id != "goal" {
				return true
			}
		}
		return false
	}
	for _, goal := range current.Goals {
		if references(goal.Sources) {
			selectedGoals[goal.ID] = true
		}
	}
	for _, step := range current.Steps {
		if contextActiveStep(step.Status) || selectedGoals[step.GoalID] || references(step.From) {
			selectedSteps[step.ID] = true
		}
	}
	// Directly selected or affected Steps retain their results. A Step reached
	// only as a producer supplies ancestry, not its unrelated sibling outputs.
	resultSteps := make(map[string]bool, len(selectedSteps))
	for id := range selectedSteps {
		resultSteps[id] = true
	}
	producers := decisionFactProducers(facts, steps)
	for _, goal := range current.Goals {
		if goal.Status == "open" {
			selectedGoals[goal.ID] = true
		}
	}
	selectedGoals["goal"] = true
	// Include each selected node's causal support, including producers of
	// process Facts and both ends of refutes/supersedes/narrows chains.
	for {
		count := len(selectedFacts) + len(selectedGoals) + len(selectedSteps) + len(selectedFindings)
		for _, finding := range current.Findings {
			if references(finding.Sources) {
				selectedFindings[finding.ID] = true
			}
		}
		for id := range selectedFindings {
			for _, source := range findings[id].Sources {
				selectedFacts[source] = true
			}
		}
		for id := range selectedFacts {
			if producer := producers[id]; producer != "" {
				selectedSteps[producer] = true
			}
		}
		for _, step := range current.Steps {
			if selectedSteps[step.ID] {
				if step.GoalID != "" {
					selectedGoals[step.GoalID] = true
				}
				for _, source := range step.From {
					selectedFacts[source] = true
				}
				if resultSteps[step.ID] && step.Result != nil {
					selectedFacts[*step.Result] = true
				}
			}
		}
		for id := range selectedGoals {
			goal := goals[id]
			if goal.ParentID != "" {
				selectedGoals[goal.ParentID] = true
			}
			for _, source := range goal.Sources {
				selectedFacts[source] = true
			}
		}
		decisionRelationClosure(current.FactRelations, selectedFacts)
		if count == len(selectedFacts)+len(selectedGoals)+len(selectedSteps)+len(selectedFindings) {
			break
		}
	}

	view := decisionView{Version: 1, Revision: current.Revision, DecisionRevision: current.DecisionRevision, Generation: project.Generation, FromRevision: result.FromRevision, Project: project, UserInputs: []Fact{}, Hints: append([]Hint{}, current.Graph.Hints...), Goals: []Goal{}, Steps: []Step{}, Facts: []FactRecord{}, Findings: []Finding{}, Relations: []FactRelation{}, Changed: changed, Removed: removed, Omitted: map[string]int{}, ReadMore: "Read omitted nodes and evidence using read_graph with ids. Omission is not absence; reads are pinned to state_version. Missing or ambiguous producers are not inferred."}
	// The same bounded discovery index accompanies full and incremental bodies.
	// Keeping the latter narrow must not erase unrelated historical knowledge.
	var overview struct {
		Overview *contextOverview `json:"overview"`
	}
	if err := json.Unmarshal(baseline, &overview); err != nil {
		return nil, err
	}
	view.Overview = overview.Overview
	for _, fact := range current.Graph.Facts {
		if fact.ID == "origin" || fact.ID == "goal" {
			view.UserInputs = append(view.UserInputs, fact)
		}
	}
	for _, goal := range current.Goals {
		if selectedGoals[goal.ID] {
			view.Goals = append(view.Goals, goal)
		}
	}
	for _, step := range current.Steps {
		if selectedSteps[step.ID] {
			view.Steps = append(view.Steps, step)
		}
	}
	inputRecords := 0
	for _, fact := range current.FactRecords {
		if fact.ID == "origin" || fact.ID == "goal" {
			inputRecords++ // These immutable records are already in user_inputs.
			continue
		}
		if selectedFacts[fact.ID] {
			view.Facts = append(view.Facts, fact)
		}
	}
	for _, finding := range current.Findings {
		if selectedFindings[finding.ID] {
			view.Findings = append(view.Findings, finding)
		}
	}
	for _, relation := range current.FactRelations {
		if selectedFacts[relation.Source] || selectedFacts[relation.Target] {
			view.Relations = append(view.Relations, relation)
		}
	}
	view.Omitted["goals"] = len(current.Goals) - len(view.Goals)
	view.Omitted["steps"] = len(current.Steps) - len(view.Steps)
	view.Omitted["fact_records"] = len(current.FactRecords) - len(view.Facts) - inputRecords
	view.Omitted["findings"] = len(current.Findings) - len(view.Findings)
	view.Omitted["fact_relations"] = len(current.FactRelations) - len(view.Relations)
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return fallback("related_context_over_budget")
	}
	result.View, result.Mode = raw, "changes"
	return result, nil
}

// Derive provenance from this version of the graph without rewriting records.
// Explicit sources take precedence even when they cannot be resolved. Only a
// unique legacy Result may fill an absent source; inputs are never outputs.
func decisionFactProducers(facts map[string]FactRecord, steps map[string]Step) map[string]string {
	producers := make(map[string]string)
	for _, step := range steps {
		id := Value(step.Result)
		fact, exists := facts[id]
		if !exists || id == "origin" || id == "goal" || fact.SourceStepID != "" {
			continue
		}
		if _, seen := producers[id]; seen {
			producers[id] = "" // Multiple legacy candidates are not provenance.
		} else {
			producers[id] = step.ID
		}
	}
	for id, fact := range facts {
		if id == "origin" || id == "goal" || fact.SourceStepID == "" {
			continue
		}
		if _, exists := steps[fact.SourceStepID]; exists {
			producers[id] = fact.SourceStepID
		}
	}
	return producers
}

func decisionIndex[T any](items []T, key func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, item := range items {
		out[key(item)] = item
	}
	return out
}

func decisionDiff[T any](kind string, current, previous map[string]T, changed, removed decisionChanges) {
	for id, item := range current {
		old, ok := previous[id]
		if !ok || !reflect.DeepEqual(item, old) {
			changed[kind] = append(changed[kind], id)
		}
	}
	for id := range previous {
		if _, ok := current[id]; !ok {
			removed[kind] = append(removed[kind], id)
		}
	}
	sort.Strings(changed[kind])
	sort.Strings(removed[kind])
}

func decisionRelationKey(relation FactRelation) string {
	key, _ := json.Marshal([]string{relation.Kind, relation.Source, relation.Target})
	return string(key)
}

func decisionRelationClosure(relations []FactRelation, selected map[string]bool) {
	for {
		count := len(selected)
		for _, relation := range relations {
			if selected[relation.Source] || selected[relation.Target] {
				selected[relation.Source], selected[relation.Target] = true, true
			}
		}
		if count == len(selected) {
			return
		}
	}
}

// Changes are also an index, so large revisions must not force an unbounded
// payload. Current nodes remain discoverable through the overview pages;
// removals have explicit counts because live pages cannot restore their bodies.
func decisionFallbackView(state State, baseline json.RawMessage, changed, removed decisionChanges, maxBytes int) (json.RawMessage, error) {
	metadata := struct {
		Changed        decisionChanges `json:"changed"`
		Removed        decisionChanges `json:"removed"`
		ChangedOmitted map[string]int  `json:"changed_omitted"`
		RemovedOmitted map[string]int  `json:"removed_omitted"`
		ReadMore       string          `json:"read_more"`
	}{decisionChanges{}, decisionChanges{}, map[string]int{}, map[string]int{}, "Read changed nodes and support using read_graph with ids. Scan overview sections to discover omitted history and changes; missing nodes are not evidence of absence. Reads are pinned to state_version."}
	for kind, ids := range changed {
		metadata.Changed[kind] = []string{}
		metadata.ChangedOmitted[kind] = len(ids)
	}
	for kind, ids := range removed {
		metadata.Removed[kind] = []string{}
		metadata.RemovedOmitted[kind] = len(ids)
	}
	index, _ := json.Marshal(metadata)
	used := len(index)
	if used > maxBytes/4 {
		return nil, errors.New("decision context: change index exceeds the budget")
	}
	appendIDs := func(source, dest decisionChanges, omitted map[string]int) {
		keys := make([]string, 0, len(source))
		for kind := range source {
			keys = append(keys, kind)
		}
		sort.Strings(keys)
		for _, kind := range keys {
			for _, id := range source[kind] {
				raw, _ := json.Marshal(id)
				extra := len(raw)
				if len(dest[kind]) > 0 {
					extra++
				}
				if used+extra > maxBytes/4 {
					continue
				}
				dest[kind] = append(dest[kind], id)
				omitted[kind]--
				used += extra
			}
		}
	}
	appendIDs(removed, metadata.Removed, metadata.RemovedOmitted)
	appendIDs(changed, metadata.Changed, metadata.ChangedOmitted)
	index, _ = json.Marshal(metadata)
	// Merge two nonempty JSON objects by replacing their adjacent braces with
	// a comma. Reserve the bounded index before choosing history bodies.
	extra := len(index) - 1
	if len(baseline)+extra > maxBytes {
		var err error
		baseline, err = ContextView(state, "", maxBytes-extra)
		if err != nil {
			return nil, err
		}
	}
	view := make([]byte, 0, len(baseline)+extra)
	view = append(view, baseline[:len(baseline)-1]...)
	view = append(view, ',')
	view = append(view, index[1:]...)
	return view, nil
}
