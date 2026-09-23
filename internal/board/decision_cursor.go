package board

import (
	"sort"
)

// DecisionCursor is scheduling metadata, not a previous agent's memory. It
// acknowledges the immutable INPUT of a successful decision, never the live
// graph at completion. All task knowledge comes from current FGS records.
type DecisionCursor struct {
	ProjectID  string
	Generation int64
	Revision   int64
}

func BuildDecisionContextFromCursor(current State, cursor *DecisionCursor, events []StateEvent, maxBytes int) (*DecisionContext, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultContextViewBytes
	}
	baseline, err := ContextView(current, "", maxBytes)
	if err != nil {
		return nil, err
	}
	result := &DecisionContext{Version: 1, StateVersion: DecisionStateVersion(current), View: baseline, Mode: "full", ToRevision: current.Revision, Generation: current.Graph.Project.Generation, BaselineBytes: len(baseline)}
	changed := decisionChanges{}
	fallback := func(reason string) (*DecisionContext, error) {
		view, err := decisionFallbackView(current, baseline, changed, maxBytes)
		if err != nil {
			return nil, err
		}
		result.View, result.Fallback = view, reason
		return result, nil
	}
	if cursor == nil {
		return fallback("no_baseline")
	}
	result.FromRevision = cursor.Revision
	if cursor.ProjectID != current.Graph.Project.ID {
		return fallback("project_changed")
	}
	if cursor.Generation != current.Graph.Project.Generation {
		return fallback("generation_changed")
	}
	if cursor.Revision < 0 || cursor.Revision > current.Revision {
		return fallback("invalid_cursor")
	}
	span := current.Revision - cursor.Revision
	if span > 1000 && int64(len(events)) < span {
		return fallback("event_limit")
	}
	if int64(len(events)) != span {
		return fallback("event_gap")
	}
	ordered := append([]StateEvent{}, events...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Revision < ordered[j].Revision })
	for i, event := range ordered {
		if event.Revision != cursor.Revision+int64(i)+1 {
			return fallback("event_gap")
		}
		section := ""
		switch event.Op {
		case "fact", "reopen":
			section = "fact_records"
		case "finding":
			section = "findings"
		case "goal":
			section = "goals"
		case "step", "step_completed", "execution_failed", "complete":
			section = "steps"
		case "hint":
			section = "hints"
		case "fact_relation":
			// Its ID is the corrected target. The current relation closure
			// brings in both the corrective source and affected consumers.
			section = "fact_records"
		default:
			return fallback("unknown_event")
		}
		if event.ID == "" {
			return fallback("event_without_identity")
		}
		changed[section] = append(changed[section], event.ID)
	}
	for key, ids := range changed {
		sort.Strings(ids)
		unique := ids[:0]
		for _, id := range ids {
			if len(unique) == 0 || unique[len(unique)-1] != id {
				unique = append(unique, id)
			}
		}
		changed[key] = unique
	}
	return buildDecisionChanges(current, baseline, result, changed, ordered, maxBytes)
}
