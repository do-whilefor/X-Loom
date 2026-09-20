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
	RequestID string            `json:"request_id"`
	Op        string            `json:"op"`
	Section   string            `json:"section,omitempty"`
	Offset    int               `json:"offset,omitempty"`
	Limit     int               `json:"limit,omitempty"`
	Action    board.StateAction `json:"action,omitempty"`
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
	if r.Op == "read_graph" {
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
			Counts           map[string]int `json:"counts"`
		}{s.Graph.Project, inputs, s.Revision, s.DecisionRevision, map[string]int{"facts": len(s.FactRecords), "goals": len(s.Goals), "steps": len(s.Steps), "findings": len(s.Findings), "relations": len(s.FactRelations), "hints": len(s.Graph.Hints)}}
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
		total := len(items)
		start := min(r.Offset, total)
		end := min(start+r.Limit, total)
		page := items[start:end]
		if page == nil {
			page = []json.RawMessage{}
		}
		value := graphPage{Section: r.Section, Offset: start, Total: total, Items: page}
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
	Section    string            `json:"section"`
	Offset     int               `json:"offset"`
	Total      int               `json:"total"`
	NextOffset *int              `json:"next_offset,omitempty"`
	Items      []json.RawMessage `json:"items"`
}
