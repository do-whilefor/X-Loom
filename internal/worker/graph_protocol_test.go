package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xloom/internal/board"
)

func checkedGraphPage(t *testing.T, state board.State, request GraphRequest) graphPage {
	t.Helper()
	request.RequestID = strings.Repeat("a", 32)
	if request.Op == "" {
		request.Op = "read_graph"
	}
	if err := ValidateGraphRequest(Job{}, request); err != nil {
		t.Fatal(err)
	}
	value, err := GraphPage(state, request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(GraphResponse{RequestID: request.RequestID, Result: raw})
	if err != nil || len(wire) > MaxGraphRPCBytes {
		t.Fatalf("page exceeds bridge frame: %d bytes, %v", len(wire), err)
	}
	var page graphPage
	if err = json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.StateVersion != board.DecisionStateVersion(state) {
		t.Fatal("page lost its input version")
	}
	return page
}

func collectGraphDetails[T any](t *testing.T, state board.State, section, id string) []T {
	t.Helper()
	request := GraphRequest{Section: section, IDs: []string{id}, Limit: 50, ExpectedVersion: board.DecisionStateVersion(state)}
	var collected []T
	for {
		page := checkedGraphPage(t, state, request)
		if page.Offset != len(collected) || len(page.MissingIDs) != 0 {
			t.Fatalf("detail page skipped support: offset=%d collected=%d missing=%v", page.Offset, len(collected), page.MissingIDs)
		}
		for _, raw := range page.Items {
			var item T
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatal(err)
			}
			collected = append(collected, item)
		}
		if page.NextOffset == nil {
			if len(collected) != page.Total {
				t.Fatalf("incomplete support: got %d want %d", len(collected), page.Total)
			}
			return collected
		}
		if *page.NextOffset <= request.Offset || len(page.Items) == 0 {
			t.Fatal("detail pagination did not advance")
		}
		request.Offset = *page.NextOffset
	}
}

func TestGraphPageShrinksCountWithoutLosingRecords(t *testing.T) {
	state := board.State{}
	for i := 0; i < 20; i++ {
		state.FactRecords = append(state.FactRecords, board.FactRecord{ID: fmt.Sprintf("f%d", i), Description: "Observed", Evidence: []board.EvidenceRef{{RunID: "run", Path: "retained.txt", Excerpt: strings.Repeat("x", 8192)}}})
	}
	request := GraphRequest{Section: "facts", Limit: 20}
	var got []board.FactRecord
	for {
		page := checkedGraphPage(t, state, request)
		if request.Offset == 0 && (len(page.Items) >= 20 || page.NextOffset == nil) {
			t.Fatal("oversized default page was not bounded with a continuation")
		}
		for _, raw := range page.Items {
			var fact board.FactRecord
			if err := json.Unmarshal(raw, &fact); err != nil {
				t.Fatal(err)
			}
			got = append(got, fact)
		}
		if page.NextOffset == nil {
			break
		}
		request.Offset = *page.NextOffset
	}
	if !reflect.DeepEqual(got, state.FactRecords) {
		t.Fatal("paging changed or omitted complete records that individually fit")
	}
}

