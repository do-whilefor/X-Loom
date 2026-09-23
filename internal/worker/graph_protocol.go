package worker

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"xloom/internal/board"
)

const MaxGraphRPCBytes = 128 << 10

type GraphRequest struct {
	RequestID       string               `json:"request_id"`
	Op              string               `json:"op"`
	Section         string               `json:"section,omitempty"`
	Offset          int                  `json:"offset,omitempty"`
	Limit           int                  `json:"limit,omitempty"`
	ExpectedVersion string               `json:"expected_version,omitempty"`
	IDs             []string             `json:"ids,omitempty"`
	Action          board.StateAction    `json:"action,omitempty"`
	Batch           *board.DecisionBatch `json:"batch,omitempty"`
}

type GraphRequestEvent struct {
	Type    string       `json:"type"`
	Request GraphRequest `json:"request"`
}

type GraphResponse struct {
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func ValidGraphRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && id == strings.ToLower(id)
}

func ValidateGraphRequest(j Job, r GraphRequest) error {
	if !ValidGraphRequestID(r.RequestID) {
		return errors.New("invalid graph request_id")
	}
	if r.Op == "decision_preview" || r.Op == "decision_commit" || r.Op == "decision_receipt" {
		if j.Kind != "reason" || j.Decision == nil || j.Decision.Version != 2 || !j.GraphRPC {
			return errors.New("decision batches require a registered version 2 Decide bridge")
		}
		if r.Op != "decision_receipt" && (r.Batch == nil || len(r.Batch.Actions) > 64 || r.Batch.ExpectedVersion == "") {
			return errors.New("decision batch requires a bound version and at most 64 actions")
		}
		return nil
	}
	if r.Op == "read_graph" {
		if err := validateGraphIDs(r); err != nil {
			return err
		}
		if r.Offset < 0 || r.Limit < 0 || r.Limit > 50 {
			return errors.New("graph page requires offset >= 0 and limit <= 50")
		}
		if !slices.Contains([]string{"", "overview", "facts", "goals", "steps", "findings", "relations", "hints"}, r.Section) {
			return errors.New("unknown graph section")
		}
		return nil
	}
	if r.Op != "graph_action" {
		return errors.New("unknown graph operation")
	}
	if j.Kind == "reason" && j.Decision != nil && j.Decision.Version == 2 {
		return errors.New("version 2 Decide writes require a batch")
	}
	allowed := []string{"fact", "finding"}
	if j.Kind == "reason" {
		allowed = []string{"goal", "step", "fact_relation"}
	}
	if !slices.Contains(allowed, r.Action.Op) {
		return errors.New("graph action is not allowed in this task mode")
	}
	if len(r.Action.IdempotencyKey) == 0 || len(r.Action.IdempotencyKey) > 256 || !json.Valid(r.Action.Payload) {
		return errors.New("invalid graph action key or payload")
	}
	return nil
}

// GraphPage retains complete objects and reports the page boundary explicitly.
// The dispatcher obtains State under the current execution fence before calling.
func GraphPage(s board.State, r GraphRequest) (any, error) {
	if err := validateGraphIDs(r); err != nil {
		return nil, err
	}
	version := board.DecisionStateVersion(s)
	if r.ExpectedVersion != "" && r.ExpectedVersion != version {
		return nil, errors.New("state_changed: graph changed since the previous view; read overview and re-read affected evidence before deciding")
	}
	if r.Offset < 0 || r.Limit < 0 || r.Limit > 50 {
		return nil, errors.New("invalid graph page")
	}
	if r.Section == "" {
		r.Section = "overview"
	}
	if r.Limit == 0 {
		r.Limit = 20
	}
	var data any
	if r.Section == "overview" {
		inputs := []board.Fact{}
		for _, fact := range s.Graph.Facts {
			if fact.ID == "origin" || fact.ID == "goal" {
				inputs = append(inputs, fact)
			}
		}
		data = struct {
			Project          board.Project  `json:"project"`
			Inputs           []board.Fact   `json:"user_inputs"`
			Revision         int64          `json:"revision"`
			DecisionRevision int64          `json:"decision_revision"`
			StateVersion     string         `json:"state_version"`
			Counts           map[string]int `json:"counts"`
		}{s.Graph.Project, inputs, s.Revision, s.DecisionRevision, version, map[string]int{"facts": len(s.FactRecords), "goals": len(s.Goals), "steps": len(s.Steps), "findings": len(s.Findings), "relations": len(s.FactRelations), "hints": len(s.Graph.Hints)}}
	} else {
		var items []json.RawMessage
		var raw []byte
		var err error
		switch r.Section {
		case "facts":
			raw, err = json.Marshal(s.FactRecords)
		case "goals":
			raw, err = json.Marshal(s.Goals)
		case "steps":
			raw, err = json.Marshal(s.Steps)
		case "findings":
			raw, err = json.Marshal(s.Findings)
		case "relations":
			raw, err = json.Marshal(s.FactRelations)
		case "hints":
			raw, err = json.Marshal(s.Graph.Hints)
		default:
			return nil, errors.New("unknown graph section")
		}
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		missing := []string{}
		if len(r.IDs) > 0 {
			matched := map[string]bool{}
			filtered := []json.RawMessage{}
			for _, item := range items {
				var identity struct{ ID, Source, Target string }
				if err := json.Unmarshal(item, &identity); err != nil {
					return nil, err
				}
				include := false
				for _, id := range r.IDs {
					if identity.ID == id || (r.Section == "relations" && (identity.Source == id || identity.Target == id)) {
						include, matched[id] = true, true
					}
				}
				if include {
					filtered = append(filtered, item)
				}
			}
			items = filtered
			for _, id := range r.IDs {
				if !matched[id] {
					missing = append(missing, id)
				}
			}
		}
		total := len(items)
		start := min(r.Offset, total)
		end := min(start+r.Limit, total)
		page := items[start:end]
		if page == nil {
			page = []json.RawMessage{}
		}
		value := graphPage{Section: r.Section, Offset: start, Total: total, Items: page, Revision: s.Revision, Generation: s.Graph.Project.Generation, StateVersion: version, RequestedIDs: r.IDs, MissingIDs: missing}
		if end < total {
			value.NextOffset = &end
		}
		data = value
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxGraphRPCBytes-1024 {
		return nil, fmt.Errorf("graph page exceeds %d bytes; request a smaller limit or narrower section", MaxGraphRPCBytes-1024)
	}
	return data, nil
}

type graphPage struct {
	RequestedIDs []string          `json:"requested_ids,omitempty"`
	MissingIDs   []string          `json:"missing_ids,omitempty"`
	Revision     int64             `json:"revision"`
	Generation   int64             `json:"generation"`
	StateVersion string            `json:"state_version"`
	Section      string            `json:"section"`
	Offset       int               `json:"offset"`
	Total        int               `json:"total"`
	NextOffset   *int              `json:"next_offset,omitempty"`
	Items        []json.RawMessage `json:"items"`
}

func validateGraphIDs(r GraphRequest) error {
	if len(r.IDs) > 50 {
		return errors.New("at most 50 graph IDs are allowed")
	}
	if len(r.IDs) > 0 && (r.Section == "" || r.Section == "overview") {
		return errors.New("graph IDs require a specific section")
	}
	seen := map[string]bool{}
	for _, id := range r.IDs {
		if strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			return errors.New("graph IDs must be unique, nonempty and at most 256 bytes")
		}
		seen[id] = true
	}
	return nil
}
