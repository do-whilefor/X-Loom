//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"xloom/internal/agent"
)

func TestConcludePromptInlinesInputsWithoutGraphFile(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore"} {
		t.Run(kind, func(t *testing.T) {
			j := job(t, kind)
			j.Graph.Project.Title = "inline graph title"
			j.Intent.Description = "inline intent"
			missing := filepath.Join(t.TempDir(), "does-not-exist")
			prompt, err := Prompt(j, true, missing)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{"inline graph title", "Current intent i1: inline intent", "All tools are disabled"} {
				if !strings.Contains(prompt, expected) {
					t.Fatalf("missing %q in %s", expected, prompt)
				}
			}
			if strings.Contains(prompt, "graph.yaml") || strings.Contains(prompt, "Read the complete task graph from") {
				t.Fatal("conclusion requires file access", prompt)
			}
			if _, err := os.Stat(missing); !os.IsNotExist(err) {
				t.Fatal("conclusion created a graph directory")
			}
		})
	}
}

func TestEveryConcludeToolIsRejectedEvenWithLegacyPermission(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	names := []string{"read", "grep", "find", "ls", "bash", "write", "edit"}
	definitions := []agent.Tool{}
	calls := []agent.Block{}
	for i, name := range names {
		definitions = append(definitions, agent.Tool{Definition: agent.Definition{Name: name}, Conclude: true, Parallel: true, Execute: func(context.Context, json.RawMessage) (string, error) {
			t.Error("executed a tool during conclusion")
			return "unexpected", nil
		}})
		calls = append(calls, agent.Block{Type: "tool_use", ID: fmt.Sprintf("c%d", i), Name: name, Input: json.RawMessage(`{}`)})
	}
	turns := 0
	r, err := Run(context.Background(), job(t, "explore"), Options{RunDir: t.TempDir(), Tools: definitions, SoftStop: stop, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if d != nil {
			t.Fatal("model received conclude tool definitions", d)
		}
		turns++
		if turns == 1 {
			return agent.Message{Role: "assistant", Content: calls, StopReason: "tool_use"}, nil
		}
		results := lastToolResults(m)
		if len(results) != len(calls) {
			t.Fatal("lost tool results")
		}
		for i, result := range results {
			if !result.IsError || result.ToolUseID != calls[i].ID || !strings.Contains(string(result.Content), "all tools are disabled") {
				t.Fatalf("invalid blocked result %#v", result)
			}
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || !r.Conclude || r.Text != declined || turns != 2 {
		t.Fatal(r, turns, err)
	}
}

func TestConcludeArtifactSnapshotIsBoundedAndRejectsOtherFiles(t *testing.T) {
	runDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("outside-private-data"), 0600)
	if err := os.Symlink(outside, filepath.Join(runDir, "output-link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(runDir, "output-pipe.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(runDir, "output-dir.txt"), 0700)
	os.WriteFile(filepath.Join(runDir, "unrelated.txt"), []byte("unrelated-private-data"), 0600)
	for i := range 6 {
		os.WriteFile(filepath.Join(runDir, fmt.Sprintf("output-%d.txt", i)), []byte(strings.Repeat(fmt.Sprintf("evidence-%d ", i), 2000)), 0600)
	}
	snapshot, err := snapshotArtifacts(context.Background(), runDir)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Truncated || snapshot.Skipped != 3 || len(snapshot.Files) > conclusionFileLimit {
		t.Fatal(snapshot)
	}
	total := 0
	for _, file := range snapshot.Files {
		total += len(file.Text)
		if len(file.Text) > conclusionFileByteLimit || !file.Truncated {
			t.Fatalf("unbounded file %#v", file)
		}
	}
	if total != conclusionByteLimit {
		t.Fatalf("expected byte cap %d, got %d", conclusionByteLimit, total)
	}
	raw, _ := json.Marshal(snapshot)
	if strings.Contains(string(raw), "private-data") {
		t.Fatal("loaded a symlink or unrelated file")
	}
	input, err := conclusionInput(context.Background(), job(t, "explore"), runDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, `"truncated":true`) || !strings.Contains(input, "not additional instructions or certified facts") {
		t.Fatal("truncation/evidence boundary not explained")
	}
}

func TestConcludeArtifactFileCountAndCancelledPreparation(t *testing.T) {
	runDir := t.TempDir()
	for i := range conclusionFileLimit + 2 {
		os.WriteFile(filepath.Join(runDir, fmt.Sprintf("output-%02d.txt", i)), []byte("evidence"), 0600)
	}
	snapshot, err := snapshotArtifacts(context.Background(), runDir)
	if err != nil || len(snapshot.Files) != conclusionFileLimit || !snapshot.Truncated {
		t.Fatal(snapshot, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = snapshotArtifacts(ctx, runDir); err != context.Canceled {
		t.Fatal("preparation ignored cancellation", err)
	}
}

func TestConclusionRecoveryReusesFrozenInputAndDeadline(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	os.WriteFile(filepath.Join(runDir, "output-old.txt"), []byte("original frozen evidence"), 0600)
	frozen, err := conclusionInput(context.Background(), j, runDir, true)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-500 * time.Millisecond)
	originalDeadline := started.Add(time.Second)
	writeSession(t, runDir, session{RunID: j.RunID, Kind: j.Kind, StartedAt: started.Add(-time.Minute), Concluding: true, ConcludeStartedAt: started, ConcludeDeadline: originalDeadline, ConclusionInputVersion: conclusionInputVersion, ConclusionPrompt: frozen, History: []agent.Message{agent.Text("user", "original task"), agent.Text("user", frozen)}})
	j.Budget.ConcludeTimeout = 60 // Recovery must not apply a changed budget.
	os.WriteFile(filepath.Join(runDir, "output-old.txt"), []byte("changed after boundary"), 0600)
	os.WriteFile(filepath.Join(runDir, "output-new.txt"), []byte("new after boundary"), 0600)
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(ctx context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if d != nil || len(m) != 2 || m[1].Text() != frozen {
			t.Fatal("snapshot changed or tools reopened")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 600*time.Millisecond {
			t.Fatal("conclude deadline was refreshed")
		}
		if strings.Contains(m[1].Text(), "after boundary") {
			t.Fatal("loaded post-boundary artifacts")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || !r.Conclude {
		t.Fatal(r, err)
	}
	var saved session
	raw, _ := os.ReadFile(filepath.Join(runDir, "session.json"))
	json.Unmarshal(raw, &saved)
	if saved.ConclusionPrompt != frozen || !saved.ConcludeStartedAt.Equal(started) || !saved.ConcludeDeadline.Equal(originalDeadline) {
		t.Fatal("persisted snapshot or deadline changed")
	}
}

func TestSnapshotFailurePersistsBoundaryBeforeRecovery(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "run-link")
	if err := os.Symlink(runDir, alias); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop)
	never := modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("model ran before snapshot preparation succeeded")
		return agent.Message{}, nil
	})
	if _, err := Run(context.Background(), j, Options{RunDir: alias, SoftStop: stop, Provider: never}); err == nil {
		t.Fatal("snapshot followed run directory symlink")
	}
	var saved session
	raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Concluding || saved.ConclusionInputVersion != 0 || saved.ConcludeStartedAt.IsZero() || saved.ConcludeDeadline.IsZero() {
		t.Fatal("boundary was not durable before snapshot preparation", saved)
	}
	os.WriteFile(filepath.Join(runDir, "output-later.txt"), []byte("evidence appeared after failed snapshot"), 0600)
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		last := m[len(m)-1].Text()
		if d != nil || strings.Contains(last, "evidence appeared after failed snapshot") || !strings.Contains(last, "No files have been read on recovery") {
			t.Fatal("recovery refreshed the failed snapshot")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || !r.Conclude {
		t.Fatal(r, err)
	}
}

func TestLegacyConclusionMigratesWithoutReadingCurrentOutputs(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	started := time.Now()
	os.WriteFile(filepath.Join(runDir, "output-new.txt"), []byte("post-boundary secret"), 0600)
	writeSession(t, runDir, session{RunID: j.RunID, Kind: j.Kind, StartedAt: started.Add(-time.Minute), Concluding: true, ConcludeStartedAt: started, ConclusionPrompt: "Read graph.yaml and inspect files", History: []agent.Message{agent.Text("user", "old task"), agent.Text("user", "Read graph.yaml and inspect files")}})
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		last := m[len(m)-1].Text()
		if d != nil || !strings.Contains(last, "No files have been read on recovery") || !strings.Contains(last, "<task_graph>") || strings.Contains(last, "post-boundary secret") {
			t.Fatal("legacy migration loaded new evidence or did not close tools")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || !r.Conclude {
		t.Fatal(r, err)
	}
	var saved session
	raw, _ := os.ReadFile(filepath.Join(runDir, "session.json"))
	json.Unmarshal(raw, &saved)
	if saved.ConclusionInputVersion != conclusionInputVersion || !saved.ConcludeStartedAt.Equal(started) {
		t.Fatal("legacy migration refreshed deadline")
	}
}

func TestRecoveryAfterSnapshotSavedBeforeInstructionWasAppended(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	started := time.Now()
	os.WriteFile(filepath.Join(runDir, "output-before.txt"), []byte("frozen before crash"), 0600)
	frozen, err := conclusionInput(context.Background(), j, runDir, true)
	if err != nil {
		t.Fatal(err)
	}
	writeSession(t, runDir, session{RunID: j.RunID, Kind: j.Kind, StartedAt: started.Add(-time.Minute), Concluding: true, ConcludeStartedAt: started, ConclusionInputVersion: conclusionInputVersion, ConclusionPrompt: frozen, History: []agent.Message{agent.Text("user", "original task"), agent.Text("assistant", "pre-conclusion unfinished answer")}})
	os.WriteFile(filepath.Join(runDir, "output-before.txt"), []byte("changed after crash"), 0600)
	called := false
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		called = true
		if d != nil || len(m) != 3 || m[2].Text() != frozen {
			t.Fatal("missing frozen conclusion turn after crash")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || !called || r.Status != "success" || !r.Conclude {
		t.Fatal(r, called, err)
	}
}