func TestMergedFindingReceiptAndSupportRemainReadable(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Now = func() time.Time { return time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC) }
	fence := board.ExecutionFence{Run: "executor@run", Lease: "explore", Intent: "i001"}
	tx := func(fn func(*board.Tx) error) {
		t.Helper()
		if err := store.Do(context.Background(), fn); err != nil {
			t.Fatal(err)
		}
	}
	tx(func(b *board.Tx) error {
		return b.Save(board.Graph{Project: board.Project{ID: "proj_001", Title: "Paging", Status: "active", CreatedAt: b.Now}, Facts: []board.Fact{{ID: "origin", Description: "input"}, {ID: "goal", Description: "goal"}}, Intents: []board.Intent{{ID: "i001", From: []string{"origin"}, Description: "collect evidence", Creator: "planner", Worker: board.Ptr(fence.Run), Heartbeat: board.Ptr(b.Now), CreatedAt: b.Now}}})
	})
	action := func(op, key string, payload any) board.StateActionResult {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := GraphRequest{RequestID: strings.Repeat("a", 32), Op: "graph_action", Action: board.StateAction{Op: op, IdempotencyKey: key, Payload: raw}}
		wire, _ := json.Marshal(GraphRequestEvent{Type: "graph_request", Request: request})
		if len(wire) > MaxGraphRPCBytes {
			t.Fatal("fixture must use individually valid graph requests")
		}
		var result board.StateActionResult
		tx(func(b *board.Tx) (err error) {
			result, err = b.StateAction("proj_001", fence, request.Action)
			return err
		})
		return result
	}
	fact := action("fact", "source", map[string]any{"description": "observed", "scope": "fixture", "observed_at": "2026-09-23T01:00:00Z", "evidence": []board.EvidenceRef{{RunID: "run", Path: "source.raw", Excerpt: "observed"}}})
	var result board.StateActionResult
	var expected []board.EvidenceRef
	for batch := 0; batch < 2; batch++ {
		var refs []board.EvidenceRef
		for i := 0; i < 8; i++ {
			refs = append(refs, board.EvidenceRef{RunID: "run", Path: fmt.Sprintf("retained/%d.raw", batch*8+i), Excerpt: strings.Repeat("x", 8192)})
		}
		expected = append(expected, refs...)
		result = action("finding", fmt.Sprintf("merge%d", batch), map[string]any{"claim": "fixture claim", "scope": "fixture", "status": "verified", "sources": []string{fact.ID}, "evidence": refs})
	}
	raw, _ := json.Marshal(result)
	if len(raw) <= MaxGraphRPCBytes {
		t.Fatal("fixture must exercise an oversized committed entity")
	}
	compact, err := CompactGraphActionResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		board.StateActionResult
		ResultOmitted bool `json:"result_omitted"`
	}
	if err = json.Unmarshal(compact, &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.ResultOmitted || receipt.ID != result.ID || receipt.Op != result.Op || receipt.Revision != result.Revision || receipt.StateVersion != result.StateVersion || len(receipt.Result) != 0 {
		t.Fatal("compact receipt lost the authoritative write acknowledgement")
	}
	var state board.State
	tx(func(b *board.Tx) (err error) { state, err = b.State("proj_001"); return err })
	page := checkedGraphPage(t, state, GraphRequest{Section: "findings", IDs: []string{result.ID}, Limit: 1})
	var record map[string]json.RawMessage
	if err = json.Unmarshal(page.Items[0], &record); err != nil {
		t.Fatal(err)
	}
	if string(record["evidence_omitted"]) != "true" || string(record["evidence_count"]) != "16" || record["evidence"] != nil || len(record["read_more"]) == 0 {
		t.Fatal("oversized support was not explicitly exposed for detail reads")
	}
	if got := collectGraphDetails[board.EvidenceRef](t, state, "evidence", result.ID); !reflect.DeepEqual(got, expected) {
		t.Fatal("merged evidence was lost or changed across pages")
	}
	if got := collectGraphDetails[string](t, state, "sources", result.ID); !reflect.DeepEqual(got, []string{fact.ID}) {
		t.Fatal("finding sources were lost")
	}
}

func TestGraphDetailsPaginateAccumulatedSourcesAndFenceVersions(t *testing.T) {
	finding := board.Finding{ID: "finding_many", Claim: "many observations"}
	for i := 0; i < 6000; i++ {
		finding.Sources = append(finding.Sources, fmt.Sprintf("fact_%032d", i))
	}
	state := board.State{Findings: []board.Finding{finding}}
	page := checkedGraphPage(t, state, GraphRequest{Section: "findings", IDs: []string{finding.ID}})
	if !strings.Contains(string(page.Items[0]), `"sources_omitted":true`) {
		t.Fatal("oversized accumulated sources need a readable continuation")
	}
	if got := collectGraphDetails[string](t, state, "sources", finding.ID); !reflect.DeepEqual(got, finding.Sources) {
		t.Fatal("source pagination lost IDs or order")
	}
	for _, section := range []string{"evidence", "sources"} {
		for _, ids := range [][]string{nil, {"one", "two"}} {
			r := GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph", Section: section, IDs: ids}
			if ValidateGraphRequest(Job{}, r) == nil {
				t.Fatal("detail request must identify exactly one record")
			}
			if _, err := GraphPage(state, r); err == nil {
				t.Fatal("direct detail reads must validate their selector")
			}
		}
		if _, err := GraphPage(state, GraphRequest{Section: section, IDs: []string{finding.ID}, ExpectedVersion: strings.Repeat("b", 64)}); err == nil || !strings.Contains(err.Error(), "state_changed") {
			t.Fatal("detail reads bypassed version fencing")
		}
		missing := checkedGraphPage(t, state, GraphRequest{Section: section, IDs: []string{"missing"}})
		if missing.Total != 0 || !reflect.DeepEqual(missing.MissingIDs, []string{"missing"}) {
			t.Fatal("unknown detail record was reported as empty existing support")
		}
	}
}

