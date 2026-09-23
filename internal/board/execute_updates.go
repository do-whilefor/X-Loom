package board

import (
	"encoding/json"
	"errors"
)

const MaxExecuteUpdateBytes = 16 << 10

// ExecuteUpdateCursor acknowledges only corrections to this run's registered
// Step.From. It is not an acknowledgement that the whole graph was read.
type ExecuteUpdateCursor struct {
	ProjectID  string `json:"project_id"`
	Generation int64  `json:"generation"`
	StepID     string `json:"step_id"`
	RunID      string `json:"run_id"`
	Revision   int64  `json:"revision"`
}

type ExecuteUpdates struct {
	Version int `json:"version"`
	ExecuteUpdateCursor
	FromRevision   int64          `json:"from_revision"`
	ToRevision     int64          `json:"to_revision"`
	StateVersion   string         `json:"state_version"`
	Complete       bool           `json:"complete"`
	PendingReason  string         `json:"pending_reason,omitempty"`
	InvalidSources []string       `json:"invalid_sources"`
	Facts          []FactRecord   `json:"facts"`
	Relations      []FactRelation `json:"relations"`
	Omitted        map[string]int `json:"omitted"`
	ReadMore       string         `json:"read_more,omitempty"`
}

// BuildExecuteUpdates uses one current State and its transaction's event index.
// The caller derives sources and the initial revision from the immutable Job.
// Incomplete responses retain the old cursor, including when they contain a
// useful current review. No evidence or relation reason is silently truncated.
func BuildExecuteUpdates(current State, cursor ExecuteUpdateCursor, sources []string, events []StateChange, baselineKnown bool, maxBytes int) (*ExecuteUpdates, error) {
	if cursor.ProjectID != current.Graph.Project.ID || cursor.Generation != current.Graph.Project.Generation || cursor.StepID == "" || cursor.RunID == "" || cursor.Revision < 0 || cursor.Revision > current.Revision {
		return nil, errors.New("execute updates: invalid cursor identity or revision")
	}
	if maxBytes <= 0 || maxBytes > MaxExecuteUpdateBytes {
		maxBytes = MaxExecuteUpdateBytes
	}
	version := DecisionStateVersion(current)
	current = contextState(current)
	out := &ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: current.Revision,
		StateVersion: version, Complete: true, InvalidSources: []string{}, Facts: []FactRecord{}, Relations: []FactRelation{}, Omitted: map[string]int{}}
	// Even a silent acknowledgement has a bound envelope. Check it before the
	// unrelated-event fast path, which does not enter record-budget assembly.
	envelope, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(envelope) > maxBytes {
		return nil, errors.New("execute updates: identity envelope exceeds budget")
	}
	pending := func(reason string) {
		out.Complete = false
		if out.PendingReason == "" {
			out.PendingReason = reason
		}
	}
	if !baselineKnown {
		pending("legacy_revision_unknown")
	}
	span := current.Revision - cursor.Revision
	if span > 1000 {
		pending("event_limit")
	} else if int64(len(events)) != span {
		pending("event_gap")
	}
	for i, event := range events {
		if event.Revision != cursor.Revision+int64(i)+1 || event.Revision > current.Revision || event.ID == "" {
			pending("event_gap")
		}
		switch event.Op {
		case "fact", "reopen", "finding", "goal", "step", "step_completed", "execution_failed", "complete", "hint", "fact_relation":
		default:
			pending("unknown_event")
		}
	}
	// Follow only explicit corrective sources, never producers, consumers, or
	// ordinary newly published facts. Cap the traversal even for corrupt cycles.
	const maxNodes = 256
	selected := map[string]bool{}
	queue := []string{}
	add := func(id string) {
		if selected[id] {
			return
		}
		if len(queue) >= maxNodes {
			out.Omitted["closure_frontier"]++
			pending("closure_limit")
			return
		}
		selected[id] = true
		queue = append(queue, id)
	}
	for _, id := range sources {
		add(id)
	}
	byTarget := map[string][]int{}
	for i, relation := range current.FactRelations {
		if executeCorrection(relation.Kind) {
			byTarget[relation.Target] = append(byTarget[relation.Target], i)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, index := range byTarget[queue[i]] {
			add(current.FactRelations[index].Source)
		}
	}
	relevant := !out.Complete
	for _, event := range events {
		if selected[event.ID] {
			relevant = true
		}
	}
	if !relevant {
		return out, nil
	}
	out.ReadMore = "read_graph sections facts/relations with the dependency or correction IDs; evidence by one fact ID. Use state_version as expected_version; refresh after state_changed. Omission is not absence."
	// Reserve space for all omission keys and a pending reason before adding
	// records. Never marshal the entire selected evidence set just to trim it.
	fits := func() bool {
		raw, err := json.Marshal(out)
		return err == nil && len(raw) <= maxBytes-192
	}
	seenSources := map[string]bool{}
	for _, id := range sources {
		if !seenSources[id] && current.ValidateFactSources([]string{id}, false) != nil {
			seenSources[id] = true
			out.InvalidSources = append(out.InvalidSources, id)
			if !fits() {
				out.InvalidSources = out.InvalidSources[:len(out.InvalidSources)-1]
				out.Omitted["invalid_sources"]++
				pending("context_budget")
			}
		}
	}
	for _, relation := range current.FactRelations {
		if executeCorrection(relation.Kind) && selected[relation.Target] && selected[relation.Source] {
			out.Relations = append(out.Relations, relation)
			if !fits() {
				out.Relations = out.Relations[:len(out.Relations)-1]
				out.Omitted["relations"]++
				pending("context_budget")
			}
		}
	}
	for _, fact := range current.FactRecords {
		if selected[fact.ID] {
			out.Facts = append(out.Facts, fact)
			delete(selected, fact.ID)
			if !fits() {
				out.Facts = out.Facts[:len(out.Facts)-1]
				out.Omitted["facts"]++
				pending("context_budget")
			}
		}
	}
	if len(selected) != 0 {
		out.Omitted["missing_facts"] = len(selected)
		pending("missing_fact")
	}
	// A caller may select a tiny custom budget. Even the envelope must fit;
	// returned records and their evidence otherwise remain exact.
	for {
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		if len(raw) <= maxBytes {
			return out, nil
		}
		pending("context_budget")
		switch {
		case len(out.Facts) > 0:
			out.Facts = out.Facts[:len(out.Facts)-1]
			out.Omitted["facts"]++
		case len(out.Relations) > 0:
			out.Relations = out.Relations[:len(out.Relations)-1]
			out.Omitted["relations"]++
		case len(out.InvalidSources) > 0:
			out.InvalidSources = out.InvalidSources[:len(out.InvalidSources)-1]
			out.Omitted["invalid_sources"]++
		default:
			return nil, errors.New("execute updates: identity envelope exceeds budget")
		}
	}
}

func executeCorrection(kind string) bool {
	return kind == "refutes" || kind == "supersedes" || kind == "narrows"
}
