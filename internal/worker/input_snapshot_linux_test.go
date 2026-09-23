//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func snapshotRuntimeFixture(t *testing.T, kind string) (Job, board.State) {
	t.Helper()
	intent := board.Intent{ID: "step_original", From: []string{"origin"}, Description: "Inspect the local fixture"}
	state := board.State{
		Graph: board.Graph{
			Project: board.Project{ID: "snapshot_project", Generation: 3, Status: "active"},
			Facts:   []board.Fact{{ID: "origin", Description: "Original user input remains intact"}, {ID: "goal", Description: "Read the local fixture"}},
			Intents: []board.Intent{intent},
		},
		Goals:       []board.Goal{{ID: "goal", Condition: "Read the local fixture", Status: "open"}},
		Steps:       []board.Step{{ID: intent.ID, From: intent.From, GoalID: "goal", Description: intent.Description, Status: "open"}},
		FactRecords: []board.FactRecord{{ID: "fact_original", Description: "Frozen fixture observation", Scope: "local fixture", Status: "confirmed"}},
		Revision:    7, DecisionRevision: 5,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	ref := &board.InputSnapshot{Version: 1, ID: hex.EncodeToString(sum[:]), ProjectID: state.Graph.Project.ID,
		Generation: state.Graph.Project.Generation, Revision: state.Revision, DecisionRevision: state.DecisionRevision,
		StateVersion: board.DecisionStateVersion(state), FactCount: len(state.Graph.Facts), OpenCount: 1, StepCount: 1}
	j := Job{RunID: "snapshot_run", Kind: kind, GraphRPC: true, ResultContractVersion: 2,
		Graph: board.Graph{Project: state.Graph.Project}, InputSnapshot: ref, Workspace: t.TempDir()}
	if kind == "reason" {
		j.Decision, err = board.BuildDecisionContextFromCursor(state, nil, nil, board.DefaultContextViewBytes)
		if j.Decision != nil {
			j.Decision.Version = 2
		}
	} else {
		j.Intent = &intent
		step := ""
		if kind == "explore" {
			step = intent.ID
		}
		j.InputView, err = board.ContextView(state, step, board.DefaultContextViewBytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	return j, state
}

func snapshotRuntimeTool(t *testing.T, opts Options, name string) agent.Tool {
	t.Helper()
	for _, tool := range opts.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is unavailable", name)
	return agent.Tool{}
}

func TestSnapshotExecutePromptDoesNotWritePartialLegacyGraph(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		t.Run(kind, func(t *testing.T) {
			job, state := snapshotRuntimeFixture(t, kind)
			dir := t.TempDir()
			prompt, err := Prompt(job, false, dir)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, "Original user input remains intact") || !strings.Contains(prompt, "read_snapshot") || !strings.Contains(prompt, "read_graph") {
				t.Fatal("snapshot prompt lost original input or its separate immutable/current read tools")
			}
			if strings.Contains(prompt, "graph.yaml") || strings.Contains(prompt, "complete original legacy graph") {
				t.Fatal("snapshot prompt advertises a nonexistent complete legacy graph")
			}
			if _, err = os.Stat(filepath.Join(dir, "graph.yaml")); !os.IsNotExist(err) {
				t.Fatalf("snapshot prompt wrote a partial graph.yaml: %v", err)
			}
			// Compatibility jobs still retain their complete original inline graph.
			legacy := job
			legacy.InputSnapshot, legacy.InputView = nil, nil
			legacy.Graph, legacy.State = state.Graph, &state
			legacyDir := t.TempDir()
			if _, err = Prompt(legacy, false, legacyDir); err != nil {
				t.Fatal(err)
			}
			full, err := os.ReadFile(filepath.Join(legacyDir, "graph.yaml"))
			if err != nil || !strings.Contains(string(full), "Original user input remains intact") {
				t.Fatalf("legacy inline recovery lost its full graph: %v", err)
			}
		})
	}
}