func TestGraphSupportContinuationKeepsReadSource(t *testing.T) {
	makeState := func(label string) board.State {
		var evidence []board.EvidenceRef
		for i := 0; i < 16; i++ {
			evidence = append(evidence, board.EvidenceRef{RunID: label, Path: fmt.Sprintf("%s/%d.raw", label, i), Excerpt: strings.Repeat(label, 8192/len(label))})
		}
		var sources []string
		for i := 0; i < 600; i++ {
			sources = append(sources, fmt.Sprintf("%s_%03d_%s", label, i, strings.Repeat("x", 240)))
		}
		return board.State{
			FactRecords: []board.FactRecord{{ID: "fact", Description: label, Evidence: evidence}},
			Findings:    []board.Finding{{ID: "finding", Claim: label, Evidence: evidence, Sources: sources}},
		}
	}
	// Frozen and current support differ, so continuation must retain the read
	// tool that supplied the compact record instead of directing it to live data.
	frozen, live := makeState("frozen"), makeState("current")
	for _, op := range []string{"read_graph", "read_snapshot"} {
		for _, tc := range []struct{ section, id, support string }{
			{"facts", "fact", "evidence"},
			{"findings", "finding", "evidence"},
			{"findings", "finding", "sources"},
		} {
			t.Run(op+"/"+tc.section+"/"+tc.support, func(t *testing.T) {
				state := live
				if op == "read_snapshot" {
					state = frozen
				}
				request := GraphRequest{Op: op, Section: tc.section, IDs: []string{tc.id}, Limit: 50, ExpectedVersion: board.DecisionStateVersion(state)}
				page := checkedGraphPage(t, state, request)
				var record map[string]json.RawMessage
				if len(page.Items) != 1 || json.Unmarshal(page.Items[0], &record) != nil || string(record[tc.support+"_omitted"]) != "true" {
					t.Fatal("fixture must expose omitted support")
				}
				var readMore string
				if json.Unmarshal(record["read_more"], &readMore) != nil || !strings.Contains(readMore, "same read tool") || strings.Contains(readMore, "read_graph") || strings.Contains(readMore, "read_snapshot") {
					t.Errorf("support continuation must retain its original read tool: %q", readMore)
				}
				request.Section = tc.support
				var collected []json.RawMessage
				for {
					detail := checkedGraphPage(t, state, request)
					collected = append(collected, detail.Items...)
					if detail.NextOffset == nil {
						break
					}
					request.Offset = *detail.NextOffset
				}
				var expected any = state.Findings[0].Evidence
				if tc.support == "sources" {
					expected = state.Findings[0].Sources
				}
				got, _ := json.Marshal(collected)
				want, _ := json.Marshal(expected)
				if string(got) != string(want) {
					t.Fatal("support continuation lost or changed the original read's data")
				}
			})
		}
	}
}

func TestCompactGraphActionResultPreservesSmallResponsesAndUnchanged(t *testing.T) {
	raw := json.RawMessage(`{"op":"fact","id":"f1","revision":3,"result":{"description":"ok"},"state_version":"version"}`)
	got, err := CompactGraphActionResult(raw)
	if err != nil || string(got) != string(raw) {
		t.Fatal("small action response changed")
	}
	result := board.StateActionResult{Op: "finding", ID: "finding_1", Revision: 4, StateVersion: strings.Repeat("a", 64), Unchanged: true, Result: json.RawMessage(`{"content":"` + strings.Repeat("x", MaxGraphRPCBytes) + `"}`)}
	raw, _ = json.Marshal(result)
	got, err = CompactGraphActionResult(raw)
	if err != nil || !strings.Contains(string(got), `"unchanged":true`) || !strings.Contains(string(got), `"result_omitted":true`) {
		t.Fatal("compact idempotent result lost its acknowledgement")
	}
}

