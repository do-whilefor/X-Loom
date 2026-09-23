package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"xloom/internal/board"
)

const MaxGraphRPCBytes = 128 << 10

// Leave room for the request ID and the GraphResponse envelope.
const maxGraphPageBytes = MaxGraphRPCBytes - 1024

type GraphRequest struct {
	RequestID       string                     `json:"request_id"`
	Op              string                     `json:"op"`
	Section         string                     `json:"section,omitempty"`
	Offset          int                        `json:"offset,omitempty"`
	ByteOffset      *int                       `json:"byte_offset,omitempty"`
	RecordVersion   string                     `json:"record_version,omitempty"`
	Limit           int                        `json:"limit,omitempty"`
	ExpectedVersion string                     `json:"expected_version,omitempty"`
	IDs             []string                   `json:"ids,omitempty"`
	Action          board.StateAction          `json:"action,omitempty"`
	Batch           *board.DecisionBatch       `json:"batch,omitempty"`
	Updates         *board.ExecuteUpdateCursor `json:"updates,omitempty"`
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
	if r.Op == "read_updates" {
		if !j.GraphRPC || j.Kind == "reason" || j.Intent == nil || j.RunID == "" || j.Graph.Project.ID == "" {
			return errors.New("execution updates require a registered Execute bridge")
		}
		if r.Section != "" || r.Offset != 0 || r.ByteOffset != nil || r.Limit != 0 || r.ExpectedVersion != "" || r.RecordVersion != "" || len(r.IDs) != 0 || r.Batch != nil || r.Action.Op != "" {
			return errors.New("execution updates use the registered dependencies only")
		}
		if c := r.Updates; c != nil && (c.ProjectID != j.Graph.Project.ID || c.Generation != j.Graph.Project.Generation || c.StepID != j.Intent.ID || c.RunID != j.RunID || c.Revision < 0) {
			return errors.New("execution update cursor identity mismatch")
		}
		return nil
	}
	if r.Updates != nil {
		return errors.New("update cursor requires read_updates")
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
	if r.Op == "read_graph" || r.Op == "read_snapshot" {
		if err := validateGraphIDs(r); err != nil {
			return err
		}
		if err := validateGraphOffsets(r); err != nil {
			return err
		}
		if !slices.Contains([]string{"", "overview", "facts", "goals", "steps", "findings", "relations", "hints", "evidence", "sources"}, r.Section) {
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

// GraphPage retains complete objects when they fit. Oversized evidence/support
// arrays have explicit omission markers and their own lossless detail pages.
// The server obtains State under the current execution fence before calling.
func GraphPage(s board.State, r GraphRequest) (any, error) {
	if err := validateGraphIDs(r); err != nil {
		return nil, err
	}
	version := board.DecisionStateVersion(s)
	if r.ExpectedVersion != "" && r.ExpectedVersion != version {
		return nil, errors.New("state_changed: graph changed since the previous view; read overview and re-read affected evidence before deciding")
	}
	if err := validateGraphOffsets(r); err != nil {
		return nil, err
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
		if r.Section == "evidence" || r.Section == "sources" {
			items, found, err := graphDetails(s, r)
			if err != nil {
				return nil, err
			}
			missing := []string{}
			if !found {
				missing = append(missing, r.IDs[0])
			}
			return graphItemsPage(s, r, version, items, missing)
		}
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
		return graphItemsPage(s, r, version, items, missing)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if r.ByteOffset != nil {
		if r.Offset != 0 {
			return nil, errors.New("overview record offset must be zero")
		}
		return graphRecordContent(r, version, raw)
	}
	if len(raw) > maxGraphPageBytes {
		return graphRecordReference{true, 0, len(raw), version, graphRecordVersion(raw), graphRecordReadMore}, nil
	}
	return data, nil
}

// byte_offset addresses UTF-8 bytes in the JSON encoding of one complete record,
// before support compaction. The state version binds every fragment to that input.
func validateGraphOffsets(r GraphRequest) error {
	if r.Offset < 0 || r.Limit < 0 || r.Limit > 50 {
		return errors.New("graph page requires offset >= 0 and limit <= 50")
	}
	if r.ByteOffset != nil && (*r.ByteOffset < 0 || r.ExpectedVersion == "") {
		return errors.New("byte pages require byte_offset >= 0 and expected_version from the original page")
	}
	if r.ByteOffset != nil && *r.ByteOffset > 0 && r.RecordVersion == "" {
		return errors.New("byte continuation requires record_version from the original page")
	}
	return nil
}

func graphItemsPage(s board.State, r GraphRequest, version string, items []json.RawMessage, missing []string) (any, error) {
	if r.ByteOffset != nil {
		if r.Offset >= len(items) {
			return nil, errors.New("graph record offset is outside the selected records")
		}
		return graphRecordContent(r, version, items[r.Offset])
	}
	return boundedGraphPage(s, r, version, items, missing)
}

const graphRecordReadMore = "Read the same section and ids with offset:record_offset, byte_offset:0, expected_version:state_version and record_version. Concatenate content fragments, following next_byte_offset until absent, then parse the complete JSON record."

type graphRecordReference struct {
	RecordOmitted bool   `json:"record_omitted"`
	RecordOffset  int    `json:"record_offset"`
	RecordBytes   int    `json:"record_bytes"`
	StateVersion  string `json:"state_version"`
	RecordVersion string `json:"record_version"`
	ReadMore      string `json:"read_more"`
}

type graphContentPage struct {
	StateVersion   string `json:"state_version"`
	RecordVersion  string `json:"record_version"`
	Section        string `json:"section"`
	Offset         int    `json:"offset"`
	ByteOffset     int    `json:"byte_offset"`
	TotalBytes     int    `json:"total_bytes"`
	NextByteOffset *int   `json:"next_byte_offset,omitempty"`
	Content        string `json:"content"`
}

func graphRecordContent(r GraphRequest, version string, raw []byte) (graphContentPage, error) {
	recordVersion := graphRecordVersion(raw)
	if r.RecordVersion != "" && r.RecordVersion != recordVersion {
		return graphContentPage{}, errors.New("state_changed: record changed since the previous fragment; re-read this record from byte_offset 0")
	}
	start := *r.ByteOffset
	if start > len(raw) || (start < len(raw) && !utf8.RuneStart(raw[start])) {
		return graphContentPage{}, errors.New("byte_offset must be within the record at a UTF-8 boundary")
	}
	// JSON-encoded JSON text can expand through quoting and escaping. Keep each
	// fragment small enough even when every byte needs escaping in the envelope.
	end := start + min(len(raw)-start, (maxGraphPageBytes-1024)/6)
	for end < len(raw) && !utf8.RuneStart(raw[end]) {
		end--
	}
	page := graphContentPage{StateVersion: version, RecordVersion: recordVersion, Section: r.Section, Offset: r.Offset, ByteOffset: start, TotalBytes: len(raw), Content: string(raw[start:end])}
	if end < len(raw) {
		page.NextByteOffset = &end
	}
	return page, nil
}

func graphRecordVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func graphDetails(s board.State, r GraphRequest) ([]json.RawMessage, bool, error) {
	var value any
	found := false
	if r.Section == "evidence" {
		for _, fact := range s.FactRecords {
			if fact.ID == r.IDs[0] {
				value, found = fact.Evidence, true
				break
			}
		}
	}
	if !found {
		for _, finding := range s.Findings {
			if finding.ID == r.IDs[0] {
				value, found = finding.Evidence, true
				if r.Section == "sources" {
					value = finding.Sources
				}
				break
			}
		}
	}
	var items []json.RawMessage
	if found {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, false, err
		}
		if err = json.Unmarshal(raw, &items); err != nil {
			return nil, false, err
		}
	}
	return items, found, nil
}

func boundedGraphPage(s board.State, r GraphRequest, version string, items []json.RawMessage, missing []string) (graphPage, error) {
	total := len(items)
	start := min(r.Offset, total)
	end := min(start+r.Limit, total)
	value := graphPage{Section: r.Section, Offset: start, Total: total, Revision: s.Revision, Generation: s.Graph.Project.Generation, StateVersion: version, RequestedIDs: r.IDs, MissingIDs: missing}
	// Echoing the filter is optional; missing IDs are authoritative. Repeating
	// maximally escaped IDs in both fields can consume the entire response frame.
	if raw, err := json.Marshal(value); err != nil {
		return graphPage{}, err
	} else if len(raw) > maxGraphPageBytes/2 {
		value.RequestedIDs = nil
	}
	for {
		value.Items = items[start:end]
		if value.Items == nil {
			value.Items = []json.RawMessage{}
		}
		value.NextOffset = nil
		if end < total {
			next := end
			value.NextOffset = &next
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return graphPage{}, err
		}
		if len(raw) <= maxGraphPageBytes {
			return value, nil
		}
		if end-start > 1 {
			end--
			continue
		}
		if end > start {
			compact, err := compactGraphRecord(r.Section, items[start])
			if err != nil {
				return graphPage{}, err
			}
			value.Items = []json.RawMessage{compact}
			raw, err = json.Marshal(value)
			if err != nil {
				return graphPage{}, err
			}
			if len(raw) <= maxGraphPageBytes {
				return value, nil
			}
			reference, err := json.Marshal(graphRecordReference{true, start, len(items[start]), version, graphRecordVersion(items[start]), graphRecordReadMore})
			if err != nil {
				return graphPage{}, err
			}
			value.Items = []json.RawMessage{reference}
			return value, nil
		}
		return graphPage{}, fmt.Errorf("graph %s record at offset %d has scalar content exceeding the response budget; reducing limit cannot retrieve it", r.Section, start)
	}
}

func compactGraphRecord(section string, raw json.RawMessage) (json.RawMessage, error) {
	if section != "facts" && section != "findings" {
		return raw, nil
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	omitted := false
	for _, field := range []string{"evidence", "sources"} {
		var entries []json.RawMessage
		if len(record[field]) == 0 {
			continue
		}
		if err := json.Unmarshal(record[field], &entries); err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		delete(record, field)
		record[field+"_omitted"] = json.RawMessage("true")
		record[field+"_count"], _ = json.Marshal(len(entries))
		omitted = true
	}
	if omitted {
		record["read_more"], _ = json.Marshal("Omitted support is not absent. Use the same read tool with section evidence or sources, ids:[this record's id], offset:0, then follow next_offset until absent. Sources pages apply to Findings.")
	}
	return json.Marshal(record)
}

// CompactGraphActionResult keeps the successful write acknowledgement within
// the bridge frame. The saved entity remains available through graph pages.
func CompactGraphActionResult(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) <= maxGraphPageBytes {
		return raw, nil
	}
	var result board.StateActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Op            string `json:"op"`
		ID            string `json:"id"`
		Revision      int64  `json:"revision"`
		StateVersion  string `json:"state_version,omitempty"`
		Unchanged     bool   `json:"unchanged,omitempty"`
		ResultOmitted bool   `json:"result_omitted"`
		ReadMore      string `json:"read_more"`
	}{result.Op, result.ID, result.Revision, result.StateVersion, result.Unchanged, true, "Write succeeded. Read this entity by id in its graph section; follow any evidence_omitted or sources_omitted detail pages. Do not resubmit the write because its entity payload is omitted."})
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
	if (r.Section == "evidence" || r.Section == "sources") && len(r.IDs) != 1 {
		return errors.New("evidence and sources pages require exactly one record ID; evidence accepts Fact or Finding IDs, sources accepts a Finding ID")
	}
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
