//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/board"
)

func finalEvidenceOutput(path string) string {
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{"description": "Fixture denied the unauthenticated request", "scope": "one fixture request", "observed_at": "2026-09-22T10:00:00Z", "evidence": []map[string]string{{"path": path}}}}})
	return string(raw)
}

func finalEvidenceRefs(t *testing.T, j Job, r Result) []board.EvidenceRef {
	t.Helper()
	parsed, err := parseOutput(j, r.Conclude, r.Text)
	if err != nil {
		t.Fatal(err)
	}
	var fact struct {
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if err = json.Unmarshal(parsed.FactPayload, &fact); err != nil {
		t.Fatal(err)
	}
	return fact.Evidence
}

func TestFinalEvidenceFreezesBeforeDurableResultAndReusesOriginal(t *testing.T) {
	j := outcomeJob(t, "explore")
	j.ResultContractVersion = 2
	runDir := t.TempDir()
	source := filepath.Join(j.Workspace, "response.txt")
	const original = "HTTP/1.1 401 Unauthorized\n"
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	input := Result{Status: "success", Text: finalEvidenceOutput(source)}
	first, err := prepareFinalEvidence(context.Background(), j, runDir, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, []byte("HTTP/1.1 200 OK\n"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := prepareFinalEvidence(context.Background(), j, runDir, input, nil)
	if err != nil || again.Text != first.Text {
		t.Fatalf("result refreshed mutable evidence: %v", err)
	}
	refs := finalEvidenceRefs(t, j, first)
	if len(refs) != 1 || refs[0].RunID != j.RunID || refs[0].Path == source || refs[0].Excerpt != original {
		t.Fatalf("bad frozen evidence: %+v", refs)
	}
	raw, err := os.ReadFile(refs[0].Path)
	if err != nil || string(raw) != original {
		t.Fatalf("original snapshot lost: %s %v", raw, err)
	}
}

func TestConclusionUsesOnlyPersistedBoundaryEvidence(t *testing.T) {
	j := outcomeJob(t, "explore")
	j.ResultContractVersion = 2
	runDir := t.TempDir()
	source := filepath.Join(runDir, "output-fixture.txt")
	const original = "HTTP/1.1 401 Unauthorized\n"
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	prompt, refs, err := conclusionInputWithEvidence(context.Background(), j, runDir, true)
	if err != nil || len(refs) != 1 || !strings.Contains(prompt, refs[0].Path) {
		t.Fatalf("snapshot not offered: %v %+v", err, refs)
	}
	if err = os.WriteFile(source, []byte("changed after conclusion"), 0600); err != nil {
		t.Fatal(err)
	}
	// Recovery restores a missing retained fragment from the bytes already
	// saved in the session, never by reopening the changed source file.
	if err = os.Remove(refs[0].Path); err != nil {
		t.Fatal(err)
	}
	input := Result{Status: "success", Conclude: true, Text: finalEvidenceOutput(refs[0].Path)}
	got, err := prepareFinalEvidence(context.Background(), j, runDir, input, refs)
	if err != nil {
		t.Fatal(err)
	}
	if selected := finalEvidenceRefs(t, j, got); len(selected) != 1 || selected[0].Excerpt != original {
		t.Fatalf("boundary bytes changed: %+v", selected)
	}
	if raw, err := os.ReadFile(refs[0].Path); err != nil || string(raw) != original {
		t.Fatalf("retained fragment not restored: %s %v", raw, err)
	}
	input.Text = finalEvidenceOutput(source)
	if _, err = prepareFinalEvidence(context.Background(), j, runDir, input, refs); err == nil {
		t.Fatal("conclusion read a mutable file")
	}
	recoveredPrompt, recovered, err := conclusionInputWithEvidence(context.Background(), j, runDir, false)
	if err != nil || len(recovered) != 0 {
		t.Fatal("recovery minted new boundary evidence")
	}
	if !strings.Contains(recoveredPrompt, "No frozen fragments are available for a new final fact") || !strings.Contains(recoveredPrompt, "fact_id from this Step") {
		t.Fatal("recovered conclusion taught the model to select an unavailable evidence file")
	}
	if _, err := prepareFinalEvidence(context.Background(), j, runDir, input, recovered); err == nil {
		t.Fatal("recovery without a snapshot accepted a new final fact")
	}
	input.Text = `{"accepted":true,"outcome":"completed","data":{"fact_id":"published_fact"}}`
	if _, err := prepareFinalEvidence(context.Background(), j, runDir, input, recovered); err != nil {
		t.Fatalf("missing snapshot prevented referencing an already published fact: %v", err)
	}
}

func TestConclusionInvalidUTF8NeverBecomesOriginalEvidence(t *testing.T) {
	j := outcomeJob(t, "explore")
	j.ResultContractVersion = 2
	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, "output-invalid.txt"), []byte{'a', 0xff, 'b'}, 0600); err != nil {
		t.Fatal(err)
	}
	_, refs, err := conclusionInputWithEvidence(context.Background(), j, runDir, true)
	if err != nil || len(refs) != 0 {
		t.Fatalf("invalid source was certified: %+v %v", refs, err)
	}
}

func TestWorkerVersionTwoFinalizesAndReplaysFrozenEvidence(t *testing.T) {
	j := outcomeJob(t, "explore")
	j.ResultContractVersion = 2
	runDir := t.TempDir()
	source := filepath.Join(j.Workspace, "response.txt")
	const original = "HTTP/1.1 401 Unauthorized\n"
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	turns := 0
	provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns > 1 {
			t.Fatal("a verified terminal fact requested a publication or confirmation turn")
		}
		prompt := phaseHistoryText(history)
		if !strings.Contains(prompt, "As soon as the assigned checks and required artifacts are verified") ||
			!strings.Contains(prompt, "still share important intermediate discoveries while work remains") {
			t.Fatal("direct completion lost its verification or intermediate sharing boundary")
		}
		return agent.Text("assistant", finalEvidenceOutput(source)), nil
	})
	first, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: provider})
	if err != nil || first.Status != "success" || first.Conclude || turns != 1 {
		t.Fatalf("final result: %+v %v", first, err)
	}
	refs := finalEvidenceRefs(t, j, first)
	if len(refs) != 1 || refs[0].RunID != j.RunID || refs[0].Path == source || refs[0].Excerpt != original {
		t.Fatalf("direct completion did not freeze evidence: %+v", refs)
	}
	if saved := outcomeSession(t, runDir); saved.Result == nil || saved.Result.Text != first.Text || saved.RepairCount != 0 || saved.ContinuationCount != 0 {
		t.Fatal("direct completion did not persist its frozen result without extra work")
	}
	if err = os.WriteFile(source, []byte("changed after result"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: provider})
	if err != nil || first.Text != again.Text || turns != 1 {
		t.Fatalf("replay re-executed or changed evidence: turns=%d err=%v", turns, err)
	}
	if raw, err := os.ReadFile(refs[0].Path); err != nil || string(raw) != original {
		t.Fatalf("direct completion lost its original frozen evidence: %q %v", raw, err)
	}
}