func TestSnapshotReadDoesNotRebindLiveGraphVersion(t *testing.T) {
	job, frozen := snapshotRuntimeFixture(t, "reason")
	live := frozen
	live.FactRecords = append([]board.FactRecord{}, frozen.FactRecords...)
	live.FactRecords[0].Description = "New live fixture observation"
	live.Revision++
	liveVersion := board.DecisionStateVersion(live)
	version := job.InputSnapshot.StateVersion
	opts := Options{RunDir: t.TempDir(), GraphVersion: &version}
	var requests []GraphRequest
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(r GraphRequest) (any, error) {
		requests = append(requests, r)
		if r.Op == "read_snapshot" {
			if r.ExpectedVersion != job.InputSnapshot.StateVersion {
				t.Fatalf("frozen read used live/model version: %+v", r)
			}
			return GraphPage(frozen, r)
		}
		if r.Op != "read_graph" {
			t.Fatalf("unexpected operation %s", r.Op)
		}
		if r.Section != "overview" && r.ExpectedVersion != liveVersion {
			t.Fatalf("live evidence read did not retain the refreshed version: %+v", r)
		}
		return GraphPage(live, r)
	}}
	if err := ConfigureRuntimeTools(job, &opts); err != nil {
		t.Fatal(err)
	}
	currentRead := snapshotRuntimeTool(t, opts, "read_graph")
	frozenRead := snapshotRuntimeTool(t, opts, "read_snapshot")
	if _, err := currentRead.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`)); err != nil {
		t.Fatal(err)
	}
	if version != liveVersion {
		t.Fatal("read_graph did not refresh the runtime version")
	}
	for n := 0; n < 2; n++ {
		result, err := frozenRead.Execute(context.Background(), json.RawMessage(`{"section":"facts","ids":["fact_original"],"expected_version":"model-must-not-select-a-different-input"}`))
		if err != nil || !strings.Contains(result, "Frozen fixture observation") || strings.Contains(result, "New live fixture observation") {
			t.Fatalf("original input was replaced by current evidence: %s %v", result, err)
		}
		if version != liveVersion {
			t.Fatal("read_snapshot rebound the live decision version to old input")
		}
	}
	result, err := currentRead.Execute(context.Background(), json.RawMessage(`{"section":"facts","ids":["fact_original"]}`))
	if err != nil || !strings.Contains(result, "New live fixture observation") {
		t.Fatalf("live graph no longer reads current evidence: %s %v", result, err)
	}
	if len(requests) != 4 || requests[0].Op != "read_graph" || requests[1].Op != "read_snapshot" || requests[2].Op != "read_snapshot" || requests[3].Op != "read_graph" {
		t.Fatalf("snapshot/live requests crossed their boundaries: %+v", requests)
	}
}

func TestSnapshotShadowReplanReadsOnlyFrozenInput(t *testing.T) {
	job, frozen := snapshotRuntimeFixture(t, "reason")
	// Exclude the support from the supplied projection so the shadow must use
	// the actual snapshot bridge before citing it as its judgment's basis.
	job.Decision.View = json.RawMessage(`{"version":1,"fact_records":[],"omitted":{"fact_records":1}}`)
	liveVersion := strings.Repeat("c", 64)
	opts := Options{RunDir: t.TempDir(), GraphVersion: &liveVersion}
	reads := 0
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(r GraphRequest) (any, error) {
		reads++
		if r.Op != "read_snapshot" || r.ExpectedVersion != job.InputSnapshot.StateVersion {
			t.Fatalf("shadow accessed live state instead of its bound input: %+v", r)
		}
		return GraphPage(frozen, r)
	}}
	calls := 0
	opts.Provider = scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if len(definitions) != 1 || definitions[0].Name != "read_graph" {
			t.Fatalf("shadow acquired tools other than its frozen read: %+v", definitions)
		}
		if calls == 1 {
			return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "read-frozen-support", Name: "read_graph", Input: json.RawMessage(`{"section":"facts","ids":["fact_original"]}`)}}}, nil
		}
		raw, err := json.Marshal(history)
		if err != nil || !strings.Contains(string(raw), "Frozen fixture observation") {
			t.Fatal("shadow judgment did not receive original support through its read tool")
		}
		return agent.Text("assistant", `{"decision":"keep","basis":["fact_original"],"missing":[]}`), nil
	})
	observation := &ReplanObservation{}
	saves := 0
	err := runReplanCheck(context.Background(), job, opts, observation, func(agent.Event) {}, func() error { saves++; return nil })
	if err != nil || observation.Status != "judged" || observation.Judgment == nil || observation.Judgment.Decision != "keep" {
		t.Fatalf("shadow snapshot judgment failed: %+v err=%v", observation, err)
	}
	if calls != 2 || reads != 1 || saves == 0 || liveVersion != strings.Repeat("c", 64) {
		t.Fatalf("shadow changed live state or skipped frozen input: calls=%d reads=%d saves=%d version=%s", calls, reads, saves, liveVersion)
	}
}
