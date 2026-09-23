//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestFinalFileEvidenceRemainsReadableAcrossGraphPages(t *testing.T) {
	j := outcomeJob(t, "explore")
	j.ResultContractVersion = 2
	var selections []map[string]string
	var originals []string
	for i := 0; i < 16; i++ {
		name := filepath.Join(j.Workspace, fmt.Sprintf("evidence-%02d.txt", i))
		original := fmt.Sprintf("%02d", i) + strings.Repeat("λ<\n", 2047) + "xx"
		if len(original) != 8192 {
			t.Fatal("fixture must use the accepted 8192-byte excerpt bound")
		}
		if err := os.WriteFile(name, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		selections = append(selections, map[string]string{"path": name})
		originals = append(originals, original)
	}
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{"description": "Sixteen original artifacts", "scope": "fixture", "observed_at": "2026-09-23T01:00:00Z", "evidence": selections}}})
	result, err := prepareFinalEvidence(context.Background(), j, t.TempDir(), Result{Status: "success", Text: string(raw)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs := finalEvidenceRefs(t, j, result)
	state := board.State{FactRecords: []board.FactRecord{{ID: "f_large", Description: "Sixteen original artifacts", Evidence: refs}}}
	page := checkedGraphPage(t, state, GraphRequest{Section: "facts", IDs: []string{"f_large"}, Limit: 1})
	if !strings.Contains(string(page.Items[0]), `"evidence_omitted":true`) {
		t.Fatal("large final fact did not advertise its support pages")
	}
	got := collectGraphDetails[board.EvidenceRef](t, state, "evidence", "f_large")
	if !reflect.DeepEqual(got, refs) {
		t.Fatal("detail pages changed retained references")
	}
	for i, ref := range got {
		if ref.Excerpt != originals[i] {
			t.Fatalf("detail page changed original UTF-8 or escaping in artifact %d", i)
		}
	}
}

func TestReadGraphRuntimeContinuesOversizedOverview(t *testing.T) {
	state := board.State{Graph: board.Graph{Facts: []board.Fact{{ID: "origin", Description: strings.Repeat("λ", MaxGraphRPCBytes)}, {ID: "goal", Description: "goal"}}}}
	for _, kind := range []string{"reason", "explore"} {
		t.Run(kind, func(t *testing.T) {
			job := Job{Kind: kind, GraphRPC: true}
			if kind == "reason" {
				job.Decision = &board.DecisionContext{Version: 1, StateVersion: board.DecisionStateVersion(state)}
			}
			runDir := t.TempDir()
			options := Options{RunDir: runDir, Output: &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) { return GraphPage(state, request) }}}
			if err := ConfigureRuntimeTools(job, &options); err != nil {
				t.Fatal(err)
			}
			for _, tool := range options.Tools {
				if tool.Definition.Name != "read_graph" {
					continue
				}
				initial, err := tool.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`))
				if err != nil {
					t.Fatal(err)
				}
				var reference graphRecordReference
				if err = json.Unmarshal([]byte(initial), &reference); err != nil || !reference.RecordOmitted {
					t.Fatal("runtime hid the oversized overview continuation")
				}
				zero := 0
				request := GraphRequest{Section: "overview", ByteOffset: &zero, ExpectedVersion: reference.StateVersion, RecordVersion: reference.RecordVersion}
				var result strings.Builder
				for {
					raw, _ := json.Marshal(request)
					fragment, err := tool.Execute(context.Background(), raw)
					if err != nil {
						t.Fatal(err)
					}
					var page graphContentPage
					if err = json.Unmarshal([]byte(fragment), &page); err != nil {
						t.Fatal(err)
					}
					result.WriteString(page.Content)
					if page.NextByteOffset == nil {
						break
					}
					request.ByteOffset = page.NextByteOffset
				}
				var overview struct {
					Inputs []board.Fact `json:"user_inputs"`
				}
				if err = json.Unmarshal([]byte(result.String()), &overview); err != nil || !reflect.DeepEqual(overview.Inputs, state.Graph.Facts) {
					t.Fatal("runtime could not recover the original overview inputs")
				}
				return
			}
			t.Fatal("runtime has no read_graph tool")
		})
	}
}