// Byte pages reconstruct the JSON for a single selected record, not a compact
// summary. This also covers retained legacy inputs which exceed current limits.
func collectGraphRecord(t *testing.T, state board.State, request GraphRequest) json.RawMessage {
	t.Helper()
	request.ExpectedVersion = board.DecisionStateVersion(state)
	offset := 0
	request.ByteOffset = &offset
	var result []byte
	for {
		request.RequestID, request.Op = strings.Repeat("a", 32), "read_graph"
		if err := ValidateGraphRequest(Job{}, request); err != nil {
			t.Fatal(err)
		}
		value, err := GraphPage(state, request)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(GraphResponse{RequestID: request.RequestID, Result: raw})
		if err != nil || len(wire) > MaxGraphRPCBytes {
			t.Fatalf("byte page exceeds bridge frame: %d, %v", len(wire), err)
		}
		var page graphContentPage
		if err = json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if page.StateVersion != request.ExpectedVersion || page.Offset != request.Offset || page.ByteOffset != len(result) || !utf8.ValidString(page.Content) {
			t.Fatalf("invalid byte page: offset=%d bytes=%d content=%d", page.Offset, page.ByteOffset, len(page.Content))
		}
		result = append(result, page.Content...)
		if page.NextByteOffset == nil {
			if len(result) != page.TotalBytes || !json.Valid(result) {
				t.Fatal("byte pagination lost content")
			}
			return result
		}
		if *page.NextByteOffset != len(result) || *page.NextByteOffset <= offset {
			t.Fatal("byte pagination did not advance exactly")
		}
		offset = *page.NextByteOffset
		request.RecordVersion = page.RecordVersion
	}
}

func TestGraphPageContinuesOversizedSingleRecordsLosslessly(t *testing.T) {
	large := strings.Repeat("λ😀<\\\"\n", 20000)
	state := board.State{
		Graph:         board.Graph{Hints: []board.Hint{{ID: "h1", Content: large}}},
		FactRecords:   []board.FactRecord{{ID: "f1", Description: large, Evidence: []board.EvidenceRef{{Path: large}}}},
		Findings:      []board.Finding{{ID: "finding1", Claim: large, Evidence: []board.EvidenceRef{{Excerpt: large}}, Sources: []string{large}}},
		Goals:         []board.Goal{{ID: "g1", Condition: large}},
		Steps:         []board.Step{{ID: "s1", Description: large}},
		FactRelations: []board.FactRelation{{Source: "f1", Target: "f2", Reason: large}},
	}
	for _, tc := range []struct {
		section string
		ids     []string
		want    any
	}{
		{"facts", nil, state.FactRecords[0]},
		{"findings", nil, state.Findings[0]},
		{"goals", nil, state.Goals[0]},
		{"steps", nil, state.Steps[0]},
		{"relations", []string{"f1"}, state.FactRelations[0]},
		{"hints", []string{"h1"}, state.Graph.Hints[0]},
		{"evidence", []string{"f1"}, state.FactRecords[0].Evidence[0]},
		{"evidence", []string{"finding1"}, state.Findings[0].Evidence[0]},
		{"sources", []string{"finding1"}, state.Findings[0].Sources[0]},
	} {
		t.Run(tc.section+strings.Join(tc.ids, "_"), func(t *testing.T) {
			request := GraphRequest{Section: tc.section, IDs: tc.ids, Limit: 1}
			page := checkedGraphPage(t, state, request)
			var reference graphRecordReference
			if err := json.Unmarshal(page.Items[0], &reference); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(tc.want)
			if !reference.RecordOmitted || reference.RecordOffset != 0 || reference.RecordBytes != len(want) || reference.StateVersion != page.StateVersion || reference.RecordVersion != graphRecordVersion(want) {
				t.Fatal("oversized record did not expose a bound continuation")
			}
			if got := collectGraphRecord(t, state, request); string(got) != string(want) {
				t.Fatal("byte pages changed the original record")
			}
		})
	}
}

