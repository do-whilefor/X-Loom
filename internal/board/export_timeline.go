package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ExportTimeline exports the current round's domain history. Legacy YAML
// exports remain available to old callers; the HTTP timeline uses FGS events.
// Current records without events are included explicitly as legacy snapshots,
// rather than inventing historical changes that were never recorded.
func (t *Tx) ExportTimeline(project string) (string, error) {
	state, err := t.State(project)
	if err != nil {
		return "", err
	}
	events := []StateEvent{}
	var after int64
	for {
		page, err := t.StateEvents(project, after)
		if err != nil {
			return "", err
		}
		events = append(events, page...)
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].Revision
	}
	seen := map[string]bool{}
	factEvents := map[string]bool{}
	for _, event := range events {
		seen[event.Op+":"+event.ID] = true
		if event.Op == "fact" {
			factEvents[event.ID] = true
		}
		if event.Op == "step_completed" || event.Op == "reopen" {
			var result struct {
				Fact   Fact   `json:"fact"`
				Intent Intent `json:"intent"`
			}
			if err := json.Unmarshal(timelineEventResult(event), &result); err != nil {
				return "", err
			}
			if result.Fact.ID != "" {
				seen["fact:"+result.Fact.ID] = true
			}
		}
	}
	type entry struct {
		at   string
		text string
	}
	entries := []entry{}
	inputs := map[string]string{}
	for _, fact := range state.Graph.Facts {
		inputs[fact.ID] = fact.Description
	}
	entries = append(entries, entry{state.Graph.Project.CreatedAt, fmt.Sprintf("[%s] PROJECT CREATED %s (generation %d)\n  origin: %s\n  goal: %s", displayTime(state.Graph.Project.CreatedAt), project, state.Graph.Project.Generation, inputs["origin"], inputs["goal"])})
	for _, hint := range state.Graph.Hints {
		if !seen["hint:"+hint.ID] {
			entries = append(entries, entry{hint.CreatedAt, fmt.Sprintf("[%s] HINT by %s\n  %s", displayTime(hint.CreatedAt), hint.Creator, hint.Content)})
		}
	}
	for _, intent := range state.Graph.Intents {
		if !seen["step:"+intent.ID] && !seen["reopen:"+intent.ID] && !seen["complete:"+intent.ID] {
			entries = append(entries, entry{intent.CreatedAt, fmt.Sprintf("[%s] INTENT DECLARED %s by %s\n  from: %s\n  %s", displayTime(intent.CreatedAt), intent.ID, intent.Creator, strings.Join(intent.From, ", "), intent.Description)})
		}
		if intent.ConcludedAt != nil && intent.To != nil && !seen["step_completed:"+intent.ID] && !seen["complete:"+intent.ID] {
			entries = append(entries, entry{*intent.ConcludedAt, fmt.Sprintf("[%s] INTENT CONCLUDED %s\n  produced: %s", displayTime(*intent.ConcludedAt), intent.ID, *intent.To)})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at < entries[j].at })
	var out strings.Builder
	for _, event := range entries {
		out.WriteString(event.text + "\n\n")
	}
	// FGS revision order is authoritative even if the system clock moved back.
	out.WriteString("FGS CHANGES (revision order)\n\n")
	for _, event := range events {
		result := timelineEventResult(event)
		if event.Op == "step_completed" {
			var completed struct {
				Fact   Fact   `json:"fact"`
				Intent Intent `json:"intent"`
			}
			if err := json.Unmarshal(result, &completed); err != nil {
				return "", err
			}
			// Modern final Fact creation has its own event. A Step completion
			// links to that observation instead of exporting it a second time.
			if factEvents[completed.Fact.ID] {
				result, _ = json.Marshal(struct {
					FactID string `json:"fact_id"`
					Intent Intent `json:"intent"`
				}{completed.Fact.ID, completed.Intent})
			}
		}
		var body bytes.Buffer
		if err := json.Indent(&body, result, "  ", "  "); err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "[%s] %s %s (revision %d, run %s)\n  %s\n\n", displayTime(event.CreatedAt), strings.ToUpper(event.Op), event.ID, event.Revision, event.RunID, body.String())
	}
	fmt.Fprintf(&out, "CURRENT FGS generation=%d revision=%d status=%s\n", state.Graph.Project.Generation, state.Revision, state.Graph.Project.Status)
	writeRecord := func(kind, id, status string, record any) error {
		fmt.Fprintf(&out, "  %s %s status=%s\n", strings.ToUpper(kind), id, status)
		if seen[kind+":"+id] || id == "origin" || id == "goal" {
			return nil
		}
		raw, err := json.MarshalIndent(record, "    ", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "    snapshot (no creation event recorded): %s\n", raw)
		return nil
	}
	for _, fact := range state.FactRecords {
		if err := writeRecord("fact", fact.ID, fact.Status, fact); err != nil {
			return "", err
		}
	}
	for _, goal := range state.Goals {
		if err := writeRecord("goal", goal.ID, fmt.Sprintf("%s support_valid=%t", goal.Status, goal.SupportValid), goal); err != nil {
			return "", err
		}
	}
	for _, step := range state.Steps {
		if err := writeRecord("step", step.ID, step.Status, step); err != nil {
			return "", err
		}
	}
	for _, finding := range state.Findings {
		if err := writeRecord("finding", finding.ID, fmt.Sprintf("%s support_valid=%t", finding.Status, finding.SupportValid), finding); err != nil {
			return "", err
		}
	}
	for _, relation := range state.FactRelations {
		fmt.Fprintf(&out, "  FACT_RELATION %s %s -> %s reason=%s\n", relation.Kind, relation.Source, relation.Target, relation.Reason)
	}
	return out.String(), nil
}

func timelineEventResult(event StateEvent) json.RawMessage {
	for _, raw := range []json.RawMessage{event.Result, event.Payload} {
		if len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return raw
		}
	}
	return json.RawMessage("null")
}
