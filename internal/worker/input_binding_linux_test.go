//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"xloom/internal/board"
)

func bindingSnapshotJob(t *testing.T, j Job) Job {
	t.Helper()
	state := board.State{Graph: j.Graph, Revision: 7, DecisionRevision: 3}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	j.InputSnapshot = &board.InputSnapshot{Version: 1, ID: hex.EncodeToString(sum[:]),
		ProjectID: state.Graph.Project.ID, Generation: state.Graph.Project.Generation,
		Revision: state.Revision, DecisionRevision: state.DecisionRevision,
		StateVersion: board.DecisionStateVersion(state), FactCount: len(state.Graph.Facts),
		HintCount: len(state.Graph.Hints), OpenCount: state.Graph.OpenCount(), StepCount: len(state.Steps)}
	if j.Kind == "reason" {
		j.Intent = nil
		j.Decision, err = board.BuildDecisionContextFromCursor(state, nil, nil, board.DefaultContextViewBytes)
	} else {
		j.InputView, err = board.ContextView(state, "", board.DefaultContextViewBytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	j.Graph = board.Graph{Project: j.Graph.Project}
	j.State = nil
	return j
}

func TestSnapshotPromptRejectsInvalidBindings(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		for name, mutate := range map[string]func(*Job){
			"missing reference": func(j *Job) { j.InputSnapshot = nil; j.PreparationKey = "prepared" },
			"wrong project":     func(j *Job) { j.InputSnapshot.ProjectID = "another" },
			"wrong generation":  func(j *Job) { j.InputSnapshot.Generation++ },
			"unknown format":    func(j *Job) { j.InputSnapshot.Version++ },
			"invalid digest":    func(j *Job) { j.InputSnapshot.ID = strings.Repeat("z", 64) },
			"inline state":      func(j *Job) { j.State = &board.State{} },
			"inline graph":      func(j *Job) { j.Graph.Facts = []board.Fact{{ID: "stale"}} },
			"wrong decision": func(j *Job) {
				if kind == "reason" {
					j.Decision.ToRevision++
				} else {
					j.Decision = &board.DecisionContext{}
				}
			},
			"oversize view": func(j *Job) {
				view, _ := json.Marshal(strings.Repeat("x", board.DefaultContextViewBytes))
				if kind == "reason" {
					j.Decision.View = view
				} else {
					j.InputView = view
				}
			},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				j := bindingSnapshotJob(t, outcomeJob(t, kind))
				if _, err := Prompt(j, false, t.TempDir()); err != nil {
					t.Fatal(err)
				}
				mutate(&j)
				if _, err := Prompt(j, false, t.TempDir()); err == nil {
					t.Fatal("accepted invalid snapshot binding")
				}
			})
		}
	}
	j := bindingSnapshotJob(t, outcomeJob(t, "reason"))
	j.Intent = &board.Intent{ID: "invented", Description: "unbound instruction"}
	if _, err := Prompt(j, false, t.TempDir()); err == nil {
		t.Fatal("Decide accepted an unrelated Intent")
	}
}

func assertSnapshotResumeRejectsChangedInput(t *testing.T, j Job, options Options) {
	t.Helper()
	for _, field := range []string{"snapshot", "view"} {
		changed := j
		ref := *j.InputSnapshot
		changed.InputSnapshot = &ref
		if field == "snapshot" {
			changed.InputSnapshot.ID = strings.Repeat("b", 64)
		} else {
			changed.InputView = json.RawMessage(`{"changed":true}`)
		}
		if _, err := Run(context.Background(), changed, options); err == nil || !strings.Contains(err.Error(), "immutable input mismatch") {
			t.Fatalf("recovery accepted changed %s: %v", field, err)
		}
	}
}