func TestGraphOverviewContinuesCompleteUserInputs(t *testing.T) {
	state := board.State{Graph: board.Graph{Project: board.Project{Title: strings.Repeat("title", MaxGraphRPCBytes)}, Facts: []board.Fact{{ID: "origin", Description: strings.Repeat("input", MaxGraphRPCBytes)}, {ID: "goal", Description: "the exact goal"}}}}
	value, err := GraphPage(state, GraphRequest{Section: "overview"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var reference graphRecordReference
	if err := json.Unmarshal(raw, &reference); err != nil || !reference.RecordOmitted || len(raw) > maxGraphPageBytes {
		t.Fatal("oversized overview cannot be continued")
	}
	got := collectGraphRecord(t, state, GraphRequest{Section: "overview"})
	var overview struct {
		Project board.Project `json:"project"`
		Inputs  []board.Fact  `json:"user_inputs"`
	}
	if err := json.Unmarshal(got, &overview); err != nil || !reflect.DeepEqual(overview.Project, state.Graph.Project) || !reflect.DeepEqual(overview.Inputs, state.Graph.Facts) {
		t.Fatal("overview continuation lost original project constraints")
	}
}

func TestGraphBytePagesValidateOffsetsAndVersion(t *testing.T) {
	state := board.State{Graph: board.Graph{Hints: []board.Hint{{ID: "h1", Content: "λ😀"}, {ID: "h2", Content: "second"}}}}
	encoded, _ := json.Marshal(state.Graph.Hints[0])
	version := board.DecisionStateVersion(state)
	insideRune := strings.Index(string(encoded), "λ") + 1
	for _, tc := range []struct {
		offset, byteOffset int
		version            string
	}{
		{0, -1, version}, {0, 0, ""}, {0, 0, "changed"},
		{0, insideRune, version}, {0, math.MaxInt, version}, {math.MaxInt, 0, version},
	} {
		_, err := GraphPage(state, GraphRequest{Section: "hints", Offset: tc.offset, ByteOffset: &tc.byteOffset, ExpectedVersion: tc.version})
		if err == nil {
			t.Fatalf("invalid byte cursor accepted: %+v", tc)
		}
	}
	request := GraphRequest{Section: "hints", IDs: []string{"h2"}, Offset: 0}
	want, _ := json.Marshal(state.Graph.Hints[1])
	if got := collectGraphRecord(t, state, request); string(got) != string(want) {
		t.Fatal("byte offset selected an unfiltered record")
	}
	page := checkedGraphPage(t, state, GraphRequest{Section: "hints", Offset: math.MaxInt, Limit: 50})
	if page.Offset != 2 || len(page.Items) != 0 || page.NextOffset != nil {
		t.Fatal("large record offset overflowed or generated a false continuation")
	}
}

func TestGraphPagesBoundEscapedSelectorMetadata(t *testing.T) {
	var ids []string
	for i := 0; i < 50; i++ {
		ids = append(ids, fmt.Sprintf("%02d", i)+strings.Repeat("\x00", 254))
	}
	page := checkedGraphPage(t, board.State{}, GraphRequest{Section: "facts", IDs: ids})
	if !reflect.DeepEqual(page.MissingIDs, ids) || page.NextOffset != nil || len(page.Items) != 0 {
		t.Fatal("escaped selector metadata lost missing IDs")
	}
}

func TestGraphByteContinuationRejectsRuntimeOnlyChanges(t *testing.T) {
	state := board.State{Steps: []board.Step{{ID: "step", Description: strings.Repeat("x", MaxGraphRPCBytes), Status: "open"}}}
	version := board.DecisionStateVersion(state)
	zero := 0
	request := GraphRequest{Section: "steps", ByteOffset: &zero, ExpectedVersion: version}
	value, err := GraphPage(state, request)
	if err != nil {
		t.Fatal(err)
	}
	page := value.(graphContentPage)
	if page.NextByteOffset == nil {
		t.Fatal("fixture must span multiple byte pages")
	}
	request.ByteOffset, request.RecordVersion = page.NextByteOffset, page.RecordVersion
	state.Steps[0].Status, state.Steps[0].Worker = "running", board.Ptr("worker")
	if board.DecisionStateVersion(state) != version {
		t.Fatal("fixture must change raw record bytes without staling the decision")
	}
	if _, err = GraphPage(state, request); err == nil || !strings.Contains(err.Error(), "state_changed") {
		t.Fatal("different record bytes were allowed into the same continuation")
	}
	request.RecordVersion = ""
	if _, err = GraphPage(state, request); err == nil || !strings.Contains(err.Error(), "record_version") {
		t.Fatal("unbound later byte fragment was accepted")
	}
}
